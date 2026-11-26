// Package datadir locates the directory where a plugin keeps its files.
package datadir

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default returns <plugins dir>/../data/<pluginID>. A store install needs
// that directory writable and persistent, while CLIProxyAPI's working
// directory often is neither (in the official image it is inside the
// container). The host passes a plugin only its own config and, since
// v8.0.31, loads a copy of the library from the temporary directory, so the
// plugins directory is read from the host's config file, as the host does to
// bootstrap its plugins.
func Default(pluginID string) string {
	wd, errWd := os.Getwd()
	if errWd != nil {
		wd = string(filepath.Separator)
	}
	return ForPluginsDir(PluginsDir(os.Args[1:], wd, os.ReadFile), pluginID)
}

// ForPluginsDir returns the data directory of pluginID beside plugins.
func ForPluginsDir(plugins, pluginID string) string {
	return filepath.Join(filepath.Dir(plugins), "data", pluginID)
}

// PluginsDir resolves plugins.dir the way CLIProxyAPI's
// pluginBootstrapConfigPath and config.ResolvePluginsDir do: the config file
// is the -config or --config argument, else config.yaml in the working
// directory; plugins.dir defaults to plugins, and a relative one is relative
// to the working directory.
func PluginsDir(args []string, wd string, readFile func(string) ([]byte, error)) string {
	path := configPath(args)
	if path == "" {
		path = "config.yaml"
	}
	dir := ""
	if raw, errRead := readFile(absUnder(wd, path)); errRead == nil {
		var cfg struct {
			Plugins struct {
				Dir string `yaml:"dir"`
			} `yaml:"plugins"`
		}
		if yaml.Unmarshal(raw, &cfg) == nil {
			dir = strings.TrimSpace(cfg.Plugins.Dir)
		}
	}
	if dir == "" {
		dir = "plugins"
	}
	if rest, ok := strings.CutPrefix(dir, "~"); ok {
		if home, errHome := os.UserHomeDir(); errHome == nil {
			dir = filepath.Join(home, strings.TrimLeft(rest, `/\`))
		}
	}
	return absUnder(wd, dir)
}

func configPath(args []string) string {
	for i, arg := range args {
		switch {
		case arg == "--":
			return ""
		case arg == "-config" || arg == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return ""
}

func absUnder(wd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(wd, path)
}
