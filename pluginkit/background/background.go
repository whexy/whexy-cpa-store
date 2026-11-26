// Package background runs work that outlives a plugin call, such as stream
// pumps and pollers, under a context that ends before the host unloads the
// library underneath it.
package background

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/whexy/whexy-cpa-store/pluginkit/host"
)

// Group runs tasks under one lifetime at a time. Stop ends the current
// lifetime and the next Go starts a new one, because a host that quiesces a
// plugin may register it again when a hot upgrade rolls back. Close ends the
// group for good, for the shutdown hook the host calls before unloading the
// library. The zero Group is ready to use.
type Group struct {
	mu     sync.Mutex
	life   *lifetime
	closed bool
}

// lifetime counts only its own tasks, so Stop never waits on work started
// after it, and a WaitGroup is never reused while a Wait may be pending.
type lifetime struct {
	ctx    context.Context
	cancel context.CancelFunc
	work   sync.WaitGroup
}

// Context returns the current lifetime's context, starting one if needed.
// Work that runs within a call but must end at shutdown derives from it.
// After Close it returns a done context.
func (g *Group) Context() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.currentLocked().ctx
}

func (g *Group) currentLocked() *lifetime {
	if g.life == nil {
		ctx, cancel := context.WithCancel(context.Background())
		if g.closed {
			cancel()
		}
		g.life = &lifetime{ctx: ctx, cancel: cancel}
	}
	return g.life
}

// Go runs fn on its own goroutine; fn must return promptly once its context
// is done. A panic in fn is logged instead of aborting the host.
func (g *Group) Go(fn func(ctx context.Context)) {
	g.mu.Lock()
	life := g.currentLocked()
	life.work.Add(1)
	g.mu.Unlock()
	go func() {
		defer life.work.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				host.Log("error", fmt.Sprintf("background task panicked: %v", recovered), nil)
			}
		}()
		fn(life.ctx)
	}()
}

// Stop cancels the current lifetime and waits up to grace for its tasks to
// return.
func (g *Group) Stop(grace time.Duration) { g.stop(grace, false) }

// Close stops the group like Stop and keeps it stopped: later work gets a
// done context.
func (g *Group) Close(grace time.Duration) { g.stop(grace, true) }

func (g *Group) stop(grace time.Duration, final bool) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	life := g.life
	g.life = nil
	g.closed = final
	g.mu.Unlock()
	if life == nil {
		return
	}
	life.cancel()
	done := make(chan struct{})
	go func() {
		life.work.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

var plugin Group

// Context returns the plugin-wide group's context.
func Context() context.Context { return plugin.Context() }

// Go runs fn in the plugin-wide group.
func Go(fn func(ctx context.Context)) { plugin.Go(fn) }

// Stop stops the plugin-wide group.
func Stop(grace time.Duration) { plugin.Stop(grace) }

// Close stops the plugin-wide group for good.
func Close(grace time.Duration) { plugin.Close(grace) }
