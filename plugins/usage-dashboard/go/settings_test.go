package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// lifecycle wraps configYAML in a plugin.register request.
func lifecycle(configYAML string) []byte {
	raw, _ := json.Marshal(rpc.LifecycleRequest{ConfigYAML: []byte(configYAML)})
	return raw
}

func TestParseSettings(t *testing.T) {
	cfg, errParse := parseSettings(lifecycle("enabled: true\nretention_days: 90\npricing: false\nmodel_families:\n  my-*: Mine\n"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cfg.RetentionDays != 90 || cfg.PricingURL != "" || len(cfg.FamilyRules) != 1 || cfg.DataDir != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
	cfg, errParse = parseSettings(lifecycle("data_dir: rel/dir\npricing_url: null\n"))
	if errParse != nil || !filepath.IsAbs(cfg.DataDir) || cfg.PricingURL != defaultPricingURL || cfg.RetentionDays != defaultRetentionDays {
		t.Fatalf("cfg = %+v err=%v", cfg, errParse)
	}
	for _, bad := range []string{"retention_days: -3", "retention_days: 1000", "pricing_url: ftp://x", "model_families: {'[': X}"} {
		if _, errParse := parseSettings(lifecycle(bad)); errParse == nil {
			t.Errorf("parseSettings(%q) must fail", bad)
		}
	}
}
