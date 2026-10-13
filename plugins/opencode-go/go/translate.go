package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// routePlan says how one request travels between the client's format and
// the model's native protocol.
type routePlan struct {
	// Source is the format of the client's request body.
	Source sdktranslator.Format
	// Response is the format the host expects the response in.
	Response sdktranslator.Format
	// Upstream is the model's native protocol.
	Upstream sdktranslator.Format
	// RequestTo is the translator target for the request body, empty when
	// the body is forwarded with only the model rewritten.
	RequestTo sdktranslator.Format
	// ResponseFrom is the translator source for the response, empty when
	// the upstream response is already in the Response format.
	ResponseFrom sdktranslator.Format
}

func normalizeFormat(name string) sdktranslator.Format {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "openai", "chat-completions", "chat_completions":
		return sdktranslator.FormatOpenAI
	case "openai-response", "openai-responses", "responses":
		return sdktranslator.FormatOpenAIResponse
	case "claude", "anthropic", "messages":
		return sdktranslator.FormatClaude
	default:
		return sdktranslator.Format(strings.ToLower(strings.TrimSpace(name)))
	}
}

// translatorSide names the format CPA's translators use for an upstream
// protocol. CPA registers no pair into openai-response; Responses upstreams
// are reached through the Codex pair, which speaks the Responses wire.
func translatorSide(upstream sdktranslator.Format) sdktranslator.Format {
	if upstream == sdktranslator.FormatOpenAIResponse {
		return sdktranslator.FormatCodex
	}
	return upstream
}

func planRoute(source, response, upstream sdktranslator.Format) (routePlan, error) {
	plan := routePlan{Source: source, Response: response, Upstream: upstream}
	if source != upstream {
		plan.RequestTo = translatorSide(upstream)
		if !sdktranslator.HasRequestTransformer(source, plan.RequestTo) {
			return routePlan{}, fmt.Errorf("no translator from %s requests to %s", source, upstream)
		}
	}
	if response != upstream {
		plan.ResponseFrom = translatorSide(upstream)
		if !sdktranslator.HasStreamResponseTransformer(response, plan.ResponseFrom) || !sdktranslator.HasNonStreamResponseTransformer(response, plan.ResponseFrom) {
			return routePlan{}, fmt.Errorf("no translator from %s responses to %s", upstream, response)
		}
	}
	return plan, nil
}

// buildUpstreamBody turns the client's request into the model's native
// request with the bare Go model ID.
func buildUpstreamBody(ctx context.Context, plan routePlan, payload []byte, upstreamModel string, stream bool) ([]byte, error) {
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return nil, errors.New("request body is not a JSON object")
	}
	body := payload
	if plan.RequestTo != "" {
		translated := sdktranslator.TranslateRequestEnvelope(ctx, plan.Source, plan.RequestTo, sdktranslator.RequestEnvelope{
			Format: plan.Source,
			Model:  upstreamModel,
			Stream: stream,
			Body:   payload,
		})
		if translated.Err != nil {
			return nil, translated.Err
		}
		body = translated.Body
		if plan.RequestTo == sdktranslator.FormatCodex {
			body = adaptCodexBody(body, payload, plan.Source)
		}
	}
	body, errSet := setJSON(body, "model", upstreamModel)
	if errSet != nil {
		return nil, errSet
	}
	return setStream(body, stream)
}

// minResponsesOutputTokens is the lowest max_output_tokens Go's Responses
// models accept; Chat and Messages clients go lower, as Claude Code's
// max_tokens 1 probe does.
const minResponsesOutputTokens = 16

// adaptCodexBody undoes Codex translator defaults written for the ChatGPT
// backend: it drops the client's max tokens and always asks for medium
// reasoning and parallel tool calls, whether or not the client did.
func adaptCodexBody(body, source []byte, sourceFormat sdktranslator.Format) []byte {
	root := gjson.ParseBytes(source)
	if limit := sourceMaxTokens(root, sourceFormat); limit > 0 {
		body, _ = sjson.SetBytes(body, "max_output_tokens", max(limit, minResponsesOutputTokens))
	}
	if !sourceAsksReasoning(root, sourceFormat) {
		body, _ = sjson.DeleteBytes(body, "reasoning.effort")
		if reasoning := gjson.GetBytes(body, "reasoning"); reasoning.IsObject() && len(reasoning.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "reasoning")
		}
	}
	if tools := gjson.GetBytes(body, "tools"); !tools.IsArray() || len(tools.Array()) == 0 {
		body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	} else if parallel := root.Get("parallel_tool_calls"); sourceFormat == sdktranslator.FormatOpenAI && (parallel.Type == gjson.True || parallel.Type == gjson.False) {
		body, _ = sjson.SetBytes(body, "parallel_tool_calls", parallel.Bool())
	}
	return body
}

func sourceMaxTokens(root gjson.Result, format sdktranslator.Format) int64 {
	if limit := root.Get("max_completion_tokens"); format == sdktranslator.FormatOpenAI && limit.Exists() {
		return limit.Int()
	}
	return root.Get("max_tokens").Int()
}

// sourceAsksReasoning reports whether the client chose a reasoning level;
// without one the model's own default applies, and models that take no
// effort parameter are not sent one.
func sourceAsksReasoning(root gjson.Result, format sdktranslator.Format) bool {
	if format == sdktranslator.FormatClaude {
		return root.Get("thinking").IsObject() || root.Get("output_config.effort").Exists()
	}
	return root.Get("reasoning_effort").Exists()
}

func setJSON(body []byte, path string, value any) ([]byte, error) {
	if current := gjson.GetBytes(body, path); current.Exists() && current.Value() == value {
		return body, nil
	}
	updated, errSet := sjson.SetBytes(body, path, value)
	if errSet != nil {
		return nil, fmt.Errorf("set %s: %w", path, errSet)
	}
	return updated, nil
}

// setStream makes the body's stream flag match the call: the host decides
// streaming by method, and the Claude-to-Codex translator always sets it.
func setStream(body []byte, stream bool) ([]byte, error) {
	current := gjson.GetBytes(body, "stream")
	if current.Exists() && current.Bool() == stream {
		return body, nil
	}
	if !current.Exists() && !stream {
		return body, nil
	}
	return setJSON(body, "stream", stream)
}

// convertResponse turns a buffered upstream response into the host's
// response format.
func convertResponse(ctx context.Context, plan routePlan, model string, original, request, body []byte) ([]byte, error) {
	if plan.ResponseFrom == "" {
		return body, nil
	}
	if plan.ResponseFrom == sdktranslator.FormatCodex {
		event, errEvent := codexTerminalEvent(body)
		if errEvent != nil {
			return nil, errEvent
		}
		body = event
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, plan.ResponseFrom, plan.Response, model, original, request, body, &param)
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("translating the %s response to %s produced nothing", plan.Upstream, plan.Response)
	}
	return out, nil
}

// codexTerminalEvent wraps a buffered Responses object in the terminal
// stream event the Codex translators read, since the Codex backend they were
// written for only streams.
func codexTerminalEvent(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("OpenCode Go returned a Responses body that is not JSON")
	}
	eventType := "response.completed"
	switch gjson.GetBytes(body, "status").String() {
	case "incomplete":
		eventType = "response.incomplete"
	case "failed":
		return nil, streamFailure(body, "")
	}
	event, errSet := sjson.SetRawBytes([]byte(`{"type":""}`), "response", body)
	if errSet != nil {
		return nil, errSet
	}
	return sjson.SetBytes(event, "type", eventType)
}

// sseFrame is one server-sent event.
type sseFrame struct {
	Event string
	// Data joins the frame's data lines; nil when it has none.
	Data []byte
	// Raw is the frame as received, ending in a blank line.
	Raw []byte
}

// sseSplitter cuts a byte stream into SSE frames; host stream reads are not
// aligned to lines.
type sseSplitter struct {
	pending []byte
	lines   [][]byte
}

func (s *sseSplitter) push(data []byte) []sseFrame {
	s.pending = append(s.pending, data...)
	var frames []sseFrame
	for {
		end := bytes.IndexByte(s.pending, '\n')
		if end < 0 {
			break
		}
		line := bytes.TrimSuffix(s.pending[:end], []byte("\r"))
		s.pending = s.pending[end+1:]
		if len(line) == 0 {
			if frame, ok := s.frame(); ok {
				frames = append(frames, frame)
			}
			continue
		}
		s.lines = append(s.lines, bytes.Clone(line))
	}
	if len(s.pending) == 0 {
		s.pending = nil
	}
	return frames
}

// flush returns the frame left open when the body ended without a blank line.
func (s *sseSplitter) flush() []sseFrame {
	if len(s.pending) > 0 {
		s.lines = append(s.lines, bytes.TrimSuffix(s.pending, []byte("\r")))
		s.pending = nil
	}
	if frame, ok := s.frame(); ok {
		return []sseFrame{frame}
	}
	return nil
}

func (s *sseSplitter) frame() (sseFrame, bool) {
	if len(s.lines) == 0 {
		return sseFrame{}, false
	}
	var frame sseFrame
	var raw bytes.Buffer
	var data [][]byte
	for _, line := range s.lines {
		raw.Write(line)
		raw.WriteByte('\n')
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		} else if value, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			frame.Event = strings.TrimSpace(string(value))
		}
	}
	raw.WriteByte('\n')
	frame.Raw = raw.Bytes()
	if data != nil {
		frame.Data = bytes.Join(data, []byte("\n"))
		if frame.Data == nil {
			frame.Data = []byte{}
		}
	}
	s.lines = nil
	return frame, true
}

// streamConverter turns upstream SSE frames into the chunks the host's
// handler for the response format writes: bare JSON for Chat Completions,
// whole SSE frames for Messages and Responses.
type streamConverter struct {
	ctx      context.Context
	plan     routePlan
	model    string
	original []byte
	request  []byte
	param    any
	// done is set once the upstream sent its terminal event.
	done bool
	// stopReason is the upstream's finish_reason or stop_reason.
	stopReason string
	usage      tokenUsage
	responses  responsesTrail
}

// tokenUsage is the upstream's token accounting, kept to complete a
// Responses terminal event the translator left out.
type tokenUsage struct {
	seen                                  bool
	input, cached, output, reasoning, all int64
}

func newStreamConverter(ctx context.Context, plan routePlan, model string, original, request []byte) *streamConverter {
	return &streamConverter{ctx: ctx, plan: plan, model: model, original: original, request: request}
}

// convert returns the client chunks for one upstream frame, or the error
// that ends the stream.
func (c *streamConverter) convert(frame sseFrame) ([][]byte, error) {
	if frame.Data == nil {
		if text := bytes.TrimSpace(frame.Raw); len(text) > 0 && text[0] == '{' {
			return nil, streamFailure(text, "")
		}
		return nil, nil
	}
	data := bytes.TrimSpace(frame.Data)
	if len(data) == 0 {
		return nil, nil
	}
	if errObserve := c.observe(data); errObserve != nil {
		return nil, errObserve
	}
	if c.plan.ResponseFrom == "" {
		if c.plan.Upstream != sdktranslator.FormatOpenAI {
			return [][]byte{frame.Raw}, nil
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return nil, nil
		}
		return [][]byte{data}, nil
	}
	return c.translate(data), nil
}

func (c *streamConverter) translate(data []byte) [][]byte {
	line := append([]byte("data: "), data...)
	chunks := sdktranslator.TranslateStream(c.ctx, c.plan.ResponseFrom, c.plan.Response, c.model, c.original, c.request, line, &c.param)
	if c.plan.Response == sdktranslator.FormatOpenAIResponse {
		for _, chunk := range chunks {
			c.responses.observe(chunk)
		}
	}
	return chunks
}

// observe tracks the upstream's terminal event and turns error events into
// stream errors. An error event already in the client's own protocol is
// passed on instead, since the client understands it.
func (c *streamConverter) observe(data []byte) error {
	switch c.plan.Upstream {
	case sdktranslator.FormatOpenAI:
		if bytes.Equal(data, []byte("[DONE]")) {
			c.done = true
			return nil
		}
		root := gjson.ParseBytes(data)
		if root.Get("error").Exists() && !root.Get("choices").Exists() {
			return streamFailure(data, "")
		}
		for _, choice := range root.Get("choices").Array() {
			if reason := choice.Get("finish_reason"); reason.Type == gjson.String && reason.String() != "" {
				c.stopReason = reason.String()
			}
		}
		if usage := root.Get("usage"); usage.IsObject() {
			c.usage = tokenUsage{
				seen:      true,
				input:     usage.Get("prompt_tokens").Int(),
				cached:    usage.Get("prompt_tokens_details.cached_tokens").Int(),
				output:    usage.Get("completion_tokens").Int(),
				reasoning: usage.Get("completion_tokens_details.reasoning_tokens").Int(),
				all:       usage.Get("total_tokens").Int(),
			}
		}
	case sdktranslator.FormatClaude:
		root := gjson.ParseBytes(data)
		if usage := root.Get("message.usage"); usage.IsObject() {
			c.observeClaudeUsage(usage)
		}
		if usage := root.Get("usage"); usage.IsObject() {
			c.observeClaudeUsage(usage)
		}
		switch root.Get("type").String() {
		case "message_delta":
			if reason := root.Get("delta.stop_reason").String(); reason != "" {
				c.stopReason = reason
			}
		case "message_stop":
			c.done = true
		case "error":
			c.done = true
			if c.plan.Response != sdktranslator.FormatClaude {
				return streamFailure(data, "")
			}
		}
	case sdktranslator.FormatOpenAIResponse:
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed", "response.incomplete":
			c.done = true
		case "response.failed":
			c.done = true
			if c.plan.Response != sdktranslator.FormatOpenAIResponse {
				return streamFailure(data, "response")
			}
		case "error":
			c.done = true
			if c.plan.Response != sdktranslator.FormatOpenAIResponse {
				return streamFailure(data, "")
			}
		}
	}
	return nil
}

// observeClaudeUsage merges Messages usage, which arrives split between
// message_start and message_delta. Messages input_tokens excludes cache
// reads and writes; Responses input_tokens includes them.
func (c *streamConverter) observeClaudeUsage(usage gjson.Result) {
	c.usage.seen = true
	if usage.Get("input_tokens").Exists() {
		c.usage.input = usage.Get("input_tokens").Int() + usage.Get("cache_read_input_tokens").Int() + usage.Get("cache_creation_input_tokens").Int()
		c.usage.cached = usage.Get("cache_read_input_tokens").Int()
	}
	if usage.Get("output_tokens").Exists() {
		c.usage.output = usage.Get("output_tokens").Int()
		c.usage.reasoning = usage.Get("output_tokens_details.thinking_tokens").Int()
	}
	c.usage.all = c.usage.input + c.usage.output
}

// finish is called at the end of the upstream body. Chat Completions
// providers may close after the finish chunk without [DONE]; any other
// early end is a truncated response.
func (c *streamConverter) finish() ([][]byte, error) {
	var out [][]byte
	if !c.done {
		if c.plan.Upstream != sdktranslator.FormatOpenAI || c.stopReason == "" {
			return nil, errors.New("OpenCode Go closed the stream before the response completed")
		}
		c.done = true
		if c.plan.ResponseFrom != "" {
			out = c.translate([]byte("[DONE]"))
		}
	}
	if c.plan.ResponseFrom != "" && c.plan.Response == sdktranslator.FormatOpenAIResponse {
		if event := c.responses.terminalEvent(c.stopReason, c.usage); event != nil {
			out = append(out, event)
		}
	}
	return out, nil
}

// responsesTrail records what a translator sent a Responses client. CPA's
// Chat-to-Responses translator (v8.0.27) emits no terminal event for a
// response that produced only reasoning, which CPA's Responses handler then
// reports as a broken stream; the trail lets finish supply the event.
type responsesTrail struct {
	created  []byte
	items    map[int64][]byte
	sequence int64
	terminal bool
}

func (r *responsesTrail) observe(chunk []byte) {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		event := gjson.ParseBytes(bytes.TrimSpace(data))
		r.sequence = max(r.sequence, event.Get("sequence_number").Int())
		switch event.Get("type").String() {
		case "response.created":
			r.created = []byte(event.Get("response").Raw)
		case "response.output_item.done":
			if r.items == nil {
				r.items = make(map[int64][]byte)
			}
			r.items[event.Get("output_index").Int()] = []byte(event.Get("item").Raw)
		case "response.completed", "response.incomplete", "response.failed":
			r.terminal = true
		}
	}
}

// terminalEvent builds the missing terminal event from the response the
// translator announced, the items it finished and the upstream's usage; a
// response cut off by the token limit ends incomplete, as OpenAI reports it.
func (r *responsesTrail) terminalEvent(stopReason string, usage tokenUsage) []byte {
	if r.terminal || len(r.created) == 0 {
		return nil
	}
	r.terminal = true
	eventType, status, details := "response.completed", "completed", "null"
	if stopReason == "length" || stopReason == "max_tokens" {
		eventType, status, details = "response.incomplete", "incomplete", `{"reason":"max_output_tokens"}`
	}
	indexes := make([]int64, 0, len(r.items))
	for index := range r.items {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	items := make([][]byte, 0, len(indexes))
	for _, index := range indexes {
		items = append(items, r.items[index])
	}
	response := r.created
	response, _ = sjson.SetBytes(response, "status", status)
	response, _ = sjson.SetRawBytes(response, "incomplete_details", []byte(details))
	response, _ = sjson.SetRawBytes(response, "output", append(append([]byte("["), bytes.Join(items, []byte(","))...), ']'))
	if usage.seen {
		response, _ = sjson.SetBytes(response, "usage", map[string]any{
			"input_tokens":          usage.input,
			"input_tokens_details":  map[string]int64{"cached_tokens": usage.cached},
			"output_tokens":         usage.output,
			"output_tokens_details": map[string]int64{"reasoning_tokens": usage.reasoning},
			"total_tokens":          max(usage.all, usage.input+usage.output),
		})
	}
	event, _ := sjson.SetBytes([]byte(`{"type":""}`), "type", eventType)
	event, _ = sjson.SetBytes(event, "sequence_number", r.sequence+1)
	event, _ = sjson.SetRawBytes(event, "response", response)
	return []byte("event: " + eventType + "\ndata: " + string(event) + "\n\n")
}

// streamFailure reads an error event or error body. Under prefix the error
// object is nested, as in a Responses response.failed event.
func streamFailure(data []byte, prefix string) error {
	root := gjson.ParseBytes(data)
	if prefix != "" {
		root = root.Get(prefix)
	}
	errType := root.Get("error.type").String()
	if errType == "" {
		errType = root.Get("error.code").String()
	}
	message := root.Get("error.message").String()
	if message == "" {
		message = root.Get("message").String()
	}
	if message == "" {
		message = strings.TrimSpace(string(data))
		if len(message) > 512 {
			message = message[:512] + "…"
		}
	}
	text := "OpenCode Go stream error"
	if errType != "" {
		text += " " + errType
	}
	return errors.New(redactSecrets(text + ": " + message))
}
