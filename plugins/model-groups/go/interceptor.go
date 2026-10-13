package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// interceptRequestBeforeAuth is a no-op: the host calls it because the plugin
// declares a request interceptor, but only the after-auth hook has the
// selected credential this plugin records.
func interceptRequestBeforeAuth(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// interceptRequestAfterAuth observes which credential CPA picked for a session.
// It runs once per credential attempt, so the recorded credential is the last
// one CPA tried for that session's request.
//
// host.affinity.lookup cannot answer this: the host installs itself as the
// plugin scheduler, and that makes LookupSessionAffinity report "unsupported"
// for every request. The post-credential interceptor does see the selection,
// in `selected_auth_id` and `selected_auth_index`, which is what the page's
// account column is built from.
//
// The interceptor changes nothing about the request: it answers with an empty
// response for every request that is not one of this plugin's group IDs.
func interceptRequestAfterAuth(request []byte) ([]byte, error) {
	// This hook runs for every model execution in the host, so a request that
	// is not this plugin's business is answered from a field lookup alone,
	// without decoding the payload.
	if _, ok := pluginStateRef.groupIDForRequested(gjson.GetBytes(request, "RequestedModel").String()); !ok {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	groupID, ok := pluginStateRef.groupIDForRequested(req.RequestedModel)
	if !ok {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	sessionID := sessionIdentityOf(req.Headers, req.Body, req.Metadata, req.SourceFormat)
	if sessionID == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	pluginStateRef.recordAccount(groupID, sessionID, metadataString(req.Metadata, "selected_auth_id"), metadataString(req.Metadata, "selected_auth_index"), time.Now())
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// groupIDForRequested reports whether a requested model is one of the enabled
// groups and returns its ID.
func (s *pluginState) groupIDForRequested(requested string) (string, bool) {
	base, _ := splitThinkingSuffix(strings.TrimSpace(requested))
	if base == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.byID[base]
	if !ok || !group.cfg.Enabled {
		return "", false
	}
	return group.cfg.ID, true
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
