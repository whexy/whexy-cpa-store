package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// zedLine is one newline-delimited object from POST /completions.
type zedLine struct {
	// Event is the native provider event, nil for a status line.
	Event []byte
	// Ended is the stream_ended status.
	Ended bool
	// Failure is a status.failed line.
	Failure *zedFailure
}

// parseZedLine decodes one line. Both the wrapped form (status messages on)
// and the bare form (status messages off) are accepted.
func parseZedLine(line []byte) (zedLine, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return zedLine{}, nil
	}
	if prefix, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		line = bytes.TrimSpace(prefix)
	}
	if len(line) == 0 || line[0] != '{' {
		return zedLine{}, nil
	}
	root := gjson.ParseBytes(line)
	if !root.IsObject() {
		return zedLine{}, nil
	}
	if root.Get("status").IsObject() {
		failed := root.Get("status.failed")
		if failed.IsObject() {
			message := failed.Get("message").String()
			if message == "" {
				message = failed.Get("error").String()
			}
			if message == "" {
				message = firstLine(failed.Raw)
			}
			status := int(failed.Get("status").Int())
			if status == 0 {
				status = statusFromCode(failed.Get("code").String())
				if status == 0 {
					status = 502
				}
			}
			return zedLine{Failure: &zedFailure{Status: status, Message: message}}, nil
		}
	}
	if root.Get("status").Type == gjson.String && root.Get("status").String() == "stream_ended" {
		return zedLine{Ended: true}, nil
	}
	// A status line with another state (queued/started) carries nothing to
	// forward.
	if event := root.Get("event"); event.Exists() {
		return zedLine{Event: []byte(event.Raw)}, nil
	}
	if root.Get("status").Exists() && !root.Get("type").Exists() && !root.Get("candidates").Exists() && !root.Get("choices").Exists() {
		return zedLine{}, nil
	}
	return zedLine{Event: line}, nil
}

// statusFromCode reads the numeric status out of Zed's upstream_http_<n>
// failure codes.
func statusFromCode(code string) int {
	code = strings.TrimSpace(code)
	if index := strings.LastIndexByte(code, '_'); index >= 0 {
		code = code[index+1:]
	}
	value := 0
	for _, r := range code {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int(r-'0')
	}
	return value
}

// lineSplitter cuts a byte stream into newline-delimited objects. Zed's body
// is NDJSON but a proxy may deliver it in arbitrary chunks.
type lineSplitter struct {
	pending []byte
}

func (s *lineSplitter) push(data []byte) [][]byte {
	s.pending = append(s.pending, data...)
	var lines [][]byte
	for {
		index := bytes.IndexByte(s.pending, '\n')
		if index < 0 {
			break
		}
		line := bytes.TrimSuffix(s.pending[:index], []byte("\r"))
		s.pending = s.pending[index+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, bytes.Clone(line))
		}
	}
	if len(s.pending) == 0 {
		s.pending = nil
	}
	return lines
}

func (s *lineSplitter) flush() [][]byte {
	if len(bytes.TrimSpace(s.pending)) == 0 {
		s.pending = nil
		return nil
	}
	line := bytes.TrimSpace(s.pending)
	s.pending = nil
	return [][]byte{line}
}

// streamConverter turns Zed's native events into the chunks the client's
// handler writes.
type streamConverter struct {
	ctx      context.Context
	plan     routePlan
	model    string
	original []byte
	request  []byte
	param    any
	done     bool
	usage    tokenUsage
}

type tokenUsage struct {
	seen      bool
	input     int64
	cached    int64
	output    int64
	reasoning int64
}

func newStreamConverter(ctx context.Context, plan routePlan, model string, original, request []byte) *streamConverter {
	return &streamConverter{ctx: ctx, plan: plan, model: model, original: original, request: request}
}

// convert returns the client chunks for one event.
func (c *streamConverter) convert(event []byte) ([][]byte, error) {
	if errObserve := c.observe(event); errObserve != nil {
		return nil, errObserve
	}
	if c.plan.ResponseFrom == "" {
		// Chat and Gemini handlers prepend the data: frame themselves; the
		// Messages and Responses handlers take whole SSE frames.
		if c.plan.Upstream == sdktranslator.FormatOpenAI || c.plan.Upstream == sdktranslator.FormatGemini {
			if bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
				return nil, nil
			}
			return [][]byte{event}, nil
		}
		return [][]byte{sseFrame(event)}, nil
	}
	line := append([]byte("data: "), event...)
	chunks := sdktranslator.TranslateStream(c.ctx, c.plan.ResponseFrom, c.plan.Response, c.model, c.original, c.request, line, &c.param)
	return chunks, nil
}

// sseFrame wraps a native event as a server-sent event.
func sseFrame(event []byte) []byte {
	event = bytes.TrimSpace(event)
	frame := []byte("data: ")
	if kind := gjson.GetBytes(event, "type").String(); kind != "" {
		frame = []byte("event: " + kind + "\ndata: ")
	}
	return append(append(frame, event...), '\n', '\n')
}

// observe tracks the terminal event and turns in-band errors into stream
// errors.
func (c *streamConverter) observe(event []byte) error {
	event = bytes.TrimSpace(event)
	switch c.plan.Upstream {
	case sdktranslator.FormatClaude:
		root := gjson.ParseBytes(event)
		if usage := root.Get("message.usage"); usage.IsObject() {
			c.observeClaudeUsage(usage)
		}
		if usage := root.Get("usage"); usage.IsObject() {
			c.observeClaudeUsage(usage)
		}
		switch root.Get("type").String() {
		case "message_stop":
			c.done = true
		case "error":
			c.done = true
			if c.plan.Response != sdktranslator.FormatClaude {
				return streamFailure(event)
			}
		}
	case sdktranslator.FormatOpenAIResponse:
		switch gjson.GetBytes(event, "type").String() {
		case "response.completed", "response.incomplete":
			c.done = true
			if usage := gjson.GetBytes(event, "response.usage"); usage.IsObject() {
				c.observeResponsesUsage(usage)
			}
		case "response.failed", "error":
			c.done = true
			if c.plan.Response != sdktranslator.FormatOpenAIResponse {
				return streamFailure(event)
			}
		}
	case sdktranslator.FormatGemini:
		root := gjson.ParseBytes(event)
		c.observeGeminiUsage(root.Get("usageMetadata"))
		if len(root.Get("candidates").Array()) > 0 {
			for _, candidate := range root.Get("candidates").Array() {
				if candidate.Get("finishReason").Exists() {
					c.done = true
				}
			}
		}
	case sdktranslator.FormatOpenAI:
		if bytes.Equal(event, []byte("[DONE]")) {
			c.done = true
			return nil
		}
		root := gjson.ParseBytes(event)
		if root.Get("error").Exists() && !root.Get("choices").Exists() {
			return streamFailure(event)
		}
		for _, choice := range root.Get("choices").Array() {
			if choice.Get("finish_reason").Type == gjson.String && choice.Get("finish_reason").String() != "" {
				c.done = true
			}
		}
		if usage := root.Get("usage"); usage.IsObject() {
			c.usage.seen = true
			c.usage.input = usage.Get("prompt_tokens").Int()
			c.usage.cached = usage.Get("prompt_tokens_details.cached_tokens").Int()
			c.usage.output = usage.Get("completion_tokens").Int()
			c.usage.reasoning = usage.Get("completion_tokens_details.reasoning_tokens").Int()
		}
	}
	return nil
}

func (c *streamConverter) observeClaudeUsage(usage gjson.Result) {
	if usage.Get("input_tokens").Exists() {
		c.usage.seen = true
		c.usage.cached = usage.Get("cache_read_input_tokens").Int()
		c.usage.input = usage.Get("input_tokens").Int() + usage.Get("cache_read_input_tokens").Int() + usage.Get("cache_creation_input_tokens").Int()
	}
	if usage.Get("output_tokens").Exists() {
		c.usage.seen = true
		c.usage.output = usage.Get("output_tokens").Int()
		c.usage.reasoning = usage.Get("output_tokens_details.thinking_tokens").Int()
	}
}

func (c *streamConverter) observeResponsesUsage(usage gjson.Result) {
	c.usage.seen = true
	c.usage.input = usage.Get("input_tokens").Int()
	c.usage.cached = usage.Get("input_tokens_details.cached_tokens").Int()
	c.usage.output = usage.Get("output_tokens").Int()
	c.usage.reasoning = usage.Get("output_tokens_details.reasoning_tokens").Int()
}

func (c *streamConverter) observeGeminiUsage(usage gjson.Result) {
	if !usage.IsObject() {
		return
	}
	c.usage.seen = true
	c.usage.input = usage.Get("promptTokenCount").Int()
	c.usage.cached = usage.Get("cachedContentTokenCount").Int()
	c.usage.output = usage.Get("candidatesTokenCount").Int()
	c.usage.reasoning = usage.Get("thoughtsTokenCount").Int()
}

// finish is called when the body or stream_ended ends the response.
func (c *streamConverter) finish() ([][]byte, error) {
	if !c.done {
		if c.plan.Upstream != sdktranslator.FormatOpenAI {
			return nil, errors.New("Zed closed the stream before the response completed")
		}
		c.done = true
		if c.plan.ResponseFrom != "" {
			return sdktranslator.TranslateStream(c.ctx, c.plan.ResponseFrom, c.plan.Response, c.model, c.original, c.request, []byte("data: [DONE]"), &c.param), nil
		}
	}
	return nil, nil
}

// streamFailure turns an in-band error event into a failure.
func streamFailure(event []byte) error {
	root := gjson.ParseBytes(event)
	message := root.Get("error.message").String()
	if message == "" {
		message = root.Get("error").String()
	}
	if message == "" {
		message = root.Get("response.error.message").String()
	}
	if message == "" {
		message = firstLine(string(event))
	}
	status := int(root.Get("error.status").Int())
	if status == 0 {
		status = 502
	}
	return &zedFailure{Status: status, Message: message}
}

// assembleNative folds a native event stream into one response object, for
// clients that asked for a non-streaming call.
func assembleNative(upstream sdktranslator.Format, events [][]byte) ([]byte, error) {
	switch upstream {
	case sdktranslator.FormatClaude:
		return assembleClaude(events)
	case sdktranslator.FormatOpenAIResponse:
		return assembleResponses(events)
	case sdktranslator.FormatGemini:
		return assembleGemini(events)
	case sdktranslator.FormatOpenAI:
		return assembleChat(events)
	}
	return nil, errors.New("the model names no upstream protocol")
}

func assembleClaude(events [][]byte) ([]byte, error) {
	var message map[string]any
	blocks := map[int64]map[string]any{}
	var order []int64
	for _, event := range events {
		root := gjson.ParseBytes(event)
		switch root.Get("type").String() {
		case "message_start":
			message = gjson.Parse(root.Get("message").Raw).Value().(map[string]any)
		case "content_block_start":
			index := root.Get("index").Int()
			block := root.Get("content_block").Value().(map[string]any)
			if _, ok := blocks[index]; !ok {
				order = append(order, index)
			}
			blocks[index] = block
		case "content_block_delta":
			index := root.Get("index").Int()
			block, ok := blocks[index]
			if !ok {
				block = map[string]any{"type": "text", "text": ""}
				blocks[index] = block
				order = append(order, index)
			}
			delta := root.Get("delta")
			switch delta.Get("type").String() {
			case "text_delta":
				block["text"] = stringValue(block["text"]) + delta.Get("text").String()
			case "thinking_delta":
				block["thinking"] = stringValue(block["thinking"]) + delta.Get("thinking").String()
			case "signature_delta":
				block["signature"] = delta.Get("signature").String()
			case "input_json_delta":
				block["_partial"] = stringValue(block["_partial"]) + delta.Get("partial_json").String()
			}
		case "message_delta":
			if message == nil {
				message = map[string]any{}
			}
			message["stop_reason"] = root.Get("delta.stop_reason").Value()
			message["stop_sequence"] = root.Get("delta.stop_sequence").Value()
			if usage := root.Get("usage"); usage.IsObject() {
				current, _ := message["usage"].(map[string]any)
				if current == nil {
					current = map[string]any{}
				}
				for key, value := range usage.Map() {
					current[key] = value.Value()
				}
				message["usage"] = current
			}
		}
	}
	if message == nil {
		return nil, errors.New("Zed sent no Claude message_start")
	}
	content := make([]any, 0, len(order))
	for _, index := range order {
		block := blocks[index]
		if partial, ok := block["_partial"].(string); ok {
			delete(block, "_partial")
			var input any = map[string]any{}
			if partial != "" {
				_ = json.Unmarshal([]byte(partial), &input)
			}
			block["input"] = input
		}
		content = append(content, block)
	}
	message["content"] = content
	return json.Marshal(message)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func assembleResponses(events [][]byte) ([]byte, error) {
	var failed error
	for _, event := range events {
		root := gjson.ParseBytes(event)
		switch root.Get("type").String() {
		case "response.completed", "response.incomplete":
			return []byte(root.Get("response").Raw), nil
		case "response.failed":
			failed = streamFailure(event)
		}
	}
	if failed != nil {
		return nil, failed
	}
	return nil, errors.New("Zed sent no completed Responses object")
}

func assembleGemini(events [][]byte) ([]byte, error) {
	merged := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{}}}}}
	parts := []any{}
	var usage map[string]any
	finish := ""
	for _, event := range events {
		root := gjson.ParseBytes(event)
		for _, candidate := range root.Get("candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				parts = append(parts, part.Value())
			}
			if reason := candidate.Get("finishReason").String(); reason != "" {
				finish = reason
			}
		}
		if value := root.Get("usageMetadata"); value.IsObject() {
			usage = value.Value().(map[string]any)
		}
	}
	candidates := merged["candidates"].([]any)
	candidate := candidates[0].(map[string]any)
	content := candidate["content"].(map[string]any)
	content["parts"] = parts
	if finish != "" {
		candidate["finishReason"] = finish
	}
	if usage != nil {
		merged["usageMetadata"] = usage
	}
	return json.Marshal(merged)
}

func assembleChat(events [][]byte) ([]byte, error) {
	var content strings.Builder
	var reasoning strings.Builder
	finish := ""
	var usage map[string]any
	for _, event := range events {
		if bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
			continue
		}
		root := gjson.ParseBytes(event)
		if usageValue := root.Get("usage"); usageValue.IsObject() {
			usage = usageValue.Value().(map[string]any)
		}
		for _, choice := range root.Get("choices").Array() {
			content.WriteString(choice.Get("delta.content").String())
			reasoning.WriteString(choice.Get("delta.reasoning_content").String())
			if reason := choice.Get("finish_reason").String(); reason != "" {
				finish = reason
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	response := map[string]any{"object": "chat.completion", "choices": []any{choice}}
	if usage != nil {
		response["usage"] = usage
	}
	return json.Marshal(response)
}
