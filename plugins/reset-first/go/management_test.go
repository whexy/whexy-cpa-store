package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegistrationDeclaresTheScheduler(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginRegistration())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var got struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      pluginapi.Metadata
		Capabilities  map[string]bool `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, capability := range []string{"scheduler", "usage_plugin", "management_api"} {
		if !got.Capabilities[capability] {
			t.Errorf("capability %s not declared: %s", capability, raw)
		}
	}
	// Only the top priority tier: CPA's priorities decide before the reset.
	if got.Capabilities["scheduler_across_priorities"] {
		t.Error("scheduler_across_priorities is declared")
	}
	if got.SchemaVersion != pluginabi.SchemaVersion || got.Metadata.Version != pluginVersion || got.Metadata.GitHubRepository == "" {
		t.Errorf("registration = %s", raw)
	}
}

func TestManagementRoutes(t *testing.T) {
	current = newState()
	t.Cleanup(func() { current = newState() })
	now := time.Now()
	codexAccount(current, "a", 48*time.Hour, 0)
	current.pick(pickRequest("gpt-5", "a"), now)

	page := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: resourcePagePath}, now)
	csp := page.Headers.Get("Content-Security-Policy")
	if page.StatusCode != http.StatusOK || !bytes.Contains(page.Body, []byte("<title>Reset First</title>")) {
		t.Fatalf("page status %d", page.StatusCode)
	}
	if strings.Count(csp, "'sha256-") != 3 || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP %q, want one hash per inline script and style", csp)
	}

	logo := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: logoPath}, now)
	if logo.StatusCode != http.StatusOK || !bytes.Contains(logo.Body, []byte("<svg")) {
		t.Fatalf("logo status %d", logo.StatusCode)
	}

	decisions := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: decisionsPath}, now)
	var view decisionsView
	if decisions.StatusCode != http.StatusOK || json.Unmarshal(decisions.Body, &view) != nil || len(view.Cards) != 1 {
		t.Fatalf("decisions %d: %s", decisions.StatusCode, decisions.Body)
	}
	if card := view.Cards[0]; card.Model != "gpt-5" || card.Chosen != "a" || card.Accounts[0].Label != "a@example.com" {
		t.Errorf("card = %+v", card)
	}

	if missing := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: decisionsPath + "/x"}, now); missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route status %d", missing.StatusCode)
	}
	if post := routeManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: decisionsPath}, now); post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status %d", post.StatusCode)
	}
}

func TestDecisionsAreSortedByTheLatest(t *testing.T) {
	s := newState()
	codexAccount(s, "a", 48*time.Hour, 0)
	s.pick(pickRequest("first", "a"), t0)
	s.pick(pickRequest("second", "a"), t0.Add(time.Minute))
	s.pick(pickRequest("first", "a"), t0.Add(2*time.Minute))
	s.pick(pickRequest("stale", "a"), t0.Add(-history-time.Minute))
	view := s.decisionsView(t0.Add(3 * time.Minute))
	var models []string
	for _, card := range view.Cards {
		models = append(models, card.Model)
	}
	if strings.Join(models, ",") != "first,second" {
		t.Errorf("cards = %v", models)
	}
}

// TestManagementPageLogic runs the page's card script under node, which the
// nix build provides to the check phase.
func TestManagementPageLogic(t *testing.T) {
	node, errLook := exec.LookPath("node")
	if errLook != nil {
		t.Skip("node is not installed")
	}
	out, errRun := exec.Command(node, "--test", "testdata/page_test.mjs").CombinedOutput()
	if errRun != nil {
		t.Fatalf("node --test: %v\n%s", errRun, out)
	}
}
