package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// routeModel answers the model.route hook. Requests for a model this plugin
// does not own are left alone; a group request is answered with the provider
// key and the model the group decided on, and CPA then serves it through its
// normal credential selection, cooldown, translation and streaming path.
func routeModel(request []byte) ([]byte, error) {
	var req pluginapi.ModelRouteRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model route request: %w", errUnmarshal)
	}
	resp, handled := pluginStateRef.route(req)
	if !handled {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(resp)
}

// sessionIdentity derives the same canonical session identity CPA's session
// affinity uses, from the same inputs: an explicit protocol session in the
// headers or body first, then an execution-derived identity, then a hash of
// the conversation root. Sharing that identity is what keeps this plugin's
// member sticky and CPA's credential sticky in step with each other.
func sessionIdentity(req pluginapi.ModelRouteRequest) string {
	return sessionIdentityOf(req.Headers, req.Body, req.Metadata, req.SourceFormat)
}

// sessionIdentityOf is sessionIdentity for any hook that carries the raw
// request, so the same session is recognised on the routing and the
// post-credential side of one request.
func sessionIdentityOf(headers http.Header, body []byte, metadata map[string]any, sourceFormat string) string {
	if info, ok := cliproxysession.ExtractSessionInfo(headers, body, metadata); ok {
		if id := strings.TrimSpace(info.SessionID); id != "" {
			return cliproxysession.BoundSessionIdentity(id)
		}
	}
	if derived := cliproxysession.DerivedID(metadata); derived != "" {
		return "derived:" + derived
	}
	callerScope := ""
	if metadata != nil {
		if value, ok := metadata[coreexecutor.CallerScopeMetadataKey].(string); ok {
			callerScope = value
		}
	}
	format := sdktranslator.FromString(strings.TrimSpace(sourceFormat))
	if derived := cliproxysession.DeriveID(format, body, callerScope); derived != "" {
		return "derived:" + derived
	}
	return ""
}

// splitThinkingSuffix separates the model(value) thinking suffix CPA parses
// out of model names, so a request for auto-group/fast(high) keeps its suffix
// on the model the group decided on.
func splitThinkingSuffix(model string) (string, string) {
	open := strings.LastIndex(model, "(")
	if open < 0 || !strings.HasSuffix(model, ")") {
		return model, ""
	}
	return model[:open], model[open:]
}
