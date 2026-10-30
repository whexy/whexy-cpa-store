package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// clientVersion is the Zed release whose wire format this plugin speaks. The
// edge rejects stale clients, so it is raised when a response names a newer
// minimum and otherwise refreshed from GitHub once a day.
const clientVersion = "1.26.2"

const (
	defaultLLMTokenLifetime = time.Hour
	tokenRefreshSkew        = time.Minute
	clientVersionTTL        = 6 * time.Hour
	clientVersionFallback   = clientVersion
)

// credential is one Zed account as the gateway sees it.
type credential struct {
	// ID keys the token cache; a user may sign in on several systems.
	ID             string
	UserID         string
	AccessToken    string
	SystemID       string
	OrganizationID string
	Username       string
	Plan           string
}

func (c credential) valid() bool {
	return strings.TrimSpace(c.UserID) != "" && strings.TrimSpace(c.AccessToken) != ""
}

func newCredential(userID, accessToken, systemID, organizationID, username, plan string) credential {
	userID = strings.TrimSpace(userID)
	accessToken = strings.TrimSpace(accessToken)
	systemID = strings.TrimSpace(systemID)
	return credential{
		ID:             userID + "|" + systemID,
		UserID:         userID,
		AccessToken:    accessToken,
		SystemID:       systemID,
		OrganizationID: strings.TrimSpace(organizationID),
		Username:       strings.TrimSpace(username),
		Plan:           strings.TrimSpace(plan),
	}
}

// accountInfo is the part of /client/users/me the plugin uses.
type accountInfo struct {
	UserID         int64
	Username       string
	OrganizationID string
	Plan           string
}

var (
	versionMu      sync.Mutex
	versionValue   string
	versionFetched time.Time
)

// zedVersion is the x-zed-version and User-Agent release the next request
// carries.
func zedVersion() string {
	versionMu.Lock()
	defer versionMu.Unlock()
	if versionValue == "" {
		versionValue = clientVersionFallback
		versionFetched = time.Now()
	}
	return versionValue
}

func raiseZedVersion(minimum string) {
	minimum = strings.TrimSpace(minimum)
	if !validVersion(minimum) {
		return
	}
	versionMu.Lock()
	defer versionMu.Unlock()
	if versionValue == "" || compareVersions(minimum, versionValue) > 0 {
		versionValue = minimum
		versionFetched = time.Now()
	}
}

func validVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if _, errAtoi := strconv.Atoi(part); errAtoi != nil {
			return false
		}
	}
	return true
}

func compareVersions(left, right string) int {
	a := strings.Split(left, ".")
	b := strings.Split(right, ".")
	for index := 0; index < 3; index++ {
		av, _ := strconv.Atoi(a[index])
		bv, _ := strconv.Atoi(b[index])
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

// refreshZedVersion reads the latest stable Zed release from GitHub. It is
// best effort: a failure keeps the current value. Background callers only.
func refreshZedVersion(ctx context.Context, h host) {
	versionMu.Lock()
	fresh := time.Since(versionFetched) < clientVersionTTL
	versionMu.Unlock()
	if fresh {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	resp, errDo := h.Do(ctx, hostRequest(http.MethodGet, "https://api.github.com/repos/zed-industries/zed/releases?per_page=20", nil, nil))
	if errDo != nil || resp.StatusCode != http.StatusOK {
		return
	}
	latest := ""
	for _, release := range gjson.ParseBytes(resp.Body).Array() {
		if release.Get("draft").Bool() || release.Get("prerelease").Bool() {
			continue
		}
		tag := strings.TrimPrefix(strings.TrimSpace(release.Get("tag_name").String()), "v")
		if validVersion(tag) {
			latest = tag
			break
		}
	}
	if latest == "" {
		return
	}
	raiseZedVersion(latest)
	versionMu.Lock()
	versionFetched = time.Now()
	versionMu.Unlock()
}

func userAgent(cfg settings) string {
	return cfg.userAgent()
}

func hostRequest(method, url string, headers http.Header, body []byte) pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{Method: method, URL: url, Headers: headers, Body: body}
}

// accountHeaders authenticate an account-scoped call: the user id and access
// token in one space-separated header.
func accountHeaders(cfg settings, cred credential) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", cred.UserID+" "+cred.AccessToken)
	headers.Set("Content-Type", "application/json")
	if cred.SystemID != "" {
		headers.Set("x-zed-system-id", cred.SystemID)
	}
	headers.Set("User-Agent", userAgent(cfg))
	return headers
}

// llmHeaders authenticate a gateway call with the minted JWT.
func llmHeaders(cfg settings, token string, statusMessages bool) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("x-zed-version", zedVersion())
	headers.Set("x-zed-client-supports-x-ai", "true")
	headers.Set("User-Agent", userAgent(cfg))
	if statusMessages {
		headers.Set("x-zed-client-supports-status-messages", "true")
		headers.Set("x-zed-client-supports-stream-ended-request-completion-status", "true")
	}
	return headers
}

// fetchAccountInfo reads the user, plan and organization the token request
// needs.
func fetchAccountInfo(ctx context.Context, h host, cfg settings, cred credential) (accountInfo, error) {
	resp, errDo := h.Do(ctx, hostRequest(http.MethodGet, cfg.BaseURL+"/client/users/me", accountHeaders(cfg, cred), []byte("{}")))
	if errDo != nil {
		return accountInfo{}, errDo
	}
	if resp.StatusCode != http.StatusOK {
		return accountInfo{}, upstreamErrorFrom(resp, "read the Zed account")
	}
	root := gjson.ParseBytes(resp.Body)
	info := accountInfo{
		UserID:         root.Get("user.legacy_user_id").Int(),
		Username:       root.Get("user.username").String(),
		OrganizationID: root.Get("default_organization_id").String(),
		Plan:           root.Get("plan.plan_v3").String(),
	}
	if info.Plan == "" {
		info.Plan = root.Get("plan.plan_v2").String()
	}
	if info.Plan == "" {
		info.Plan = root.Get("plan.plan").String()
	}
	if info.OrganizationID == "" {
		for _, org := range root.Get("organizations").Array() {
			if org.Get("is_personal").Bool() || info.OrganizationID == "" {
				info.OrganizationID = org.Get("id").String()
				if org.Get("is_personal").Bool() {
					break
				}
			}
		}
	}
	if info.OrganizationID == "" {
		return accountInfo{}, errors.New("the Zed account belongs to no organization")
	}
	return info, nil
}

// llmTokenCache keeps the short-lived JWT per credential.
type llmTokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedLLMToken
	now    func() time.Time
}

type cachedLLMToken struct {
	token     string
	expiresAt time.Time
}

var llmTokens = &llmTokenCache{tokens: make(map[string]cachedLLMToken), now: time.Now}

func (c *llmTokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.tokens[key]
	if !ok || c.now().Add(tokenRefreshSkew).After(cached.expiresAt) {
		return "", false
	}
	return cached.token, true
}

func (c *llmTokenCache) put(key, token string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[key] = cachedLLMToken{token: token, expiresAt: expiresAt}
}

func (c *llmTokenCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, key)
}

func (c *llmTokenCache) key(cred credential) string {
	return cred.ID + "|" + cred.OrganizationID
}

// llmToken returns the cached JWT, minting one when needed.
func llmToken(ctx context.Context, h host, cfg settings, cred credential) (string, error) {
	if !cred.valid() {
		return "", errors.New("Zed credential is incomplete")
	}
	key := llmTokens.key(cred)
	if token, ok := llmTokens.get(key); ok {
		return token, nil
	}
	organizationID := cred.OrganizationID
	if organizationID == "" {
		info, errInfo := fetchAccountInfo(ctx, h, cfg, cred)
		if errInfo != nil {
			return "", errInfo
		}
		organizationID = info.OrganizationID
	}
	body, _ := json.Marshal(map[string]string{"organization_id": organizationID})
	resp, errDo := h.Do(ctx, hostRequest(http.MethodPost, cfg.BaseURL+"/client/llm_tokens", accountHeaders(cfg, cred), body))
	if errDo != nil {
		return "", errDo
	}
	if resp.StatusCode != http.StatusOK {
		return "", upstreamErrorFrom(resp, "mint a Zed LLM token")
	}
	token := gjson.GetBytes(resp.Body, "token").String()
	if token == "" {
		return "", errors.New("Zed returned no LLM token")
	}
	expiresAt := jwtExpiry(token)
	if expiresAt.IsZero() {
		expiresAt = llmTokens.now().Add(defaultLLMTokenLifetime)
	}
	llmTokens.put(key, token, expiresAt)
	return token, nil
}

// jwtExpiry reads a JWT's exp without verifying it.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload := parts[1]
	if padding := len(payload) % 4; padding != 0 {
		payload += strings.Repeat("=", 4-padding)
	}
	decoded, errDecode := base64.URLEncoding.DecodeString(payload)
	if errDecode != nil {
		return time.Time{}
	}
	exp := gjson.GetBytes(decoded, "exp").Int()
	if exp <= 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0)
}

// zedModel is one entry of GET /models.
type zedModel struct {
	ID                 string
	Provider           string
	DisplayName        string
	ContextLength      int64
	OutputTokens       int64
	SupportsTools      bool
	SupportsImages     bool
	SupportsThinking   bool
	SupportsFastMode   bool
	IsDisabled         bool
	DisabledReason     string
	EffortLevels       []string
	SupportsStreaming  bool
	SupportsParallel   bool
	SupportsCompaction bool
}

func (m zedModel) nativeProtocol() string {
	switch m.Provider {
	case "anthropic":
		return "claude"
	case "open_ai":
		return "openai-response"
	case "google":
		return "gemini"
	case "x_ai":
		return "openai"
	default:
		return ""
	}
}

// fetchModels reads the account's plan-filtered catalog.
func fetchModels(ctx context.Context, h host, cfg settings, cred credential) ([]zedModel, error) {
	token, errToken := llmToken(ctx, h, cfg, cred)
	if errToken != nil {
		return nil, errToken
	}
	headers := llmHeaders(cfg, token, false)
	headers.Set("x-zed-client-supports-x-ai", "true")
	resp, errDo := h.Do(ctx, hostRequest(http.MethodGet, cfg.BaseURL+"/models", headers, nil))
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode == http.StatusUnauthorized {
		llmTokens.drop(llmTokens.key(cred))
		return nil, upstreamErrorFrom(resp, "list Zed models")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, upstreamErrorFrom(resp, "list Zed models")
	}
	root := gjson.ParseBytes(resp.Body)
	models := make([]zedModel, 0, len(root.Get("models").Array()))
	for _, entry := range root.Get("models").Array() {
		model := zedModel{
			ID:                 entry.Get("id").String(),
			Provider:           entry.Get("provider").String(),
			DisplayName:        entry.Get("display_name").String(),
			ContextLength:      entry.Get("max_token_count").Int(),
			OutputTokens:       entry.Get("max_output_tokens").Int(),
			SupportsTools:      entry.Get("supports_tools").Bool(),
			SupportsImages:     entry.Get("supports_images").Bool(),
			SupportsThinking:   entry.Get("supports_thinking").Bool(),
			SupportsFastMode:   entry.Get("supports_fast_mode").Bool(),
			IsDisabled:         entry.Get("is_disabled").Bool(),
			DisabledReason:     entry.Get("disabled_reason").String(),
			SupportsStreaming:  entry.Get("supports_streaming_tools").Bool(),
			SupportsParallel:   entry.Get("supports_parallel_tool_calls").Bool(),
			SupportsCompaction: entry.Get("supports_server_side_compaction").Bool(),
		}
		for _, level := range entry.Get("supported_effort_levels").Array() {
			if value := level.Get("value").String(); value != "" {
				model.EffortLevels = append(model.EffortLevels, value)
			}
		}
		if model.ID != "" && model.Provider != "" {
			models = append(models, model)
		}
	}
	return models, nil
}

// completionEnvelope is the body of POST /completions.
type completionEnvelope struct {
	ThreadID        string          `json:"thread_id,omitempty"`
	PromptID        string          `json:"prompt_id,omitempty"`
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	ProviderRequest json.RawMessage `json:"provider_request"`
}

func completionBody(provider, model string, request []byte) ([]byte, error) {
	envelope := completionEnvelope{
		ThreadID:        randomUUID(),
		PromptID:        randomUUID(),
		Provider:        provider,
		Model:           model,
		ProviderRequest: request,
	}
	return json.Marshal(envelope)
}

// zedFailure is an upstream error with the status CPA should act on.
type zedFailure struct {
	Status         int
	Message        string
	PlanRestricted bool
	// RetryAfter is a Retry-After the upstream named, when any.
	RetryAfter time.Duration
}

func (e *zedFailure) Error() string { return e.Message }

func upstreamErrorFrom(resp pluginapi.HTTPResponse, action string) error {
	if minimum := resp.Headers.Get("x-zed-minimum-required-version"); minimum != "" {
		raiseZedVersion(minimum)
	}
	message := strings.TrimSpace(string(resp.Body))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	failure := &zedFailure{Status: resp.StatusCode, Message: action + ": " + firstLine(message)}
	if resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(message), "plan") {
		failure.PlanRestricted = true
	}
	if seconds, errAtoi := strconv.Atoi(strings.TrimSpace(resp.Headers.Get("Retry-After"))); errAtoi == nil && seconds > 0 {
		failure.RetryAfter = time.Duration(seconds) * time.Second
	}
	return failure
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	text = strings.TrimSpace(text)
	if len(text) > 400 {
		text = text[:400]
	}
	return text
}

// dashboardUsage is the dashboard's own plan summary. It needs a
// dashboard.zed.dev session cookie, which the account token cannot replace.
type dashboardUsage struct {
	Plan          string
	SpendCents    float64
	LimitCents    float64
	HasLimit      bool
	EditUsed      int64
	EditLimit     int64
	EditUnlimited bool
}

func fetchDashboardUsage(ctx context.Context, h host, cfg settings) (dashboardUsage, error) {
	if cfg.DashboardCookie == "" {
		return dashboardUsage{}, errors.New("no dashboard cookie is configured")
	}
	headers := http.Header{}
	headers.Set("Cookie", cfg.DashboardCookie)
	headers.Set("Accept", "application/json")
	headers.Set("Origin", "https://dashboard.zed.dev")
	headers.Set("Referer", "https://dashboard.zed.dev/")
	headers.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
	resp, errDo := h.Do(ctx, hostRequest(http.MethodGet, cfg.BaseURL+"/frontend/billing/usage", headers, nil))
	if errDo != nil {
		return dashboardUsage{}, errDo
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return dashboardUsage{}, errors.New("the dashboard cookie is not signed in")
	}
	if resp.StatusCode != http.StatusOK {
		return dashboardUsage{}, upstreamErrorFrom(resp, "read the dashboard usage")
	}
	root := gjson.ParseBytes(resp.Body)
	usage := dashboardUsage{Plan: root.Get("plan").String()}
	usage.SpendCents = root.Get("current_usage.token_spend.spend_in_cents").Float()
	limit := root.Get("current_usage.token_spend.limit_in_cents")
	usage.HasLimit = limit.Exists() && limit.Type != gjson.Null
	usage.LimitCents = limit.Float()
	usage.EditUsed = root.Get("current_usage.edit_predictions.used").Int()
	editLimit := root.Get("current_usage.edit_predictions.limit")
	usage.EditUnlimited = editLimit.Type == gjson.String && editLimit.String() == "unlimited"
	usage.EditLimit = editLimit.Int()
	return usage, nil
}

func randomUUID() string {
	var b [16]byte
	if _, errRead := rand.Read(b[:]); errRead != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
