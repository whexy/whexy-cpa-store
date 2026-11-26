package main

import (
	_ "embed"
	"net/http"
	"time"

	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

// managementPage holds no data: it reads rules through the host's config API
// and what the plugin applied through the status route.
//
//go:embed management.html
var managementPage []byte

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

var statusPath = manage.APIPath(pluginID, "status")

var router = manage.NewRouter(manage.Config{
	PluginID: pluginID,
	Name:     displayName,
	Page:     &manage.Page{Menu: displayName, Description: "Append a system prompt to requests for the models you choose.", HTML: managementPage},
	Logo:     logoSVG,
	Routes: []manage.Route{
		{Method: http.MethodGet, Path: statusPath, Description: "Read the applied rules, the rules that do not apply and why, and the config generation.", Handle: handleStatus},
	},
})

type statusView struct {
	Generation uint64        `json:"generation"`
	AppliedAt  string        `json:"applied_at,omitempty"`
	Error      string        `json:"error,omitempty"`
	Rules      int           `json:"rules"`
	Problems   []ruleProblem `json:"problems"`
}

// handleStatus reports the configuration as the plugin applied it, so the
// page can tell a saved rule from one that is in effect.
func handleStatus(manage.Request) (int, any, error) {
	rules := currentRules()
	view := statusView{
		Generation: rules.generation,
		Error:      rules.err,
		Rules:      len(rules.rules),
		Problems:   rules.problems,
	}
	if view.Problems == nil {
		view.Problems = []ruleProblem{}
	}
	if !rules.appliedAt.IsZero() {
		view.AppliedAt = rules.appliedAt.Format(time.RFC3339)
	}
	return http.StatusOK, view, nil
}
