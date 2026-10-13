package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	var expired *loginExpiredError
	if errors.As(errCred, &expired) {
		return errorEnvelope("login_expired", errCred.Error(), http.StatusUnauthorized), nil
	}
	if errCred != nil {
		return errorEnvelope("invalid_auth", errCred.Error(), http.StatusBadRequest), nil
	}
	report, errFetch := fetchUsage(context.Background(), newHost(req.HostCallbackID), currentSettings(), cred.Bearer)
	if errFetch != nil {
		return errorEnvelope("upstream_error", errFetch.Error(), quotaErrorStatus(errFetch)), nil
	}
	return okEnvelope(quotaResponse(report, cred.Name))
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
