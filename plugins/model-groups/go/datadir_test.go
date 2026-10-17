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
	if got := dataDirForPlugins("/data/plugins"); got != "/data/data/model-groups" {
		t.Errorf("dataDirForPlugins = %q", got)
	}
}
