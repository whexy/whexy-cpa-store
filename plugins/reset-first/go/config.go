package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
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

func parseSettings(configYAML []byte) (settings, error) {
	var raw struct {
		PollInterval string `yaml:"poll_interval"`
	}
	if len(configYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(configYAML, &raw); errUnmarshal != nil {
			return settings{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
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
func configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	cfg, errParse := parseSettings(request.ConfigYAML)
	if errParse != nil {
		return errParse
	}
	currentConfig.Store(&cfg)
	current.load(defaultDataDir())
	background.start()
	return nil
}

// quiesce hands the state file to the library replacing this one: it saves
// everything and stops writing. This library may still answer an in-flight
// call afterwards, from memory.
func quiesce() {
	background.stop(shutdownGrace)
	current.persist(time.Now())
}

func shutdown() {
	background.stop(shutdownGrace)
	current.persist(time.Now())
}

// loop is the background work; start and stop may each come more than once.
type loop struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var background loop

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
			hostLog("error", fmt.Sprintf("reset-first: background loop panicked: %v", recovered), nil)
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
	if entries, errList := hostAuthList(); errList == nil {
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
