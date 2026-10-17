package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostPluginsDir(t *testing.T) {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		t.Fatal(errHome)
	}
	files := map[string]string{
		"/data/config.yaml":       "port: 8317\nplugins:\n  enabled: true\n  dir: /data/plugins\n",
		"/srv/cpa/config.yaml":    "plugins:\n  dir: lib/plugins\n",
		"/srv/cpa/conf/cpa.yaml":  "plugins:\n  dir: ' /etc/cpa/plugins '\n",
		"/srv/cpa/home.yaml":      "plugins:\n  dir: ~/cpa-plugins\n",
		"/srv/cpa/no-plugins.yml": "port: 8317\n",
		"/srv/cpa/broken.yaml":    "plugins: [\n",
	}
	readFile := func(path string) ([]byte, error) {
		if content, ok := files[path]; ok {
			return []byte(content), nil
		}
		return nil, os.ErrNotExist
	}
	cases := []struct {
		args []string
		wd   string
		want string
	}{
		{[]string{"--config", "/data/config.yaml"}, "/CLIProxyAPI", "/data/plugins"},
		{[]string{"-config=/data/config.yaml"}, "/CLIProxyAPI", "/data/plugins"},
		{[]string{"-debug", "--config=conf/cpa.yaml"}, "/srv/cpa", "/etc/cpa/plugins"},
		{nil, "/srv/cpa", "/srv/cpa/lib/plugins"},
		{[]string{"-config", "home.yaml"}, "/srv/cpa", filepath.Join(home, "cpa-plugins")},
		{[]string{"-config", "no-plugins.yml"}, "/srv/cpa", "/srv/cpa/plugins"},
		{[]string{"-config", "broken.yaml"}, "/srv/cpa", "/srv/cpa/plugins"},
		{[]string{"-home-jwt", "token"}, "/CLIProxyAPI", "/CLIProxyAPI/plugins"},
		{[]string{"--", "--config", "/data/config.yaml"}, "/CLIProxyAPI", "/CLIProxyAPI/plugins"},
		{[]string{"--config"}, "/srv/cpa", "/srv/cpa/lib/plugins"},
	}
	for _, c := range cases {
		if got := hostPluginsDir(c.args, c.wd, readFile); got != c.want {
			t.Errorf("hostPluginsDir(%q, %q) = %q, want %q", c.args, c.wd, got, c.want)
		}
	}
	if got := dataDirForPlugins("/data/plugins"); got != "/data/data/usage-dashboard" {
		t.Errorf("dataDirForPlugins = %q", got)
	}
}

func TestParseSettings(t *testing.T) {
	cfg, errParse := parseSettings([]byte("enabled: true\nretention_days: 90\npricing: false\nmodel_families:\n  my-*: Mine\n"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cfg.RetentionDays != 90 || cfg.PricingURL != "" || len(cfg.FamilyRules) != 1 || cfg.DataDir != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
	cfg, errParse = parseSettings([]byte("data_dir: rel/dir\npricing_url: null\n"))
	if errParse != nil || !filepath.IsAbs(cfg.DataDir) || cfg.PricingURL != defaultPricingURL || cfg.RetentionDays != defaultRetentionDays {
		t.Fatalf("cfg = %+v err=%v", cfg, errParse)
	}
	for _, bad := range []string{"retention_days: -3", "retention_days: 1000", "pricing_url: ftp://x", "model_families: {'[': X}"} {
		if _, errParse := parseSettings([]byte(bad)); errParse == nil {
			t.Errorf("parseSettings(%q) must fail", bad)
		}
	}
}
