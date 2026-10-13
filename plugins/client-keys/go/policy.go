package main

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// policyType is one kind of rule a key can carry under policies.<name>.
// parse validates the settings and returns a rule implementing
// requestChecker, modelLister or both.
type policyType struct {
	name  string
	parse func(settings *yaml.Node) (any, error)
}

// policyTypes lists every policy a key may configure. A key's request
// checks run in this order.
var policyTypes = []policyType{
	{name: "responses-api", parse: parseResponsesAPIPolicy},
	{name: "models", parse: parseModelsPolicy},
}

// requestChecker refuses model requests.
type requestChecker interface {
	check(req modelRequest) (denial, bool)
}

// upstreamChecker refuses model requests by the upstream model CLIProxyAPI
// selected, which aliases, model pools and model.route can make differ from
// the name the client sent. It runs once per credential attempt, after
// requestChecker passed the request.
type upstreamChecker interface {
	checkUpstream(req modelRequest) (denial, bool)
}

// modelLister decides which models the key's model lists show. It is
// called with normalized model names, as is the request path.
type modelLister interface {
	lists(model string) bool
}

// modelRequest is the part of a model request that policies see.
type modelRequest struct {
	// format is the client protocol, the host's SourceFormat.
	format string
	// model is the normalized name the request is judged by.
	model string
	// requested is the normalized name refusals quote, the one the client
	// knows the model by.
	requested string
	// websocket marks a Responses API WebSocket turn.
	websocket bool
}

// denial is a terminated request. A nil body is sent as an empty body.
type denial struct {
	status int
	body   []byte
}

func (d denial) response() pluginapi.RequestInterceptResponse {
	resp := pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: d.status}
	if len(d.body) > 0 {
		resp.ResponseHeaders = http.Header{"Content-Type": {"application/json"}}
		resp.ResponseBody = d.body
	}
	return resp
}
