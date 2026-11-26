package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

type rpcQuotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// Window names Management Center orders and sizes (5h, weekly); monthly
// sorts after them.
var quotaBucketNames = map[string][2]string{
	"rolling": {"5h", "5-hour"},
	"weekly":  {"weekly", "Weekly"},
	"monthly": {"monthly", "Monthly"},
}

func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req rpcQuotaFetchRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode quota fetch request: %w", errUnmarshal)
	}
	// The 5-hour, weekly and monthly windows are the Go subscription's; Zen
	// models bill Console credits and Free models nothing, so neither has any.
	switch authFamily(req.Attributes) {
	case familyZen:
		return rpc.OK(pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: zenDisplayName}})
	case familyFree:
		return rpc.OK(pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: freeDisplayName}})
	}
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	var expired *loginExpiredError
	if errors.As(errCred, &expired) {
		return rpc.Fail("login_expired", errCred.Error(), rpc.WithStatus(http.StatusUnauthorized)), nil
	}
	if errCred != nil {
		return rpc.Fail("invalid_auth", errCred.Error(), rpc.WithStatus(http.StatusBadRequest)), nil
	}
	report, errFetch := fetchUsage(context.Background(), host.New(req.HostCallbackID), currentSettings(), cred.Bearer)
	if errFetch != nil {
		return rpc.Fail("upstream_error", errFetch.Error(), rpc.WithStatus(quotaErrorStatus(errFetch))), nil
	}
	return rpc.OK(quotaResponse(report, cred.Name))
}

func quotaErrorStatus(err error) int {
	var upstream *upstreamError
	if errors.As(err, &upstream) && upstream.Status == http.StatusUnauthorized {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// quotaGroupName names a key, because Management Center titles every card
// from one multi-key file with the same file name.
func quotaGroupName(key keyEntry) string {
	if key.Label == "" {
		return maskKey(key.APIKey)
	}
	return key.Label + " · " + maskKey(key.APIKey)
}

func quotaResponse(report usageReport, groupName string) pluginapi.QuotaFetchResponse {
	buckets := make([]pluginapi.QuotaBucket, 0, len(report.Windows))
	for _, window := range report.Windows {
		names, ok := quotaBucketNames[window.Name]
		if !ok {
			continue
		}
		description := names[1]
		if window.Status != "" && window.Status != "ok" {
			description += " (" + window.Status + ")"
		}
		bucket := pluginapi.QuotaBucket{
			Window:            names[0],
			RemainingFraction: window.remainingFraction(),
			Description:       description,
		}
		if !window.ResetsAt.IsZero() {
			bucket.ResetTime = window.ResetsAt.Format(time.RFC3339)
		}
		buckets = append(buckets, bucket)
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: displayName},
		Groups:       []pluginapi.QuotaGroup{{DisplayName: groupName, Buckets: buckets}},
	}
}
