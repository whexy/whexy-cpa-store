package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeEncodedIDPrefix = "claude-fable-5-dd-"

// capturedCatalog is the last unfiltered OpenAI model list, served by the
// models route because plugins cannot read the host's model registry.
type capturedCatalog struct {
	models     []string
	capturedAt time.Time
}

var lastCatalog atomic.Pointer[capturedCatalog]

// interceptResponse filters model lists. The host's WriteModelListResponse
// sends them without a model, request body or caller_scope; every
// successful non-streaming execution response arrives here too and is left
// alone.
func interceptResponse(raw []byte) ([]byte, error) {
	// Execution responses carry a model. Checking that first spares
	// decoding their bodies.
	var probe struct {
		Model          string
		RequestedModel string
	}
	if errUnmarshal := json.Unmarshal(raw, &probe); errUnmarshal != nil {
		return nil, fmt.Errorf("decode response intercept request: %w", errUnmarshal)
	}
	if probe.Model != "" || probe.RequestedModel != "" {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	var req pluginapi.ResponseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode response intercept request: %w", errUnmarshal)
	}
	if len(req.RequestBody) > 0 || len(req.OriginalRequest) > 0 {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	return okEnvelope(filterModelList(currentRules(), req, time.Now()))
}

// filterModelList returns an empty response, which leaves the list as it is,
// unless the caller's lists are restricted.
func filterModelList(rules *ruleSet, req pluginapi.ResponseInterceptRequest, now time.Time) pluginapi.ResponseInterceptResponse {
	if req.SourceFormat == openAIFormat {
		captureCatalog(req.Body, now)
	}
	shows, restricted := rules.modelListFor(req.RequestHeaders)
	if !restricted {
		return pluginapi.ResponseInterceptResponse{}
	}
	body, ok := filterListBody(req.SourceFormat, req.Body, shows)
	if !ok {
		return pluginapi.ResponseInterceptResponse{}
	}
	return pluginapi.ResponseInterceptResponse{Body: body}
}

// callerKeys returns the client key candidates in the order of
// CLIProxyAPI's config API key provider. Keys sent as ?key= or ?auth_token=
// do not reach response interceptors.
func callerKeys(headers http.Header) []string {
	var keys []string
	for _, candidate := range []string{
		bearerToken(headers.Get("Authorization")),
		headers.Get("X-Goog-Api-Key"),
		headers.Get("X-Api-Key"),
	} {
		if candidate != "" {
			keys = append(keys, candidate)
		}
	}
	return keys
}

func bearerToken(header string) string {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || strings.ToLower(scheme) != "bearer" {
		return header
	}
	return strings.TrimSpace(token)
}

// modelListFor returns which models the lists of the caller sending headers
// show, and false when they are not restricted. Callers that requests would
// all be refused for see empty lists.
func (r *ruleSet) modelListFor(headers http.Header) (func(model string) bool, bool) {
	candidates := callerKeys(headers)
	if len(candidates) == 0 {
		return nil, false
	}
	if r.err != "" {
		return listsNothing, true
	}
	// The provider authenticates with the first candidate that is a
	// configured key, which the plugin cannot see; the first candidate with
	// an entry stands in for it, so a non-key header such as a proxy's
	// Authorization: Basic does not hide the client key behind it.
	for _, candidate := range candidates {
		if key, found := r.keys[session.CallerScope(candidate)]; found {
			return key.modelList()
		}
	}
	return nil, false
}

// modelList returns which models the key's lists show, and false when they
// are not restricted.
func (k keyRules) modelList() (func(model string) bool, bool) {
	if k.err != "" {
		return listsNothing, true
	}
	var listers []modelLister
	for _, policy := range k.policies {
		if lister, ok := policy.rule.(modelLister); ok {
			listers = append(listers, lister)
		}
	}
	if len(listers) == 0 {
		return nil, false
	}
	return func(model string) bool {
		model = normalizeModel(model)
		for _, lister := range listers {
			if !lister.lists(model) {
				return false
			}
		}
		return true
	}, true
}

func listsNothing(string) bool { return false }

func captureCatalog(body []byte, now time.Time) {
	if !gjson.ValidBytes(body) {
		return
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return
	}
	models := []string{}
	data.ForEach(func(_, entry gjson.Result) bool {
		if id := entry.Get("id"); id.Type == gjson.String && id.Str != "" {
			models = append(models, id.Str)
		}
		return true
	})
	lastCatalog.Store(&capturedCatalog{models: models, capturedAt: now.UTC()})
}

// filterListBody keeps the entries shows accepts, leaving every other byte
// of the list as it was. ok is false for shapes it does not know.
func filterListBody(format string, body []byte, shows func(model string) bool) (filtered []byte, ok bool) {
	if !gjson.ValidBytes(body) {
		return nil, false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, false
	}
	if data := root.Get("data"); data.IsArray() {
		if format == claudeFormat {
			return filterClaudeList(body, data, shows)
		}
		kept := keepEntries(data, func(entry gjson.Result) string { return entry.Get("id").String() }, shows)
		return replaceArray(body, "data", kept)
	}
	// Codex catalogs list slugs; Gemini lists "models/<id>" names.
	if models := root.Get("models"); models.IsArray() {
		kept := keepEntries(models, func(entry gjson.Result) string {
			if slug := entry.Get("slug"); slug.Exists() {
				return slug.String()
			}
			return entry.Get("name").String()
		}, shows)
		return replaceArray(body, "models", kept)
	}
	return nil, false
}

// filterClaudeList matches the decoded IDs and recomputes first_id and
// last_id from what is kept.
func filterClaudeList(body []byte, data gjson.Result, shows func(model string) bool) ([]byte, bool) {
	kept := keepEntries(data, func(entry gjson.Result) string {
		return decodeClaudeModelID(entry.Get("id").String())
	}, shows)
	filtered, ok := replaceArray(body, "data", kept)
	if !ok {
		return nil, false
	}
	firstID, lastID := "", ""
	if len(kept) > 0 {
		firstID, lastID = kept[0].Get("id").String(), kept[len(kept)-1].Get("id").String()
	}
	for field, value := range map[string]string{"first_id": firstID, "last_id": lastID} {
		if !gjson.GetBytes(filtered, field).Exists() {
			continue
		}
		updated, errSet := sjson.SetBytes(filtered, field, value)
		if errSet != nil {
			return nil, false
		}
		filtered = updated
	}
	return filtered, true
}

func keepEntries(list gjson.Result, idOf func(entry gjson.Result) string, shows func(model string) bool) []gjson.Result {
	var kept []gjson.Result
	list.ForEach(func(_, entry gjson.Result) bool {
		if shows(idOf(entry)) {
			kept = append(kept, entry)
		}
		return true
	})
	return kept
}

func replaceArray(body []byte, field string, entries []gjson.Result) ([]byte, bool) {
	var array bytes.Buffer
	array.WriteByte('[')
	for index, entry := range entries {
		if index > 0 {
			array.WriteByte(',')
		}
		array.WriteString(entry.Raw)
	}
	array.WriteByte(']')
	updated, errSet := sjson.SetRawBytes(body, field, array.Bytes())
	if errSet != nil {
		return nil, false
	}
	return updated, true
}

// decodeClaudeModelID mirrors the host's ResolveClaudeModelIDPrefix: Claude
// model lists show IDs not starting with "claude-" as
// "claude-fable-5-dd-" plus the reversed ID.
func decodeClaudeModelID(id string) string {
	base, suffix := id, ""
	if open := strings.LastIndex(id, "("); open >= 0 && strings.HasSuffix(id, ")") {
		base, suffix = id[:open], id[open:]
	}
	encoded, found := strings.CutPrefix(base, claudeEncodedIDPrefix)
	if !found || encoded == "" {
		return id
	}
	runes := []rune(encoded)
	for left, right := 0, len(runes)-1; left < right; left, right = left+1, right-1 {
		runes[left], runes[right] = runes[right], runes[left]
	}
	return string(runes) + suffix
}
