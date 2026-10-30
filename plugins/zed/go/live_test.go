package main

// Live checks against a real Zed account. They are skipped unless
// ZED_LIVE_AUTH points at a JSON file holding user_id, access_token and
// optionally system_id and organization_id. The file is never committed.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func liveAuth(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("ZED_LIVE_AUTH")
	if path == "" {
		t.Skip("ZED_LIVE_AUTH is not set")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var source struct {
		UserID         string `json:"user_id"`
		AccessToken    string `json:"access_token"`
		Credential     string `json:"credential"`
		SystemID       string `json:"system_id"`
		OrganizationID string `json:"organization_id"`
		Username       string `json:"username"`
	}
	if errUnmarshal := json.Unmarshal(raw, &source); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if source.AccessToken == "" {
		source.AccessToken = source.Credential
	}
	storage := map[string]any{
		"type":         "zed",
		"user_id":      source.UserID,
		"access_token": source.AccessToken,
		"system_id":    source.SystemID,
	}
	if source.OrganizationID != "" {
		storage["organization_id"] = source.OrganizationID
	}
	if source.Username != "" {
		storage["username"] = source.Username
	}
	return mustJSON(t, storage)
}

func installLiveHost(t *testing.T) *fakeHost {
	t.Helper()
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(json.RawMessage) (any, error) {
		return map[string]any{"operation_id": "live"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPCancel, func(json.RawMessage) (any, error) { return map[string]any{}, nil })
	client := &http.Client{Timeout: 120 * time.Second}
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, strings.NewReader(string(req.Body)))
		if errNew != nil {
			return nil, errNew
		}
		for name, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(name, value)
			}
		}
		resp, errDo := client.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, nil
	})
	return fake
}

func TestLiveZedCatalog(t *testing.T) {
	installLiveHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	cred, errCred := resolveCredential(storage, nil, nil)
	if errCred != nil {
		t.Fatal(errCred)
	}
	models, errModels := catalogFor(t.Context(), newHost(""), currentSettings(), cred, true)
	if errModels != nil {
		t.Fatal(errModels)
	}
	if len(models) == 0 {
		t.Fatal("no models returned")
	}
	seen := false
	for _, model := range models {
		if model.nativeProtocol() == "" {
			t.Errorf("model %s has no protocol", model.ID)
		}
		if model.ID == "claude-sonnet-5-5" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("expected claude-sonnet-5-5 in %d models", len(models))
	}
	t.Logf("%d models", len(models))
}

func TestLiveZedClaudeCompletion(t *testing.T) {
	installLiveHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "Reply with exactly: pong"},
		}}},
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/claude-sonnet-5-5",
		Format:       "claude",
		SourceFormat: "claude",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	response := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if !strings.Contains(string(response.Payload), "pong") {
		t.Fatalf("response: %s", response.Payload)
	}
}

func TestLiveZedResponsesCompletion(t *testing.T) {
	installLiveHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":             "gpt-6-luna",
		"max_output_tokens": 32,
		"input": []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "Reply with exactly: pong"}},
		}},
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/gpt-6-luna",
		Format:       "openai-response",
		SourceFormat: "openai-response",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	response := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if !strings.Contains(string(response.Payload), "pong") {
		t.Fatalf("response: %s", response.Payload)
	}
}

func TestLiveZedChatToGemini(t *testing.T) {
	installLiveHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":      "gemini-4-argon",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}},
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/gemini-4-argon",
		Format:       "openai",
		SourceFormat: "openai",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	response := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if !strings.Contains(string(response.Payload), "pong") {
		t.Fatalf("response: %s", response.Payload)
	}
}

// liveStreamHost serves host.http.do_stream from a real upstream body and
// records the chunks the plugin emits.
type liveStreamHost struct {
	mu       sync.Mutex
	body     []byte
	offset   int
	chunks   [][]byte
	closed   bool
	closeErr string
	done     chan struct{}
}

func installLiveStreamHost(t *testing.T) *liveStreamHost {
	t.Helper()
	fake := installLiveHost(t)
	stream := &liveStreamHost{done: make(chan struct{})}
	fake.handle(pluginabi.MethodHostHTTPDoStream, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, strings.NewReader(string(req.Body)))
		if errNew != nil {
			return nil, errNew
		}
		for name, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(name, value)
			}
		}
		resp, errDo := (&http.Client{Timeout: 120 * time.Second}).Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		stream.mu.Lock()
		stream.body = body
		stream.mu.Unlock()
		return hostHTTPStreamStart{StatusCode: resp.StatusCode, Headers: resp.Header, StreamID: "live-stream"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPStreamRead, func(json.RawMessage) (any, error) {
		stream.mu.Lock()
		defer stream.mu.Unlock()
		if stream.offset >= len(stream.body) {
			return hostStreamReadResponse{Done: true}, nil
		}
		end := stream.offset + 80
		if end > len(stream.body) {
			end = len(stream.body)
		}
		chunk := append([]byte(nil), stream.body[stream.offset:end]...)
		stream.offset = end
		return hostStreamReadResponse{Payload: chunk}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPStreamClose, func(json.RawMessage) (any, error) { return map[string]any{}, nil })
	fake.handle(pluginabi.MethodHostStreamEmit, func(payload json.RawMessage) (any, error) {
		var req hostStreamEmitRequest
		_ = json.Unmarshal(payload, &req)
		stream.mu.Lock()
		stream.chunks = append(stream.chunks, append([]byte(nil), req.Payload...))
		stream.mu.Unlock()
		return map[string]any{}, nil
	})
	fake.handle(pluginabi.MethodHostStreamClose, func(payload json.RawMessage) (any, error) {
		var req hostStreamEmitRequest
		_ = json.Unmarshal(payload, &req)
		stream.mu.Lock()
		if !stream.closed {
			stream.closed = true
			stream.closeErr = req.Error
			close(stream.done)
		}
		stream.mu.Unlock()
		return map[string]any{}, nil
	})
	return stream
}

func (s *liveStreamHost) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out strings.Builder
	for _, chunk := range s.chunks {
		out.Write(chunk)
	}
	return out.String()
}

func TestLiveZedClaudeStream(t *testing.T) {
	stream := installLiveStreamHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "Reply with exactly: pong"},
		}}},
	})
	req := rpcExecutorRequest{StreamID: "s1", ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/claude-sonnet-5-5",
		Format:       "claude",
		SourceFormat: "claude",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, errCall := executorExecuteStream(mustJSON(t, req))
	if errCall != nil {
		t.Fatal(errCall)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("stream start failed: %+v", env.Error)
	}
	select {
	case <-stream.done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream did not close")
	}
	stream.mu.Lock()
	closeErr := stream.closeErr
	stream.mu.Unlock()
	if closeErr != "" {
		t.Fatalf("stream error: %s", closeErr)
	}
	out := stream.output()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop", "pong"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

func TestLiveZedResponsesStream(t *testing.T) {
	stream := installLiveStreamHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":             "gpt-6-luna",
		"max_output_tokens": 32,
		"input": []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "Reply with exactly: pong"}},
		}},
	})
	req := rpcExecutorRequest{StreamID: "s1", ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/gpt-6-luna",
		Format:       "openai-response",
		SourceFormat: "openai-response",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecuteStream(mustJSON(t, req))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("stream start failed: %+v", env.Error)
	}
	select {
	case <-stream.done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream did not close")
	}
	stream.mu.Lock()
	closeErr := stream.closeErr
	stream.mu.Unlock()
	if closeErr != "" {
		t.Fatalf("stream error: %s", closeErr)
	}
	out := stream.output()
	for _, want := range []string{"response.output_text.delta", "response.completed", "pong"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

func TestLiveZedClaudeFromChatStream(t *testing.T) {
	stream := installLiveStreamHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}},
	})
	req := rpcExecutorRequest{StreamID: "s1", ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/claude-sonnet-5-5",
		Format:       "openai",
		SourceFormat: "openai",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecuteStream(mustJSON(t, req))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("stream start failed: %+v", env.Error)
	}
	select {
	case <-stream.done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream did not close")
	}
	stream.mu.Lock()
	closeErr := stream.closeErr
	stream.mu.Unlock()
	if closeErr != "" {
		t.Fatalf("stream error: %s", closeErr)
	}
	if out := stream.output(); !strings.Contains(out, "pong") {
		t.Fatalf("stream did not carry the answer:\n%s", out)
	}
}

func TestLiveZedChatToGPT(t *testing.T) {
	installLiveHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":      "gpt-6-luna",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}},
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/gpt-6-luna",
		Format:       "openai",
		SourceFormat: "openai",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecute(mustJSON(t, req))
	response := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if !strings.Contains(string(response.Payload), "pong") {
		t.Fatalf("response: %s", response.Payload)
	}
}

func TestLiveZedClaudeToGPTStream(t *testing.T) {
	stream := installLiveStreamHost(t)
	useSettings(t, defaultSettings())
	storage := liveAuth(t)
	payload := mustJSON(t, map[string]any{
		"model":      "gpt-6-luna",
		"max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "Reply with exactly: pong"},
		}}},
	})
	req := rpcExecutorRequest{StreamID: "s1", ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "live",
		Model:        "zed/gpt-6-luna",
		Format:       "claude",
		SourceFormat: "claude",
		Payload:      payload,
		StorageJSON:  storage,
	}}
	raw, _ := executorExecuteStream(mustJSON(t, req))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("stream start failed: %+v", env.Error)
	}
	select {
	case <-stream.done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream did not close")
	}
	stream.mu.Lock()
	closeErr := stream.closeErr
	stream.mu.Unlock()
	if closeErr != "" {
		t.Fatalf("stream error: %s", closeErr)
	}
	out := stream.output()
	for _, want := range []string{"event: message_start", "content_block_delta", "pong", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}
