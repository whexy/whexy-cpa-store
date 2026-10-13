package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// gatewayReply is one fake upstream response.
type gatewayReply struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// fakeGateway answers the host HTTP callbacks the executor makes and
// records the executor stream it writes.
type fakeGateway struct {
	t     *testing.T
	fake  *fakeHost
	reply func(req hostHTTPRequest) gatewayReply
	// readSize splits streamed bodies into reads of this many bytes so the
	// SSE splitter sees lines cut at arbitrary points.
	readSize int

	mu       sync.Mutex
	requests []hostHTTPRequest
	bodies   map[string][]byte
	nextID   int
	emitted  map[string][][]byte
	closes   map[string]chan string
}

func installFakeGateway(t *testing.T, reply func(req hostHTTPRequest) gatewayReply) *fakeGateway {
	t.Helper()
	gateway := &fakeGateway{
		t:        t,
		fake:     installFakeHost(t),
		reply:    reply,
		readSize: 37,
		bodies:   make(map[string][]byte),
		emitted:  make(map[string][][]byte),
		closes:   make(map[string]chan string),
	}
	ok := func(json.RawMessage) (any, error) { return struct{}{}, nil }
	gateway.fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(json.RawMessage) (any, error) {
		return hostOperationResponse{OperationID: "op-1"}, nil
	})
	gateway.fake.handle(pluginabi.MethodHostHTTPCancel, ok)
	gateway.fake.handle(pluginabi.MethodHostLog, ok)
	gateway.fake.handle(pluginabi.MethodHostHTTPStreamClose, ok)
	gateway.fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		reply := gateway.record(payload)
		return pluginapi.HTTPResponse{StatusCode: reply.Status, Headers: reply.Headers, Body: reply.Body}, nil
	})
	gateway.fake.handle(pluginabi.MethodHostHTTPDoStream, func(payload json.RawMessage) (any, error) {
		reply := gateway.record(payload)
		gateway.mu.Lock()
		gateway.nextID++
		id := "up-" + strconv.Itoa(gateway.nextID)
		gateway.bodies[id] = reply.Body
		gateway.mu.Unlock()
		return hostHTTPStreamStart{StatusCode: reply.Status, Headers: reply.Headers, StreamID: id}, nil
	})
	gateway.fake.handle(pluginabi.MethodHostHTTPStreamRead, func(payload json.RawMessage) (any, error) {
		var req hostStreamIDRequest
		_ = json.Unmarshal(payload, &req)
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		rest := gateway.bodies[req.StreamID]
		if len(rest) == 0 {
			return hostStreamReadResponse{Done: true}, nil
		}
		size := min(gateway.readSize, len(rest))
		gateway.bodies[req.StreamID] = rest[size:]
		return hostStreamReadResponse{Payload: rest[:size]}, nil
	})
	gateway.fake.handle(pluginabi.MethodHostStreamEmit, func(payload json.RawMessage) (any, error) {
		var req hostStreamEmitRequest
		_ = json.Unmarshal(payload, &req)
		gateway.mu.Lock()
		gateway.emitted[req.StreamID] = append(gateway.emitted[req.StreamID], req.Payload)
		gateway.mu.Unlock()
		return struct{}{}, nil
	})
	gateway.fake.handle(pluginabi.MethodHostStreamClose, func(payload json.RawMessage) (any, error) {
		var req hostStreamEmitRequest
		_ = json.Unmarshal(payload, &req)
		gateway.closeChannel(req.StreamID) <- req.Error
		return struct{}{}, nil
	})
	return gateway
}

func (g *fakeGateway) record(payload json.RawMessage) gatewayReply {
	var req hostHTTPRequest
	if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
		g.t.Errorf("http request %s: %v", payload, errUnmarshal)
	}
	g.mu.Lock()
	g.requests = append(g.requests, req)
	g.mu.Unlock()
	return g.reply(req)
}

func (g *fakeGateway) closeChannel(streamID string) chan string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.closes[streamID]
	if !ok {
		ch = make(chan string, 4)
		g.closes[streamID] = ch
	}
	return ch
}

// waitClosed waits for the executor stream to close and returns its chunks
// and the close error.
func (g *fakeGateway) waitClosed(streamID string) ([][]byte, string) {
	g.t.Helper()
	select {
	case errText := <-g.closeChannel(streamID):
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.emitted[streamID], errText
	case <-time.After(10 * time.Second):
		g.t.Fatalf("stream %s was never closed", streamID)
		return nil, ""
	}
}

func (g *fakeGateway) inferenceRequests() []hostHTTPRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []hostHTTPRequest
	for _, req := range g.requests {
		if !strings.HasSuffix(req.URL, "/v1/usage") && !strings.Contains(req.URL, "/console/") {
			out = append(out, req)
		}
	}
	return out
}

// fixtureGateway answers each native endpoint with the recorded Go
// responses in testdata.
func fixtureGateway(t *testing.T) func(req hostHTTPRequest) gatewayReply {
	t.Helper()
	sse := http.Header{"Content-Type": {"text/event-stream"}, "X-Opencode-Log-Id": {"log-test"}}
	plain := http.Header{"Content-Type": {"application/json"}, "X-Opencode-Log-Id": {"log-test"}, "Set-Cookie": {"a=b"}}
	fixtures := map[string][2]string{
		"/chat/completions": {"chat_response.json", "chat_stream.txt"},
		"/messages":         {"messages_response.json", "messages_stream.txt"},
		"/responses":        {"responses_response.json", "responses_stream.txt"},
	}
	loaded := make(map[string][2][]byte)
	for suffix, names := range fixtures {
		loaded[suffix] = [2][]byte{readFixture(t, names[0]), readFixture(t, names[1])}
	}
	return func(req hostHTTPRequest) gatewayReply {
		for suffix, bodies := range loaded {
			if strings.HasSuffix(req.URL, suffix) {
				if req.Headers.Get("Accept") == "text/event-stream" {
					return gatewayReply{Status: 200, Headers: sse, Body: bodies[1]}
				}
				return gatewayReply{Status: 200, Headers: plain, Body: bodies[0]}
			}
		}
		return gatewayReply{Status: 404, Body: []byte(`{"error":{"type":"not_found","message":"no route"}}`)}
	}
}

// useTestCatalog serves testdata/config.json as the catalog of every test key.
func useTestCatalog(t *testing.T) {
	t.Helper()
	store := useCatalogStore(t)
	cat, errParse := parseCatalog(readFixture(t, "config.json"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	for _, key := range []string{testKeyA, testKeyB, testKeyC} {
		store.byKey[keyIDFor(key)] = catalogEntry{catalog: cat, fetchedAt: time.Now()}
	}
}

// useExhaustion gives the test its own usage-block state with a fake clock
// and captured expiry callbacks.
func useExhaustion(t *testing.T, now time.Time) (*exhaustionTracker, *[]func(), *[][]string) {
	t.Helper()
	previous := exhaustion
	tracker := newExhaustionTracker()
	clock := now
	tracker.now = func() time.Time { return clock }
	var scheduled []func()
	var resets [][]string
	tracker.schedule = func(_ time.Time, fn func()) { scheduled = append(scheduled, fn) }
	tracker.resetCooldown = func(ids []string) { resets = append(resets, ids) }
	exhaustion = tracker
	t.Cleanup(func() { exhaustion = previous })
	return tracker, &scheduled, &resets
}

type testCall struct {
	Source  string
	Format  string
	Model   string
	Stream  bool
	Payload string
	Headers http.Header
	Meta    map[string]any
	APIKey  string
}

func (c testCall) raw(t *testing.T) []byte {
	t.Helper()
	apiKey := c.APIKey
	if apiKey == "" {
		apiKey = testKeyA
	}
	keyID := keyIDFor(apiKey)
	storage := mustJSON(t, map[string]any{"type": providerKey, keyIDAttribute: keyID, "api_key": apiKey})
	headers := c.Headers
	if headers == nil {
		headers = http.Header{"Authorization": {"Bearer sk-client-secret"}, "Cookie": {"session=client"}}
	}
	format := c.Format
	if format == "" {
		format = c.Source
	}
	req := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:          authIDFor(keyFileName, keyID),
			AuthProvider:    providerKey,
			Model:           c.Model,
			Format:          format,
			Stream:          c.Stream,
			Headers:         headers,
			SourceFormat:    c.Source,
			Payload:         []byte(c.Payload),
			OriginalRequest: []byte(c.Payload),
			Metadata:        c.Meta,
			StorageJSON:     storage,
			AuthAttributes:  map[string]string{keyIDAttribute: keyID},
		},
		HostCallbackID: "cb-exec",
	}
	if c.Stream {
		req.StreamID = "exec-1"
	}
	return mustJSON(t, req)
}

// clientPayload is a minimal "Reply with OK" request in each client format.
func clientPayload(format, model string, stream bool) string {
	switch format {
	case "claude":
		return `{"model":"` + model + `","max_tokens":32,"system":"Be brief.","messages":[{"role":"user","content":"Reply with OK"}],"stream":` + strconv.FormatBool(stream) + `}`
	case "openai-response":
		return `{"model":"` + model + `","instructions":"Be brief.","input":"Reply with OK","max_output_tokens":32,"stream":` + strconv.FormatBool(stream) + `}`
	default:
		return `{"model":"` + model + `","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Reply with OK"}],"max_tokens":32,"stream":` + strconv.FormatBool(stream) + `}`
	}
}
