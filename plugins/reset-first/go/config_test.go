package main

import (
	"testing"
	"time"
)

func TestParseSettings(t *testing.T) {
	cases := []struct {
		yaml string
		want time.Duration
	}{
		{"", defaultPollInterval},
		{"enabled: true\npriority: 0\n", defaultPollInterval},
		{"poll_interval: 3m\n", 3 * time.Minute},
		{"poll_interval: off\n", 0},
		{"poll_interval: \"0\"\n", 0},
	}
	for _, c := range cases {
		cfg, errParse := parseSettings([]byte(c.yaml))
		if errParse != nil || cfg.PollInterval != c.want {
			t.Errorf("parseSettings(%q) = %v, %v; want %v", c.yaml, cfg.PollInterval, errParse, c.want)
		}
	}
	for _, bad := range []string{"poll_interval: 30s\n", "poll_interval: often\n", "poll_interval: [\n"} {
		if _, errParse := parseSettings([]byte(bad)); errParse == nil {
			t.Errorf("parseSettings(%q) accepted", bad)
		}
	}
}
