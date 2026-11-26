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

var (
	ctx, cancel = context.WithCancel(context.Background())
	work        sync.WaitGroup
)

// Go runs fn on its own goroutine; fn must return promptly once its context
// is done. A panic in fn is logged instead of aborting the host.
func Go(fn func(ctx context.Context)) {
	work.Add(1)
	go func() {
		defer work.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				host.Log("error", fmt.Sprintf("background task panicked: %v", recovered), nil)
			}
		}()
		fn(ctx)
	}()
}

// Stop cancels every task and waits up to grace for them to return. Tasks
// started after Stop see a done context.
func Stop(grace time.Duration) {
	cancel()
	done := make(chan struct{})
	go func() {
		work.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}
