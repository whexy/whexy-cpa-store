package main

import (
	_ "embed"

	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

// router serves only the logo, which Management Center shows on the Plugins
// page.
var router = manage.NewRouter(manage.Config{PluginID: pluginID, Name: displayName, Logo: logoSVG})
