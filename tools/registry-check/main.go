// Command registry-check validates the store registry and every plugin's
// metadata with the plugin store parser of the pinned CLIProxyAPI host. The
// host rejects a whole store source when one entry is invalid, so metadata it
// would refuse must fail here, before a release publishes it.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginstore"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: registry-check <registry.json> <plugins-dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "registry-check:", err)
		os.Exit(1)
	}
}

func run(registryPath, pluginsDir string) error {
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return err
	}
	if _, err := pluginstore.ParseRegistry(data); err != nil {
		return fmt.Errorf("%s: %w", registryPath, err)
	}
	paths, err := filepath.Glob(filepath.Join(pluginsDir, "*", "plugin.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		if err := checkMetadata(path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// checkMetadata validates plugin.json as the registry entry the publisher
// derives from it. The version and artifact are placeholders: the publisher
// takes both from the published release asset.
func checkMetadata(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		return err
	}
	entry["version"] = "0.0.0"
	entry["install"] = map[string]any{
		"type": pluginstore.InstallTypeDirect,
		"artifacts": []map[string]any{{
			"goos":   "linux",
			"goarch": "amd64",
			"url":    "https://example.invalid/artifact.zip",
			"sha256": "0000000000000000000000000000000000000000000000000000000000000000",
			"size":   0,
		}},
	}
	registry, err := json.Marshal(map[string]any{
		"schema_version": pluginstore.SchemaVersionV2,
		"plugins":        []any{entry},
	})
	if err != nil {
		return err
	}
	_, err = pluginstore.ParseRegistry(registry)
	return err
}
