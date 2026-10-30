package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func testAuthJSON(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"type":            "zed",
		"user_id":         "12345",
		"access_token":    "access-token-value",
		"system_id":       "11111111-2222-4333-8444-555555555555",
		"organization_id": "org_test",
		"username":        "tester",
		"plan":            "zed_student",
		"label":           "Work",
		"priority":        3,
	})
}

func TestParseAuthFile(t *testing.T) {
	file, errParse := parseAuthFile(testAuthJSON(t))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if file.UserID != "12345" || file.AccessToken != "access-token-value" {
		t.Fatalf("credentials: %+v", file)
	}
	if file.Plan != "zed_student" || file.Username != "tester" {
		t.Fatalf("account: %+v", file)
	}
	if _, ok := file.extra["priority"]; !ok {
		t.Fatal("unrelated fields must survive a rewrite")
	}
	auth := file.authData("zed-oauth-tester.json", testAuthJSON(t))
	if auth.ID != "zed-oauth-tester.json" || auth.FileName != "zed-oauth-tester.json" {
		t.Fatalf("auth identity: %+v", auth)
	}
	if auth.Metadata["access_token"] != "access-token-value" {
		t.Fatal("access token missing from metadata")
	}
	if auth.Attributes[authKindAttribute] != "oauth" {
		t.Fatal("auth must classify as oauth")
	}
	if !strings.Contains(auth.Label, "Student") {
		t.Fatalf("label: %q", auth.Label)
	}
}

func TestParseAuthFileDerivesSystemID(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"type":         "zed",
		"user_id":      "9",
		"access_token": "token",
	})
	first, errFirst := parseAuthFile(raw)
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, _ := parseAuthFile(raw)
	if first.SystemID == "" || first.SystemID != second.SystemID {
		t.Fatalf("system id must be stable, got %q and %q", first.SystemID, second.SystemID)
	}
	if !validUUIDShape(first.SystemID) {
		t.Fatalf("system id %q is not UUID-shaped", first.SystemID)
	}
}

func validUUIDShape(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-'
}

func TestParseAuthRejectsOtherTypes(t *testing.T) {
	if _, errParse := parseAuthFile([]byte(`{"type":"claude","access_token":"x"}`)); errParse == nil {
		t.Fatal("a non-Zed file must not parse")
	}
}

func TestAuthFileMarshalKeepsExtraFields(t *testing.T) {
	file, _ := parseAuthFile(testAuthJSON(t))
	file.Plan = "zed_pro"
	raw, errMarshal := file.marshal()
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var top map[string]any
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if top["type"] != "zed" || top["plan"] != "zed_pro" {
		t.Fatalf("saved file: %s", raw)
	}
	if top["priority"] != float64(3) {
		t.Fatalf("extra field lost: %s", raw)
	}
}

func TestPlanLabel(t *testing.T) {
	cases := map[string]string{
		"token_based_zed_student": "Student",
		"token_based_zed_pro":     "Pro",
		"zed_business":            "Business",
		"zed_free":                "Free",
	}
	for plan, want := range cases {
		if got := planLabel(plan); got != want {
			t.Errorf("planLabel(%q) = %q, want %q", plan, got, want)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3OTM0MDY3NzR9.c2lnbmF0dXJlLXNpZ25hdHVyZQ"
	text := redactSecrets(`{"access_token":"abcdefghijklmnopqrstuvwxyz","token":"` + jwt + `"}`)
	if strings.Contains(text, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(text, jwt) {
		t.Fatalf("secret survived redaction: %s", text)
	}
}

func TestResolveCredentialFromMetadata(t *testing.T) {
	cred, errCred := resolveCredential(nil, map[string]string{
		userIDAttribute:   "42",
		systemIDAttribute: "sys",
	}, map[string]any{
		"access_token":    "token",
		"organization_id": "org",
		"username":        "tester",
		"plan":            "zed_student",
	})
	if errCred != nil {
		t.Fatal(errCred)
	}
	if cred.UserID != "42" || cred.AccessToken != "token" || cred.OrganizationID != "org" {
		t.Fatalf("credential: %+v", cred)
	}
}
