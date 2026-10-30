package main

import (
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

func fakeJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(exp) + `}`))
	return header + "." + payload + ".signature"
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestJWTExpiry(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	if got := jwtExpiry(fakeJWT(exp)); got.Unix() != exp {
		t.Fatalf("expiry = %v, want %v", got, time.Unix(exp, 0))
	}
	if got := jwtExpiry("not-a-jwt"); !got.IsZero() {
		t.Fatalf("invalid token expiry = %v", got)
	}
}

func TestSettingsModelIDs(t *testing.T) {
	cfg := defaultSettings()
	if got := cfg.exposedID("claude-sonnet-5-5"); got != "zed/claude-sonnet-5-5" {
		t.Fatalf("prefixed id = %q", got)
	}
	if got := cfg.upstreamID("zed/claude-sonnet-5-5"); got != "claude-sonnet-5-5" {
		t.Fatalf("upstream id = %q", got)
	}
	if got := cfg.upstreamID("claude-sonnet-5-5"); got != "claude-sonnet-5-5" {
		t.Fatalf("bare upstream id = %q", got)
	}
	cfg.DisablePrefix = true
	if got := cfg.exposedID("gpt-6-luna"); got != "gpt-6-luna" {
		t.Fatalf("bare id = %q", got)
	}
	if got := cfg.upstreamID("zed/gpt-6-luna"); got != "gpt-6-luna" {
		t.Fatalf("prefixed request with bare mode = %q", got)
	}
}

func TestNativeFormat(t *testing.T) {
	cases := map[string]string{
		"anthropic": "claude",
		"open_ai":   "openai-response",
		"google":    "gemini",
		"x_ai":      "openai",
	}
	for provider, want := range cases {
		if got := (zedModel{Provider: provider}).nativeProtocol(); got != want {
			t.Errorf("provider %s = %q, want %q", provider, got, want)
		}
	}
}

func TestValidVersion(t *testing.T) {
	if !validVersion("1.26.2") || validVersion("1.26") || validVersion("v1.26.2") {
		t.Fatal("version parsing")
	}
	if compareVersions("1.26.2", "1.25.3") <= 0 || compareVersions("1.25.3", "1.26.2") >= 0 {
		t.Fatal("version ordering")
	}
}
