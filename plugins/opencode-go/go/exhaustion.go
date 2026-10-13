package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// unknownResetBlock is how long a credential stays blocked when the gateway reports
// GoUsageLimitError but /v1/usage names no exhausted window with a reset
// time. It is short because the next request simply asks the gateway again.
const unknownResetBlock = time.Minute

// usageBlock stops paid-model requests on a credential whose subscription
// quota is used up, so they fail fast instead of reaching the gateway.
type usageBlock struct {
	Until   time.Time
	Windows []string
	// authIDs are the auths that hit the block; CPA cooled them down and
	// gets told to stop when the block ends.
	authIDs map[string]struct{}
	gen     uint64
}

// retryAfter is the cooldown CPA should apply for a request this block
// rejects: the time left until the exhausted window resets.
func (b usageBlock) retryAfter(now time.Time) time.Duration {
	if retryAfter := b.Until.Sub(now); retryAfter > 0 {
		return retryAfter
	}
	return 0
}

func (b usageBlock) message() string {
	window := "usage"
	if len(b.Windows) > 0 {
		window = strings.Join(b.Windows, " and ")
	}
	return fmt.Sprintf("OpenCode Go %s limit reached on this credential; paid models are blocked until %s, free models stay available",
		window, b.Until.UTC().Format(time.RFC3339))
}

type exhaustionTracker struct {
	mu     sync.Mutex
	now    func() time.Time
	blocks map[string]*usageBlock
	gen    uint64
	// schedule runs fn at the given time; tests replace it.
	schedule func(at time.Time, fn func())
	// resetCooldown clears CPA's cooldown for the given auth IDs.
	resetCooldown func(authIDs []string)
}

func newExhaustionTracker() *exhaustionTracker {
	return &exhaustionTracker{
		now:           time.Now,
		blocks:        make(map[string]*usageBlock),
		schedule:      scheduleAt,
		resetCooldown: resetAuthCooldowns,
	}
}

var exhaustion = newExhaustionTracker()

// active returns the block on credID, remembering authID as blocked.
func (t *exhaustionTracker) active(credID, authID string) (usageBlock, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	block, ok := t.blocks[credID]
	if !ok {
		return usageBlock{}, false
	}
	if !t.now().Before(block.Until) {
		return usageBlock{}, false
	}
	if authID != "" {
		block.authIDs[authID] = struct{}{}
	}
	return *block, true
}

// block blocks credID until until and arranges for CPA's cooldowns on the
// blocked auths to be cleared then: CPA backs a 429 off for up to 30
// minutes on its own, which would outlast a window that just reset.
func (t *exhaustionTracker) block(credID, authID string, until time.Time, windows []string) usageBlock {
	t.mu.Lock()
	t.gen++
	gen := t.gen
	block := &usageBlock{Until: until, Windows: windows, authIDs: make(map[string]struct{}), gen: gen}
	if previous, ok := t.blocks[credID]; ok {
		for id := range previous.authIDs {
			block.authIDs[id] = struct{}{}
		}
	}
	if authID != "" {
		block.authIDs[authID] = struct{}{}
	}
	t.blocks[credID] = block
	snapshot := *block
	t.mu.Unlock()
	t.schedule(until, func() { t.expire(credID, gen) })
	return snapshot
}

func (t *exhaustionTracker) expire(credID string, gen uint64) {
	t.mu.Lock()
	block, ok := t.blocks[credID]
	if !ok || block.gen != gen {
		t.mu.Unlock()
		return
	}
	delete(t.blocks, credID)
	authIDs := make([]string, 0, len(block.authIDs))
	for id := range block.authIDs {
		authIDs = append(authIDs, id)
	}
	t.mu.Unlock()
	sort.Strings(authIDs)
	if len(authIDs) > 0 {
		t.resetCooldown(authIDs)
	}
}

// blockFromUsage turns a /v1/usage report taken after a GoUsageLimitError
// into the block's end and the exhausted windows' names.
func blockFromUsage(report usageReport, errUsage error, now time.Time) (time.Time, []string) {
	if errUsage == nil {
		if until, windows, ok := report.exhausted(); ok && until.After(now) {
			names := make([]string, 0, len(windows))
			for _, window := range windows {
				names = append(names, windowLabel(window.Name))
			}
			return until, names
		}
	}
	return now.Add(unknownResetBlock), nil
}

func windowLabel(name string) string {
	if names, ok := quotaBucketNames[name]; ok {
		return strings.ToLower(names[1])
	}
	return name
}

// scheduleAt runs fn at the given time under the plugin lifetime, so a
// pending reset never outlives the library.
func scheduleAt(at time.Time, fn func()) {
	startBackground(func(ctx context.Context) {
		timer := time.NewTimer(time.Until(at))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			fn()
		}
	})
}

// resetAuthCooldowns clears CPA's cooldowns for auth IDs. The executor sees
// auth IDs while the host addresses auths by auth_index, so it is looked up.
func resetAuthCooldowns(authIDs []string) {
	h := newHost("")
	files, errList := hostAuthList()
	if errList != nil {
		h.Log("warn", "OpenCode Go usage block ended but the auth list is unavailable; CPA cooldowns stay", map[string]any{"error": errList.Error()})
		return
	}
	wanted := make(map[string]struct{}, len(authIDs))
	for _, id := range authIDs {
		wanted[id] = struct{}{}
	}
	for _, file := range files {
		if _, ok := wanted[file.ID]; !ok || file.AuthIndex == "" {
			continue
		}
		if _, errReset := hostResetCooldown(file.AuthIndex); errReset != nil {
			h.Log("warn", "OpenCode Go usage block ended but clearing the CPA cooldown failed", map[string]any{"auth_id": file.ID, "error": errReset.Error()})
			continue
		}
		h.Log("info", "OpenCode Go usage block ended; CPA cooldown cleared", map[string]any{"auth_id": file.ID})
	}
}
