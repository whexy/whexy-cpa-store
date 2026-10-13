package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	// Client protocols as the host's SourceFormat names them.
	openAIFormat          = "openai"
	openAIResponsesFormat = "openai-response"
	claudeFormat          = "claude"
	geminiFormat          = "gemini"
	interactionsFormat    = "interactions"

	disableHeader = "WHEXY_CPA_DISABLE_RESPONSE_API"
	// callerScopeKey is the execution metadata the host fills with
	// session.CallerScope of the client key the request authenticated with.
	callerScopeKey = "caller_scope"
)

// interceptRequest never returns an error, because the host would pass the
// request on unchecked.
func interceptRequest(raw []byte, decide func(*ruleSet, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse) ([]byte, error) {
	rules := currentRules()
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return okEnvelope(decideUndecodable(rules, raw))
	}
	return okEnvelope(decide(rules, req))
}

// decideUndecodable judges a request whose fields cannot be read by the
// caller scope alone: it is refused whenever a policy might apply.
func decideUndecodable(rules *ruleSet, raw []byte) pluginapi.RequestInterceptResponse {
	scope := gjson.GetBytes(raw, "Metadata."+callerScopeKey)
	_, configured := rules.keys[scope.Str]
	var message string
	switch {
	case rules.err != "":
		message = rulesUnavailableMessage
	case scope.Type == gjson.String && configured:
		message = requestUnreadableMessage
	default:
		return pluginapi.RequestInterceptResponse{}
	}
	return policiesUnavailable(modelRequest{format: gjson.GetBytes(raw, "SourceFormat").Str}, message).response()
}

// decide runs request.intercept_before, once per request before a
// credential is selected.
func decide(rules *ruleSet, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	scope, _ := req.Metadata[callerScopeKey].(string)
	model := requestedModel(req)
	call := modelRequest{format: req.SourceFormat, model: model, requested: model, websocket: isWebSocketUpgrade(req.Headers)}
	if rules.err != "" && scope != "" {
		return policiesUnavailable(call, rulesUnavailableMessage).response()
	}
	if req.SourceFormat == openAIResponsesFormat && hasDisableHeader(req.Headers) {
		return responsesAPIDisabled.response()
	}
	key, found := rules.keys[scope]
	if scope == "" || !found {
		return pluginapi.RequestInterceptResponse{}
	}
	if key.err != "" {
		return keyInvalid(call).response()
	}
	for _, policy := range key.policies {
		checker, ok := policy.rule.(requestChecker)
		if !ok {
			continue
		}
		if refusal, refused := checker.check(call); refused {
			return refusal.response()
		}
	}
	return pluginapi.RequestInterceptResponse{}
}

// decideAfterAuth runs request.intercept_after, once per credential attempt
// with Model set to the upstream model. Invalid keys and an unreadable
// configuration were already refused before credential selection.
func decideAfterAuth(rules *ruleSet, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	scope, _ := req.Metadata[callerScopeKey].(string)
	key, found := rules.keys[scope]
	if scope == "" || !found || key.err != "" {
		return pluginapi.RequestInterceptResponse{}
	}
	requested := normalizeModel(req.RequestedModel)
	upstream := normalizeModel(req.Model)
	if requested == "" {
		requested = upstream
	}
	call := modelRequest{format: req.SourceFormat, model: upstream, requested: requested, websocket: isWebSocketUpgrade(req.Headers)}
	for _, policy := range key.policies {
		checker, ok := policy.rule.(upstreamChecker)
		if !ok {
			continue
		}
		if refusal, refused := checker.checkUpstream(call); refused {
			return refusal.response()
		}
	}
	return pluginapi.RequestInterceptResponse{}
}

// requestedModel is the name the client asked for. "auto" is judged by the
// model the host already resolved it to.
func requestedModel(req pluginapi.RequestInterceptRequest) string {
	model := normalizeModel(req.RequestedModel)
	if model == "" || strings.EqualFold(model, "auto") {
		model = normalizeModel(req.Model)
	}
	return model
}

// Go keeps underscore header names non-canonical, and reverse proxies such as
// nginx drop them by default, so the hyphenated spelling is accepted as well.
func hasDisableHeader(headers http.Header) bool {
	for key := range headers {
		if strings.EqualFold(strings.ReplaceAll(key, "-", "_"), disableHeader) {
			return true
		}
	}
	return false
}

// isWebSocketUpgrade marks a turn on the Responses API WebSocket, whose
// request headers are those of the upgrade request.
func isWebSocketUpgrade(headers http.Header) bool {
	for _, value := range headers.Values("Upgrade") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}

const (
	keyInvalidMessage        = "This API key's client-keys policy is invalid, so every model request with it is refused. Ask the administrator to fix it in Management Center → Client keys."
	rulesUnavailableMessage  = "Client key policies are unavailable because the client-keys plugin configuration could not be read. Ask the administrator to fix it in Management Center → Client keys."
	requestUnreadableMessage = "The client-keys plugin could not read this request, so it cannot check the API key's policies."
)

// requestTooLarge refuses a request the plugin cannot read. Its protocol is
// unknown, so the body has the OpenAI shape.
var requestTooLarge = jsonDenial(modelRequest{}, http.StatusRequestEntityTooLarge, openAIError("invalid_request_error", "request_too_large", "", "The request is too large for the client-keys plugin to check."))

func modelNotFound(req modelRequest) denial {
	m := req.requested
	switch protocolOf(req.format) {
	case claudeFormat:
		return jsonDenial(req, http.StatusNotFound, claudeError("not_found_error", "model: "+m))
	case geminiFormat:
		return jsonDenial(req, http.StatusNotFound, googleError(http.StatusNotFound, "NOT_FOUND", "models/"+m+" is not available for this API key."))
	default:
		return jsonDenial(req, http.StatusNotFound, openAIError("invalid_request_error", "model_not_found", "model", "The model `"+m+"` does not exist or you do not have access to it."))
	}
}

func keyInvalid(req modelRequest) denial {
	switch protocolOf(req.format) {
	case claudeFormat:
		return jsonDenial(req, http.StatusForbidden, claudeError("permission_error", keyInvalidMessage))
	case geminiFormat:
		return jsonDenial(req, http.StatusForbidden, googleError(http.StatusForbidden, "PERMISSION_DENIED", keyInvalidMessage))
	default:
		return jsonDenial(req, http.StatusForbidden, openAIError("permission_error", "client_key_policy_invalid", "", keyInvalidMessage))
	}
}

func policiesUnavailable(req modelRequest, message string) denial {
	switch protocolOf(req.format) {
	case claudeFormat:
		return jsonDenial(req, http.StatusServiceUnavailable, claudeError("api_error", message))
	case geminiFormat:
		return jsonDenial(req, http.StatusServiceUnavailable, googleError(http.StatusServiceUnavailable, "UNAVAILABLE", message))
	default:
		return jsonDenial(req, http.StatusServiceUnavailable, openAIError("server_error", "client_key_policy_unavailable", "", message))
	}
}

// protocolOf groups client formats by the error shape their clients parse.
func protocolOf(format string) string {
	switch format {
	case claudeFormat:
		return claudeFormat
	case geminiFormat, interactionsFormat:
		return geminiFormat
	default:
		return openAIFormat
	}
}

// jsonDenial answers a WebSocket turn with 400: the host closes the socket
// without a message for other statuses and ignores the body there.
func jsonDenial(req modelRequest, status int, body any) denial {
	if req.websocket {
		status = http.StatusBadRequest
	}
	raw, _ := json.Marshal(body)
	return denial{status: status, body: raw}
}

type claudeErrorBody struct {
	Type  string            `json:"type"`
	Error claudeErrorDetail `json:"error"`
}

type claudeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func claudeError(kind, message string) claudeErrorBody {
	return claudeErrorBody{Type: "error", Error: claudeErrorDetail{Type: kind, Message: message}}
}

type googleErrorBody struct {
	Error googleErrorDetail `json:"error"`
}

type googleErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func googleError(code int, status, message string) googleErrorBody {
	return googleErrorBody{Error: googleErrorDetail{Code: code, Message: message, Status: status}}
}

type openAIErrorBody struct {
	Error openAIErrorDetail `json:"error"`
}

type openAIErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func openAIError(kind, code, param, message string) openAIErrorBody {
	detail := openAIErrorDetail{Message: message, Type: kind, Code: code}
	if param != "" {
		detail.Param = &param
	}
	return openAIErrorBody{Error: detail}
}
