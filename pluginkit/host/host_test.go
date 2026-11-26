package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/host/hosttest"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

func TestInvokeWithoutTransport(t *testing.T) {
	t.Cleanup(host.SetTransport(nil))
	var callErr *host.CallError
	if errCall := host.Invoke("host.x", struct{}{}, nil); !errors.As(errCall, &callErr) || callErr.Message != "host API is unavailable" {
		t.Errorf("Invoke = %v", errCall)
	}
}

func TestInvokeReportsHostFailures(t *testing.T) {
	fake := hosttest.Install(t)
	fake.Handle("host.x", func(json.RawMessage) (any, error) {
		return nil, &host.CallError{Code: "rate_limited", Message: "slow down", Status: http.StatusTooManyRequests}
	})
	var callErr *host.CallError
	if errCall := host.Invoke("host.x", struct{}{}, nil); !errors.As(errCall, &callErr) || callErr.Code != "rate_limited" || callErr.Status != http.StatusTooManyRequests || callErr.Method != "host.x" {
		t.Errorf("Invoke = %#v", errCall)
	}
	if errCall := host.Invoke("host.unhandled", struct{}{}, nil); errCall == nil || !strings.Contains(errCall.Error(), "unsupported host callback") {
		t.Errorf("unhandled Invoke = %v", errCall)
	}
}

func TestDoOpensACancellableOperation(t *testing.T) {
	fake := hosttest.Install(t)
	fake.Handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte("pong")}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, errDo := host.New(" cb-1 ").Do(ctx, pluginapi.HTTPRequest{URL: "https://example.test/ping"})
	if errDo != nil || string(resp.Body) != "pong" {
		t.Fatalf("Do = %+v, %v", resp, errDo)
	}
	calls := fake.Calls(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("http.do calls = %d", len(calls))
	}
	var sent map[string]any
	_ = json.Unmarshal(calls[0], &sent)
	if sent["host_callback_id"] != "cb-1" || sent["operation_id"] != "op-1" || sent["method"] != http.MethodGet {
		t.Errorf("request = %v", sent)
	}
}

func TestDoWithoutCancellationSkipsTheOperation(t *testing.T) {
	fake := hosttest.Install(t)
	fake.Handle(pluginabi.MethodHostHTTPDo, func(json.RawMessage) (any, error) { return pluginapi.HTTPResponse{}, nil })
	if _, errDo := host.Detached().Do(context.Background(), pluginapi.HTTPRequest{}); errDo != nil {
		t.Fatal(errDo)
	}
	if opened := fake.Calls(pluginabi.MethodHostHTTPOperationOpen); len(opened) != 0 {
		t.Errorf("opened %d operations", len(opened))
	}
}

func TestStreamReadsUntilDone(t *testing.T) {
	fake := hosttest.Install(t)
	fake.Handle(pluginabi.MethodHostHTTPDoStream, func(json.RawMessage) (any, error) {
		return map[string]any{"status_code": http.StatusOK, "stream_id": "s-1"}, nil
	})
	chunks := []map[string]any{{"payload": []byte("ab")}, {"payload": []byte("c"), "done": true}}
	fake.Handle(pluginabi.MethodHostHTTPStreamRead, func(json.RawMessage) (any, error) {
		next := chunks[0]
		chunks = chunks[1:]
		return next, nil
	})
	fake.Handle(pluginabi.MethodHostHTTPStreamClose, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	stream, errStream := host.Detached().DoStream(context.Background(), pluginapi.HTTPRequest{})
	if errStream != nil {
		t.Fatal(errStream)
	}
	body, errRead := stream.ReadAll()
	stream.Close()
	stream.Close()
	if errRead != nil || string(body) != "abc" || stream.StatusCode != http.StatusOK {
		t.Errorf("ReadAll = %q, %v", body, errRead)
	}
	if closed := fake.Calls(pluginabi.MethodHostHTTPStreamClose); len(closed) != 1 {
		t.Errorf("closed %d times", len(closed))
	}
}

func TestDoStreamClosesAStreamItsContextAbandoned(t *testing.T) {
	fake := hosttest.Install(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake.Handle(pluginabi.MethodHostHTTPDoStream, func(json.RawMessage) (any, error) {
		cancel()
		return map[string]any{"status_code": http.StatusOK, "stream_id": "s-1"}, nil
	})
	fake.Handle(pluginabi.MethodHostHTTPCancel, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	fake.Handle(pluginabi.MethodHostHTTPStreamClose, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	if _, errStream := host.Detached().DoStream(ctx, pluginapi.HTTPRequest{}); !errors.Is(errStream, context.Canceled) {
		t.Fatalf("DoStream = %v, want context.Canceled", errStream)
	}
	if closed := fake.Calls(pluginabi.MethodHostHTTPStreamClose); len(closed) != 1 || !strings.Contains(string(closed[0]), `"s-1"`) {
		t.Errorf("stream_close calls = %s", closed)
	}
}

func TestLogTagsAndRedacts(t *testing.T) {
	fake := hosttest.Install(t)
	host.SetPluginID("demo")
	rpc.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "sk-1", "[redacted]") })
	t.Cleanup(func() { rpc.SetRedactor(nil) })
	host.Log("warn", "key sk-1 failed", map[string]any{"body": "sk-1", "n": 2})
	logs := fake.Logs()
	if len(logs) != 1 {
		t.Fatalf("logs = %v", logs)
	}
	line := logs[0]
	if line.Level != "warn" || line.Message != "key [redacted] failed" || line.Fields["body"] != "[redacted]" || line.Fields["plugin"] != "demo" || line.Fields["n"] != float64(2) {
		t.Errorf("line = %+v", line)
	}
}

func TestAuthHelpers(t *testing.T) {
	fake := hosttest.Install(t)
	fake.HandleAuths(pluginapi.HostAuthFileEntry{AuthIndex: "1", Name: "a.json", Provider: "codex"})
	fake.Handle(pluginabi.MethodHostAuthSave, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(payload, &req)
		return pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auths/" + req.Name}, nil
	})
	auths, errList := host.AuthList()
	if errList != nil || len(auths) != 1 || auths[0].Provider != "codex" {
		t.Errorf("AuthList = %+v, %v", auths, errList)
	}
	saved, errSave := host.AuthSave("b.json", []byte(`{}`))
	if errSave != nil || saved.Path != "/auths/b.json" {
		t.Errorf("AuthSave = %+v, %v", saved, errSave)
	}
}
