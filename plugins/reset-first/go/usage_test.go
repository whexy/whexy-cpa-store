package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func codexHeaders(fiveUsed, weekUsed string) http.Header {
	return http.Header{
		"X-Codex-Primary-Used-Percent":     {fiveUsed},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {"1761498000"},
		"X-Codex-Secondary-Used-Percent":   {weekUsed},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {"1761800000"},
	}
}

func TestObserveReadsQuotaFromResponses(t *testing.T) {
	s := newState()
	s.observe(usageRecord{Provider: "codex", AuthID: "a", Model: "gpt-5", RequestedAt: t0, ResponseHeaders: codexHeaders("10", "20")}, t0.Add(time.Second), defaultPollInterval)
	a := s.accounts["a"]
	if a == nil || a.Quota == nil || a.Quota.Windows[1].Used != 20 {
		t.Fatalf("account = %+v", a)
	}
	if !a.nextPoll.After(t0.Add(time.Minute)) {
		t.Errorf("next poll %v: a response should postpone it", a.nextPoll)
	}
	// A slower request answering later with an older reading changes nothing.
	s.observe(usageRecord{Provider: "codex", AuthID: "a", RequestedAt: t0.Add(-time.Minute), ResponseHeaders: codexHeaders("5", "15")}, t0.Add(2*time.Second), defaultPollInterval)
	if a.Quota.Windows[1].Used != 20 {
		t.Errorf("an older reading replaced a newer one: %+v", a.Quota)
	}
	// Other providers report no subscription quota.
	s.observe(usageRecord{Provider: "openai-compatible-x", AuthID: "b", ResponseHeaders: codexHeaders("1", "1")}, t0, defaultPollInterval)
	if s.accounts["b"] != nil {
		t.Errorf("an openai-compatible response gave quota: %+v", s.accounts["b"])
	}
}

func TestObserveCountsOnTheModelsCard(t *testing.T) {
	s := newState()
	codexAccount(s, "a", 48*time.Hour, 0)
	codexAccount(s, "b", 96*time.Hour, 0)
	s.pick(pickRequest("gpt-5", "a", "b"), t0)

	s.observe(usageRecord{Provider: "codex", AuthID: "a", Model: "gpt-5-2026", Alias: "gpt-5(high)", Generate: true, RequestedAt: t0}, t0.Add(time.Second), 0)
	s.observe(usageRecord{Provider: "codex", AuthID: "b", Model: "gpt-5", Generate: true, Failed: true, RequestedAt: t0}, t0.Add(time.Second), 0)
	s.observe(usageRecord{Provider: "codex", AuthID: "a", Model: "gpt-5", Generate: false, RequestedAt: t0}, t0.Add(time.Second), 0)

	view := s.decisionsView(t0.Add(time.Minute))
	if len(view.Cards) != 1 {
		t.Fatalf("cards = %+v", view.Cards)
	}
	card := view.Cards[0]
	if card.Served != 1 || card.Failed != 1 {
		t.Errorf("served %d failed %d, want a token count left out", card.Served, card.Failed)
	}
	if card.Outcome == nil || card.Outcome.Failed {
		t.Errorf("outcome = %+v, want the chosen account's answer", card.Outcome)
	}

	s.observe(usageRecord{Provider: "codex", AuthID: "a", Model: "gpt-5", Generate: true, Failed: true, Failure: struct{ StatusCode int }{429}, RequestedAt: t0}, t0.Add(2*time.Second), 0)
	if out := s.decisions["gpt-5"].Outcome; out == nil || !out.Failed || out.Status != 429 {
		t.Errorf("outcome = %+v, want the 429", out)
	}
}

func TestObserveNamesTheAccountCPAChose(t *testing.T) {
	s := newState()
	apiKeyAccounts(s, "key-a", "key-b")
	s.pick(pickRequest("gemini-3-pro", "key-a", "key-b"), t0)
	s.observe(usageRecord{Provider: "gemini", AuthID: "key-b", Model: "gemini-3-pro", Generate: true, RequestedAt: t0}, t0.Add(time.Second), 0)
	d := s.decisions["gemini-3-pro"]
	if !d.ByCPA || d.Chosen != "key-b" || d.Outcome == nil {
		t.Fatalf("decision = %+v", d)
	}
	since := d.Since
	s.pick(pickRequest("gemini-3-pro", "key-a", "key-b"), t0.Add(time.Minute))
	s.observe(usageRecord{Provider: "gemini", AuthID: "key-b", Model: "gemini-3-pro", Generate: true, RequestedAt: t0.Add(time.Minute)}, t0.Add(time.Minute+time.Second), 0)
	if !d.Since.Equal(since) {
		t.Errorf("since moved to %v while CPA stayed on key-b", d.Since)
	}
}

func TestHandleUsageReadsTheHostWire(t *testing.T) {
	current = newState()
	t.Cleanup(func() { current = newState() })
	current.pick(pickRequest("gpt-5", "a"), time.Now())
	// pluginapi.UsageRecord has no JSON tags and Latency is in nanoseconds.
	raw := []byte(`{"Provider":"codex","Model":"gpt-5","AuthID":"a","AuthIndex":"3","Generate":true,
		"RequestedAt":"` + time.Now().Add(-3*time.Second).Format(time.RFC3339Nano) + `","Latency":2000000000,
		"Failed":false,"Failure":{"StatusCode":0,"Body":""},
		"ResponseHeaders":{"X-Codex-Primary-Used-Percent":["7"],"X-Codex-Primary-Window-Minutes":["10080"],"X-Codex-Primary-Reset-After-Seconds":["86400"]}}`)
	out, errHandle := handleUsage(raw)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var env envelope
	if json.Unmarshal(out, &env) != nil || !env.OK {
		t.Fatalf("envelope %s", out)
	}
	a := current.accounts["a"]
	if a.Quota == nil || a.Quota.Windows[0].Used != 7 {
		t.Fatalf("quota = %+v", a.Quota)
	}
	// The reset counts from the response, two seconds after the request.
	wantReset := a.Quota.At.Add(24 * time.Hour)
	if !a.Quota.Windows[0].ResetAt.Equal(wantReset) || time.Since(a.Quota.At) < time.Second/2 {
		t.Errorf("reading at %v resets at %v", a.Quota.At, a.Quota.Windows[0].ResetAt)
	}
}
