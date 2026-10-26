package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// usageRecord is the part of pluginapi.UsageRecord read here. The record
// travels without JSON tags, so keys are the Go field names and Latency is in
// nanoseconds.
type usageRecord struct {
	Provider    string
	Model       string
	Alias       string
	AuthID      string
	Generate    bool
	RequestedAt time.Time
	Latency     time.Duration
	Failed      bool
	Failure     struct {
		StatusCode int
	}
	ResponseHeaders http.Header
}

func handleUsage(raw []byte) ([]byte, error) {
	var rec usageRecord
	if errUnmarshal := json.Unmarshal(raw, &rec); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage record: %w", errUnmarshal)
	}
	current.observe(rec, time.Now(), currentSettings().PollInterval)
	return okEnvelope(struct{}{})
}

// observe reads the quota a response reported and counts it on its model's
// card. A response keeps its account current, so the poll waits.
func (s *state) observe(rec usageRecord, now time.Time, interval time.Duration) {
	id := strings.TrimSpace(rec.AuthID)
	provider := providerKeyOf(rec.Provider)
	at := now
	if answered := rec.RequestedAt.Add(rec.Latency); !rec.RequestedAt.IsZero() && answered.Before(now) {
		at = answered
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" && len(rec.ResponseHeaders) > 0 {
		var q *quota
		switch provider {
		case providerCodex:
			q = codexQuotaFromHeaders(rec.ResponseHeaders, at)
		case providerClaude:
			q = claudeQuotaFromHeaders(rec.ResponseHeaders, at)
		}
		if q != nil {
			a := s.accountLocked(id, provider)
			s.applyReadingLocked(a, q)
			if interval > 0 {
				a.nextPoll = nextPoll(a.Quota, now, interval)
			}
		}
	}
	if !rec.Generate {
		return
	}
	// The scheduler sees the model the client asked for, which the record
	// keeps as Alias when CPA mapped it to another one.
	d := s.decisions[modelKey(firstNonEmpty(rec.Alias, rec.Model))]
	if d == nil {
		d = s.decisions[modelKey(rec.Model)]
	}
	if d == nil {
		return
	}
	d.observe(rec, at)
	s.dirty = true
}
