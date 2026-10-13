package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// canonicalSessionKey is the execution metadata key under which CPA's
// conductor stores the session it derived from the client's signals.
const canonicalSessionKey = "canonical_session_id"

// defaultAnthropicVersion is sent on Messages requests from clients that
// name none, such as Chat Completions clients reaching a Messages model.
const defaultAnthropicVersion = "2023-06-01"

// sessionHeaders are the client headers that carry a session, in the order
// they are trusted.
var sessionHeaders = []string{
	"x-opencode-session",
	"X-Claude-Code-Session-Id",
	"session_id",
	"Session-Id",
	"X-Session-Id",
	"X-Session-Affinity",
}

// sessionID picks the x-opencode-session value. The Go gateway rejects any
// inference call without one, so it falls back to a hash of the
// conversation's opening, which stays the same on every turn.
func sessionID(metadata map[string]any, headers http.Header, format sdktranslator.Format, payload []byte) string {
	if value, ok := metadata[canonicalSessionKey].(string); ok {
		if id := cleanSessionID(value); id != "" {
			return id
		}
	}
	for _, name := range sessionHeaders {
		if id := cleanSessionID(headerValue(headers, name)); id != "" {
			return id
		}
	}
	return conversationSessionID(format, payload)
}

// cleanSessionID keeps a session ID that is safe as a header value and
// hashes any other, so a client-chosen ID cannot break the upstream request.
func cleanSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return "cpa-" + shortHash(id)
		}
	}
	return id
}

func shortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// conversationSessionID hashes the system prompt and the first user turn.
// Only their text is used: clients such as Claude Code move cache_control
// markers between turns, which changes the raw JSON but not the text.
func conversationSessionID(format sdktranslator.Format, payload []byte) string {
	root := gjson.ParseBytes(payload)
	var system, firstUser []string
	switch format {
	case sdktranslator.FormatClaude:
		system = contentTexts(root.Get("system"))
		firstUser = firstUserTexts(root.Get("messages"), "content")
	case sdktranslator.FormatOpenAIResponse:
		system = contentTexts(root.Get("instructions"))
		input := root.Get("input")
		if input.Type == gjson.String {
			firstUser = []string{input.String()}
		} else {
			system = append(system, roleTexts(input, "content")...)
			firstUser = firstUserTexts(input, "content")
		}
	default:
		messages := root.Get("messages")
		system = roleTexts(messages, "content")
		firstUser = firstUserTexts(messages, "content")
	}
	if len(system) == 0 && len(firstUser) == 0 {
		return "cpa-" + shortHash(string(payload))
	}
	return "cpa-" + shortHash(string(format)+"\x00"+strings.Join(system, "\x00")+"\x01"+strings.Join(firstUser, "\x00"))
}

// roleTexts collects the text of system and developer messages that come
// before the first user message.
func roleTexts(messages gjson.Result, field string) []string {
	var out []string
	for _, message := range messages.Array() {
		role := message.Get("role").String()
		if role == "user" {
			break
		}
		if role == "system" || role == "developer" {
			out = append(out, contentTexts(message.Get(field))...)
		}
	}
	return out
}

func firstUserTexts(messages gjson.Result, field string) []string {
	for _, message := range messages.Array() {
		if message.Get("role").String() == "user" {
			return contentTexts(message.Get(field))
		}
	}
	return nil
}

// contentTexts reads a string or an array of content parts; non-text parts
// count by their type so an image-only turn still hashes distinctly.
func contentTexts(content gjson.Result) []string {
	if content.Type == gjson.String {
		return []string{content.String()}
	}
	var out []string
	for _, part := range content.Array() {
		if part.Type == gjson.String {
			out = append(out, part.String())
			continue
		}
		if text := part.Get("text"); text.Exists() {
			out = append(out, text.String())
			continue
		}
		out = append(out, "["+part.Get("type").String()+"]")
	}
	return out
}

// headerValue reads a header case-insensitively: the host passes header
// maps through JSON, and underscore names such as session_id are not
// canonicalised the way http.Header.Get expects.
func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

// headerValues joins every value of a repeated header, as anthropic-beta
// may arrive split across lines.
func headerValues(headers http.Header, name string) string {
	var out []string
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
	}
	return strings.Join(out, ",")
}

// inferenceHeaders builds the upstream headers from scratch. Client headers
// are read, never copied, so the client's CPA key, cookies and hop-by-hop
// headers cannot reach opencode.ai.
func inferenceHeaders(cfg settings, apiKey string, protocol sdktranslator.Format, client http.Header, stream bool, session string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	headers.Set("x-opencode-session", session)
	userAgent := headerValue(client, "User-Agent")
	if userAgent == "" {
		userAgent = cfg.userAgent()
	}
	headers.Set("User-Agent", userAgent)
	if protocol == sdktranslator.FormatClaude {
		version := headerValue(client, "anthropic-version")
		if version == "" {
			version = defaultAnthropicVersion
		}
		headers.Set("anthropic-version", version)
		if beta := headerValues(client, "anthropic-beta"); beta != "" {
			headers.Set("anthropic-beta", beta)
		}
	}
	return headers
}
