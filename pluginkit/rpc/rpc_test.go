package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOK(t *testing.T) {
	raw, errOK := OK(map[string]int{"n": 1})
	if errOK != nil || string(raw) != `{"ok":true,"result":{"n":1}}` {
		t.Errorf("OK = %s, %v", raw, errOK)
	}
	if _, errBad := OK(func() {}); errBad == nil {
		t.Error("unencodable value accepted")
	}
}

func TestFail(t *testing.T) {
	cases := []struct {
		raw  []byte
		want string
	}{
		{Fail("bad", "nope"), `{"ok":false,"error":{"code":"bad","message":"nope"}}`},
		{Fail("limit", "wait", WithStatus(429), WithRetryAfter(90*time.Second)), `{"ok":false,"error":{"code":"limit","message":"wait","http_status":429,"retry_after_seconds":90}}`},
		{Fail("limit", "wait", WithRetryAfter(-time.Second)), `{"ok":false,"error":{"code":"limit","message":"wait"}}`},
	}
	for _, c := range cases {
		if string(c.raw) != c.want {
			t.Errorf("Fail = %s, want %s", c.raw, c.want)
		}
	}
}

func TestRedactor(t *testing.T) {
	SetRedactor(func(s string) string { return strings.ReplaceAll(s, "sk-1", "[redacted]") })
	t.Cleanup(func() { SetRedactor(nil) })
	var env Envelope
	if errUnmarshal := json.Unmarshal(Fail("bad", "key sk-1"), &env); errUnmarshal != nil || env.Error.Message != "key [redacted]" {
		t.Errorf("message = %+v, %v", env.Error, errUnmarshal)
	}
	SetRedactor(nil)
	if Redact("sk-1") != "sk-1" {
		t.Error("nil redactor changes text")
	}
}

func TestRegistrationOmitsUndeclaredCapabilities(t *testing.T) {
	raw, _ := json.Marshal(Registration{SchemaVersion: 6, Metadata: NewMetadata("Demo", "1.0.0", "/logo.svg"), Capabilities: Capabilities{ManagementAPI: true}})
	got := string(raw)
	for _, want := range []string{`"capabilities":{"management_api":true}`, `"Author":"Whexy"`, `"GitHubRepository":"https://github.com/whexy/whexy-cpa-store"`, `"Version":"1.0.0"`} {
		if !strings.Contains(got, want) {
			t.Errorf("registration %s lacks %s", got, want)
		}
	}
}

func TestTransportFailure(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("dial: %w", context.Canceled), `{"ok":false,"error":{"code":"canceled","message":"Demo request canceled","http_status":408}}`},
		{errors.New("connection refused"), `{"ok":false,"error":{"code":"upstream_unreachable","message":"Demo request failed: connection refused","http_status":502}}`},
	}
	for _, tc := range cases {
		if got := string(TransportFailure("Demo", tc.err)); got != tc.want {
			t.Errorf("TransportFailure(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}
