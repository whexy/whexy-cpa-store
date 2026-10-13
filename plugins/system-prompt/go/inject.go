package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// promptSeparator sits between the client's system prompt and each appended
// prompt.
const promptSeparator = "\n\n"

// Source formats are the host's handler types. Interceptors see the body in
// the format the client sent, before any translation.
const (
	formatClaude          = "claude"
	formatOpenAI          = "openai"
	formatOpenAIResponses = "openai-response"
	formatGemini          = "gemini"
)

func interceptRequest(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode request intercept request: %w", errUnmarshal)
	}
	return okEnvelope(decide(req))
}

// decide matches the model the client asked for and the model it resolved to
// after aliases, model pools and model routing, so a rule can name either.
func decide(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	prompt := currentRules().promptFor(req.RequestedModel, req.Model)
	if prompt == "" {
		return pluginapi.RequestInterceptResponse{}
	}
	body, ok := appendSystemPrompt(req.SourceFormat, req.Body, prompt)
	if !ok {
		return pluginapi.RequestInterceptResponse{}
	}
	return pluginapi.RequestInterceptResponse{Body: body}
}

// appendSystemPrompt adds prompt to the end of the body's system prompt,
// creating one when the client sent none. It reports false for bodies that
// carry no conversation, such as embeddings, and for formats it does not know.
func appendSystemPrompt(format string, body []byte, prompt string) ([]byte, bool) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, false
	}
	switch format {
	case formatClaude:
		return appendClaude(body, prompt)
	case formatOpenAI:
		return appendChatCompletions(body, prompt)
	case formatOpenAIResponses:
		return appendResponses(body, prompt)
	case formatGemini:
		return appendGemini(body, prompt)
	default:
		return nil, false
	}
}

// appendClaude extends the last system text block rather than adding one:
// CLIProxyAPI recognizes Claude Code's helper requests by their exact number
// of system blocks, and an extra block would get them cloaked.
func appendClaude(body []byte, prompt string) ([]byte, bool) {
	if !gjson.GetBytes(body, "messages").IsArray() {
		return nil, false
	}
	system := gjson.GetBytes(body, "system")
	switch {
	case !system.Exists() || system.Type == gjson.Null:
		return setString(body, "system", prompt)
	case system.Type == gjson.String:
		return setString(body, "system", joined(system.String(), prompt))
	case system.IsArray():
		return appendToTextList(body, "system", system, isTypedText, `{"type":"text"}`, prompt)
	default:
		return nil, false
	}
}

// appendChatCompletions extends the last system or developer message that
// opens the conversation, or opens it with a system message.
func appendChatCompletions(body []byte, prompt string) ([]byte, bool) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil, false
	}
	items := messages.Array()
	last := -1
	for index, message := range items {
		role := message.Get("role").String()
		if role != "system" && role != "developer" {
			break
		}
		last = index
	}
	if last < 0 {
		message, errSet := sjson.Set(`{"role":"system"}`, "content", prompt)
		if errSet != nil {
			return nil, false
		}
		return prependToArray(body, "messages", messages, message)
	}
	path := fmt.Sprintf("messages.%d.content", last)
	content := items[last].Get("content")
	switch {
	case !content.Exists() || content.Type == gjson.Null:
		return setString(body, path, prompt)
	case content.Type == gjson.String:
		return setString(body, path, joined(content.String(), prompt))
	case content.IsArray():
		return appendToTextList(body, path, content, isTypedText, `{"type":"text"}`, prompt)
	default:
		return nil, false
	}
}

func appendResponses(body []byte, prompt string) ([]byte, bool) {
	instructions := gjson.GetBytes(body, "instructions")
	switch {
	case !instructions.Exists() || instructions.Type == gjson.Null:
		return setString(body, "instructions", prompt)
	case instructions.Type == gjson.String:
		return setString(body, "instructions", joined(instructions.String(), prompt))
	default:
		return nil, false
	}
}

// appendGemini accepts both spellings Google's JSON parser accepts.
func appendGemini(body []byte, prompt string) ([]byte, bool) {
	if !gjson.GetBytes(body, "contents").IsArray() {
		return nil, false
	}
	path := "systemInstruction"
	instruction := gjson.GetBytes(body, path)
	if snake := gjson.GetBytes(body, "system_instruction"); !instruction.Exists() && snake.Exists() {
		path, instruction = "system_instruction", snake
	}
	if !instruction.Exists() || instruction.Type == gjson.Null {
		created, errSet := sjson.Set(`{"parts":[{}]}`, "parts.0.text", prompt)
		if errSet != nil {
			return nil, false
		}
		return setRaw(body, path, created)
	}
	if !instruction.IsObject() {
		return nil, false
	}
	parts := instruction.Get("parts")
	switch {
	case !parts.Exists() || parts.Type == gjson.Null:
		return appendToTextList(body, path+".parts", gjson.Result{}, isGeminiText, `{}`, prompt)
	case parts.IsArray():
		return appendToTextList(body, path+".parts", parts, isGeminiText, `{}`, prompt)
	default:
		return nil, false
	}
}

func isTypedText(element gjson.Result) bool {
	return element.Get("type").String() == "text" && element.Get("text").Type == gjson.String
}

func isGeminiText(part gjson.Result) bool {
	return part.Get("text").Type == gjson.String && !part.Get("thought").Bool()
}

// appendToTextList extends the text of the array's last element when it is
// text, and otherwise adds a text element built from template.
func appendToTextList(body []byte, path string, list gjson.Result, isText func(gjson.Result) bool, template, prompt string) ([]byte, bool) {
	items := list.Array()
	if count := len(items); count > 0 && isText(items[count-1]) {
		return setString(body, fmt.Sprintf("%s.%d.text", path, count-1), joined(items[count-1].Get("text").String(), prompt))
	}
	element, errSet := sjson.Set(template, "text", prompt)
	if errSet != nil {
		return nil, false
	}
	return setRaw(body, path+".-1", element)
}

// prependToArray keeps the existing elements' bytes as the client sent them.
func prependToArray(body []byte, path string, list gjson.Result, element string) ([]byte, bool) {
	rest := strings.TrimSpace(list.Raw)
	rest = strings.TrimSpace(rest[1 : len(rest)-1])
	if rest == "" {
		return setRaw(body, path, "["+element+"]")
	}
	return setRaw(body, path, "["+element+","+rest+"]")
}

func joined(existing, prompt string) string {
	if strings.TrimSpace(existing) == "" {
		return prompt
	}
	return existing + promptSeparator + prompt
}

func setString(body []byte, path, value string) ([]byte, bool) {
	out, errSet := sjson.SetBytes(body, path, value)
	return out, errSet == nil
}

func setRaw(body []byte, path, raw string) ([]byte, bool) {
	out, errSet := sjson.SetRawBytes(body, path, []byte(raw))
	return out, errSet == nil
}
