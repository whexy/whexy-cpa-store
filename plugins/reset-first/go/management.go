package main

import (
	_ "embed"
	"net/http"
	"time"

	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

// managementPage holds no data: it reads the decisions route.
//
//go:embed management.html
var managementPage []byte

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

// decisionsPath is authenticated: the cards name accounts by email.
var decisionsPath = manage.APIPath(pluginID, "decisions")

var router = manage.NewRouter(manage.Config{
	PluginID: pluginID,
	Name:     displayName,
	Page:     &manage.Page{Menu: displayName, Description: "See which account each model was routed to and why.", HTML: managementPage},
	Logo:     logoSVG,
	Routes: []manage.Route{
		{Method: http.MethodGet, Path: decisionsPath, Description: "Read the latest routing decision of every model routed in the past 24 hours.", Handle: handleDecisions},
	},
})

func handleDecisions(manage.Request) (int, any, error) {
	return http.StatusOK, current.decisionsView(time.Now()), nil
}
