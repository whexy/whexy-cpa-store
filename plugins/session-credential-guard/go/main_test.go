package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistrationDeclaresRequestInterceptor(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginSchemaVersion || !registration.Capabilities.RequestInterceptor {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Metadata.Version != pluginVersion {
		t.Fatalf("version = %q, want %q", registration.Metadata.Version, pluginVersion)
	}
}

func TestConfigureBindingTTL(t *testing.T) {
	t.Cleanup(func() { sessions.setTTL(defaultBindingTTL) })
	register := func(configYAML string) error {
		raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		_, errHandle := handleMethod(pluginabi.MethodPluginReconfigure, raw)
		return errHandle
	}
	if errRegister := register("enabled: true\nbinding_ttl: 90m\n"); errRegister != nil || sessions.ttl != 90*time.Minute {
		t.Fatalf("ttl = %v, err = %v", sessions.ttl, errRegister)
	}
	if errRegister := register("enabled: true\n"); errRegister != nil || sessions.ttl != defaultBindingTTL {
		t.Fatalf("ttl = %v, err = %v", sessions.ttl, errRegister)
	}
	for _, bad := range []string{"binding_ttl: soon\n", "binding_ttl: -1h\n", "binding_ttl: 0s\n"} {
		if errRegister := register(bad); errRegister == nil {
			t.Fatalf("config %q accepted", bad)
		}
	}
}

// interceptor drives after-auth calls through the RPC entry point against a
// fresh guard installed for the duration of the test.
type interceptor struct {
	t *testing.T
}

func newInterceptor(t *testing.T) interceptor {
	t.Helper()
	g, _ := testGuard(t)
	previous := sessions
	sessions = g
	t.Cleanup(func() { sessions = previous })
	return interceptor{t: t}
}

func (in interceptor) call(headers http.Header, metadata map[string]any) pluginapi.RequestInterceptResponse {
	in.t.Helper()
	raw, errMarshal := json.Marshal(pluginapi.RequestInterceptRequest{
		SourceFormat:   "claude",
		ToFormat:       "claude",
		Model:          "claude-opus-4-8",
		RequestedModel: "opus",
		Stream:         true,
		Headers:        headers,
		Body:           []byte(`{"model":"opus"}`),
		Metadata:       metadata,
	})
	if errMarshal != nil {
		in.t.Fatal(errMarshal)
	}
	out, errHandle := handleMethod(pluginabi.MethodRequestInterceptAfter, raw)
	if errHandle != nil {
		in.t.Fatal(errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil || !env.OK {
		in.t.Fatalf("envelope = %s, err = %v", out, errUnmarshal)
	}
	var resp pluginapi.RequestInterceptResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		in.t.Fatal(errUnmarshal)
	}
	return resp
}

func selectedMetadata(sel selection) map[string]any {
	return map[string]any{
		metadataSessionID:         "session-1",
		metadataCallerScope:       "caller",
		metadataSelectedAuthID:    sel.authID,
		metadataSelectedAuthIndex: sel.authIndex,
		"request_path":            "/v1/messages",
	}
}

func optedIn() http.Header {
	return http.Header{"X-Cpa-Session-Guard": {"1"}, "Authorization": {"Bearer k"}}
}

func withAck(id string) http.Header {
	headers := optedIn()
	headers.Set(ackHeader, id)
	return headers
}

func mustPassThrough(t *testing.T, resp pluginapi.RequestInterceptResponse) {
	t.Helper()
	if resp.Terminate || resp.Headers != nil || len(resp.ClearHeaders) != 0 || len(resp.Body) != 0 {
		t.Fatalf("response = %+v, want unmodified pass-through", resp)
	}
}

func mustConflict(t *testing.T, resp pluginapi.RequestInterceptResponse, code string) rejection {
	t.Helper()
	if !resp.Terminate || resp.StatusCode != http.StatusConflict {
		t.Fatalf("response = %+v, want terminating 409", resp)
	}
	if got := resp.ResponseHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	var body rejection
	if errUnmarshal := json.Unmarshal(resp.ResponseBody, &body); errUnmarshal != nil {
		t.Fatalf("body %s: %v", resp.ResponseBody, errUnmarshal)
	}
	if body.Type != "error" || body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("body = %s, want %s", resp.ResponseBody, code)
	}
	pendingID := ""
	if body.SessionBinding != nil && body.SessionBinding.Pending != nil {
		pendingID = body.SessionBinding.Pending.ID
	}
	if got := resp.ResponseHeaders.Get(migrationHeader); got != pendingID {
		t.Fatalf("%s = %q, body pending id = %q", migrationHeader, got, pendingID)
	}
	return body
}

func TestInterceptAfterAuthMigrationProtocol(t *testing.T) {
	in := newInterceptor(t)

	mustPassThrough(t, in.call(optedIn(), selectedMetadata(authA)))
	mustPassThrough(t, in.call(optedIn(), selectedMetadata(authA)))

	body := mustConflict(t, in.call(optedIn(), selectedMetadata(authB)), codeMigrationRequired)
	id := mustPending(t, body.SessionBinding, "idx-a", 1, "idx-b")
	if body.Error.Type != "session_migration_required" {
		t.Fatalf("error type = %q", body.Error.Type)
	}
	again := mustConflict(t, in.call(optedIn(), selectedMetadata(authB)), codeMigrationRequired)
	if mustPending(t, again.SessionBinding, "idx-a", 1, "idx-b") != id {
		t.Fatalf("repeated request changed the pending migration: %+v", again.SessionBinding)
	}

	mustConflict(t, in.call(withAck("smig_wrong"), selectedMetadata(authB)), codeAckInvalid)

	mustPassThrough(t, in.call(withAck(id), selectedMetadata(authB)))
	mustPassThrough(t, in.call(withAck(id), selectedMetadata(authB)))
	mustPassThrough(t, in.call(optedIn(), selectedMetadata(authB)))

	// An acknowledgement alone also opts the request in.
	ackOnly := http.Header{}
	ackOnly.Set(ackHeader, id)
	mustPassThrough(t, in.call(ackOnly, selectedMetadata(authB)))
}

func TestInterceptAfterAuthTargetUnavailable(t *testing.T) {
	in := newInterceptor(t)
	mustPassThrough(t, in.call(optedIn(), selectedMetadata(authA)))
	id := mustPending(t, mustConflict(t, in.call(optedIn(), selectedMetadata(authB)), codeMigrationRequired).SessionBinding, "idx-a", 1, "idx-b")

	body := mustConflict(t, in.call(withAck(id), selectedMetadata(authC)), codeTargetUnavailable)
	mustNoPending(t, body.SessionBinding, "idx-a", 1)

	next := mustPending(t, mustConflict(t, in.call(optedIn(), selectedMetadata(authC)), codeMigrationRequired).SessionBinding, "idx-a", 1, "idx-c")
	if next == id {
		t.Fatalf("migration to C reused id %s", id)
	}
}

func TestInterceptAfterAuthIgnoresUnguardedRequests(t *testing.T) {
	in := newInterceptor(t)
	mustPassThrough(t, in.call(optedIn(), selectedMetadata(authA)))

	// Clients that did not opt in keep CLIProxyAPI's silent failover.
	mustPassThrough(t, in.call(http.Header{"Authorization": {"Bearer k"}}, selectedMetadata(authB)))

	noSession := selectedMetadata(authB)
	delete(noSession, metadataSessionID)
	mustPassThrough(t, in.call(optedIn(), noSession))

	noSelection := selectedMetadata(authB)
	delete(noSelection, metadataSelectedAuthID)
	mustPassThrough(t, in.call(optedIn(), noSelection))

	mustPassThrough(t, in.call(optedIn(), nil))
}

func TestInterceptBeforeAuthPassesThrough(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginapi.RequestInterceptRequest{SourceFormat: "claude", Headers: withAck("smig_1")})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	out, errHandle := handleMethod(pluginabi.MethodRequestInterceptBefore, raw)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("envelope = %s, err = %v", out, errUnmarshal)
	}
	var resp pluginapi.RequestInterceptResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	mustPassThrough(t, resp)
}
