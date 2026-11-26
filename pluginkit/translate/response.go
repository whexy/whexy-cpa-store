package translate

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertResponse turns a buffered upstream response into the host's
// response format. original is the client's request and request the native
// one, which response translators read tool names and options from.
// passthrough, when not nil, rewrites a response the client gets in the
// model's own protocol, which no translator touches.
func (p Provider) ConvertResponse(ctx context.Context, plan Plan, model string, original, request, body []byte, passthrough func([]byte) []byte) ([]byte, error) {
	if plan.ResponseFrom == "" {
		if passthrough != nil {
			return passthrough(body), nil
		}
		return body, nil
	}
	if plan.ResponseFrom == sdktranslator.FormatCodex {
		event, errEvent := p.CodexTerminalEvent(body)
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

// CodexTerminalEvent wraps a buffered Responses object in the terminal
// stream event the Codex translators read, since the Codex backend they were
// written for only streams. A failed response is reported through Failed.
func (p Provider) CodexTerminalEvent(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New(p.Name + " returned a Responses body that is not JSON")
	}
	eventType := "response.completed"
	switch gjson.GetBytes(body, "status").String() {
	case "incomplete":
		eventType = "response.incomplete"
	case "failed":
		if p.Failed == nil {
			return nil, errors.New(p.Name + " returned a failed response")
		}
		return nil, p.Failed(body)
	}
	event, errSet := sjson.SetRawBytes([]byte(`{"type":""}`), "response", body)
	if errSet != nil {
		return nil, fmt.Errorf("wrap the Responses body: %w", errSet)
	}
	return sjson.SetBytes(event, "type", eventType)
}
