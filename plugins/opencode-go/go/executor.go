package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"

	// Registers CPA's protocol translators with sdk/translator for in-process
	// conversion between client formats and each model's native protocol.
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

// rpcExecutorRequest is the wire shape of executor.execute, execute_stream
// and count_tokens. StreamID is set for execute_stream only.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse answers execute_stream. Returning headers
// without chunks keeps the stream open; chunks then go through
// host.EmitStream and the stream ends with host.CloseStream.
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

// executorCall is one inference request, resolved and ready to send.
type executorCall struct {
	req   rpcExecutorRequest
	host  host.Client
	cfg   settings
	cred  credential
	route modelRoute
	plan  routePlan
	// cloaked says the request is presented as the OpenCode client because the
	// model is a free one of the pay-as-you-go gateway.
	cloaked bool
	// filter undoes whatever the cloak did to the request's tool list in the
	// reply (cloakFilter); it is inert unless the request was cloaked.
	filter cloakFilter
	// payload is the client's request in plan.Source; response translators
	// read tool names and options from it.
	payload []byte
	body    []byte
	headers http.Header
}

func executorExecute(raw []byte) ([]byte, error) {
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

// executorCountTokens declines token counting: the Go gateway has no
// endpoint for it. CPA records a count_tokens 404 that does not name the
// model as availability-neutral; any other error status would also cool the
// model down for real requests.
func executorCountTokens(raw []byte) ([]byte, error) {
	return rpc.Fail("unsupported", "OpenCode Go has no token counting endpoint", rpc.WithStatus(http.StatusNotFound)), nil
}

// executorHTTPRequest declines raw requests: CPA uses it only for Codex
// auths, and signing arbitrary URLs with the key would leak it.
func executorHTTPRequest(raw []byte) ([]byte, error) {
	return rpc.Fail("unsupported", "OpenCode Go does not proxy raw HTTP requests", rpc.WithStatus(http.StatusNotImplemented)), nil
}

// prepareExecutorCall resolves the key, model and route and builds the
// upstream request; a non-nil []byte is the error envelope to return.
func prepareExecutorCall(ctx context.Context, raw []byte, stream bool) (*executorCall, []byte) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, rpc.Fail("invalid_request", "decode executor request: "+errUnmarshal.Error(), rpc.WithStatus(http.StatusBadRequest))
	}
	h := host.New(req.HostCallbackID)
	cfg := currentSettings()
	cred, errCred := resolveCredential(req.StorageJSON, req.AuthAttributes, req.AuthMetadata)
	if errCred != nil {
		return nil, rpc.Fail("invalid_auth", "OpenCode Go auth "+req.AuthID+": "+errCred.Error(), rpc.WithStatus(http.StatusUnauthorized))
	}
	route, found := resolveRoute(ctx, h, cfg, req.Model, cred, authFamily(req.AuthAttributes))
	if !found {
		return nil, rpc.Fail("model_not_found", fmt.Sprintf("model %s is not in the OpenCode catalog", req.Model), rpc.WithStatus(http.StatusNotFound))
	}
	// Only Go models draw on the subscription quota, so only they are held
	// back while that quota is exhausted; Zen models bill Console credits.
	if route.Family == familyGo && !route.Free {
		if block, blocked := exhaustion.active(cred.ID, req.AuthID); blocked {
			return nil, rpc.Fail("usage_limit", block.message(), rpc.WithStatus(http.StatusTooManyRequests), rpc.WithRetryAfter(block.retryAfter(exhaustion.now())))
		}
	}
	source := normalizeFormat(req.SourceFormat)
	response := source
	if strings.TrimSpace(req.Format) != "" {
		response = normalizeFormat(req.Format)
	}
	plan, errPlan := planRoute(source, response, route.Protocol)
	if errPlan != nil {
		return nil, rpc.Fail("unsupported_format", errPlan.Error(), rpc.WithStatus(http.StatusBadRequest))
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	body, errBody := buildUpstreamBody(ctx, plan, payload, route.UpstreamID, stream)
	if errBody != nil {
		return nil, rpc.Fail("invalid_request", fmt.Sprintf("build %s request for %s: %v", route.Protocol, route.UpstreamID, errBody), rpc.WithStatus(http.StatusBadRequest))
	}
	session := sessionID(req.Metadata, req.Headers, source, payload)
	// A free pay-as-you-go model is served only to the OpenCode client: the
	// request gets that identity, the streaming body its gate expects and a
	// session identifier of the shape it accepts. A caller that already is
	// OpenCode keeps its own identity and session.
	filter := newCloakFilter(route.Protocol, cloakTools{})
	cloaked := cfg.CloakFreeModels && route.Family != familyGo && route.Free
	impersonate := false
	if cloaked {
		var tools cloakTools
		body, tools, errBody = cloakBody(route.Protocol, body)
		if errBody != nil {
			return nil, rpc.Fail("invalid_request", fmt.Sprintf("prepare the free %s request for %s: %v", route.Protocol, route.UpstreamID, errBody), rpc.WithStatus(http.StatusBadRequest))
		}
		filter = newCloakFilter(route.Protocol, tools)
		if nativeSession, native := nativeOpenCodeSession(req.Headers); native {
			session = nativeSession
		} else {
			impersonate = true
			session = cloakSessions.id(session)
		}
	}
	return &executorCall{
		req:     req,
		host:    h,
		cfg:     cfg,
		cred:    cred,
		route:   route,
		plan:    plan,
		cloaked: cloaked,
		filter:  filter,
		payload: payload,
		body:    body,
		headers: inferenceHeaders(cfg, cred.Bearer, route.Protocol, req.Headers, stream, session, impersonate),
	}, nil
}

// resolveRoute finds the requested model among the auth family's models in
// the credential's catalog, refreshing a catalog older than its TTL once on
// a miss so a newly added Go model works before the host next asks for the
// model list. A refresh that fails in part still counts: models.dev and the
// workspace config are read separately, and either may have changed.
func resolveRoute(ctx context.Context, h host.Client, cfg settings, model string, cred credential, family modelFamily) (modelRoute, bool) {
	if route, ok := lookupModel(model, cred.ID, family); ok {
		return route, true
	}
	_, _, _ = catalogFor(ctx, h, cfg, cred, family)
	return lookupModel(model, cred.ID, family)
}

func (c *executorCall) upstreamRequest() pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: c.route.Endpoint, Headers: c.headers, Body: c.body}
}

func (c *executorCall) execute(ctx context.Context) []byte {
	resp, errDo := c.host.Do(ctx, c.upstreamRequest())
	if errDo != nil {
		return transportFailure(errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.upstreamFailure(ctx, resp.StatusCode, resp.Headers, resp.Body)
	}
	body := resp.Body
	// A cloaked request is answered with SSE even though the client asked for
	// JSON, so the stream is folded back into one response; and what the cloak
	// did to the request's tool list is undone in it before it is translated,
	// so no translator meets a call of a tool that is in no list it saw.
	if c.cloaked && isStreamBody(body) {
		assembled, errAssemble := assembleStream(c.route.Protocol, body)
		if errAssemble != nil {
			return rpc.Fail("translation_failed", errAssemble.Error(), rpc.WithStatus(http.StatusBadGateway))
		}
		body = assembled
	}
	if c.filter.active() {
		body = c.filter.buffered(body)
	}
	out, errConvert := convertResponse(ctx, c.plan, c.req.Model, c.payload, c.body, body)
	if errConvert != nil {
		return rpc.Fail("translation_failed", errConvert.Error(), rpc.WithStatus(http.StatusBadGateway))
	}
	raw, errEnvelope := rpc.OK(pluginapi.ExecutorResponse{Payload: out, Headers: diagnosticHeaders(resp.Headers)})
	if errEnvelope != nil {
		return rpc.Fail("plugin_error", errEnvelope.Error(), rpc.WithStatus(http.StatusInternalServerError))
	}
	return raw
}

// executeStream answers only once the upstream status is known, so an
// upstream error reaches CPA with its status (which drives retry and
// cooldown) instead of as a statusless stream error.
func (c *executorCall) executeStream(ctx context.Context) []byte {
	upstream, errStart := c.host.DoStream(ctx, c.upstreamRequest())
	if errStart != nil {
		return transportFailure(errStart)
	}
	if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
		body, _ := upstream.ReadAll()
		upstream.Close()
		return c.upstreamFailure(ctx, upstream.StatusCode, upstream.Headers, body)
	}
	converter := newStreamConverter(ctx, c.plan, c.req.Model, c.payload, c.body)
	// What the cloak did to the request's tool list is undone in the model's
	// own protocol, before the converter translates anything out of it.
	converter.filter = c.filter
	streamID := c.req.StreamID
	background.Go(func(context.Context) { pumpStream(upstream, converter, streamID) })
	raw, errEnvelope := rpc.OK(rpcExecutorStreamResponse{Headers: diagnosticHeaders(upstream.Headers)})
	if errEnvelope != nil {
		return rpc.Fail("plugin_error", errEnvelope.Error(), rpc.WithStatus(http.StatusInternalServerError))
	}
	return raw
}

// pumpStream relays the upstream body to the host stream until either side
// ends. It has no deadline of its own: the host cancels the upstream read
// when the client disconnects, and plugin shutdown cancels the context the
// upstream stream was opened with.
func pumpStream(upstream *host.Stream, converter *streamConverter, streamID string) {
	closed := false
	closeStream := func(message string) {
		if !closed {
			closed = true
			upstream.Close()
			host.CloseStream(streamID, message)
		}
	}
	defer closeStream("OpenCode Go stream ended unexpectedly")

	// emit reports false once the host stream is gone, which happens when
	// the client disconnects.
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
	var splitter sseSplitter
	for {
		payload, done, errRead := upstream.Read()
		frames := splitter.push(payload)
		if done && errRead == nil {
			frames = append(frames, splitter.flush()...)
		}
		for _, frame := range frames {
			chunks, errConvert := converter.convert(frame)
			if !emit(chunks) {
				return
			}
			if errConvert != nil {
				closeStream(errConvert.Error())
				return
			}
		}
		if errRead != nil {
			closeStream("OpenCode Go stream failed: " + errRead.Error())
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

func transportFailure(err error) []byte {
	if errors.Is(err, context.Canceled) {
		return rpc.Fail("canceled", "OpenCode Go request canceled", rpc.WithStatus(http.StatusRequestTimeout))
	}
	return rpc.Fail("upstream_unreachable", "OpenCode Go request failed: "+err.Error(), rpc.WithStatus(http.StatusBadGateway))
}

// upstreamFailure maps a gateway error to the status CPA acts on: 401, 402
// and 403 cool the auth down for 30 minutes, 404 the model for 12 hours,
// 429 backs off, 5xx is transient, other 4xx fail the request.
func (c *executorCall) upstreamFailure(ctx context.Context, status int, headers http.Header, body []byte) []byte {
	upstream := parseUpstreamError(status, headers, body)
	message := upstream.Error()
	switch {
	case upstream.Type == upstreamUsageLimit:
		block := c.blockForUsageLimit(ctx)
		return rpc.Fail("usage_limit", block.message()+": "+message, rpc.WithStatus(http.StatusTooManyRequests), rpc.WithRetryAfter(block.retryAfter(exhaustion.now())))
	case upstream.Type == upstreamProtocolUnsupported:
		c.host.Log("error", "OpenCode Go rejected the endpoint the catalog names for a model", map[string]any{
			"model":    c.route.UpstreamID,
			"protocol": string(c.route.Protocol),
			"error":    message,
		})
		return rpc.Fail("protocol_unsupported", message, rpc.WithStatus(http.StatusBadRequest))
	case strings.Contains(strings.ToLower(upstream.Message), "model is unavailable"):
		return rpc.Fail("model_unavailable", message, rpc.WithStatus(http.StatusNotFound))
	case status >= 500:
		return rpc.Fail("upstream_error", message, rpc.WithStatus(http.StatusBadGateway))
	case status >= 400:
		return rpc.Fail("upstream_error", message, rpc.WithStatus(status))
	default:
		return rpc.Fail("upstream_error", message, rpc.WithStatus(http.StatusBadGateway))
	}
}

// blockForUsageLimit asks /v1/usage which window ran out and blocks the
// credential until it resets.
func (c *executorCall) blockForUsageLimit(ctx context.Context) usageBlock {
	report, errUsage := fetchUsage(ctx, c.host, c.cfg, c.cred.Bearer)
	until, windows := blockFromUsage(report, errUsage, exhaustion.now())
	block := exhaustion.block(c.cred.ID, c.req.AuthID, until, windows)
	fields := map[string]any{"auth_id": c.req.AuthID, "until": block.Until.UTC().Format(time.RFC3339)}
	if errUsage != nil {
		fields["usage_error"] = errUsage.Error()
	}
	c.host.Log("info", "OpenCode Go usage limit reached; blocking paid models on this credential", fields)
	return block
}

// diagnosticHeaders keeps the gateway's identification headers. Others are
// dropped because the body may have been translated, so length and type no
// longer apply.
func diagnosticHeaders(upstream http.Header) http.Header {
	out := http.Header{}
	for name, values := range upstream {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-opencode-") || lower == "x-zen-model" {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}
