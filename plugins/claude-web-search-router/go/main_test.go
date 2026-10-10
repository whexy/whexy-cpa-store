package main

import "testing"

func TestPluginRegistrationReportsStoreVersion(t *testing.T) {
	if got := pluginRegistration().Metadata.Version; got != pluginVersion {
		t.Fatalf("version = %q, want %q", got, pluginVersion)
	}
}
