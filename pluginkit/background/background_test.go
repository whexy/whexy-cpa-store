package background

import (
	"context"
	"testing"
	"time"
)

func TestStopCancelsAndWaits(t *testing.T) {
	stopped := make(chan struct{})
	Go(func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	})
	Go(func(context.Context) { panic("contained") })
	Stop(time.Second)
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before the task did")
	}
}
