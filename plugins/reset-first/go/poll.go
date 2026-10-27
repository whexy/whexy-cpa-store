package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Responses keep the accounts CPA uses current; the usage endpoints cover the
// ones it does not, and use spent outside CPA. These are the endpoints and
// headers Management Center's quota page sends.
const (
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

	codexUserAgent  = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	claudeUserAgent = "claude-cli/2.1.280 (external, cli)"

	pollTimeout = 20 * time.Second
	// A reset is read back shortly after it passes, once the provider has
	// opened the next window.
	afterReset = 2 * time.Minute
	// Each consecutive 429 doubles the wait before the next read, up to this
	// many doublings of the poll interval.
	maxRateLimitDoublings = 4
)

type pollTarget struct {
	id        string
	authIndex string
	provider  string
	// rejected fingerprints the access token the usage endpoint last
	// rejected.
	rejected string
}

// authFile is the part of a Codex or Claude auth file a poll needs.
type authFile struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	ProxyURL    string `json:"proxy_url"`
	BaseURL     string `json:"base_url"`
}

type pollResult struct {
	quota   *quota
	plan    string
	problem string
	// held: the auth file still holds the rejected token, so the endpoint
	// was not called.
	held bool
	// rejected fingerprints the access token the endpoint just rejected.
	rejected string
	// limited: the endpoint answered 429, asking to wait retryAfter when it
	// said how long.
	limited    bool
	retryAfter time.Duration
}

// poll reads one account's usage endpoint. It never refreshes a token: CPA
// owns the credential, and a rejected token waits for CPA to refresh it.
func poll(ctx context.Context, target pollTarget, now time.Time) pollResult {
	raw, errFile := hostAuthFile(target.authIndex)
	if errFile != nil {
		return pollResult{problem: "auth file unreadable"}
	}
	var file authFile
	if json.Unmarshal(raw, &file) != nil || strings.TrimSpace(file.AccessToken) == "" {
		return pollResult{problem: "no access token in the auth file"}
	}
	// host.http.do applies only the global proxy, so a request for an account
	// with its own proxy or upstream would reach the provider from somewhere
	// CPA never sends that account's traffic.
	if strings.TrimSpace(file.ProxyURL) != "" || strings.TrimSpace(file.BaseURL) != "" {
		return pollResult{problem: "own proxy or base URL, read from responses only"}
	}
	// A provider rejects the same token every time and may start answering
	// 429 to the repeats, so only a token CPA stored since is sent.
	token := strings.TrimSpace(file.AccessToken)
	fingerprint := tokenFingerprint(token)
	if fingerprint == target.rejected {
		return pollResult{held: true}
	}
	headers := http.Header{
		"Authorization": {"Bearer " + token},
		"Accept":        {"application/json"},
	}
	url := claudeUsageURL
	if target.provider == providerCodex {
		url = codexUsageURL
		headers.Set("User-Agent", codexUserAgent)
		if accountID := strings.TrimSpace(file.AccountID); accountID != "" {
			headers.Set("Chatgpt-Account-Id", accountID)
		}
	} else {
		headers.Set("User-Agent", claudeUserAgent)
		headers.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
	resp, errGet := hostGet(ctx, url, headers)
	if errGet != nil {
		return pollResult{problem: "usage endpoint unreachable"}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return pollResult{problem: fmt.Sprintf("token rejected (HTTP %d)", resp.StatusCode), rejected: fingerprint}
	case resp.StatusCode == http.StatusTooManyRequests:
		return pollResult{problem: "usage endpoint answered HTTP 429", limited: true, retryAfter: retryAfter(resp.Headers)}
	case resp.StatusCode != http.StatusOK:
		return pollResult{problem: fmt.Sprintf("usage endpoint answered HTTP %d", resp.StatusCode)}
	}
	if target.provider == providerCodex {
		if q, plan, ok := codexQuotaFromUsage(resp.Body, now); ok {
			return pollResult{quota: q, plan: plan}
		}
	} else if q, ok := claudeQuotaFromUsage(resp.Body, now); ok {
		return pollResult{quota: q}
	}
	return pollResult{problem: "usage endpoint sent no quota"}
}

// tokenFingerprint identifies a token without keeping the secret in memory.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// retryAfter reads Retry-After in seconds, the form Anthropic and OpenAI
// send; zero when it is absent.
func retryAfter(headers http.Header) time.Duration {
	seconds, errParse := strconv.Atoi(strings.TrimSpace(headers.Get("Retry-After")))
	if errParse != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// applyPoll stores a poll's outcome and schedules the next one: after the
// interval, or soon after the next reset, whichever comes first. A 429 holds
// off every read until a wait that doubles with each consecutive 429, or the
// one the endpoint asked for if longer.
func (s *state) applyPoll(target pollTarget, result pollResult, now time.Time, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[target.id]
	if a == nil {
		return
	}
	if result.held {
		a.nextPoll = now.Add(interval)
		return
	}
	a.rejected = result.rejected
	if result.limited {
		a.limited++
		a.retryAt = now.Add(max(interval<<min(a.limited, maxRateLimitDoublings), result.retryAfter))
	} else {
		a.limited, a.retryAt = 0, time.Time{}
	}
	if a.Problem != result.problem {
		a.Problem = result.problem
		s.dirty = true
	}
	if result.plan != "" && result.plan != a.Plan {
		a.Plan = result.plan
		s.dirty = true
	}
	s.applyReadingLocked(a, result.quota)
	a.nextPoll = nextPoll(a.Quota, now, interval)
}

// nextPoll is the interval away, or sooner when a window resets first.
func nextPoll(q *quota, now time.Time, interval time.Duration) time.Time {
	next := now.Add(interval)
	if q == nil {
		return next
	}
	for _, w := range q.Windows {
		if after := w.ResetAt.Add(afterReset); w.ResetAt.After(now) && after.Before(next) {
			next = after
		}
	}
	return next
}

type hostHTTPRequest struct {
	OperationID string      `json:"operation_id,omitempty"`
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	Headers     http.Header `json:"headers,omitempty"`
}

type hostOperation struct {
	OperationID string `json:"operation_id,omitempty"`
}

// hostGet sends one GET through the host's HTTP client under a host
// operation, because a blocked host.http.do can only be interrupted by
// cancelling its operation.
func hostGet(ctx context.Context, url string, headers http.Header) (hostHTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	var opened hostOperation
	if errOpen := invokeHost(pluginabi.MethodHostHTTPOperationOpen, hostOperation{}, &opened); errOpen != nil {
		return hostHTTPResponse{}, errOpen
	}
	stop := context.AfterFunc(ctx, func() {
		_ = invokeHost(pluginabi.MethodHostHTTPCancel, opened, nil)
	})
	defer stop()
	var resp hostHTTPResponse
	errCall := invokeHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{OperationID: opened.OperationID, Method: http.MethodGet, URL: url, Headers: headers}, &resp)
	if errCtx := ctx.Err(); errCtx != nil {
		return hostHTTPResponse{}, errCtx
	}
	return resp, errCall
}

// hostHTTPResponse is pluginapi.HTTPResponse, which travels without JSON tags.
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
