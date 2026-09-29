package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var (
	authA = selection{authID: "a.json", authIndex: "idx-a"}
	authB = selection{authID: "b.json", authIndex: "idx-b"}
	authC = selection{authID: "c.json", authIndex: "idx-c"}
	key   = sessionKey{callerScope: "caller", sessionID: "session-1", model: "claude-opus"}
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func testGuard(t *testing.T) (*guard, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	g := newGuard(time.Hour)
	g.now = func() time.Time { return clock.now }
	var mu sync.Mutex
	next := 0
	g.newID = func() string {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("smig_%d", next)
	}
	return g, clock
}

func mustAllow(t *testing.T, v verdict) {
	t.Helper()
	if !v.allow {
		t.Fatalf("verdict = %+v (binding %+v), want allow", v, v.binding)
	}
}

func mustReject(t *testing.T, v verdict, code string) *bindingView {
	t.Helper()
	if v.allow || v.code != code {
		t.Fatalf("verdict = %+v, want rejection %s", v, code)
	}
	return v.binding
}

func mustPending(t *testing.T, view *bindingView, boundIndex string, epoch uint64, toIndex string) string {
	t.Helper()
	if view == nil || view.AuthIndex != boundIndex || view.Epoch != epoch {
		t.Fatalf("binding = %+v, want bound to %s at epoch %d", view, boundIndex, epoch)
	}
	if view.Pending == nil || view.Pending.ToAuthIndex != toIndex || view.Pending.ID == "" {
		t.Fatalf("pending = %+v, want migration to %s", view.Pending, toIndex)
	}
	return view.Pending.ID
}

func mustNoPending(t *testing.T, view *bindingView, boundIndex string, epoch uint64) {
	t.Helper()
	if view == nil || view.AuthIndex != boundIndex || view.Epoch != epoch || view.Pending != nil {
		t.Fatalf("binding = %+v, want bound to %s at epoch %d with no pending migration", view, boundIndex, epoch)
	}
}

// migrateTo opens and commits a migration from A so the session is bound to sel at epoch 2.
func migrateTo(t *testing.T, g *guard, sel selection) string {
	t.Helper()
	id := mustPending(t, mustReject(t, g.decide(key, sel, ""), codeMigrationRequired), authA.authIndex, 1, sel.authIndex)
	mustAllow(t, g.decide(key, sel, id))
	return id
}

func TestSameCredentialPasses(t *testing.T) {
	g, _ := testGuard(t)
	for range 3 {
		mustAllow(t, g.decide(key, authA, ""))
	}
}

func TestSwitchBlocksAndPinsTarget(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))

	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")
	for range 3 {
		again := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")
		if again != id {
			t.Fatalf("repeated request opened migration %s, want pending %s", again, id)
		}
	}
}

func TestAckCommitsAndLaterRequestsPass(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")

	mustAllow(t, g.decide(key, authB, id))
	mustAllow(t, g.decide(key, authB, ""))

	// Returning to A is itself a switch away from the new binding.
	back := mustReject(t, g.decide(key, authA, ""), codeMigrationRequired)
	mustPending(t, back, "idx-b", 2, "idx-a")
}

func TestDuplicateAckIsNoOp(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	id := migrateTo(t, g, authB)

	mustAllow(t, g.decide(key, authB, id))
	mustAllow(t, g.decide(key, authB, id))

	// The replayed acknowledgement must not move the binding again, so the
	// next switch still starts from epoch 2.
	view := mustReject(t, g.decide(key, authC, id), codeMigrationRequired)
	mustPending(t, view, "idx-b", 2, "idx-c")
}

func TestWrongOrStaleAckFailsWithoutStateChange(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))

	mustNoPending(t, mustReject(t, g.decide(key, authA, "smig_bogus"), codeAckInvalid), "idx-a", 1)

	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")
	view := mustReject(t, g.decide(key, authB, "smig_bogus"), codeAckInvalid)
	if mustPending(t, view, "idx-a", 1, "idx-b") != id {
		t.Fatalf("wrong ack changed the pending migration: %+v", view.Pending)
	}

	// Another session cannot acknowledge this session's migration.
	other := key
	other.callerScope = "someone-else"
	if v := g.decide(other, authB, id); v.allow || v.code != codeAckInvalid || v.binding != nil {
		t.Fatalf("foreign ack verdict = %+v", v)
	}

	mustAllow(t, g.decide(key, authB, id))
}

func TestSupersededCommitAckIsTreatedAsPlainRequest(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	first := migrateTo(t, g, authB)

	second := mustPending(t, mustReject(t, g.decide(key, authC, ""), codeMigrationRequired), "idx-b", 2, "idx-c")
	// A late retry of the first commit neither commits nor disturbs the second migration.
	view := mustReject(t, g.decide(key, authC, first), codeMigrationRequired)
	if mustPending(t, view, "idx-b", 2, "idx-c") != second {
		t.Fatalf("late ack changed the pending migration: %+v", view.Pending)
	}
	mustAllow(t, g.decide(key, authC, second))
	mustReject(t, g.decide(key, authC, first), codeAckInvalid)
}

func TestAckInvalidatesMigrationWhenTargetNoLongerSelected(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")

	mustNoPending(t, mustReject(t, g.decide(key, authC, id), codeTargetUnavailable), "idx-a", 1)
	mustReject(t, g.decide(key, authC, id), codeAckInvalid)

	next := mustPending(t, mustReject(t, g.decide(key, authC, ""), codeMigrationRequired), "idx-a", 1, "idx-c")
	if next == id {
		t.Fatalf("migration to C reused id %s of the migration to B", id)
	}
	mustAllow(t, g.decide(key, authC, next))
}

func TestPlainRequestInvalidatesMigrationWhenTargetNoLongerSelected(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")

	next := mustPending(t, mustReject(t, g.decide(key, authC, ""), codeMigrationRequired), "idx-a", 1, "idx-c")
	if next == id {
		t.Fatalf("migration was retargeted to C under id %s", id)
	}
	// The migration to B can no longer commit, not even to C.
	mustReject(t, g.decide(key, authC, id), codeAckInvalid)
	mustReject(t, g.decide(key, authB, id), codeAckInvalid)
}

func TestReturnToBoundCredentialDropsPendingMigration(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")

	mustAllow(t, g.decide(key, authA, ""))
	mustNoPending(t, mustReject(t, g.decide(key, authA, id), codeAckInvalid), "idx-a", 1)
}

func TestSessionsAreIsolated(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))

	otherModel := key
	otherModel.model = "claude-haiku"
	mustAllow(t, g.decide(otherModel, authB, ""))

	otherCaller := key
	otherCaller.callerScope = "other"
	mustAllow(t, g.decide(otherCaller, authB, ""))

	mustAllow(t, g.decide(key, authA, ""))
}

func TestAckWithoutBindingFails(t *testing.T) {
	g, _ := testGuard(t)
	if v := g.decide(key, authA, "smig_1"); v.allow || v.code != codeAckInvalid || v.binding != nil {
		t.Fatalf("verdict = %+v", v)
	}
	mustAllow(t, g.decide(key, authB, ""))
}

func TestIdleBindingExpires(t *testing.T) {
	g, clock := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	clock.advance(59 * time.Minute)
	mustAllow(t, g.decide(key, authA, ""))
	clock.advance(59 * time.Minute)
	mustReject(t, g.decide(key, authB, ""), codeMigrationRequired)

	clock.advance(2 * time.Hour)
	mustAllow(t, g.decide(key, authB, ""))
	if len(g.bindings) != 1 {
		t.Fatalf("bindings = %d, want expired entries swept", len(g.bindings))
	}
}

func TestConcurrentAcksCommitOnce(t *testing.T) {
	g, _ := testGuard(t)
	mustAllow(t, g.decide(key, authA, ""))
	id := mustPending(t, mustReject(t, g.decide(key, authB, ""), codeMigrationRequired), "idx-a", 1, "idx-b")

	var wg sync.WaitGroup
	results := make(chan verdict, 32)
	for range cap(results) {
		wg.Go(func() { results <- g.decide(key, authB, id) })
	}
	wg.Wait()
	close(results)
	for v := range results {
		mustAllow(t, v)
	}
	view := mustReject(t, g.decide(key, authC, ""), codeMigrationRequired)
	mustPending(t, view, "idx-b", 2, "idx-c")
}
