package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type rpcQuotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req rpcQuotaFetchRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode quota fetch request: %w", errUnmarshal)
	}
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	if errCred != nil {
		return errorEnvelope("invalid_auth", "Zed auth "+req.AuthID+": "+errCred.Error(), http.StatusUnauthorized), nil
	}
	cfg := currentSettings()
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     planLabel(cred.Plan),
			TierName: cred.Username,
			TierID:   cred.UserID,
		},
	}
	if !cfg.dashboardApplies(cred) {
		// Zed exposes plan usage only to dashboard.zed.dev, and the cookie is
		// for one account. Without a matching pair there is nothing to meter;
		// the plan card still renders.
		return okEnvelope(response)
	}
	usage, errUsage := fetchDashboardUsage(context.Background(), newHost(req.HostCallbackID), cfg)
	if errUsage != nil {
		return okEnvelope(response)
	}
	if usage.Plan != "" {
		response.Subscription.Plan = planLabel(usage.Plan)
	}
	response.Summary = append(response.Summary, pluginapi.QuotaMetric{
		Key:      "token_spend",
		Label:    "Token spend this month",
		Value:    math.Round(usage.SpendCents) / 100,
		Format:   "currency",
		Currency: "USD",
	})
	if usage.HasLimit {
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:      "token_limit",
			Label:    "Monthly spend limit",
			Value:    math.Round(usage.LimitCents) / 100,
			Format:   "currency",
			Currency: "USD",
		})
	}
	if usage.EditUnlimited || usage.EditLimit > 0 {
		limit := "unlimited"
		if !usage.EditUnlimited {
			limit = fmt.Sprintf("%d", usage.EditLimit)
		}
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:   "edit_predictions",
			Label: "Edit predictions used",
			Value: float64(usage.EditUsed),
			Unit:  "of " + limit,
		})
	}
	if usage.HasLimit && usage.LimitCents > 0 {
		remaining := 1 - usage.SpendCents/usage.LimitCents
		remaining = math.Max(0, math.Min(1, remaining))
		response.Groups = append(response.Groups, pluginapi.QuotaGroup{
			DisplayName: "Monthly token spend",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "monthly",
				RemainingFraction: remaining,
				Description:       fmt.Sprintf("$%.2f of $%.2f used", usage.SpendCents/100, usage.LimitCents/100),
			}},
		})
	}
	return okEnvelope(response)
}
