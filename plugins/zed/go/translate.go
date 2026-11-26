package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/whexy/whexy-cpa-store/pluginkit/translate"
)

// provider tells the shared translation that Gemini clients may name their
// format google, and reports a failed buffered Responses object by its first
// line, as Zed's other failures are.
var provider = translate.Provider{
	Name:    displayName,
	Aliases: map[string]sdktranslator.Format{"google": sdktranslator.FormatGemini},
	Failed:  func(body []byte) error { return errors.New(firstLine(string(body))) },
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

// buildUpstreamBody turns the client's request into the model's native
// request, without the Zed envelope. It also returns the namespaced tools
// whose names were flattened, which the response must restore.
func buildUpstreamBody(ctx context.Context, plan translate.Plan, payload []byte, model zedModel, stream bool) ([]byte, map[string]namespacedTool, error) {
	body, errBuild := translate.BuildRequest(ctx, plan, payload, model.ID, stream)
	if errBuild != nil {
		return nil, nil, errBuild
	}
	var namespaced map[string]namespacedTool
	if plan.Upstream == sdktranslator.FormatOpenAIResponse {
		body, namespaced = flattenResponsesTools(body)
	}
	body, errNormalize := normalizeNativeBody(plan.Upstream, body, model)
	if errNormalize != nil {
		return nil, nil, errNormalize
	}
	// Zed's own client always streams provider events; a Responses model
	// answers an empty 200 body to stream:false, so non-streaming clients are
	// served by folding the stream instead.
	body, errStream := translate.SetStream(body, true)
	if errStream != nil {
		return nil, nil, errStream
	}
	return body, namespaced, nil
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

// namespacedTool names a tool a Responses client declared inside a
// namespace other than the default one.
type namespacedTool struct {
	Namespace string
	Name      string
}

// defaultToolNamespace is the namespace Codex gives a call that names none.
const defaultToolNamespace = "functions"

// flattenResponsesTools turns a Responses client's tools into the top-level
// function and custom tools Zed's parser accepts. Codex declares its tools
// in an additional_tools input item and groups them into namespaces; a
// flattened tool keeps its bare name unless another tool has it, then takes
// Codex's flat form, the namespace followed by the name. Hosted tools such as
// web search run on OpenAI's side, which Zed does not offer, and are dropped.
// It returns the flattened names whose calls must get their namespace back.
func flattenResponsesTools(body []byte) ([]byte, map[string]namespacedTool) {
	type entry struct {
		namespace string
		name      string
		raw       []byte
	}
	var entries []entry
	collect := func(tools gjson.Result) {
		for _, tool := range tools.Array() {
			switch tool.Get("type").String() {
			case "function", "custom":
				entries = append(entries, entry{name: tool.Get("name").String(), raw: []byte(tool.Raw)})
			case "namespace":
				namespace := tool.Get("name").String()
				if namespace == defaultToolNamespace {
					namespace = ""
				}
				for _, nested := range tool.Get("tools").Array() {
					if kind := nested.Get("type").String(); kind == "function" || kind == "custom" {
						entries = append(entries, entry{namespace: namespace, name: nested.Get("name").String(), raw: []byte(nested.Raw)})
					}
				}
			}
		}
	}
	declared := gjson.GetBytes(body, "tools")
	collect(declared)
	input := gjson.GetBytes(body, "input")
	var items []gjson.Result
	hasAdditional := false
	for _, item := range input.Array() {
		if item.Get("type").String() == "additional_tools" {
			collect(item.Get("tools"))
			hasAdditional = true
			continue
		}
		items = append(items, item)
	}
	if !declared.Exists() && !hasAdditional {
		return body, nil
	}

	counts := map[string]int{}
	for _, tool := range entries {
		counts[tool.name]++
	}
	restore := map[string]namespacedTool{}
	flat := map[namespacedTool]string{}
	tools := make([]any, 0, len(entries))
	for _, tool := range entries {
		raw := tool.raw
		if tool.namespace != "" {
			name := tool.name
			if counts[name] > 1 {
				name = tool.namespace + tool.name
			}
			original := namespacedTool{Namespace: tool.namespace, Name: tool.name}
			restore[name] = original
			flat[original] = name
			raw, _ = sjson.SetBytes(raw, "name", name)
		}
		tools = append(tools, json.RawMessage(raw))
	}
	if len(tools) == 0 {
		body, _ = sjson.DeleteBytes(body, "tools")
	} else {
		body, _ = sjson.SetBytes(body, "tools", tools)
	}

	if input.IsArray() {
		rewritten := make([]any, 0, len(items))
		for _, item := range items {
			raw := []byte(item.Raw)
			if kind := item.Get("type").String(); (kind == "function_call" || kind == "custom_tool_call") && item.Get("namespace").Exists() {
				raw, _ = sjson.DeleteBytes(raw, "namespace")
				if namespace := item.Get("namespace").String(); namespace != "" && namespace != defaultToolNamespace {
					original := namespacedTool{Namespace: namespace, Name: item.Get("name").String()}
					name, ok := flat[original]
					if !ok {
						name = namespace + original.Name
					}
					raw, _ = sjson.SetBytes(raw, "name", name)
				}
			}
			rewritten = append(rewritten, json.RawMessage(raw))
		}
		body, _ = sjson.SetBytes(body, "input", rewritten)
	}
	return body, restore
}

// restoreToolNamespaces gives calls to flattened tools the namespace and name
// the client declared, in a Responses stream event or response object.
func restoreToolNamespaces(data []byte, restore map[string]namespacedTool) []byte {
	if len(restore) == 0 {
		return data
	}
	root := gjson.ParseBytes(data)
	rename := func(path string, item gjson.Result) {
		if kind := item.Get("type").String(); kind != "function_call" && kind != "custom_tool_call" {
			return
		}
		original, ok := restore[item.Get("name").String()]
		if !ok {
			return
		}
		data, _ = sjson.SetBytes(data, path+".name", original.Name)
		data, _ = sjson.SetBytes(data, path+".namespace", original.Namespace)
	}
	if item := root.Get("item"); item.IsObject() {
		rename("item", item)
	}
	for _, path := range []string{"response.output", "output"} {
		for index, item := range root.Get(path).Array() {
			rename(path+"."+strconv.Itoa(index), item)
		}
	}
	return data
}

// responsesInputTypes are the input items Zed's Responses parser accepts.
// The others record hosted tools Zed cannot run, so they are dropped.
var responsesInputTypes = map[string]bool{
	"message":                 true,
	"function_call":           true,
	"function_call_output":    true,
	"custom_tool_call":        true,
	"custom_tool_call_output": true,
	"reasoning":               true,
	"compaction":              true,
	"compaction_summary":      true,
}

// responsesSummaryModes are the reasoning summaries Zed's parser accepts.
var responsesSummaryModes = map[string]bool{"auto": true, "concise": true, "detailed": true}

// normalizeResponsesBody makes a Responses request acceptable to Zed's
// OpenAI route, whose parser, like the Anthropic one, rejects any value
// outside its enums.
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
			} else if !responsesInputTypes[item.Get("type").String()] {
				continue
			}
			// Zed's parser reads a missing list as empty but rejects null,
			// which Codex and CPA's Claude translator send on reasoning items.
			item.ForEach(func(key, value gjson.Result) bool {
				if value.Type == gjson.Null {
					raw, _ = sjson.DeleteBytes(raw, key.String())
				}
				return true
			})
			if item.Get("role").String() == "developer" {
				raw, _ = sjson.SetBytes(raw, "role", "system")
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
	if include := gjson.GetBytes(body, "include"); include.Exists() {
		kept := []string{}
		for _, field := range include.Array() {
			if field.String() == "reasoning.encrypted_content" {
				kept = append(kept, field.String())
			}
		}
		if len(kept) == 0 {
			body, _ = sjson.DeleteBytes(body, "include")
		} else {
			body, _ = sjson.SetBytes(body, "include", kept)
		}
	}
	// Zed's parser takes tool_choice only as a mode. A forced tool becomes
	// that tool alone with the required mode, which forces the same call.
	if choice := gjson.GetBytes(body, "tool_choice"); choice.IsObject() {
		var forced []any
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			if name := choice.Get("name").String(); name != "" && tool.Get("name").String() == name {
				forced = append(forced, json.RawMessage(tool.Raw))
			}
		}
		if len(forced) == 0 {
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		} else {
			body, _ = sjson.SetBytes(body, "tools", forced)
			body, _ = sjson.SetBytes(body, "tool_choice", "required")
		}
	}
	if reasoning := gjson.GetBytes(body, "reasoning"); reasoning.Exists() {
		if reasoning.Get("effort").Type != gjson.String {
			body, _ = sjson.DeleteBytes(body, "reasoning")
		} else if summary := reasoning.Get("summary"); summary.Type == gjson.String && !responsesSummaryModes[summary.String()] {
			body, _ = sjson.DeleteBytes(body, "reasoning.summary")
		}
	}
	if limit := gjson.GetBytes(body, "max_output_tokens"); limit.Exists() && limit.Int() < translate.MinResponsesOutputTokens {
		body, _ = sjson.SetBytes(body, "max_output_tokens", translate.MinResponsesOutputTokens)
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
