package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// routePlan says how one request travels between the client's format and the
// model's native protocol.
type routePlan struct {
	Source   sdktranslator.Format
	Response sdktranslator.Format
	Upstream sdktranslator.Format
	// RequestTo is the translator target for the request body, empty when the
	// body is forwarded with only the model rewritten.
	RequestTo sdktranslator.Format
	// ResponseFrom is the translator source for the response, empty when the
	// upstream response is already in the Response format.
	ResponseFrom sdktranslator.Format
}

// splitModelSuffix removes a CPA thinking suffix such as (high) from a model
// ID. The suffix is applied after the native request is built.
func splitModelSuffix(model string) (string, string) {
	model = strings.TrimSpace(model)
	if !strings.HasSuffix(model, ")") {
		return model, ""
	}
	open := strings.LastIndexByte(model, '(')
	if open < 0 {
		return model, ""
	}
	return strings.TrimSpace(model[:open]), strings.TrimSpace(model[open+1 : len(model)-1])
}

// applyRequestedEffort writes a CPA thinking suffix into the model's native
// thinking fields, the way Zed's own client asks for an effort. A number is a
// token budget, 0 or none drops the request's thinking, and auto or -1 keeps
// the model's default. Models Zed lists without thinking ignore any other
// suffix.
func applyRequestedEffort(upstream sdktranslator.Format, body []byte, suffix string, model zedModel) []byte {
	requested := strings.ToLower(suffix)
	budget, errAtoi := strconv.Atoi(suffix)
	if errAtoi == nil {
		switch {
		case budget == 0:
			requested = "none"
		case budget > 0:
			requested = effortForBudget(int64(budget))
		default:
			requested = "auto"
		}
	}
	if requested == "none" {
		for _, field := range thinkingFields(upstream) {
			body, _ = sjson.DeleteBytes(body, field)
		}
		return body
	}
	if !model.SupportsThinking {
		return body
	}
	if requested == "ultra" {
		requested = "max"
	}
	effort := supportedEffort(requested, model)
	if effort == "" {
		return body
	}
	switch upstream {
	case sdktranslator.FormatClaude:
		if len(model.EffortLevels) == 0 {
			// A Claude model without effort levels thinks on a token budget,
			// so only a numeric suffix applies.
			if errAtoi == nil && budget > 0 {
				body, _ = sjson.SetBytes(body, "thinking", map[string]any{"type": "enabled", "budget_tokens": budget})
				body = fitThinkingBudget(body)
			}
			return body
		}
		body, _ = sjson.SetBytes(body, "thinking", map[string]any{"type": "adaptive"})
		body, _ = sjson.SetBytes(body, "output_config.effort", effort)
	case sdktranslator.FormatOpenAIResponse:
		body, _ = sjson.SetBytes(body, "reasoning.effort", effort)
	case sdktranslator.FormatOpenAI:
		body, _ = sjson.SetBytes(body, "reasoning_effort", effort)
	case sdktranslator.FormatGemini:
		body, _ = sjson.SetBytes(body, "generationConfig.thinkingConfig", map[string]any{"thinkingLevel": strings.ToUpper(effort), "includeThoughts": true})
	}
	return body
}

// thinkingFields are the request fields that ask an upstream protocol for
// thinking.
func thinkingFields(upstream sdktranslator.Format) []string {
	switch upstream {
	case sdktranslator.FormatClaude:
		return []string{"thinking", "output_config.effort"}
	case sdktranslator.FormatOpenAIResponse:
		return []string{"reasoning"}
	case sdktranslator.FormatOpenAI:
		return []string{"reasoning_effort"}
	case sdktranslator.FormatGemini:
		return []string{"generationConfig.thinkingConfig"}
	}
	return nil
}

var effortRanks = map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// supportedEffort returns the strongest effort the model lists at or below
// the requested one, or its weakest when every level is stronger, so the
// upstream never sees a level it rejects. A model that lists no levels gets
// the request unchanged.
func supportedEffort(requested string, model zedModel) string {
	rank, known := effortRanks[requested]
	if !known {
		return ""
	}
	if len(model.EffortLevels) == 0 {
		return requested
	}
	best, weakest := "", ""
	for _, level := range model.EffortLevels {
		levelRank, okLevel := effortRanks[strings.ToLower(level)]
		if !okLevel {
			continue
		}
		if weakest == "" || levelRank < effortRanks[weakest] {
			weakest = strings.ToLower(level)
		}
		if levelRank <= rank && (best == "" || levelRank > effortRanks[best]) {
			best = strings.ToLower(level)
		}
	}
	if best == "" {
		return weakest
	}
	return best
}

func normalizeFormat(name string) sdktranslator.Format {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "openai", "chat-completions", "chat_completions":
		return sdktranslator.FormatOpenAI
	case "openai-response", "openai-responses", "responses":
		return sdktranslator.FormatOpenAIResponse
	case "claude", "anthropic", "messages":
		return sdktranslator.FormatClaude
	case "gemini", "google":
		return sdktranslator.FormatGemini
	case "codex":
		return sdktranslator.FormatCodex
	default:
		return sdktranslator.Format(strings.ToLower(strings.TrimSpace(name)))
	}
}

// nativeFormat is the wire dialect a Zed model speaks.
func nativeFormat(model zedModel) sdktranslator.Format {
	switch model.Provider {
	case "anthropic":
		return sdktranslator.FormatClaude
	case "open_ai":
		return sdktranslator.FormatOpenAIResponse
	case "google":
		return sdktranslator.FormatGemini
	case "x_ai":
		return sdktranslator.FormatOpenAI
	default:
		return ""
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
	if upstream == "" {
		return routePlan{}, errors.New("the model names no upstream protocol")
	}
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
// request, without the Zed envelope.
func buildUpstreamBody(ctx context.Context, plan routePlan, payload []byte, model zedModel, stream bool) ([]byte, error) {
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return nil, errors.New("request body is not a JSON object")
	}
	body := payload
	if plan.RequestTo != "" {
		translated := sdktranslator.TranslateRequestEnvelope(ctx, plan.Source, plan.RequestTo, sdktranslator.RequestEnvelope{
			Format: plan.Source,
			Model:  model.ID,
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
	body, errSet := setJSON(body, "model", model.ID)
	if errSet != nil {
		return nil, errSet
	}
	body, errSet = normalizeNativeBody(plan.Upstream, body, model)
	if errSet != nil {
		return nil, errSet
	}
	// Zed's own client always streams provider events; a Responses model
	// answers an empty 200 body to stream:false, so non-streaming clients are
	// served by folding the stream instead.
	return setStream(body, true)
}

// minResponsesOutputTokens is the lowest max_output_tokens the Responses
// backend accepts; Chat and Messages clients go lower, as a Claude Code
// max_tokens 1 probe does.
const minResponsesOutputTokens = 16

// adaptCodexBody undoes Codex translator defaults written for the ChatGPT
// backend: it drops the client's max tokens and always asks for medium
// reasoning and parallel tool calls.
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
	switch format {
	case sdktranslator.FormatOpenAI:
		if limit := root.Get("max_completion_tokens"); limit.Exists() {
			return limit.Int()
		}
	case sdktranslator.FormatGemini:
		if limit := root.Get("generationConfig.maxOutputTokens"); limit.Exists() {
			return limit.Int()
		}
		return root.Get("generationConfig.max_output_tokens").Int()
	}
	return root.Get("max_tokens").Int()
}

// sourceAsksReasoning reports whether the client asked for reasoning in a
// field the translator maps to reasoning.effort.
func sourceAsksReasoning(root gjson.Result, format sdktranslator.Format) bool {
	switch format {
	case sdktranslator.FormatClaude:
		return root.Get("thinking").IsObject() || root.Get("output_config.effort").Exists()
	case sdktranslator.FormatGemini:
		config := root.Get("generationConfig")
		return config.Get("thinkingConfig").IsObject() || config.Get("thinkingLevel").Exists() || config.Get("thinking_level").Exists()
	}
	return root.Get("reasoning_effort").Exists()
}

// normalizeNativeBody applies the request rules Zed's edge enforces.
func normalizeNativeBody(upstream sdktranslator.Format, body []byte, model zedModel) ([]byte, error) {
	switch upstream {
	case sdktranslator.FormatClaude:
		return normalizeClaudeBody(body, model)
	case sdktranslator.FormatOpenAIResponse:
		return normalizeResponsesBody(body, model)
	case sdktranslator.FormatGemini:
		return normalizeGeminiBody(body, model)
	case sdktranslator.FormatOpenAI:
		return normalizeChatBody(body, model)
	}
	return body, nil
}

// normalizeClaudeBody turns a Messages request into the newer API the Zed
// Anthropic route speaks: block content, adaptive thinking, and no sampling
// parameters on models that reject them. Zed's edge parses the body into its
// own request type before forwarding it, so a field it does not know is
// dropped but a value outside one of its enums fails the whole request.
func normalizeClaudeBody(body []byte, model zedModel) ([]byte, error) {
	root := gjson.ParseBytes(body)
	messages := root.Get("messages")
	if !messages.IsArray() {
		return nil, errors.New("Claude request has no messages")
	}
	if encoded, errMarshal := json.Marshal(claudeTurns(messages)); errMarshal == nil {
		body, _ = sjson.SetRawBytes(body, "messages", encoded)
	}
	body = keepClaudeTools(body)
	body = keepClaudeCompaction(body)

	if rejectsSamplingParameters(model.ID) {
		for _, field := range []string{"temperature", "top_p", "top_k"} {
			body, _ = sjson.DeleteBytes(body, field)
		}
	}
	if thinking := gjson.GetBytes(body, "thinking"); thinking.IsObject() {
		switch thinking.Get("type").String() {
		case "enabled":
			// Models with effort levels take adaptive thinking only; the
			// others keep the budget, as Zed's client sends them.
			if len(model.EffortLevels) == 0 {
				break
			}
			budget := thinking.Get("budget_tokens").Int()
			body, _ = sjson.SetBytes(body, "thinking.type", "adaptive")
			body, _ = sjson.DeleteBytes(body, "thinking.budget_tokens")
			if effort := supportedEffort(effortForBudget(budget), model); effort != "" && !gjson.GetBytes(body, "output_config.effort").Exists() {
				body, _ = sjson.SetBytes(body, "output_config.effort", effort)
			}
		case "disabled", "between_tools":
			// Zed's parser has no between_tools, and Sonnet 5.5 refuses
			// disabled, so neither can turn thinking off through Zed.
			body, _ = sjson.DeleteBytes(body, "thinking")
		}
	}
	if limit := root.Get("max_tokens").Int(); model.OutputTokens > 0 && (limit <= 0 || limit > model.OutputTokens) {
		body, _ = sjson.SetBytes(body, "max_tokens", model.OutputTokens)
	}
	return fitThinkingBudget(body), nil
}

// claudeTurns gives every message a block array and every tool_result the
// is_error flag Zed's parser requires. Zed knows only the user and assistant
// roles, so the system messages Claude Code sends between turns become user
// turns, merged with their neighbours so tool_result blocks stay first.
func claudeTurns(messages gjson.Result) []any {
	type turn struct {
		role    string
		results []any
		blocks  []any
	}
	var turns []*turn
	for _, message := range messages.Array() {
		role := message.Get("role").String()
		if role == "system" {
			role = "user"
		}
		var current *turn
		if last := len(turns) - 1; last >= 0 && role == "user" && turns[last].role == "user" {
			current = turns[last]
		} else {
			current = &turn{role: role}
			turns = append(turns, current)
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			current.blocks = append(current.blocks, map[string]any{"type": "text", "text": content.String()})
			continue
		}
		for _, block := range content.Array() {
			raw := []byte(block.Raw)
			if block.Get("type").String() != "tool_result" {
				current.blocks = append(current.blocks, json.RawMessage(raw))
				continue
			}
			if block.Get("is_error").Type != gjson.True {
				raw, _ = sjson.SetBytes(raw, "is_error", false)
			}
			current.results = append(current.results, json.RawMessage(raw))
		}
	}
	out := make([]any, 0, len(turns))
	for _, turn := range turns {
		content := append(append(make([]any, 0, len(turn.results)+len(turn.blocks)), turn.results...), turn.blocks...)
		out = append(out, map[string]any{"role": turn.role, "content": content})
	}
	return out
}

// keepClaudeTools keeps the tools Zed's parser accepts: every tool needs a
// description, so server tools such as web search, which Zed cannot run, are
// dropped along with a tool_choice that names one.
func keepClaudeTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	kept := []any{}
	names := map[string]bool{}
	for _, tool := range tools.Array() {
		if kind := tool.Get("type").String(); kind != "" && kind != "custom" {
			continue
		}
		raw := []byte(tool.Raw)
		if tool.Get("description").Type != gjson.String {
			raw, _ = sjson.SetBytes(raw, "description", "")
		}
		names[tool.Get("name").String()] = true
		kept = append(kept, json.RawMessage(raw))
	}
	if len(kept) == 0 {
		body, _ = sjson.DeleteBytes(body, "tools")
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
	body, _ = sjson.SetBytes(body, "tools", kept)
	if choice := gjson.GetBytes(body, "tool_choice"); choice.Get("type").String() == "tool" && !names[choice.Get("name").String()] {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
	}
	return body
}

// keepClaudeCompaction keeps the only context edit Zed's parser knows.
// Claude Code also asks to clear old thinking and tool uses, which Zed
// rejects outright.
func keepClaudeCompaction(body []byte) []byte {
	if !gjson.GetBytes(body, "context_management").Exists() {
		return body
	}
	kept := []any{}
	for _, edit := range gjson.GetBytes(body, "context_management.edits").Array() {
		if edit.Get("type").String() == "compact_20260112" {
			kept = append(kept, json.RawMessage(edit.Raw))
		}
	}
	if len(kept) == 0 {
		body, _ = sjson.DeleteBytes(body, "context_management")
		return body
	}
	body, _ = sjson.SetBytes(body, "context_management.edits", kept)
	return body
}

// minThinkingBudget is the smallest budget_tokens Anthropic accepts.
const minThinkingBudget = 1024

// fitThinkingBudget keeps a thinking budget within what Anthropic accepts:
// at least minThinkingBudget and below max_tokens, which the model's output
// limit may have lowered. Thinking with no room for the minimum is dropped.
func fitThinkingBudget(body []byte) []byte {
	thinking := gjson.GetBytes(body, "thinking")
	if thinking.Get("type").String() != "enabled" {
		return body
	}
	budget := max(thinking.Get("budget_tokens").Int(), minThinkingBudget)
	if limit := gjson.GetBytes(body, "max_tokens").Int(); limit > 0 {
		budget = min(budget, limit-1)
	}
	if budget < minThinkingBudget {
		body, _ = sjson.DeleteBytes(body, "thinking")
		return body
	}
	body, _ = sjson.SetBytes(body, "thinking.budget_tokens", budget)
	return body
}

func effortForBudget(budget int64) string {
	switch {
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	case budget <= 32768:
		return "high"
	default:
		return "xhigh"
	}
}

// rejectsSamplingParameters reports whether a Claude model uses the newer
// API that refuses temperature, top_p and top_k.
func rejectsSamplingParameters(id string) bool {
	id = strings.ToLower(id)
	for _, family := range []string{"claude-sonnet-5", "claude-haiku-5", "claude-opus-5", "claude-fable"} {
		if strings.Contains(id, family) {
			return true
		}
	}
	return false
}

// normalizeResponsesBody makes a Responses request acceptable to Zed's
// OpenAI route.
func normalizeResponsesBody(body []byte, model zedModel) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		converted, errMarshal := json.Marshal([]any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": input.String(),
			}},
		}})
		if errMarshal != nil {
			return nil, errMarshal
		}
		body, _ = sjson.SetRawBytes(body, "input", converted)
		input = gjson.GetBytes(body, "input")
	}
	if input.IsArray() {
		items := []any{}
		for _, item := range input.Array() {
			raw := []byte(item.Raw)
			if !item.Get("type").Exists() && item.Get("role").Exists() {
				raw, _ = sjson.SetBytes(raw, "type", "message")
			}
			if content := item.Get("content"); content.Type == gjson.String && item.Get("role").String() != "" {
				partType := "input_text"
				if item.Get("role").String() == "assistant" {
					partType = "output_text"
				}
				raw, _ = sjson.SetBytes(raw, "content", []any{map[string]any{"type": partType, "text": content.String()}})
			}
			items = append(items, json.RawMessage(raw))
		}
		if encoded, errMarshal := json.Marshal(items); errMarshal == nil {
			body, _ = sjson.SetRawBytes(body, "input", encoded)
		}
	}
	if limit := gjson.GetBytes(body, "max_output_tokens"); limit.Exists() && limit.Int() < minResponsesOutputTokens {
		body, _ = sjson.SetBytes(body, "max_output_tokens", minResponsesOutputTokens)
	}
	return body, nil
}

// normalizeGeminiBody makes a Gemini request look like the one Zed forwards.
func normalizeGeminiBody(body []byte, model zedModel) ([]byte, error) {
	body, _ = sjson.SetBytes(body, "model", "models/"+model.ID)
	// Zed's Google parser accepts only a subset of the safety categories the
	// shared translator emits; its own client sends none.
	body, _ = sjson.DeleteBytes(body, "safetySettings")
	if limit := gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int(); model.OutputTokens > 0 && (limit <= 0 || limit > model.OutputTokens) {
		body, _ = sjson.SetBytes(body, "generationConfig.maxOutputTokens", model.OutputTokens)
	}
	return body, nil
}

func normalizeChatBody(body []byte, model zedModel) ([]byte, error) {
	if limit := gjson.GetBytes(body, "max_tokens").Int(); model.OutputTokens > 0 && (limit <= 0 || limit > model.OutputTokens) {
		body, _ = sjson.SetBytes(body, "max_tokens", model.OutputTokens)
	}
	return body, nil
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

// convertResponse turns a buffered native response into the client format.
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
// stream event the Codex translators read.
func codexTerminalEvent(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("Zed returned a Responses body that is not JSON")
	}
	eventType := "response.completed"
	switch gjson.GetBytes(body, "status").String() {
	case "incomplete":
		eventType = "response.incomplete"
	case "failed":
		return nil, errors.New(firstLine(string(body)))
	}
	event, errSet := sjson.SetRawBytes([]byte(`{"type":""}`), "response", body)
	if errSet != nil {
		return nil, errSet
	}
	return sjson.SetBytes(event, "type", eventType)
}
