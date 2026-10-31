package main

import "testing"

func TestDashboardAppliesOnlyToNamedAccount(t *testing.T) {
	cfg := defaultSettings()
	cfg.DashboardCookie = "session=abc"
	cfg.DashboardAccount = "whexy"
	if !cfg.dashboardApplies(credential{Username: "whexy"}) {
		t.Fatal("the named account must get its dashboard usage")
	}
	if cfg.dashboardApplies(credential{Username: "someone-else"}) {
		t.Fatal("another account must not see the cookie owner's usage")
	}
	cfg.DashboardAccount = ""
	if cfg.dashboardApplies(credential{Username: "whexy"}) {
		t.Fatal("an unpinned cookie must not be attributed to an account")
	}
}
