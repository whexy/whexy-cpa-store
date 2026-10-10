package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validRegistry = `{
  "schema_version": 2,
  "plugins": [{
    "id": "example",
    "name": "Example",
    "description": "An example plugin.",
    "author": "Whexy",
    "version": "0.1.0",
    "install": {"type": "direct", "artifacts": [{
      "goos": "linux", "goarch": "amd64",
      "url": "https://github.com/whexy/whexy-cpa-store/releases/download/example-v0.1.0/example-v0.1.0-linux-amd64.zip",
      "sha256": "3a72f5869876c53d239b6142f5f1434d7637d4e4c96d828ff6f52a6bddc901af",
      "size": 3602130
    }]}
  }]
}`

func writeStore(t *testing.T, registry string, plugins map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	registryPath := filepath.Join(root, "registry.json")
	if err := os.WriteFile(registryPath, []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	for id, metadata := range plugins {
		dir := filepath.Join(root, "plugins", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(metadata), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return registryPath, filepath.Join(root, "plugins")
}

func TestRunAcceptsValidStore(t *testing.T) {
	registryPath, pluginsDir := writeStore(t, validRegistry, map[string]string{
		"example": `{"id": "example", "name": "Example", "description": "An example plugin.", "author": "Whexy", "tags": ["a"]}`,
	})
	if err := run(registryPath, pluginsDir); err != nil {
		t.Fatalf("run() = %v", err)
	}
}

func TestRunRejectsRegistryTheHostRejects(t *testing.T) {
	registry := strings.Replace(validRegistry, `"version": "0.1.0"`, `"version": "v0.1.0"`, 1)
	registryPath, pluginsDir := writeStore(t, registry, nil)
	if err := run(registryPath, pluginsDir); err == nil || !strings.Contains(err.Error(), "registry.json") {
		t.Fatalf("run() = %v, want registry.json error", err)
	}
}

func TestRunNamesEachInvalidPluginMetadata(t *testing.T) {
	registryPath, pluginsDir := writeStore(t, validRegistry, map[string]string{
		"no-author": `{"id": "no-author", "name": "No Author", "description": "Missing author."}`,
		"bad id":    `{"id": "bad id", "name": "Bad", "description": "Invalid id.", "author": "Whexy"}`,
	})
	err := run(registryPath, pluginsDir)
	if err == nil {
		t.Fatal("run() = nil, want metadata errors")
	}
	for _, id := range []string{"no-author", "bad id"} {
		if !strings.Contains(err.Error(), filepath.Join(id, "plugin.json")) {
			t.Errorf("error %q does not name %s", err, id)
		}
	}
}
