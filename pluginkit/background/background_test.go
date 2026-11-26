package background

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestStopCancelsAndWaits(t *testing.T) {
	var g Group
	stopped := make(chan struct{})
	g.Go(func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	})
	g.Go(func(context.Context) { panic("contained") })
	g.Stop(time.Second)
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before the task did")
	}
}

func TestGroupRestartsAfterStop(t *testing.T) {
	var g Group
	first := g.Context()
	g.Stop(time.Second)
	if first.Err() == nil {
		t.Fatal("Stop left the lifetime running")
	}
	ran := make(chan error, 1)
	g.Go(func(ctx context.Context) { ran <- ctx.Err() })
	if errCtx := <-ran; errCtx != nil {
		t.Errorf("task after Stop started with a done context: %v", errCtx)
	}
	if g.Context().Err() != nil {
		t.Error("new lifetime is already done")
	}
	g.Stop(time.Second)
}

func TestStopWaitsOnlyForItsLifetime(t *testing.T) {
	var g Group
	release := make(chan struct{})
	g.Go(func(ctx context.Context) {
		<-ctx.Done()
		<-release
	})
	first := g.Context()
	stopped := make(chan struct{})
	go func() {
		g.Stop(time.Minute)
		close(stopped)
	}()
	// Stop detaches the lifetime before cancelling it, so the next Go
	// starts a new one; that task never returns until the test ends.
	<-first.Done()
	block := make(chan struct{})
	defer close(block)
	g.Go(func(context.Context) { <-block })
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited on a task of the next lifetime")
	}
}

func TestConcurrentGoAndStop(t *testing.T) {
	var g Group
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				g.Go(func(context.Context) {})
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				g.Stop(time.Second)
			}
		}()
	}
	wg.Wait()
	g.Stop(time.Second)
}

func TestCloseIsFinal(t *testing.T) {
	var g Group
	first := g.Context()
	g.Close(time.Second)
	if first.Err() == nil {
		t.Fatal("Close left the lifetime running")
	}
	if g.Context().Err() == nil {
		t.Error("Context after Close is live")
	}
	ran := make(chan error, 1)
	g.Go(func(ctx context.Context) { ran <- ctx.Err() })
	if errCtx := <-ran; errCtx == nil {
		t.Error("task after Close started with a live context")
	}
	g.Stop(time.Second)
	if g.Context().Err() == nil {
		t.Error("Stop after Close revived the group")
	}
}
