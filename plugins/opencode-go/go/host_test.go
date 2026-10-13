package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHostDoSendsCallbackIDAndDecodesResponse(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"X-Opencode-Log-Id": {"log-1"}}, Body: []byte(`{"ok":true}`)}, nil
	})
	resp, errDo := newHost(" cb-7 ").Do(context.Background(), pluginapi.HTTPRequest{
		URL:     "https://opencode.ai/inference/go/v1/usage",
		Headers: http.Header{"Authorization": {"Bearer " + testKeyA}},
	})
	if errDo != nil {
		t.Fatal(errDo)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"ok":true}` || resp.Headers.Get("x-opencode-log-id") != "log-1" {
		t.Fatalf("resp = %+v", resp)
	}
	sent := fake.callsTo(pluginabi.MethodHostHTTPDo)
	if len(sent) != 1 {
		t.Fatalf("calls = %d", len(sent))
	}
	var req map[string]any
	if errUnmarshal := json.Unmarshal(sent[0], &req); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if req["host_callback_id"] != "cb-7" || req["method"] != "GET" || req["url"] != "https://opencode.ai/inference/go/v1/usage" {
		t.Fatalf("request = %v", req)
	}
	if _, hasOperation := req["operation_id"]; hasOperation {
		t.Fatal("an uncancellable context needs no operation")
	}
	if len(fake.callsTo(pluginabi.MethodHostHTTPOperationOpen)) != 0 {
		t.Fatal("operation opened for background context")
	}
}

func TestHostDoCancelsOperationWithContext(t *testing.T) {
	fake := installFakeHost(t)
	cancelled := make(chan string, 1)
	fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(payload json.RawMessage) (any, error) {
		var req hostOperationRequest
		_ = json.Unmarshal(payload, &req)
		if req.HostCallbackID != "cb-1" {
			t.Errorf("operation open callback id = %q", req.HostCallbackID)
		}
		return hostOperationResponse{OperationID: "op-9"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPCancel, func(payload json.RawMessage) (any, error) {
		var req hostOperationRequest
		_ = json.Unmarshal(payload, &req)
		cancelled <- req.OperationID
		return struct{}{}, nil
	})
	entered := make(chan struct{})
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		if req.OperationID != "op-9" {
			t.Errorf("operation id = %q", req.OperationID)
		}
		close(entered)
		select {
		case id := <-cancelled:
			cancelled <- id
			return nil, errors.New("context canceled")
		case <-time.After(5 * time.Second):
			return nil, errors.New("operation was never cancelled")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	_, errDo := newHost("cb-1").Do(ctx, pluginapi.HTTPRequest{URL: "https://example.test"})
	if !errors.Is(errDo, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", errDo)
	}
	if got := <-cancelled; got != "op-9" {
		t.Fatalf("cancelled %q", got)
	}
}

func TestHostDoReturnsHostErrorStatus(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDo, func(json.RawMessage) (any, error) {
		return nil, &hostCallError{Code: "host_call_failed", Message: "dial failed for " + testKeyB, Status: 502}
	})
	_, errDo := newHost("").Do(context.Background(), pluginapi.HTTPRequest{URL: "https://example.test"})
	var hostErr *hostCallError
	if !errors.As(errDo, &hostErr) || hostErr.Status != 502 || hostErr.Method != pluginabi.MethodHostHTTPDo {
		t.Fatalf("err = %#v", errDo)
	}
	if strings.Contains(errDo.Error(), testKeyB) {
		t.Fatalf("error leaks key: %v", errDo)
	}
}

func TestHostStreamReadAndClose(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(json.RawMessage) (any, error) {
		return hostOperationResponse{OperationID: "op-2"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPDoStream, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		if req.OperationID != "op-2" || req.HostCallbackID != "cb-2" || req.Method != http.MethodPost || string(req.Body) != `{}` {
			t.Errorf("stream request = %+v", req)
		}
		return hostHTTPStreamStart{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "s-1"}, nil
	})
	var mu sync.Mutex
	chunks := []hostStreamReadResponse{{Payload: []byte("data: a\n\n")}, {Payload: []byte("data: b\n\n")}, {Done: true}}
	fake.handle(pluginabi.MethodHostHTTPStreamRead, func(payload json.RawMessage) (any, error) {
		var req hostStreamIDRequest
		_ = json.Unmarshal(payload, &req)
		if req.StreamID != "s-1" {
			t.Errorf("read stream id = %q", req.StreamID)
		}
		mu.Lock()
		defer mu.Unlock()
		next := chunks[0]
		chunks = chunks[1:]
		return next, nil
	})
	fake.handle(pluginabi.MethodHostHTTPStreamClose, func(json.RawMessage) (any, error) { return struct{}{}, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, errStart := newHost("cb-2").DoStream(ctx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: "https://example.test", Body: []byte(`{}`)})
	if errStart != nil {
		t.Fatal(errStart)
	}
	if stream.StatusCode != 200 || stream.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %+v", stream)
	}
	body, errRead := stream.ReadAll()
	if errRead != nil || string(body) != "data: a\n\ndata: b\n\n" {
		t.Fatalf("body = %q, err = %v", body, errRead)
	}
	stream.Close()
	stream.Close()
	if closes := fake.callsTo(pluginabi.MethodHostHTTPStreamClose); len(closes) != 1 || !strings.Contains(string(closes[0]), `"stream_id":"s-1"`) {
		t.Fatalf("closes = %s", closes)
	}
}

func TestHostStreamClosesOnContextCancel(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(json.RawMessage) (any, error) {
		return hostOperationResponse{OperationID: "op-1"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPDoStream, func(json.RawMessage) (any, error) {
		return hostHTTPStreamStart{StatusCode: 200, StreamID: "s-2"}, nil
	})
	closed := make(chan struct{})
	fake.handle(pluginabi.MethodHostHTTPStreamClose, func(json.RawMessage) (any, error) {
		close(closed)
		return struct{}{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	stream, errStart := newHost("").DoStream(ctx, pluginapi.HTTPRequest{URL: "https://example.test"})
	if errStart != nil {
		t.Fatal(errStart)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not closed after cancel")
	}
	if _, done, errRead := stream.Read(); !done || !errors.Is(errRead, context.Canceled) {
		t.Fatalf("read after cancel: done=%v err=%v", done, errRead)
	}
}

func TestHostStreamReadError(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDoStream, func(json.RawMessage) (any, error) {
		return hostHTTPStreamStart{StatusCode: 200, StreamID: "s-3"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPStreamRead, func(json.RawMessage) (any, error) {
		return hostStreamReadResponse{Payload: []byte("partial"), Error: "unexpected EOF", Done: true}, nil
	})
	stream, errStart := newHost("").DoStream(context.Background(), pluginapi.HTTPRequest{URL: "https://example.test"})
	if errStart != nil {
		t.Fatal(errStart)
	}
	payload, done, errRead := stream.Read()
	if string(payload) != "partial" || !done || errRead == nil || !strings.Contains(errRead.Error(), "unexpected EOF") {
		t.Fatalf("read = %q %v %v", payload, done, errRead)
	}
}

func TestHostExecutorStreamEmitAndClose(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostStreamEmit, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	fake.handle(pluginabi.MethodHostStreamClose, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	if errEmit := hostStreamEmit("7", []byte("data: x\n\n")); errEmit != nil {
		t.Fatal(errEmit)
	}
	hostStreamClose("7", "upstream failed for "+testKeyA)
	var emit hostStreamEmitRequest
	_ = json.Unmarshal(fake.callsTo(pluginabi.MethodHostStreamEmit)[0], &emit)
	if emit.StreamID != "7" || string(emit.Payload) != "data: x\n\n" {
		t.Fatalf("emit = %+v", emit)
	}
	closeReq := string(fake.callsTo(pluginabi.MethodHostStreamClose)[0])
	if !strings.Contains(closeReq, `"stream_id":"7"`) || strings.Contains(closeReq, testKeyA) {
		t.Fatalf("close = %s", closeReq)
	}
}

func TestHostLogRedactsAndTagsCallback(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostLog, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	newHost("cb-3").Log("warn", "key "+testKeyA+" failed", map[string]any{"error": "echo " + testKeyB, "status": 401})
	sent := string(fake.callsTo(pluginabi.MethodHostLog)[0])
	if strings.Contains(sent, testKeyA) || strings.Contains(sent, testKeyB) {
		t.Fatalf("log leaks key: %s", sent)
	}
	for _, want := range []string{`"host_callback_id":"cb-3"`, `"level":"warn"`, `"status":401`, `"plugin":"opencode-go"`} {
		if !strings.Contains(sent, want) {
			t.Fatalf("log %s lacks %s", sent, want)
		}
	}
}

func TestHostAuthCallbacks(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		return hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{ID: "opencode-go.json#abc", AuthIndex: "idx1", Name: "opencode-go.json", Provider: "opencode-go"}}}, nil
	})
	fake.handle(pluginabi.MethodHostAuthGet, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(payload, &req)
		return pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, Name: "opencode-go.json", JSON: json.RawMessage(`{"type":"opencode-go","keys":[]}`)}, nil
	})
	fake.handle(pluginabi.MethodHostAuthSave, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(payload, &req)
		return pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auths/" + req.Name}, nil
	})
	fake.handle(pluginabi.MethodHostRoutingResetCooldown, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostRoutingResetCooldownRequest
		_ = json.Unmarshal(payload, &req)
		return pluginapi.HostRoutingResetCooldownResponse{AuthIndex: req.AuthIndex, Models: []string{"opencode-go/kimi-k3"}}, nil
	})

	files, errList := hostAuthList()
	if errList != nil || len(files) != 1 || files[0].AuthIndex != "idx1" {
		t.Fatalf("list = %+v, %v", files, errList)
	}
	got, errGet := hostAuthGet("idx1")
	if errGet != nil || string(got.JSON) != `{"type":"opencode-go","keys":[]}` {
		t.Fatalf("get = %+v, %v", got, errGet)
	}
	if !strings.Contains(string(fake.callsTo(pluginabi.MethodHostAuthGet)[0]), `"auth_index":"idx1"`) {
		t.Fatal("auth get does not send auth_index")
	}
	saved, errSave := hostAuthSave("opencode-go.json", []byte(`{"type":"opencode-go"}`))
	if errSave != nil || saved.Path != "/auths/opencode-go.json" {
		t.Fatalf("save = %+v, %v", saved, errSave)
	}
	var saveReq map[string]json.RawMessage
	_ = json.Unmarshal(fake.callsTo(pluginabi.MethodHostAuthSave)[0], &saveReq)
	if string(saveReq["name"]) != `"opencode-go.json"` || string(saveReq["json"]) != `{"type":"opencode-go"}` {
		t.Fatalf("save request = %s", fake.callsTo(pluginabi.MethodHostAuthSave)[0])
	}
	reset, errReset := hostResetCooldown("idx1")
	if errReset != nil || reset.AuthIndex != "idx1" || len(reset.Models) != 1 {
		t.Fatalf("reset = %+v, %v", reset, errReset)
	}
}

func TestInvokeHostWithoutHost(t *testing.T) {
	previous := callHost
	callHost = nil
	t.Cleanup(func() { callHost = previous })
	if errCall := invokeHost(pluginabi.MethodHostLog, struct{}{}, nil); errCall == nil {
		t.Fatal("call without host succeeded")
	}
}
