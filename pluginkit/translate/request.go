package translate

import (
	"context"
	"errors"
	"fmt"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errNotObject = errors.New("request body is not a JSON object")

// MinResponsesOutputTokens is the lowest max_output_tokens Responses
// backends accept; Chat and Messages clients go lower, as Claude Code's
// max_tokens 1 probe does.
const MinResponsesOutputTokens = 16

// BuildRequest turns the client's request body into the model's native
// request for model. The stream flag is left as the translator wrote it, for
// the caller to settle with SetStream.
func BuildRequest(ctx context.Context, plan Plan, payload []byte, model string, stream bool) ([]byte, error) {
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return nil, errNotObject
	}
	body := payload
	if plan.RequestTo != "" {
		translated := sdktranslator.TranslateRequestEnvelope(ctx, plan.Source, plan.RequestTo, sdktranslator.RequestEnvelope{
			Format: plan.Source,
			Model:  model,
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
	return setJSON(body, "model", model)
}

// adaptCodexBody undoes Codex translator defaults written for the ChatGPT
// backend: it drops the client's max tokens and always asks for medium
// reasoning and parallel tool calls, whether or not the client did.
func adaptCodexBody(body, source []byte, sourceFormat sdktranslator.Format) []byte {
	root := gjson.ParseBytes(source)
	if limit := sourceMaxTokens(root, sourceFormat); limit > 0 {
		body, _ = sjson.SetBytes(body, "max_output_tokens", max(limit, MinResponsesOutputTokens))
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

// sourceAsksReasoning reports whether the client chose a reasoning level in
// a field the translator maps to reasoning.effort; without one the model's
// own default applies, and models that take no effort parameter are not
// sent one.
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

// SetStream makes the body's stream flag match the call: the host decides
// streaming by method, and the Claude-to-Codex translator always sets it.
// A body without the flag stays without it when the call does not stream.
func SetStream(body []byte, stream bool) ([]byte, error) {
	current := gjson.GetBytes(body, "stream")
	if current.Exists() && current.Bool() == stream {
		return body, nil
	}
	if !current.Exists() && !stream {
		return body, nil
	}
	return setJSON(body, "stream", stream)
}

// setJSON leaves body untouched when path already holds value, so a body
// forwarded as sent keeps the client's bytes.
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
