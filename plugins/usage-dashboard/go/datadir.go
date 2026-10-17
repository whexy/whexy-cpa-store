package main

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultDataDir places the usage files beside the plugins directory. Store
// installs need that directory writable and persistent, while CLIProxyAPI's
// working directory often is neither (in the official image it is inside the
// container). The host passes a plugin only its own config and, since
// v8.0.31, loads a copy of the library from the temporary directory, so the
// plugins directory is read from the host's config file, as the host does to
// bootstrap its plugins.
func defaultDataDir() string {
	wd, errWd := os.Getwd()
	if errWd != nil {
		wd = string(filepath.Separator)
	}
	return dataDirForPlugins(hostPluginsDir(os.Args[1:], wd, os.ReadFile))
}

func dataDirForPlugins(plugins string) string {
	return filepath.Join(filepath.Dir(plugins), "data", pluginID)
}

// hostPluginsDir resolves plugins.dir the way CLIProxyAPI's
// pluginBootstrapConfigPath and config.ResolvePluginsDir do: the config file
// is the -config or --config argument, else config.yaml in the working
// directory; plugins.dir defaults to plugins, and a relative one is relative
// to the working directory.
func hostPluginsDir(args []string, wd string, readFile func(string) ([]byte, error)) string {
	path := hostConfigPath(args)
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

func hostConfigPath(args []string) string {
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
