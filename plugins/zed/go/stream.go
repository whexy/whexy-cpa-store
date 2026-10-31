package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Gemini's translators read bare JSON and bare [DONE]; the others take a
	// data: frame.
	line := event
	if c.plan.Upstream != sdktranslator.FormatGemini {
		line = append([]byte("data: "), event...)
	}
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
		switch gjson.GetBytes(event, "type").String() {
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
		case "response.failed", "error":
			c.done = true
			if c.plan.Response != sdktranslator.FormatOpenAIResponse {
				return streamFailure(event)
			}
		}
	case sdktranslator.FormatGemini:
		for _, candidate := range gjson.GetBytes(event, "candidates").Array() {
			if candidate.Get("finishReason").Exists() {
				c.done = true
			}
		}
	case sdktranslator.FormatOpenAI:
		if bytes.Equal(event, []byte("[DONE]")) {
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
	}
	return nil
}

// finish is called when the body or stream_ended ends the response.
func (c *streamConverter) finish() ([][]byte, error) {
	if !c.done {
		return nil, errors.New("Zed closed the stream before the response completed")
	}
	if c.plan.ResponseFrom == "" {
		return nil, nil
	}
	switch c.plan.Upstream {
	case sdktranslator.FormatGemini:
		// Gemini's translators emit message_stop only on the bare marker.
		return sdktranslator.TranslateStream(c.ctx, c.plan.ResponseFrom, c.plan.Response, c.model, c.original, c.request, []byte("[DONE]"), &c.param), nil
	case sdktranslator.FormatOpenAI:
		return sdktranslator.TranslateStream(c.ctx, c.plan.ResponseFrom, c.plan.Response, c.model, c.original, c.request, []byte("data: [DONE]"), &c.param), nil
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
	sawStop := false
	for _, event := range events {
		root := gjson.ParseBytes(event)
		switch root.Get("type").String() {
		case "error":
			return nil, streamFailure(event)
		case "message_start":
			if value, ok := gjson.Parse(root.Get("message").Raw).Value().(map[string]any); ok {
				message = value
			}
		case "content_block_start":
			index := root.Get("index").Int()
			block, ok := root.Get("content_block").Value().(map[string]any)
			if !ok {
				return nil, errors.New("Zed sent a Claude content_block_start without a block")
			}
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
		case "message_stop":
			sawStop = true
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
	if !sawStop {
		return nil, errors.New("Zed closed the Claude response before message_stop")
	}
	content := make([]any, 0, len(order))
	for _, index := range order {
		block := blocks[index]
		if partial, ok := block["_partial"].(string); ok {
			delete(block, "_partial")
			var input any = map[string]any{}
			if partial != "" {
				if errUnmarshal := json.Unmarshal([]byte(partial), &input); errUnmarshal != nil {
					return nil, fmt.Errorf("Zed sent incomplete input for tool %q", stringValue(block["name"]))
				}
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
		if root.Get("error").Exists() {
			return nil, streamFailure(event)
		}
		for _, field := range []string{"responseId", "modelVersion"} {
			if value := root.Get(field).String(); value != "" {
				merged[field] = value
			}
		}
		for _, candidate := range root.Get("candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				parts = append(parts, part.Value())
			}
			if reason := candidate.Get("finishReason").String(); reason != "" {
				finish = reason
			}
		}
		if value := root.Get("usageMetadata"); value.IsObject() {
			if object, ok := value.Value().(map[string]any); ok {
				usage = object
			}
		}
	}
	candidates := merged["candidates"].([]any)
	candidate := candidates[0].(map[string]any)
	content := candidate["content"].(map[string]any)
	content["parts"] = parts
	if finish == "" {
		return nil, errors.New("Zed closed the Gemini response before a finishReason")
	}
	candidate["finishReason"] = finish
	if usage != nil {
		merged["usageMetadata"] = usage
	}
	return json.Marshal(merged)
}

func assembleChat(events [][]byte) ([]byte, error) {
	type choiceState struct {
		content   strings.Builder
		reasoning strings.Builder
		tools     map[int64]*chatToolCall
		order     []int64
		finish    string
	}
	choices := map[int64]*choiceState{}
	var order []int64
	var usage map[string]any
	for _, event := range events {
		if bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
			continue
		}
		root := gjson.ParseBytes(event)
		if root.Get("error").Exists() && !root.Get("choices").Exists() {
			return nil, streamFailure(event)
		}
		if usageValue := root.Get("usage"); usageValue.IsObject() {
			if object, ok := usageValue.Value().(map[string]any); ok {
				usage = object
			}
		}
		for _, choice := range root.Get("choices").Array() {
			index := choice.Get("index").Int()
			state, ok := choices[index]
			if !ok {
				state = &choiceState{tools: map[int64]*chatToolCall{}}
				choices[index] = state
				order = append(order, index)
			}
			state.content.WriteString(choice.Get("delta.content").String())
			state.reasoning.WriteString(choice.Get("delta.reasoning_content").String())
			for _, call := range choice.Get("delta.tool_calls").Array() {
				toolIndex := call.Get("index").Int()
				tool, okTool := state.tools[toolIndex]
				if !okTool {
					tool = &chatToolCall{}
					state.tools[toolIndex] = tool
					state.order = append(state.order, toolIndex)
				}
				if id := call.Get("id").String(); id != "" {
					tool.id = id
				}
				if name := call.Get("function.name").String(); name != "" {
					tool.name = name
				}
				tool.arguments.WriteString(call.Get("function.arguments").String())
			}
			if reason := choice.Get("finish_reason").String(); reason != "" {
				state.finish = reason
			}
		}
	}
	for _, index := range order {
		if choices[index].finish == "" {
			return nil, errors.New("Zed closed the Chat Completions response before a finish_reason")
		}
	}
	if len(order) == 0 {
		return nil, errors.New("Zed sent no Chat Completions choices")
	}
	out := make([]any, 0, len(order))
	for _, index := range order {
		state := choices[index]
		message := map[string]any{"role": "assistant", "content": state.content.String()}
		if state.reasoning.Len() > 0 {
			message["reasoning_content"] = state.reasoning.String()
		}
		if len(state.order) > 0 {
			calls := make([]any, 0, len(state.order))
			for _, toolIndex := range state.order {
				tool := state.tools[toolIndex]
				arguments := tool.arguments.String()
				if arguments == "" {
					arguments = "{}"
				}
				calls = append(calls, map[string]any{
					"id":       tool.id,
					"type":     "function",
					"function": map[string]any{"name": tool.name, "arguments": arguments},
				})
			}
			message["tool_calls"] = calls
		}
		out = append(out, map[string]any{"index": index, "message": message, "finish_reason": state.finish})
	}
	response := map[string]any{"object": "chat.completion", "choices": out}
	if usage != nil {
		response["usage"] = usage
	}
	return json.Marshal(response)
}

type chatToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}
