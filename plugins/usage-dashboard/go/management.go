package main

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

// dashboardPage holds no data: resource routes are not authenticated, so it
// reads everything through the routes below.
//
//go:embed dashboard.html
var dashboardPage []byte

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

var (
	summaryPath = manage.APIPath(pluginID, "summary")
	pricesPath  = manage.APIPath(pluginID, "prices")
	quotaPath   = manage.APIPath(pluginID, "quota")
	poolsPath   = manage.APIPath(pluginID, "quota/pools")
)

var router = manage.NewRouter(manage.Config{
	PluginID: pluginID,
	Name:     displayName,
	Page:     &manage.Page{Menu: menuLabel, Description: "Token usage and estimated cost by model family or account, and the quota left in each provider's accounts.", HTML: dashboardPage},
	Logo:     logoSVG,
	Routes: []manage.Route{
		{Method: http.MethodGet, Path: summaryPath, Description: "Usage totals, per-group series and per-model rows: ?range=24h|7d|30d|90d&group=family|account&tz=<IANA zone>.", Handle: handleSummary},
		{Method: http.MethodGet, Path: pricesPath, Description: "Every model in retained usage with its custom and models.dev price.", Handle: handlePrices},
		{Method: http.MethodGet, Path: quotaPath, Description: "Accounts of every provider whose quota is recorded, with their readings: ?from=<RFC 3339 time>.", Handle: handleQuota},
		{Method: http.MethodGet, Path: poolsPath, Description: "Each provider's quota pool, recorded and projected: ?burn=24h&usage=1&from=<RFC 3339>&until=<RFC 3339>&step=10m&series=accounts|pool|none.", Handle: handlePools},
		{Method: http.MethodPut, Path: pricesPath, Description: `Replace the custom prices: {"models":{"<model>":{"input":..,"output":..,"cache_read":..,"cache_write":..}}}, USD per million tokens.`, Handle: handleSavePrices},
	},
})

func handleSummary(req manage.Request) (int, any, error) {
	report, errReport := summary(reportQuery{
		Range:    req.Query.Get("range"),
		GroupBy:  req.Query.Get("group"),
		TimeZone: req.Query.Get("tz"),
	})
	if errReport != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "%v", errReport)
	}
	return http.StatusOK, report, nil
}

func handleQuota(req manage.Request) (int, any, error) {
	report, errReport := quotaReport(req.Query.Get("from"))
	if errReport != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "%v", errReport)
	}
	return http.StatusOK, report, nil
}

func handlePools(req manage.Request) (int, any, error) {
	view, errPools := quotaPools(poolsQuery{
		Burn:   req.Query.Get("burn"),
		Usage:  req.Query.Get("usage"),
		From:   req.Query.Get("from"),
		Until:  req.Query.Get("until"),
		Step:   req.Query.Get("step"),
		Series: req.Query.Get("series"),
	})
	if errPools != nil {
		return 0, nil, errPools
	}
	return http.StatusOK, view, nil
}

func handlePrices(manage.Request) (int, any, error) {
	list, errList := priceList()
	if errList != nil {
		return 0, nil, manage.Errorf(http.StatusServiceUnavailable, "%v", errList)
	}
	return http.StatusOK, list, nil
}

func handleSavePrices(req manage.Request) (int, any, error) {
	var body struct {
		Models map[string]customPriceInput `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil || body.Models == nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, `body must be {"models": {"<model>": {"input": .., "output": ..}}}`)
	}
	models, errValidate := validateCustomPrices(body.Models)
	if errValidate != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "%v", errValidate)
	}
	if errSave := customPrices.save(models); errSave != nil {
		return 0, nil, errSave
	}
	return handlePrices(req)
}
