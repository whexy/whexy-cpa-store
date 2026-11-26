// Package translate carries a provider plugin's requests and responses
// between the format the client speaks and the model's native protocol,
// through the translators CLIProxyAPI registers with sdk/translator.
package translate

import (
	"errors"
	"fmt"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	// Registers CPA's protocol translators, without which no route between
	// two formats can be planned.
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

var errNoUpstream = errors.New("the model names no upstream protocol")

// Provider holds what differs between the upstreams plugins translate for.
// The zero value is usable but names no provider in its errors.
type Provider struct {
	// Name is the provider's display name in error text.
	Name string
	// Aliases are the lower-case format names the provider accepts beyond
	// those of Chat Completions, Responses and Messages.
	Aliases map[string]sdktranslator.Format
	// Failed is the error a buffered Responses object with status failed
	// stands for; nil reports a generic failure.
	Failed func(body []byte) error
}

// NormalizeFormat maps a format name the host sends to a translator format.
// A name it does not know passes through lower-cased, so a route to it fails
// in PlanRoute rather than here.
func (p Provider) NormalizeFormat(name string) sdktranslator.Format {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "", "openai", "chat-completions", "chat_completions":
		return sdktranslator.FormatOpenAI
	case "openai-response", "openai-responses", "responses":
		return sdktranslator.FormatOpenAIResponse
	case "claude", "anthropic", "messages":
		return sdktranslator.FormatClaude
	}
	if format, ok := p.Aliases[key]; ok {
		return format
	}
	return sdktranslator.Format(key)
}

// Plan says how one request travels between the client's format and the
// model's native protocol.
type Plan struct {
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

// PlanRoute plans a request from source to upstream whose response the host
// expects in response. It fails when a translator the route needs is not
// registered.
func PlanRoute(source, response, upstream sdktranslator.Format) (Plan, error) {
	if upstream == "" {
		return Plan{}, errNoUpstream
	}
	plan := Plan{Source: source, Response: response, Upstream: upstream}
	if source != upstream {
		plan.RequestTo = translatorSide(upstream)
		if !sdktranslator.HasRequestTransformer(source, plan.RequestTo) {
			return Plan{}, fmt.Errorf("no translator from %s requests to %s", source, upstream)
		}
	}
	if response != upstream {
		plan.ResponseFrom = translatorSide(upstream)
		if !sdktranslator.HasStreamResponseTransformer(response, plan.ResponseFrom) || !sdktranslator.HasNonStreamResponseTransformer(response, plan.ResponseFrom) {
			return Plan{}, fmt.Errorf("no translator from %s responses to %s", upstream, response)
		}
	}
	return plan, nil
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
