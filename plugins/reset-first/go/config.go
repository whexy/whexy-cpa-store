package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	poller.start()
	return nil
}

// quiesce saves everything and stops writing, on shutdown and to hand the
// state file to the library replacing this one in a hot upgrade. This library
// may still answer an in-flight call afterwards, from memory.
func quiesce() {
	poller.stop(shutdownGrace)
	current.persist(time.Now())
}

// loop is the background work. Unlike pluginkit/background it starts again
// after a stop: a failed hot upgrade rolls back to this library with
// plugin.register after quiesce.
type loop struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var poller loop

func (l *loop) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel, l.done = cancel, make(chan struct{})
	go l.run(ctx, l.done)
}

func (l *loop) stop(grace time.Duration) {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.cancel, l.done = nil, nil
	l.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

func (l *loop) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer func() {
		if recovered := recover(); recovered != nil {
			host.Log("error", fmt.Sprintf("reset-first: background loop panicked: %v", recovered), nil)
		}
	}()
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
