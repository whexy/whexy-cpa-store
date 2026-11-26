package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func fakeJWTExpiring(t *testing.T, lifetime time.Duration) string {
	t.Helper()
	exp := time.Now().Add(lifetime).Unix()
	return fakeJWTString(exp)
}

func fakeJWTString(exp int64) string {
	payload := []byte(`{"exp":` + itoa(exp) + `}`)
	return "eyJhbGciOiJIUzI1NiJ9." + base64Raw(payload) + ".c2ln"
}

func base64Raw(raw []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for index := 0; index < len(raw); index += 3 {
		var chunk [3]byte
		n := copy(chunk[:], raw[index:])
		out.WriteByte(alphabet[chunk[0]>>2])
		out.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out.WriteByte(alphabet[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		}
		if n > 2 {
			out.WriteByte(alphabet[chunk[2]&0x3f])
		}
	}
	return out.String()
}

func installZedHost(t *testing.T) {
	t.Helper()
	installHost(t).HandleHTTP(func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/client/llm_tokens"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"token": fakeJWTExpiring(t, time.Hour)})}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
				"models": []any{
					map[string]any{"id": "claude-sonnet-5-5", "provider": "anthropic", "display_name": "Claude Sonnet 5.5", "max_token_count": 1000000, "max_output_tokens": 64000, "supports_tools": true},
				},
			})}, nil
		case strings.HasSuffix(req.URL, "/completions"):
			events := claudeEvents()
			var body strings.Builder
			for _, event := range events {
				body.WriteString(`{"event":`)
				body.Write(event)
				body.WriteString("}\n")
			}
			body.WriteString(`{"status":"stream_ended"}`)
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body.String())}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 404, Body: []byte("unexpected " + req.URL)}, nil
	})
}

func TestExecutorExecuteClaude(t *testing.T) {
	installZedHost(t)
	useSettings(t, defaultSettings())
	storage := testAuthJSON(t)
	payload := mustJSON(t, map[string]any{
		"model":       "claude-sonnet-5-5",
		"max_tokens":  100000,
		"temperature": 0.5,
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:          "zed-oauth-tester.json",
		Model:           "zed/claude-sonnet-5-5",
		Format:          "claude",
		SourceFormat:    "claude",
		Payload:         payload,
		OriginalRequest: payload,
		StorageJSON:     storage,
		Headers:         http.Header{},
	}}
	raw, errCall := executorExecute(mustJSON(t, req))
	if errCall != nil {
		t.Fatal(errCall)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if !strings.Contains(string(response.Payload), `"text":"pong"`) {
		t.Fatalf("response: %s", response.Payload)
	}
}

func TestExecutorPlanRestrictionIsNotAuthFailure(t *testing.T) {
	cases := []struct {
		body   string
		status int
	}{
		// A 400 never cools the account down.
		{`Access to model "claude-opus-5-5" is not included in your plan`, http.StatusBadRequest},
		// An account-wide failure keeps its 403 so CPA moves on.
		{`Your plan has expired`, http.StatusForbidden},
	}
	for _, c := range cases {
		installHost(t).HandleHTTP(func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			switch {
			case strings.HasSuffix(req.URL, "/client/llm_tokens"):
				return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"token": fakeJWTExpiring(t, time.Hour)})}, nil
			case strings.HasSuffix(req.URL, "/models"):
				return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"models": []any{map[string]any{"id": "claude-opus-5-5", "provider": "anthropic"}}})}, nil
			case strings.HasSuffix(req.URL, "/completions"):
				return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(c.body)}, nil
			}
			return pluginapi.HTTPResponse{StatusCode: 404}, nil
		})
		useSettings(t, defaultSettings())
		payload := mustJSON(t, map[string]any{"model": "claude-opus-5-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "a",
			Model:        "zed/claude-opus-5-5",
			Format:       "claude",
			SourceFormat: "claude",
			Payload:      payload,
			StorageJSON:  testAuthJSON(t),
		}}
		raw, _ := executorExecute(mustJSON(t, req))
		env := decodeEnvelope(t, raw)
		if env.OK || env.Error == nil {
			t.Fatalf("expected failure envelope: %s", raw)
		}
		if env.Error.HTTPStatus != c.status {
			t.Errorf("%q: status = %d, want %d: %s", c.body, env.Error.HTTPStatus, c.status, raw)
		}
	}
}

func claudeModelHost(t *testing.T, completions string) {
	t.Helper()
	installHost(t).HandleHTTP(func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/client/llm_tokens"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"token": fakeJWTExpiring(t, time.Hour)})}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"models": []any{map[string]any{"id": "claude-sonnet-5-5", "provider": "anthropic", "max_output_tokens": 64000}}})}, nil
		case strings.HasSuffix(req.URL, "/completions"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(completions)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	})
}

func TestExecutorRejectsFailedNonStreamingGeneration(t *testing.T) {
	claudeModelHost(t, "{\"event\":{\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}}\n"+
		"{\"event\":{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}}\n"+
		"{\"status\":\"stream_ended\"}")
	useSettings(t, defaultSettings())
	payload := mustJSON(t, map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "a", Model: "zed/claude-sonnet-5-5", Format: "claude", SourceFormat: "claude",
		Payload: payload, StorageJSON: testAuthJSON(t),
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil {
		t.Fatalf("a failed generation must not be a success: %s", raw)
	}
}

func TestExecutorRejectsTruncatedNonStreamingGeneration(t *testing.T) {
	claudeModelHost(t, "{\"event\":{\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}}\n"+
		"{\"status\":\"stream_ended\"}")
	useSettings(t, defaultSettings())
	payload := mustJSON(t, map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "a", Model: "zed/claude-sonnet-5-5", Format: "claude", SourceFormat: "claude",
		Payload: payload, StorageJSON: testAuthJSON(t),
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil {
		t.Fatalf("a truncated generation must not be a success: %s", raw)
	}
}

func TestExecutorCatalogFailureKeepsStatus(t *testing.T) {
	installHost(t).HandleHTTP(func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/client/llm_tokens"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"token": fakeJWTExpiring(t, time.Hour)})}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 503, Body: []byte("catalog down")}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	})
	useSettings(t, defaultSettings())
	payload := mustJSON(t, map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "a", Model: "zed/claude-sonnet-5-5", Format: "claude", SourceFormat: "claude",
		Payload: payload, StorageJSON: testAuthJSON(t),
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil {
		t.Fatalf("catalog failure must fail: %s", raw)
	}
	if env.Error.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("catalog failure status = %d, want 502: %s", env.Error.HTTPStatus, raw)
	}
}

func TestExecutorRevokedLoginIsUnauthorized(t *testing.T) {
	fake := installHost(t)
	mints := 0
	fake.HandleHTTP(func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/client/llm_tokens"):
			// The first JWT serves the catalog; the login is revoked before
			// the retry mints another.
			mints++
			if mints > 1 {
				return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte("invalid access token")}, nil
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"token": fakeJWTExpiring(t, time.Hour)})}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"models": []any{map[string]any{"id": "claude-sonnet-5-5", "provider": "anthropic"}}})}, nil
		case strings.HasSuffix(req.URL, "/completions"):
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte("expired token")}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	})
	useSettings(t, defaultSettings())
	payload := mustJSON(t, map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "a", Model: "zed/claude-sonnet-5-5", Format: "claude", SourceFormat: "claude",
		Payload: payload, StorageJSON: testAuthJSON(t),
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("a revoked login must be a 401 CPA refreshes: %s", raw)
	}
}

func TestExecutorAppliesThinkingSuffix(t *testing.T) {
	fake := installHost(t)
	var completions gjson.Result
	fake.HandleHTTP(func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/client/llm_tokens"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"token": fakeJWTExpiring(t, time.Hour)})}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"models": []any{map[string]any{
				"id": "claude-sonnet-5-5", "provider": "anthropic", "supports_thinking": true,
				"supported_effort_levels": []any{map[string]any{"value": "low"}, map[string]any{"value": "medium"}, map[string]any{"value": "high"}},
			}}})}, nil
		case strings.HasSuffix(req.URL, "/completions"):
			completions = gjson.ParseBytes(req.Body)
			return pluginapi.HTTPResponse{StatusCode: 500, Body: []byte("stop")}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	})
	useSettings(t, defaultSettings())
	payload := mustJSON(t, map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "a", Model: "zed/claude-sonnet-5-5(high)", Format: "claude", SourceFormat: "claude",
		Payload: payload, StorageJSON: testAuthJSON(t),
	}}
	_, _ = executorExecute(mustJSON(t, req))
	if got := completions.Get("provider_request.output_config.effort").String(); got != "high" {
		t.Fatalf("effort = %q; request was %s", got, completions.Raw)
	}
	if got := completions.Get("provider_request.thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking = %q; request was %s", got, completions.Raw)
	}
}

func TestSplitModelSuffix(t *testing.T) {
	model, suffix := splitModelSuffix("zed/claude-sonnet-5-5(16384)")
	if model != "zed/claude-sonnet-5-5" || suffix != "16384" {
		t.Fatalf("split = %q %q", model, suffix)
	}
	model, suffix = splitModelSuffix("zed/gpt-6-luna")
	if model != "zed/gpt-6-luna" || suffix != "" {
		t.Fatalf("split = %q %q", model, suffix)
	}
}
