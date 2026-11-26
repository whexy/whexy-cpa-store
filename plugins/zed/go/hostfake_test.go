package main

import (
	"encoding/json"
	"testing"

	"github.com/whexy/whexy-cpa-store/pluginkit/host/hosttest"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// installHost installs a fake host and empties the caches an earlier test
// may have filled. Tests that call it must not run in parallel.
func installHost(t *testing.T) *hosttest.Fake {
	t.Helper()
	fake := hosttest.Install(t)
	modelsByAuth.clear()
	llmTokens.clear()
	return fake
}

// useSettings installs cfg for the test and restores the previous settings.
func useSettings(t *testing.T, cfg settings) {
	t.Helper()
	previous := currentConfig.Load()
	currentConfig.Store(&cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
}

func decodeEnvelope(t *testing.T, raw []byte) rpc.Envelope {
	t.Helper()
	var env rpc.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("envelope %s: %v", raw, errUnmarshal)
	}
	return env
}

func decodeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope failed: %+v", env.Error)
	}
	var out T
	if errUnmarshal := json.Unmarshal(env.Result, &out); errUnmarshal != nil {
		t.Fatalf("result %s: %v", env.Result, errUnmarshal)
	}
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}
