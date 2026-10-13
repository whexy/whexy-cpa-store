package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Error types the Go gateway returns in error.type.
const (
	upstreamUsageLimit          = "GoUsageLimitError"
	upstreamPermission          = "permission_error"
	upstreamProtocolUnsupported = "ModelProtocolUnsupported"
)

// upstreamError is a non-2xx response from opencode.ai.
type upstreamError struct {
	Status  int
	Type    string
	Message string
	// LogID is x-opencode-log-id, which OpenCode support asks for.
	LogID string
}

func (e *upstreamError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenCode Go returned %d", e.Status)
	if e.Type != "" {
		b.WriteString(" " + e.Type)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.LogID != "" {
		b.WriteString(" (log id " + e.LogID + ")")
	}
	return redactSecrets(b.String())
}

// parseUpstreamError reads both gateway error shapes,
// {"type":"error","error":{"type":...,"message":...}} and
// {"error":{"type":...,"message":...}}, falling back to the raw body.
func parseUpstreamError(status int, headers http.Header, body []byte) *upstreamError {
	out := &upstreamError{Status: status, LogID: strings.TrimSpace(headers.Get("x-opencode-log-id"))}
	var parsed struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Error != nil {
			out.Type = strings.TrimSpace(parsed.Error.Type)
			out.Message = strings.TrimSpace(parsed.Error.Message)
		} else {
			out.Message = strings.TrimSpace(parsed.Message)
		}
	}
	if out.Type == "" && out.Message == "" {
		text := strings.TrimSpace(string(body))
		if len(text) > 512 {
			text = text[:512] + "…"
		}
		if text == "" {
			text = http.StatusText(status)
		}
		out.Message = text
	}
	out.Message = redactSecrets(out.Message)
	return out
}

// goRequestHeaders are the headers of plugin-originated calls to opencode.ai.
func goRequestHeaders(cfg settings, apiKey string) http.Header {
	return http.Header{
		"Authorization": {"Bearer " + apiKey},
		"Accept":        {"application/json"},
		"User-Agent":    {cfg.userAgent()},
	}
}
