package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestDecryptCallback(t *testing.T) {
	key, errKey := rsa.GenerateKey(rand.Reader, 2048)
	if errKey != nil {
		t.Fatal(errKey)
	}
	ciphertext, errEncrypt := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, []byte("secret-token"), nil)
	if errEncrypt != nil {
		t.Fatal(errEncrypt)
	}
	query := url.Values{}
	query.Set("user_id", "42")
	query.Set("access_token", base64.RawURLEncoding.EncodeToString(ciphertext))
	result, errCallback := decryptCallback(key, query)
	if errCallback != nil {
		t.Fatal(errCallback)
	}
	if result.UserID != "42" || result.AccessToken != "secret-token" {
		t.Fatalf("result: %+v", result)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	if publicKey == "" {
		t.Fatal("public key encoding")
	}
}

func TestDecryptCallbackRejectsMissingToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	query := url.Values{}
	query.Set("user_id", "42")
	if _, errCallback := decryptCallback(key, query); errCallback == nil {
		t.Fatal("a callback without access_token must fail")
	}
}

func installAccountHost(t *testing.T) {
	t.Helper()
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(json.RawMessage) (any, error) {
		return map[string]any{"operation_id": "op"}, nil
	})
	fake.handle(pluginabi.MethodHostHTTPCancel, func(json.RawMessage) (any, error) { return map[string]any{}, nil })
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		if strings.HasSuffix(req.URL, "/client/users/me") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
				"user":                    map[string]any{"legacy_user_id": 42, "username": "tester"},
				"default_organization_id": "org_test",
				"plan":                    map[string]any{"plan_v3": "zed_student"},
			})}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	})
}

// encryptedCallback is the URL Zed's sign-in page redirects the browser to.
func encryptedCallback(t *testing.T, signInURL, token string) string {
	t.Helper()
	parsed, errParse := url.Parse(signInURL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	publicKeyRaw, errDecode := base64.RawURLEncoding.DecodeString(parsed.Query().Get("native_app_public_key"))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	publicKey, errKey := x509.ParsePKCS1PublicKey(publicKeyRaw)
	if errKey != nil {
		t.Fatal(errKey)
	}
	ciphertext, errEncrypt := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, []byte(token), nil)
	if errEncrypt != nil {
		t.Fatal(errEncrypt)
	}
	return "http://127.0.0.1:" + parsed.Query().Get("native_app_port") +
		"/?user_id=42&access_token=" + base64.RawURLEncoding.EncodeToString(ciphertext)
}

func TestLoginListenerRedirectsOnlyAValidCallback(t *testing.T) {
	installAccountHost(t)
	useSettings(t, defaultSettings())
	flow, errStart := logins.start(currentSettings())
	if errStart != nil {
		t.Fatal(errStart)
	}
	t.Cleanup(logins.closeAll)
	callback := encryptedCallback(t, flow.URL, "token-value")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	forged, _ := url.Parse(callback)
	query := forged.Query()
	query.Set("access_token", base64.RawURLEncoding.EncodeToString([]byte("forged")))
	forged.RawQuery = query.Encode()
	resp, errGet := client.Get(forged.String())
	if errGet != nil {
		t.Fatal(errGet)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("forged callback answered %d %q, want a 400 without the success redirect", resp.StatusCode, resp.Header.Get("Location"))
	}
	if pending := logins.poll(t.Context(), newHost(""), currentSettings(), flow.State); pending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("a forged callback ended the sign-in: %+v", pending)
	}

	resp, errGet = client.Get(callback)
	if errGet != nil {
		t.Fatalf("the success redirect must reach the browser: %v", errGet)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.Contains(resp.Header.Get("Location"), "native_app_signin_succeeded") {
		t.Fatalf("valid callback answered %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for attempt := 0; attempt < 2; attempt++ {
		// A repeated poll, as after a failed save, gets the same account.
		if done := logins.poll(t.Context(), newHost(""), currentSettings(), flow.State); done.Status != pluginapi.AuthLoginStatusSuccess {
			t.Fatalf("poll %d: %+v", attempt, done)
		}
	}
}

func TestLoginFlowFromCallback(t *testing.T) {
	installAccountHost(t)
	useSettings(t, defaultSettings())
	flow, errStart := logins.start(currentSettings())
	if errStart != nil {
		t.Fatal(errStart)
	}
	if errComplete := logins.completeFromCallback(flow.State, encryptedCallback(t, flow.URL, "token-value")); errComplete != nil {
		t.Fatal(errComplete)
	}
	resp := logins.poll(t.Context(), newHost(""), currentSettings(), flow.State)
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll: %+v", resp)
	}
	if resp.Auth.Metadata["username"] != "tester" || resp.Auth.Metadata["plan"] != "zed_student" {
		t.Fatalf("auth metadata: %+v", resp.Auth.Metadata)
	}
}

func TestLoginCloseAllIsIdempotent(t *testing.T) {
	useSettings(t, defaultSettings())
	flow, errStart := logins.start(currentSettings())
	if errStart != nil {
		t.Fatal(errStart)
	}
	entry := logins.find(flow.State)
	if entry == nil {
		t.Fatal("flow not registered")
	}
	entry.close()
	entry.close()
	logins.closeAll()
	logins.closeAll()
	if logins.find(flow.State) != nil {
		t.Fatal("closeAll left a flow behind")
	}
}
