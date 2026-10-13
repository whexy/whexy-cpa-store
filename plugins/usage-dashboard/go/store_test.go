package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEvent(at time.Time, account, model string, tokens int64, session string) usageEvent {
	return usageEvent{
		At:       at,
		Account:  account,
		Identity: accountInfo{Label: account + "@example.com", Provider: "claude"},
		Provider: "claude",
		Model:    model,
		Session:  session,
		Counters: counters{Requests: 1, Input: tokens, Output: 1},
	}
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func totalProcessed(rows []storedRow) int64 {
	var total int64
	for _, r := range rows {
		total += r.Counters.processed()
	}
	return total
}

func TestStorePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC)
	first := newStore(dir, 30)
	first.now = fixedClock(now)
	first.tick()
	first.add(testEvent(now, "acct", "claude-opus-5-5", 100, "s1"))
	first.add(testEvent(now.Add(-24*time.Hour), "acct", "claude-opus-5-5", 50, "s2"))
	first.add(testEvent(now, "acct", "claude-opus-5-5", 10, "s1"))
	first.tick()
	if status := first.status(); !status.Persisting || status.Error != "" {
		t.Fatalf("status = %+v", status)
	}
	for _, name := range []string{"usage-2026-10-12.json", "usage-2026-10-11.json"} {
		info, errStat := os.Stat(filepath.Join(dir, name))
		if errStat != nil {
			t.Fatalf("%s: %v", name, errStat)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", name, info.Mode().Perm())
		}
	}
	first.close()

	second := newStore(dir, 30)
	second.now = fixedClock(now)
	second.tick()
	rows, accounts := second.snapshot(time.Time{}, now.Add(time.Hour))
	if got := totalProcessed(rows); got != 163 {
		t.Fatalf("processed after reload = %d, want 163", got)
	}
	if accounts["acct"].Label != "acct@example.com" {
		t.Fatalf("accounts = %+v", accounts)
	}
	for _, r := range rows {
		if r.At.Equal(now.Truncate(time.Hour)) && (r.Counters.Requests != 2 || len(r.Sessions) != 1) {
			t.Fatalf("15:00 row = %+v, want 2 requests in 1 session", r)
		}
	}
	second.close()
}

func spillFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, errGlob := filepath.Glob(filepath.Join(dir, spillFilePrefix+"*"))
	if errGlob != nil {
		t.Fatal(errGlob)
	}
	return matches
}

func TestStoreWithoutLockSpillsToTheHolder(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	holder := newStore(dir, 30)
	holder.now = fixedClock(now)
	holder.tick()
	holder.add(testEvent(now, "a", "gpt-6.1-sol", 100, ""))

	other := newStore(dir, 30)
	other.now = fixedClock(now)
	other.add(testEvent(now, "a", "gpt-6.1-sol", 7, ""))
	other.tick()
	if other.status().Persisting {
		t.Fatal("a second store must not load while the lock is held")
	}
	if len(spillFiles(t, dir)) != 1 {
		t.Fatal("the store without the lock must spill its usage")
	}

	holder.tick()
	if spilled := spillFiles(t, dir); len(spilled) != 0 {
		t.Fatalf("merged spill files must be removed, left %v", spilled)
	}
	rows, _ := holder.snapshot(time.Time{}, now.Add(time.Hour))
	if got := totalProcessed(rows); got != 109 {
		t.Fatalf("processed = %d, want both stores' usage (109)", got)
	}
	holder.close()
	other.close()

	reread := newStore(dir, 30)
	reread.now = fixedClock(now)
	reread.tick()
	rows, _ = reread.snapshot(time.Time{}, now.Add(time.Hour))
	if got := totalProcessed(rows); got != 109 {
		t.Fatalf("processed on disk = %d, want 109", got)
	}
	reread.close()
}

// An upgrade quiesces the old library, loads the new one, and keeps the old
// one loaded; a failed upgrade registers the old one again.
func TestStoreQuiesceHandsOverAndResumes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	old := newStore(dir, 30)
	old.now = fixedClock(now)
	old.tick()
	old.add(testEvent(now, "a", "claude-opus-5-5", 100, "s1"))

	old.quiesce()
	if old.status().Persisting {
		t.Fatal("a quiesced store must release the directory")
	}
	upgraded := newStore(dir, 30)
	upgraded.now = fixedClock(now)
	upgraded.tick()
	if !upgraded.status().Persisting {
		t.Fatal("the new library must take the directory over")
	}

	// A request the old library was still serving finishes after quiesce.
	old.add(testEvent(now.Add(-48*time.Hour), "a", "claude-opus-5-5", 5, "s2"))
	old.tick()
	upgraded.tick()
	rows, _ := upgraded.snapshot(time.Time{}, now.Add(time.Hour))
	if got := totalProcessed(rows); got != 107 {
		t.Fatalf("processed = %d, want 101 + the late 6", got)
	}
	if _, errStat := os.Stat(filepath.Join(dir, "usage-2026-10-10.json")); errStat != nil {
		t.Fatalf("the late usage must reach its day file: %v", errStat)
	}

	upgraded.quiesce()
	old.resume()
	old.tick()
	rows, _ = old.snapshot(time.Time{}, now.Add(time.Hour))
	if got := totalProcessed(rows); got != 107 {
		t.Fatalf("after rollback processed = %d, want 107 without double counting", got)
	}
	old.close()
}

func TestStorePrunesPastRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	s := newStore(dir, 3)
	s.now = fixedClock(now)
	s.tick()
	for back := 0; back <= 6; back++ {
		s.add(testEvent(now.AddDate(0, 0, -back), "a", "m", 1, ""))
	}
	s.tick()
	s.setRetention(3)
	s.tick()
	entries, _ := os.ReadDir(dir)
	var days []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), dayFilePrefix) {
			days = append(days, entry.Name())
		}
	}
	// Retention 3 keeps 3 days plus one spare UTC day: Oct 8 to Oct 12.
	if len(days) != 5 || days[0] != "usage-2026-10-08.json" {
		t.Fatalf("days on disk = %v", days)
	}
	rows, _ := s.snapshot(time.Time{}, now.Add(time.Hour))
	if len(rows) != 5 {
		t.Fatalf("rows in memory = %d, want 5", len(rows))
	}
	s.close()
}

func TestStoreMovesUnreadableDayAside(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(dir, "usage-2026-10-12.json"), []byte("{not json"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	s := newStore(dir, 30)
	s.now = fixedClock(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	s.tick()
	if !s.status().Persisting {
		t.Fatal("one bad file must not stop the store")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "usage-2026-10-12.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt copies = %v", matches)
	}
	s.close()
}

func TestStoreReportsUnwritableDirectory(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "file")
	if errWrite := os.WriteFile(blocker, nil, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	s := newStore(filepath.Join(blocker, "data"), 30)
	s.tick()
	s.add(testEvent(time.Now().UTC(), "a", "m", 1, ""))
	status := s.status()
	if status.Persisting || status.Error == "" {
		t.Fatalf("status = %+v, want an error", status)
	}
	if rows, _ := s.snapshot(time.Time{}, time.Now().Add(time.Hour)); len(rows) != 1 {
		t.Fatal("usage must still be kept in memory")
	}
}
