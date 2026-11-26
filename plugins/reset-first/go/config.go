package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/config"
	"github.com/whexy/whexy-cpa-store/pluginkit/datadir"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
)

const (
	defaultPollInterval = 10 * time.Minute
	minPollInterval     = time.Minute

	// tick paces the background loop: refreshing CPA's credentials, polling
	// the accounts that are due and saving the state.
	tick = 30 * time.Second
	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

type settings struct {
	// PollInterval is how often an idle account's usage endpoint is read;
	// zero reads quota from responses only.
	PollInterval time.Duration
}

var currentConfig atomic.Pointer[settings]

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return settings{PollInterval: defaultPollInterval}
}

// CPA HTML-escapes these descriptions and Management Center shows the escapes
// literally, so they avoid angle brackets, ampersands and quotes.
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "poll_interval", Type: pluginapi.ConfigFieldTypeString, Description: "How often to read the quota of a Codex or Claude account that serves no requests, as a duration such as 10m. Default 10m, at least 1m; off reads quota from responses only."},
	}
}

// parseSettings reads plugins.configs.reset-first from a lifecycle request.
func parseSettings(request []byte) (settings, error) {
	var raw struct {
		PollInterval string `yaml:"poll_interval"`
	}
	if errDecode := config.Decode(request, &raw); errDecode != nil {
		return settings{}, errDecode
	}
	cfg := settings{PollInterval: defaultPollInterval}
	switch value := strings.ToLower(strings.TrimSpace(raw.PollInterval)); value {
	case "":
	case "off", "0":
		cfg.PollInterval = 0
	default:
		interval, errParse := time.ParseDuration(value)
		if errParse != nil || interval < minPollInterval {
			return settings{}, fmt.Errorf("poll_interval must be off or a duration of at least %s, got %q", minPollInterval, raw.PollInterval)
		}
		cfg.PollInterval = interval
	}
	return cfg, nil
}

// configure handles plugin.register and plugin.reconfigure, and register again
// after a failed hot upgrade rolled back to this library.
func configure(request []byte) error {
	cfg, errParse := parseSettings(request)
	if errParse != nil {
		return errParse
	}
	currentConfig.Store(&cfg)
	current.load(datadir.Default(pluginID))
	startLoop()
	return nil
}

var (
	loopMu sync.Mutex
	// looping is set while the background loop runs; the host registers and
	// reconfigures many times, and each must not start another loop.
	looping bool
)

func startLoop() {
	loopMu.Lock()
	defer loopMu.Unlock()
	if !looping {
		looping = true
		background.Go(loop)
	}
}

// quiesce saves everything and stops writing, on shutdown and to hand the
// state file to the library replacing this one in a hot upgrade. This library
// may still answer an in-flight call afterwards, from memory.
func quiesce() {
	loopMu.Lock()
	looping = false
	background.Stop(shutdownGrace)
	loopMu.Unlock()
	current.persist(time.Now())
}

// loop refreshes every tick until ctx ends.
func loop(ctx context.Context) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh is one round of background work.
func refresh(ctx context.Context) {
	if entries, errList := host.AuthList(); errList == nil {
		current.syncAccounts(entries, time.Now())
	}
	if interval := currentSettings().PollInterval; interval > 0 {
		for _, target := range current.pollDue(time.Now()) {
			if ctx.Err() != nil {
				return
			}
			result := poll(ctx, target, time.Now())
			if ctx.Err() != nil {
				return
			}
			current.applyPoll(target, result, time.Now(), interval)
		}
	}
	current.persist(time.Now())
}
