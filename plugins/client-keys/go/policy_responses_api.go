package main

import (
	"errors"
	"net/http"

	"gopkg.in/yaml.v3"
)

// responsesAPIPolicy is policies.responses-api: {enabled: bool}.
type responsesAPIPolicy struct {
	enabled bool
}

// responsesAPIDisabled is an empty 404: clients that probe /v1/responses
// take it as a missing route and fall back to Chat Completions.
var responsesAPIDisabled = denial{status: http.StatusNotFound}

func parseResponsesAPIPolicy(settings *yaml.Node) (any, error) {
	values, errFields := fields(settings, "enabled")
	if errFields != nil {
		return nil, errFields
	}
	enabled := resolved(values["enabled"])
	if enabled == nil {
		return nil, errors.New("enabled is required")
	}
	if enabled.Kind != yaml.ScalarNode || enabled.ShortTag() != "!!bool" {
		return nil, errors.New("enabled must be true or false")
	}
	var value bool
	if errDecode := enabled.Decode(&value); errDecode != nil {
		return nil, errors.New("enabled must be true or false")
	}
	return responsesAPIPolicy{enabled: value}, nil
}

func (p responsesAPIPolicy) check(req modelRequest) (denial, bool) {
	if p.enabled || req.format != openAIResponsesFormat {
		return denial{}, false
	}
	return responsesAPIDisabled, true
}
