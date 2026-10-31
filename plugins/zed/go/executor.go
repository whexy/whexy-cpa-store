package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	// Registers CPA's protocol translators for in-process conversion between
	// client formats and each model's native protocol.
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

// rpcExecutorRequest is the wire shape of executor.execute, execute_stream
// and count_tokens. StreamID is set for execute_stream only.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse answers execute_stream. Returning headers
// without chunks keeps the stream open; chunks then go through
// hostStreamEmit and the stream ends with hostStreamClose.
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type executorCall struct {
	req   rpcExecutorRequest
	host  host
	cfg   settings
	cred  credential
	model zedModel
	plan  routePlan
	// payload is the client's request in plan.Source.
	payload []byte
	body    []byte
}

func executorExecute(raw []byte) ([]byte, error) {
	call, failure := prepareExecutorCall(backgroundCtx, raw, false)
	if failure != nil {
		return failure, nil
	}
	return call.execute(backgroundCtx), nil
}

func executorExecuteStream(raw []byte) ([]byte, error) {
	call, failure := prepareExecutorCall(backgroundCtx, raw, true)
	if failure != nil {
		return failure, nil
	}
	if call.req.StreamID == "" {
		return errorEnvelope("invalid_request", "executor.execute_stream request has no stream_id", http.StatusBadRequest), nil
	}
	return call.executeStream(backgroundCtx), nil
}

// executorCountTokens declines counting. A 404 that does not name the model
// is availability-neutral to CPA.
func executorCountTokens(raw []byte) ([]byte, error) {
	return errorEnvelope("unsupported", "Zed does not count tokens for this model", http.StatusNotFound), nil
}

// executorHTTPRequest declines raw requests: signing arbitrary URLs with a
// Zed credential would leak it.
func executorHTTPRequest(raw []byte) ([]byte, error) {
	return errorEnvelope("unsupported", "Zed does not proxy raw HTTP requests", http.StatusNotImplemented), nil
}

func prepareExecutorCall(ctx context.Context, raw []byte, stream bool) (*executorCall, []byte) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errorEnvelope("invalid_request", "decode executor request: "+errUnmarshal.Error(), http.StatusBadRequest)
	}
	h := newHost(req.HostCallbackID)
	cfg := currentSettings()
	cred, errCred := resolveCredential(req.StorageJSON, req.AuthAttributes, req.AuthMetadata)
	if errCred != nil {
		return nil, errorEnvelope("invalid_auth", "Zed auth "+req.AuthID+": "+errCred.Error(), http.StatusUnauthorized)
	}
	requested, effort := splitModelSuffix(req.Model)
	model, errModel := modelRouteFor(ctx, h, cfg, cred, requested)
	if errModel != nil {
		if errors.Is(errModel, errModelNotFound) {
			return nil, errorEnvelope("model_not_found", errModel.Error(), http.StatusNotFound)
		}
		if errors.Is(errModel, errModelDisabled) {
			return nil, errorEnvelope("model_disabled", errModel.Error(), http.StatusBadRequest)
		}
		return nil, failureEnvelope(errModel)
	}
	upstream := nativeFormat(model)
	source := normalizeFormat(req.SourceFormat)
	response := source
	if strings.TrimSpace(req.Format) != "" {
		response = normalizeFormat(req.Format)
	}
	plan, errPlan := planRoute(source, response, upstream)
	if errPlan != nil {
		return nil, errorEnvelope("unsupported_format", errPlan.Error(), http.StatusBadRequest)
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	body, errBody := buildUpstreamBody(ctx, plan, payload, model, stream)
	if errBody != nil {
		return nil, errorEnvelope("invalid_request", fmt.Sprintf("build %s request for %s: %v", upstream, model.ID, errBody), http.StatusBadRequest)
	}
	if effort != "" {
		body = applyRequestedEffort(upstream, body, effort, model)
	}
	return &executorCall{req: req, host: h, cfg: cfg, cred: cred, model: model, plan: plan, payload: payload, body: body}, nil
}

func (c *executorCall) completionBody() ([]byte, error) {
	return completionBody(c.model.Provider, c.model.ID, c.body)
}

// sendCompletion POSTs the envelope and retries once with a fresh JWT when
// the edge rejects the cached one.
func (c *executorCall) sendCompletion(ctx context.Context) (pluginapi.HTTPResponse, error) {
	envelope, errBody := c.completionBody()
	if errBody != nil {
		return pluginapi.HTTPResponse{}, errBody
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, errToken := llmToken(ctx, c.host, c.cfg, c.cred)
		if errToken != nil {
			return pluginapi.HTTPResponse{}, errToken
		}
		resp, errDo := c.host.Do(ctx, hostRequest(http.MethodPost, c.cfg.BaseURL+"/completions", llmHeaders(c.cfg, token, true), envelope))
		if errDo != nil {
			return pluginapi.HTTPResponse{}, errDo
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			llmTokens.drop(llmTokens.key(c.cred))
			continue
		}
		return resp, nil
	}
	return pluginapi.HTTPResponse{}, errors.New("Zed rejected every LLM token")
}

func (c *executorCall) sendCompletionStream(ctx context.Context) (*hostHTTPStream, error) {
	envelope, errBody := c.completionBody()
	if errBody != nil {
		return nil, errBody
	}
	token, errToken := llmToken(ctx, c.host, c.cfg, c.cred)
	if errToken != nil {
		return nil, errToken
	}
	headers := llmHeaders(c.cfg, token, true)
	return c.host.DoStream(ctx, hostRequest(http.MethodPost, c.cfg.BaseURL+"/completions", headers, envelope))
}

func (c *executorCall) execute(ctx context.Context) []byte {
	resp, errDo := c.sendCompletion(ctx)
	if errDo != nil {
		return c.transportFailure(errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.upstreamFailure(resp.StatusCode, resp.Headers, resp.Body)
	}
	events, errEvents := c.collectEvents(resp.Body)
	if errEvents != nil {
		return c.failureEnvelope(errEvents)
	}
	native, errAssemble := assembleNative(c.plan.Upstream, events)
	if errAssemble != nil {
		return errorEnvelope("translation_failed", errAssemble.Error(), http.StatusBadGateway)
	}
	out, errConvert := convertResponse(ctx, c.plan, c.req.Model, c.payload, c.body, native)
	if errConvert != nil {
		return errorEnvelope("translation_failed", errConvert.Error(), http.StatusBadGateway)
	}
	raw, errEnvelope := okEnvelope(pluginapi.ExecutorResponse{Payload: out, Headers: diagnosticHeaders(resp.Headers)})
	if errEnvelope != nil {
		return errorEnvelope("plugin_error", errEnvelope.Error(), http.StatusInternalServerError)
	}
	return raw
}

// collectEvents parses an NDJSON body into native events, stopping at
// stream_ended or a failed status.
func (c *executorCall) collectEvents(body []byte) ([][]byte, error) {
	var splitter lineSplitter
	lines := append(splitter.push(body), splitter.flush()...)
	var events [][]byte
	for _, line := range lines {
		parsed, errLine := parseZedLine(line)
		if errLine != nil {
			return nil, errLine
		}
		if parsed.Failure != nil {
			return nil, parsed.Failure
		}
		if parsed.Ended {
			break
		}
		if parsed.Event != nil {
			events = append(events, parsed.Event)
		}
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("Zed returned no model events: %s", firstLine(string(body)))
	}
	return events, nil
}

func (c *executorCall) executeStream(ctx context.Context) []byte {
	upstream, errStart := c.sendCompletionStream(ctx)
	if errStart != nil {
		return c.transportFailure(errStart)
	}
	if upstream.StatusCode == http.StatusUnauthorized {
		_, _ = upstream.ReadAll()
		upstream.Close()
		// The cached JWT may have been revoked; mint another one and retry
		// once before letting CPA cool the account down.
		llmTokens.drop(llmTokens.key(c.cred))
		upstream, errStart = c.sendCompletionStream(ctx)
		if errStart != nil {
			return c.transportFailure(errStart)
		}
	}
	if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
		body, _ := upstream.ReadAll()
		upstream.Close()
		return c.upstreamFailure(upstream.StatusCode, upstream.Headers, body)
	}
	converter := newStreamConverter(ctx, c.plan, c.req.Model, c.payload, c.body)
	streamID := c.req.StreamID
	startBackground(func(ctx context.Context) { c.pumpStream(ctx, upstream, converter, streamID) })
	raw, errEnvelope := okEnvelope(rpcExecutorStreamResponse{Headers: diagnosticHeaders(upstream.Headers)})
	if errEnvelope != nil {
		return errorEnvelope("plugin_error", errEnvelope.Error(), http.StatusInternalServerError)
	}
	return raw
}

// pumpStream relays Zed's NDJSON events to the host stream until either side
// ends. It has no deadline of its own: the host cancels the upstream read on
// client disconnect, and shutdown cancels the context.
func (c *executorCall) pumpStream(ctx context.Context, upstream *hostHTTPStream, converter *streamConverter, streamID string) {
	closed := false
	closeStream := func(message string) {
		if !closed {
			closed = true
			upstream.Close()
			hostStreamClose(streamID, message)
		}
	}
	defer closeStream("Zed stream ended unexpectedly")

	emit := func(chunks [][]byte) bool {
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				continue
			}
			if hostStreamEmit(streamID, chunk) != nil {
				closeStream("")
				return false
			}
		}
		return true
	}
	var splitter lineSplitter
	for {
		payload, done, errRead := upstream.Read()
		lines := splitter.push(payload)
		if done && errRead == nil {
			lines = append(lines, splitter.flush()...)
		}
		for _, line := range lines {
			parsed, errLine := parseZedLine(line)
			if errLine != nil {
				closeStream(errLine.Error())
				return
			}
			if parsed.Failure != nil {
				closeStream(c.failureMessage(parsed.Failure))
				return
			}
			if parsed.Ended {
				chunks, errFinish := converter.finish()
				if !emit(chunks) {
					return
				}
				if errFinish != nil {
					closeStream(errFinish.Error())
					return
				}
				closeStream("")
				return
			}
			if parsed.Event == nil {
				continue
			}
			chunks, errConvert := converter.convert(parsed.Event)
			if !emit(chunks) {
				return
			}
			if errConvert != nil {
				closeStream(errConvert.Error())
				return
			}
		}
		if errRead != nil {
			closeStream("Zed stream failed: " + errRead.Error())
			return
		}
		if done {
			chunks, errFinish := converter.finish()
			if !emit(chunks) {
				return
			}
			if errFinish != nil {
				closeStream(errFinish.Error())
				return
			}
			closeStream("")
			return
		}
	}
}

// transportFailure maps an error from before Zed answered. A token failure
// keeps Zed's status, so a revoked login is a 401 CPA refreshes rather than an
// unreachable upstream.
func (c *executorCall) transportFailure(err error) []byte {
	var failure *zedFailure
	if errors.As(err, &failure) {
		return upstreamFailureEnvelope(failure)
	}
	if errors.Is(err, context.Canceled) {
		return errorEnvelope("canceled", "Zed request canceled", http.StatusRequestTimeout)
	}
	return errorEnvelope("upstream_unreachable", "Zed request failed: "+err.Error(), http.StatusBadGateway)
}

// upstreamFailure maps a Zed error to the status CPA acts on. A plan
// restriction is a 400 so it never cools the account down: the model is
// unavailable, the login is fine.
func (c *executorCall) upstreamFailure(status int, headers http.Header, body []byte) []byte {
	if minimum := headers.Get("x-zed-minimum-required-version"); minimum != "" {
		raiseZedVersion(minimum)
	}
	message := firstLine(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	failure := &zedFailure{Status: status, Message: message}
	if status == http.StatusForbidden && strings.Contains(strings.ToLower(message), "not included in your plan") {
		failure.PlanRestricted = true
	}
	if seconds, errAtoi := strconv.Atoi(strings.TrimSpace(headers.Get("Retry-After"))); errAtoi == nil && seconds > 0 {
		failure.RetryAfter = time.Duration(seconds) * time.Second
	}
	return upstreamFailureEnvelope(failure)
}

func (c *executorCall) failureEnvelope(err error) []byte {
	return failureEnvelope(err)
}

// failureEnvelope maps an error that did not start a stream. A catalog or
// token failure must keep its status so CPA can refresh or cool down instead
// of reporting a missing model.
func failureEnvelope(err error) []byte {
	var failure *zedFailure
	if errors.As(err, &failure) {
		return upstreamFailureEnvelope(failure)
	}
	return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway)
}

func upstreamFailureEnvelope(failure *zedFailure) []byte {
	message := failure.Message
	if failure.PlanRestricted {
		return errorEnvelope("plan_restricted", message, http.StatusBadRequest)
	}
	switch {
	case failure.Status == http.StatusUnauthorized:
		return errorEnvelope("invalid_auth", message, http.StatusUnauthorized)
	case failure.Status == http.StatusTooManyRequests:
		return errorEnvelopeWithRetryAfter("rate_limited", message, http.StatusTooManyRequests, failure.RetryAfter)
	case failure.Status >= 500:
		return errorEnvelope("upstream_error", message, http.StatusBadGateway)
	case failure.Status >= 400:
		return errorEnvelope("upstream_error", message, failure.Status)
	default:
		return errorEnvelope("upstream_error", message, http.StatusBadGateway)
	}
}

func (c *executorCall) failureMessage(failure *zedFailure) string {
	return failure.Message
}

// diagnosticHeaders keeps Zed's identification headers. Others are dropped
// because the body may have been translated, so length and type no longer
// apply.
func diagnosticHeaders(upstream http.Header) http.Header {
	out := http.Header{}
	for name, values := range upstream {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-zed-") {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}
