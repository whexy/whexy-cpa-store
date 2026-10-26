package main

import (
	"sort"
	"time"
)

// decision is a model's latest routing decision: what its card shows.
type decision struct {
	Model string    `json:"model"`
	At    time.Time `json:"at"`
	// Chosen is the account the scheduler routed to, or, when CPA chose, the
	// account a usage record last named; empty until one did.
	Chosen string `json:"chosen,omitempty"`
	ByCPA  bool   `json:"by_cpa,omitempty"`
	// Since is when the model came to its current account.
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`
	// Accounts are the offered accounts in the scheduler's order, then those
	// offered for this model earlier in the history but not this time: tried
	// already, cooling down or disabled.
	Accounts []snapshot `json:"accounts"`
	Outcome  *outcome   `json:"outcome,omitempty"`
	// Seen is when each account was last offered for this model.
	Seen map[string]time.Time `json:"seen,omitempty"`
	// Served and Failed count upstream attempts per hour, keyed by the hour's
	// Unix time.
	Served map[int64]int `json:"served,omitempty"`
	Failed map[int64]int `json:"failed,omitempty"`
}

// snapshot is one account as it stood when the decision was made.
type snapshot struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider"`
	Plan     string `json:"plan,omitempty"`
	Offered  bool   `json:"offered"`
	standing
	// ReadAt is when the quota shown was read, zero when unknown.
	ReadAt  time.Time `json:"read_at,omitzero"`
	Problem string    `json:"problem,omitempty"`
}

// outcome is how the chosen account answered.
type outcome struct {
	Failed bool      `json:"failed,omitempty"`
	Status int       `json:"status,omitempty"`
	At     time.Time `json:"at"`
}

func (d *decision) record(s *state, offered []ranked, chosen, reason string, now time.Time) {
	if d.Seen == nil {
		d.Seen = make(map[string]time.Time)
	}
	switch {
	case chosen != "":
		if chosen != d.Chosen || d.ByCPA {
			d.Since = now
		}
		d.Chosen, d.ByCPA = chosen, false
	case !d.ByCPA:
		d.Chosen, d.ByCPA, d.Since = "", true, now
	}
	d.At, d.Reason, d.Outcome = now, reason, nil

	isOffered := make(map[string]bool, len(offered))
	d.Accounts = d.Accounts[:0]
	for _, r := range offered {
		isOffered[r.id] = true
		d.Seen[r.id] = now
		d.Accounts = append(d.Accounts, s.snapshotLocked(r, true))
	}
	var others []ranked
	for id, at := range d.Seen {
		if isOffered[id] || now.Sub(at) > history {
			continue
		}
		a := s.accounts[id]
		if a == nil {
			continue
		}
		others = append(others, ranked{id: id, standing: evaluate(a.Quota, now)})
	}
	sortRanked(others)
	for _, r := range others {
		d.Accounts = append(d.Accounts, s.snapshotLocked(r, false))
	}
}

func (s *state) snapshotLocked(r ranked, offered bool) snapshot {
	snap := snapshot{ID: r.id, Offered: offered, standing: r.standing}
	if a := s.accounts[r.id]; a != nil {
		snap.Label, snap.Provider, snap.Plan = a.Label, a.Provider, a.Plan
		if a.Quota != nil {
			snap.ReadAt = a.Quota.At
		} else {
			snap.Problem = a.Problem
		}
	}
	return snap
}

// observe applies one usage record to the decision of its model: the count,
// the chosen account's answer and, when CPA chose, which account answered.
func (d *decision) observe(rec usageRecord, at time.Time) {
	hour := at.Truncate(time.Hour).Unix()
	if rec.Failed {
		if d.Failed == nil {
			d.Failed = make(map[int64]int)
		}
		d.Failed[hour]++
	} else {
		if d.Served == nil {
			d.Served = make(map[int64]int)
		}
		d.Served[hour]++
	}
	if d.ByCPA && !rec.Failed && rec.AuthID != "" && rec.AuthID != d.Chosen {
		d.Chosen, d.Since = rec.AuthID, at
	}
	if rec.AuthID == "" || rec.AuthID != d.Chosen {
		return
	}
	d.Outcome = &outcome{Failed: rec.Failed, Status: rec.Failure.StatusCode, At: at}
}

func (d *decision) prune(horizon time.Time) {
	for id, at := range d.Seen {
		if at.Before(horizon) {
			delete(d.Seen, id)
		}
	}
	for _, counts := range []map[int64]int{d.Served, d.Failed} {
		for hour := range counts {
			if time.Unix(hour, 0).Add(time.Hour).Before(horizon) {
				delete(counts, hour)
			}
		}
	}
}

func countSince(counts map[int64]int, horizon time.Time) int {
	total := 0
	for hour, count := range counts {
		if !time.Unix(hour, 0).Add(time.Hour).Before(horizon) {
			total += count
		}
	}
	return total
}

type decisionsView struct {
	Now   time.Time  `json:"now"`
	Cards []cardView `json:"cards"`
}

type cardView struct {
	Model    string     `json:"model"`
	At       time.Time  `json:"at"`
	Since    time.Time  `json:"since"`
	Chosen   string     `json:"chosen,omitempty"`
	ByCPA    bool       `json:"by_cpa,omitempty"`
	Reason   string     `json:"reason"`
	Served   int        `json:"served"`
	Failed   int        `json:"failed"`
	Outcome  *outcome   `json:"outcome,omitempty"`
	Accounts []snapshot `json:"accounts"`
}

// decisionsView lists the latest decision of every model routed in the
// history, the most recent first.
func (s *state) decisionsView(now time.Time) decisionsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	horizon := now.Add(-history)
	view := decisionsView{Now: now, Cards: []cardView{}}
	for _, d := range s.decisions {
		if d.At.Before(horizon) {
			continue
		}
		card := cardView{
			Model: d.Model, At: d.At, Since: d.Since, Chosen: d.Chosen, ByCPA: d.ByCPA, Reason: d.Reason,
			Served: countSince(d.Served, horizon), Failed: countSince(d.Failed, horizon), Outcome: d.Outcome,
			Accounts: append([]snapshot(nil), d.Accounts...),
		}
		for i := range card.Accounts {
			if a := s.accounts[card.Accounts[i].ID]; a != nil && a.Label != "" {
				card.Accounts[i].Label = a.Label
			}
			if card.Accounts[i].Label == "" {
				card.Accounts[i].Label = card.Accounts[i].ID
			}
		}
		view.Cards = append(view.Cards, card)
	}
	sort.Slice(view.Cards, func(i, j int) bool {
		if !view.Cards[i].At.Equal(view.Cards[j].At) {
			return view.Cards[i].At.After(view.Cards[j].At)
		}
		return view.Cards[i].Model < view.Cards[j].Model
	})
	return view
}
