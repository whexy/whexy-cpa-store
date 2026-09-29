package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const (
	codeMigrationRequired = "SESSION_MIGRATION_REQUIRED"
	codeTargetUnavailable = "SESSION_MIGRATION_TARGET_UNAVAILABLE"
	codeAckInvalid        = "SESSION_MIGRATION_ACK_INVALID"

	sweepInterval = time.Minute
)

// sessionKey mirrors the granularity of CLIProxyAPI's own session affinity
// (session and model), scoped by caller so one API key cannot observe or
// acknowledge another key's migrations.
type sessionKey struct {
	callerScope string
	sessionID   string
	model       string
}

// selection is the credential CLIProxyAPI picked for the current attempt.
type selection struct {
	authID    string
	authIndex string
}

type binding struct {
	authID    string
	authIndex string
	// epoch increments on every committed migration.
	epoch uint64
	// committedID is the migration that produced the current epoch. Replaying
	// its acknowledgement is a no-op so a lost commit response can be retried.
	committedID string
	pending     *migration
	lastSeen    time.Time
}

// migration is pinned to the target CLIProxyAPI selected when it was created;
// it can only ever commit to that target.
type migration struct {
	id          string
	toAuthID    string
	toAuthIndex string
}

type verdict struct {
	allow bool
	code  string
	// binding is nil when the session has no binding yet.
	binding *bindingView
}

type bindingView struct {
	AuthIndex string         `json:"auth_index,omitempty"`
	Epoch     uint64         `json:"epoch"`
	Pending   *migrationView `json:"pending_migration,omitempty"`
}

type migrationView struct {
	ID          string `json:"id"`
	ToAuthIndex string `json:"to_auth_index,omitempty"`
}

// guard serializes every decision under one mutex, so a pending migration's id
// is the compare-and-swap token for moving a binding to its target.
type guard struct {
	mu        sync.Mutex
	bindings  map[sessionKey]*binding
	ttl       time.Duration
	lastSweep time.Time
	now       func() time.Time
	newID     func() string
}

func newGuard(ttl time.Duration) *guard {
	return &guard{
		bindings: make(map[sessionKey]*binding),
		ttl:      ttl,
		now:      time.Now,
		newID:    newMigrationID,
	}
}

func (g *guard) setTTL(ttl time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ttl = ttl
}

// decide evaluates one upstream attempt after CLIProxyAPI selected sel for the
// session. ack is the migration id the client acknowledged, or empty.
func (g *guard) decide(key sessionKey, sel selection, ack string) verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweep(now)

	b := g.bindings[key]
	if b != nil && now.Sub(b.lastSeen) > g.ttl {
		delete(g.bindings, key)
		b = nil
	}
	if b == nil {
		if ack != "" {
			return verdict{code: codeAckInvalid}
		}
		g.bindings[key] = &binding{authID: sel.authID, authIndex: sel.authIndex, epoch: 1, lastSeen: now}
		return verdict{allow: true}
	}
	b.lastSeen = now

	if ack != "" && ack != b.committedID {
		switch {
		case b.pending == nil || ack != b.pending.id:
			return b.reject(codeAckInvalid)
		case sel.authID != b.pending.toAuthID:
			// The pinned target is no longer what CLIProxyAPI selects, so the
			// migration dies instead of committing the session to a different
			// credential. A later plain request opens a new migration.
			b.pending = nil
			return b.reject(codeTargetUnavailable)
		default:
			b.authID, b.authIndex = b.pending.toAuthID, b.pending.toAuthIndex
			b.epoch++
			b.committedID = b.pending.id
			b.pending = nil
			return verdict{allow: true}
		}
	}

	if b.pending != nil {
		if sel.authID == b.pending.toAuthID {
			return b.reject(codeMigrationRequired)
		}
		b.pending = nil
	}
	if sel.authID == b.authID {
		return verdict{allow: true}
	}
	b.pending = &migration{id: g.newID(), toAuthID: sel.authID, toAuthIndex: sel.authIndex}
	return b.reject(codeMigrationRequired)
}

func (b *binding) reject(code string) verdict {
	view := bindingView{AuthIndex: b.authIndex, Epoch: b.epoch}
	if b.pending != nil {
		view.Pending = &migrationView{ID: b.pending.id, ToAuthIndex: b.pending.toAuthIndex}
	}
	return verdict{code: code, binding: &view}
}

func (g *guard) sweep(now time.Time) {
	if now.Sub(g.lastSweep) < sweepInterval {
		return
	}
	g.lastSweep = now
	for key, b := range g.bindings {
		if now.Sub(b.lastSeen) > g.ttl {
			delete(g.bindings, key)
		}
	}
}

func newMigrationID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "smig_" + hex.EncodeToString(raw[:])
}
