package plugin

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestDispatchRecoversPanics(t *testing.T) {
	Serve(Plugin{ID: "demo", Handle: func(method string, _ []byte) ([]byte, error) {
		if method == "boom" {
			panic("bad state")
		}
		return []byte(method), nil
	}})
	if raw, errDispatch := Dispatch("echo", nil); errDispatch != nil || string(raw) != "echo" {
		t.Errorf("Dispatch = %s, %v", raw, errDispatch)
	}
	if _, errDispatch := Dispatch("boom", nil); errDispatch == nil || !strings.Contains(errDispatch.Error(), "boom panicked: bad state") {
		t.Errorf("panic = %v", errDispatch)
	}
}

func TestOversized(t *testing.T) {
	Serve(Plugin{ID: "demo", Handle: func(string, []byte) ([]byte, error) { return nil, nil }})
	if _, errOversized := dispatchOversized("m", 5); errOversized == nil {
		t.Error("oversized request accepted without a handler")
	}
	Serve(Plugin{ID: "demo", Handle: func(string, []byte) ([]byte, error) { return nil, nil }, Oversized: func(method string, n uint64) ([]byte, error) {
		return nil, errors.New("custom")
	}})
	if _, errOversized := dispatchOversized("m", 5); errOversized == nil || errOversized.Error() != "custom" {
		t.Errorf("Oversized = %v", errOversized)
	}
	Serve(Plugin{ID: "demo", Handle: func(string, []byte) ([]byte, error) { return nil, nil }, Oversized: func(string, uint64) ([]byte, error) {
		panic("bad state")
	}})
	if _, errOversized := dispatchOversized("m", 5); errOversized == nil || !strings.Contains(errOversized.Error(), "m panicked: bad state") {
		t.Errorf("panic = %v", errOversized)
	}
}

func TestShutdownRunsOnceAndContainsPanics(t *testing.T) {
	shutdownOnce = sync.Once{}
	t.Cleanup(func() { shutdownOnce = sync.Once{} })
	runs := 0
	Serve(Plugin{ID: "demo", Handle: func(string, []byte) ([]byte, error) { return nil, nil }, Shutdown: func() {
		runs++
		panic("bad state")
	}})
	shutdown()
	shutdown()
	if runs != 1 {
		t.Errorf("Shutdown ran %d times", runs)
	}
}
