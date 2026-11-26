package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
	"github.com/whexy/whexy-cpa-store/pluginkit/translate"
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
// host.EmitStream and the stream ends with host.CloseStream.
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type executorCall struct {
	req    rpcExecutorRequest
	client host.Client
	cfg    settings
	cred   credential
	model  zedModel
	plan   translate.Plan
	// payload is the client's request in plan.Source.
	payload []byte
	body    []byte
	// namespaced maps flattened tool names back to the client's namespaced
	// tools.
	namespaced map[string]namespacedTool
}

func executorExecute(raw []byte) ([]byte, error) {
	// A call still in flight at shutdown must stop before the host unloads
	// the library underneath it.
	ctx := background.Context()
	call, failure := prepareExecutorCall(ctx, raw, false)
	if failure != nil {
		return failure, nil
	}
	return call.execute(ctx), nil
}

func executorExecuteStream(raw []byte) ([]byte, error) {
	ctx := background.Context()
	call, failure := prepareExecutorCall(ctx, raw, true)
	if failure != nil {
		return failure, nil
	}
	if call.req.StreamID == "" {
		return rpc.Fail("invalid_request", "executor.execute_stream request has no stream_id", rpc.WithStatus(http.StatusBadRequest)), nil
	}
	return call.executeStream(ctx), nil
}

// executorCountTokens declines counting. A 404 that does not name the model
// is availability-neutral to CPA.
func executorCountTokens() ([]byte, error) {
	return rpc.Fail("unsupported", "Zed does not count tokens for this model", rpc.WithStatus(http.StatusNotFound)), nil
}

// executorHTTPRequest declines raw requests: signing arbitrary URLs with a
// Zed credential would leak it.
func executorHTTPRequest() ([]byte, error) {
	return rpc.Fail("unsupported", "Zed does not proxy raw HTTP requests", rpc.WithStatus(http.StatusNotImplemented)), nil
}

func prepareExecutorCall(ctx context.Context, raw []byte, stream bool) (*executorCall, []byte) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, rpc.Fail("invalid_request", "decode executor request: "+errUnmarshal.Error(), rpc.WithStatus(http.StatusBadRequest))
	}
	client := host.New(req.HostCallbackID)
	cfg := currentSettings()
	cred, errCred := resolveCredential(req.StorageJSON, req.AuthAttributes, req.AuthMetadata)
	if errCred != nil {
		return nil, rpc.Fail("invalid_auth", "Zed auth "+req.AuthID+": "+errCred.Error(), rpc.WithStatus(http.StatusUnauthorized))
	}
	requested, effort := splitModelSuffix(req.Model)
	model, errModel := modelRouteFor(ctx, client, cfg, cred, requested)
	if errModel != nil {
		if errors.Is(errModel, errModelNotFound) {
			return nil, rpc.Fail("model_not_found", errModel.Error(), rpc.WithStatus(http.StatusNotFound))
		}
		if errors.Is(errModel, errModelDisabled) {
			return nil, rpc.Fail("model_disabled", errModel.Error(), rpc.WithStatus(http.StatusBadRequest))
		}
		return nil, failureEnvelope(errModel)
	}
	upstream := nativeFormat(model)
	source := provider.NormalizeFormat(req.SourceFormat)
	response := source
	if strings.TrimSpace(req.Format) != "" {
		response = provider.NormalizeFormat(req.Format)
	}
	plan, errPlan := translate.PlanRoute(source, response, upstream)
	if errPlan != nil {
		return nil, rpc.Fail("unsupported_format", errPlan.Error(), rpc.WithStatus(http.StatusBadRequest))
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	body, namespaced, errBody := buildUpstreamBody(ctx, plan, payload, model, stream)
	if errBody != nil {
		return nil, rpc.Fail("invalid_request", fmt.Sprintf("build %s request for %s: %v", upstream, model.ID, errBody), rpc.WithStatus(http.StatusBadRequest))
	}
	if effort != "" {
		body = applyRequestedEffort(upstream, body, effort, model)
	}
	return &executorCall{req: req, client: client, cfg: cfg, cred: cred, model: model, plan: plan, payload: payload, body: body, namespaced: namespaced}, nil
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
		token, errToken := llmToken(ctx, c.client, c.cfg, c.cred)
		if errToken != nil {
			return pluginapi.HTTPResponse{}, errToken
		}
		resp, errDo := c.client.Do(ctx, hostRequest(http.MethodPost, c.cfg.BaseURL+"/completions", llmHeaders(c.cfg, token, true), envelope))
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

func (c *executorCall) sendCompletionStream(ctx context.Context) (*host.Stream, error) {
	envelope, errBody := c.completionBody()
	if errBody != nil {
		return nil, errBody
	}
	token, errToken := llmToken(ctx, c.client, c.cfg, c.cred)
	if errToken != nil {
		return nil, errToken
	}
	headers := llmHeaders(c.cfg, token, true)
	return c.client.DoStream(ctx, hostRequest(http.MethodPost, c.cfg.BaseURL+"/completions", headers, envelope))
}

func (c *executorCall) execute(ctx context.Context) []byte {
	resp, errDo := c.sendCompletion(ctx)
	if errDo != nil {
		return transportFailure(errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return upstreamFailureEnvelope(completionFailure(resp))
	}
	events, errEvents := c.collectEvents(resp.Body)
	if errEvents != nil {
		return failureEnvelope(errEvents)
	}
	native, errAssemble := assembleNative(c.plan.Upstream, events)
	if errAssemble != nil {
		return rpc.Fail("translation_failed", errAssemble.Error(), rpc.WithStatus(http.StatusBadGateway))
	}
	restore := func(body []byte) []byte { return restoreToolNamespaces(body, c.namespaced) }
	out, errConvert := provider.ConvertResponse(ctx, c.plan, c.req.Model, c.payload, c.body, native, restore)
	if errConvert != nil {
		return rpc.Fail("translation_failed", errConvert.Error(), rpc.WithStatus(http.StatusBadGateway))
	}
	raw, errEnvelope := rpc.OK(pluginapi.ExecutorResponse{Payload: out, Headers: diagnosticHeaders(resp.Headers)})
	if errEnvelope != nil {
		return rpc.Fail("plugin_error", errEnvelope.Error(), rpc.WithStatus(http.StatusInternalServerError))
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
		return transportFailure(errStart)
	}
	if upstream.StatusCode == http.StatusUnauthorized {
		_, _ = upstream.ReadAll()
		upstream.Close()
		// The cached JWT may have been revoked; mint another one and retry
		// once before letting CPA cool the account down.
		llmTokens.drop(llmTokens.key(c.cred))
		upstream, errStart = c.sendCompletionStream(ctx)
		if errStart != nil {
			return transportFailure(errStart)
		}
	}
	if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
		body, _ := upstream.ReadAll()
		upstream.Close()
		return upstreamFailureEnvelope(completionFailure(pluginapi.HTTPResponse{StatusCode: upstream.StatusCode, Headers: upstream.Headers, Body: body}))
	}
	converter := newStreamConverter(ctx, c.plan, c.req.Model, c.payload, c.body, c.namespaced)
	streamID := c.req.StreamID
	background.Go(func(ctx context.Context) { c.pumpStream(ctx, upstream, converter, streamID) })
	raw, errEnvelope := rpc.OK(rpcExecutorStreamResponse{Headers: diagnosticHeaders(upstream.Headers)})
	if errEnvelope != nil {
		return rpc.Fail("plugin_error", errEnvelope.Error(), rpc.WithStatus(http.StatusInternalServerError))
	}
	return raw
}

// pumpStream relays Zed's NDJSON events to the host stream until either side
// ends. It has no deadline of its own: the host cancels the upstream read on
// client disconnect, and shutdown cancels the context.
func (c *executorCall) pumpStream(ctx context.Context, upstream *host.Stream, converter *streamConverter, streamID string) {
	closed := false
	closeStream := func(message string) {
		if !closed {
			closed = true
			upstream.Close()
			host.CloseStream(streamID, message)
		}
	}
	defer closeStream("Zed stream ended unexpectedly")

	emit := func(chunks [][]byte) bool {
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				continue
			}
			if host.EmitStream(streamID, chunk) != nil {
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
				closeStream(parsed.Failure.Message)
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
func transportFailure(err error) []byte {
	var failure *zedFailure
	if errors.As(err, &failure) {
		return upstreamFailureEnvelope(failure)
	}
	return rpc.TransportFailure(displayName, err)
}

// failureEnvelope maps an error that did not start a stream. A catalog or
// token failure must keep its status so CPA can refresh or cool down instead
// of reporting a missing model.
func failureEnvelope(err error) []byte {
	var failure *zedFailure
	if errors.As(err, &failure) {
		return upstreamFailureEnvelope(failure)
	}
	return rpc.Fail("upstream_error", err.Error(), rpc.WithStatus(http.StatusBadGateway))
}

// completionFailure reads a failed completion. Only a model outside the
// account's plan is a plan restriction here: any other 403 that mentions the
// plan, such as an expired one, must keep its status so CPA moves to another
// account.
func completionFailure(resp pluginapi.HTTPResponse) *zedFailure {
	failure := failureFrom(resp, "")
	failure.PlanRestricted = resp.StatusCode == http.StatusForbidden &&
		strings.Contains(strings.ToLower(failure.Message), "not included in your plan")
	return failure
}

// upstreamFailureEnvelope maps a Zed error to the status CPA acts on. A plan
// restriction is a 400 so it never cools the account down: the model is
// unavailable, the login is fine.
func upstreamFailureEnvelope(failure *zedFailure) []byte {
	message := failure.Message
	if failure.PlanRestricted {
		return rpc.Fail("plan_restricted", message, rpc.WithStatus(http.StatusBadRequest))
	}
	switch {
	case failure.Status == http.StatusUnauthorized:
		return rpc.Fail("invalid_auth", message, rpc.WithStatus(http.StatusUnauthorized))
	case failure.Status == http.StatusTooManyRequests:
		return rpc.Fail("rate_limited", message, rpc.WithStatus(http.StatusTooManyRequests), rpc.WithRetryAfter(failure.RetryAfter))
	case failure.Status >= 500:
		return rpc.Fail("upstream_error", message, rpc.WithStatus(http.StatusBadGateway))
	case failure.Status >= 400:
		return rpc.Fail("upstream_error", message, rpc.WithStatus(failure.Status))
	default:
		return rpc.Fail("upstream_error", message, rpc.WithStatus(http.StatusBadGateway))
	}
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
