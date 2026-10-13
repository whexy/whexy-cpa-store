package main

import (
	"path/filepath"
	"testing"
)

func TestDataDirForLibrary(t *testing.T) {
	cases := map[string]string{
		"/data/plugins/linux/amd64/usage-dashboard-v0.1.0.so": "/data/data/usage-dashboard",
		"/srv/cpa/plugins/linux/usage-dashboard.so":           "/srv/cpa/data/usage-dashboard",
		"/srv/cpa/plugins/usage-dashboard.so":                 "/srv/cpa/data/usage-dashboard",
		"/opt/custom-libs/usage-dashboard.so":                 "/opt/custom-libs/usage-dashboard-data",
		"/plugins/a/b/c/usage-dashboard.so":                   "/plugins/a/b/c/usage-dashboard-data",
	}
	for library, want := range cases {
		if got := dataDirForLibrary(library); got != want {
			t.Errorf("dataDirForLibrary(%q) = %q, want %q", library, got, want)
		}
	}
	if got := dataDirForLibrary(""); !filepath.IsAbs(got) || filepath.Base(got) != pluginID {
		t.Errorf("fallback = %q", got)
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
