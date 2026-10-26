package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// stickiness is how much sooner another account must reset before a model
// leaves the account it is on. A plugin's pick replaces CPA's session
// affinity, so every move costs the sessions on that model their prompt
// cache; readings derived from reset-after-seconds also drift by seconds, and
// would otherwise swap two accounts that reset together.
const stickiness = time.Hour

// trialSpacing paces the requests that read an account's quota from its
// response: an account the usage endpoint cannot reach would otherwise never
// be routed to while another one has quota known, and never be read.
const trialSpacing = 10 * time.Minute

const (
	reasonTrial     = "trial"
	reasonSoonest   = "soonest"
	reasonKept      = "kept"
	reasonOnlySpare = "only_spare"
	reasonOnly      = "only"
	reasonNoQuota   = "no_quota"
)

func handlePick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode scheduler pick: %w", errUnmarshal)
	}
	return okEnvelope(current.pick(req, time.Now()))
}

// pick answers one credential selection: the offered account with quota to
// spare whose allowance resets soonest, unless an account's quota is yet to be
// read. Without either, CPA's own strategy and session affinity choose.
func (s *state) pick(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	key := modelKey(req.Model)
	if key == "" || len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	offered := make([]ranked, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}
		a := s.accountLocked(id, candidate.Provider)
		s.describeLocked(a, candidate.Attributes)
		offered = append(offered, ranked{id: id, standing: evaluate(a.Quota, now)})
	}
	if len(offered) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	sortRanked(offered)
	d := s.decisions[key]
	if d == nil {
		d = &decision{Model: key}
		s.decisions[key] = d
	}
	chosen, reason := s.trialLocked(offered, now), reasonTrial
	if chosen == "" {
		chosen, reason = choose(offered, d.Chosen)
	}
	d.record(s, offered, chosen, reason, now)
	s.dirty = true
	if chosen == "" {
		return pluginapi.SchedulerPickResponse{}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: chosen}
}

// describeLocked fills what a candidate says about an account host.auth.list
// does not cover.
func (s *state) describeLocked(a *account, attributes map[string]string) {
	if a.Plan == "" {
		if plan := strings.TrimSpace(attributes["plan_type"]); plan != "" {
			a.Plan = plan
			s.dirty = true
		}
	}
	if a.Label == "" {
		if label := firstNonEmpty(attributes["email"], attributes["compat_name"], endpointLabel(attributes["base_url"]), keyLabel(a.ID)); label != "" {
			a.Label = label
			s.dirty = true
		}
	}
}

// endpointLabel names an API key by the upstream it reaches, as in
// relay.example.com/v1.
func endpointLabel(baseURL string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host + strings.TrimRight(parsed.Path, "/")
}

// keyLabel names an API key from config.yaml, whose ID is
// <provider>:apikey:<hash>, by the start of its hash.
func keyLabel(id string) string {
	_, hash, ok := strings.Cut(id, ":apikey:")
	if !ok || hash == "" {
		return ""
	}
	return "API key " + hash[:min(6, len(hash))]
}

// trialLocked returns an offered Codex or Claude account whose quota was never
// read and that had no trial within trialSpacing, and starts its trial.
func (s *state) trialLocked(offered []ranked, now time.Time) string {
	for _, r := range offered {
		a := s.accounts[r.id]
		if a == nil || a.Quota != nil || !quotaProvider(a.Provider) || now.Sub(a.trialAt) < trialSpacing {
			continue
		}
		a.trialAt = now
		return r.id
	}
	return ""
}

type ranked struct {
	id       string
	standing standing
}

var kindOrder = map[standingKind]int{standingSpare: 0, standingSpent: 1, standingUnknown: 2}

// sortRanked orders accounts as the scheduler prefers them: with quota to
// spare, soonest reset first; then spent, soonest back first; then unknown.
// An unknown time sorts after every known one.
func sortRanked(list []ranked) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].standing, list[j].standing
		if kindOrder[a.Kind] != kindOrder[b.Kind] {
			return kindOrder[a.Kind] < kindOrder[b.Kind]
		}
		var at, bt time.Time
		switch a.Kind {
		case standingSpare:
			at, bt = a.ResetAt, b.ResetAt
		case standingSpent:
			at, bt = a.BackAt, b.BackAt
		}
		if !at.Equal(bt) {
			if at.IsZero() || bt.IsZero() {
				return bt.IsZero()
			}
			return at.Before(bt)
		}
		return list[i].id < list[j].id
	})
}

// choose returns the account to route to, or "" to leave the choice to CPA,
// and why. The account the model is on stays while it resets within
// stickiness of the soonest.
func choose(list []ranked, previous string) (string, string) {
	if len(list) == 0 || list[0].standing.Kind != standingSpare {
		return "", reasonNoQuota
	}
	best := list[0]
	spare := 0
	for _, r := range list {
		if r.standing.Kind != standingSpare {
			continue
		}
		spare++
		if r.id == previous && r.id != best.id && withinStickiness(r.standing.ResetAt, best.standing.ResetAt) {
			return r.id, reasonKept
		}
	}
	switch {
	case len(list) == 1:
		return best.id, reasonOnly
	case spare == 1:
		return best.id, reasonOnlySpare
	default:
		return best.id, reasonSoonest
	}
}

func withinStickiness(reset, soonest time.Time) bool {
	if reset.IsZero() || soonest.IsZero() {
		return reset.IsZero() && soonest.IsZero()
	}
	return reset.Sub(soonest) <= stickiness
}

// modelKey names a model's card: the requested model without a thinking
// suffix such as (high), which changes the effort but not the accounts.
func modelKey(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasSuffix(model, ")") {
		if open := strings.LastIndex(model, "("); open > 0 {
			model = strings.TrimSpace(model[:open])
		}
	}
	return model
}
