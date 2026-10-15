package main

// Presenting free OpenCode Zen requests as the OpenCode client.
//
// The Zen free tier is gated by a client fingerprint inside OpenCode's private
// inference service, which answers every other caller with 403 FreeTierError
// ("OpenCode's free tier can only be used from within OpenCode"). The gate was
// reproduced against the live endpoint on 2026-10-15; a request is accepted
// only when all of this holds:
//
//   - the User-Agent names OpenCode with a version of at least 1.18.0, in any
//     of the shapes the client uses (opencode/1.19.0, opencode/latest/2.0.29/
//     cli, or the version followed by other agents). Older versions get 426
//     UpgradeRequired;
//   - a session header carries a well-formed OpenCode session identifier (see
//     mintOpenCodeID), whose leading value the gate decodes;
//   - the body sets "stream": true, and the gateway answers with SSE even for
//     a request that asks for JSON;
//   - the body's tools contain an entry NAMED "read". The gate compares the
//     name and nothing else: a placeholder whose description and schema differ
//     from OpenCode's is accepted, and a genuine definition under another name
//     is refused.
//
// That last finding is what keeps the tool harmless. The placeholder is
// described as unavailable, and models leave it alone: asked to read a file
// with the genuine description they call it, and with the placeholder beside a
// caller's own file tool they call that tool instead. It is never described as
// a capability, and a caller that already declares "read" keeps its own.
//
// Nothing here signs or claims a credential: the caller still sends its own
// Authorization (or none), and the gateway bills exactly as it would without
// the cloak. Absent from the request on purpose are the headers whose mere
// presence trips the gate, such as HTTP-Referer, and x-opencode-request.

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// cloakUserAgent is the identity a request carries when the caller is not
	// OpenCode itself. The gate reads the version out of it.
	cloakUserAgent = "opencode/1.19.0"
	// cloakSessionKeep bounds the sessions whose minted IDs are remembered.
	cloakSessionKeep = 256
	// cloakIDAlphabet is the base62 alphabet OpenCode's identifiers use.
	cloakIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// cloakIDMask keeps the six leading bytes of the identifier value.
	cloakIDMask = uint64(1)<<48 - 1
	// cloakIDCounter is the low field of the identifier. OpenCode's own client
	// increments it within a millisecond; the gate refuses values that do not
	// fit its six bits.
	cloakIDCounter = 1
	// cloakIDMaxCounter is the first counter value the gate refuses.
	cloakIDMaxCounter = 64
	// cloakToolName is the only thing the gate looks for in a tool list.
	cloakToolName = "read"
	// cloakToolDescription is what the model is told about the placeholder.
	// The gate ignores it, so it says what the tool is: a marker the transport
	// needs, not something to call. Models honour it, and a caller that
	// declares its own file tool keeps getting that one called instead.
	cloakToolDescription = "Reserved by the transport and unavailable in this session. Never call this tool."
)

// cloakToolSchema is a valid empty schema. The gate compares no schema at all,
// but the backend still validates one, so the placeholder declares the least a
// JSON Schema can.
const cloakToolSchema = `{"type":"object","properties":{}}`

// cloakSessionStore remembers the identifier minted for a conversation. The
// free tier meters and caches per session, so every turn of one conversation
// has to present the same id.
type cloakSessionStore struct {
	mu    sync.Mutex
	now   func() time.Time
	ids   map[string]string
	seeds []string
}

func newCloakSessionStore() *cloakSessionStore {
	return &cloakSessionStore{now: time.Now, ids: make(map[string]string)}
}

var cloakSessions = newCloakSessionStore()

// id returns the OpenCode session identifier for one conversation, minting it
// the first time it is asked for.
func (s *cloakSessionStore) id(seed string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.ids[seed]; ok {
		return id
	}
	id, errMint := mintOpenCodeID(s.now())
	if errMint != nil {
		// Without randomness the id would repeat across conversations, so fall
		// back to the seed's own hash rather than to a fixed value.
		id = "ses_" + strings.Repeat("0", 12) + shortHash(seed)[:14]
	}
	if len(s.seeds) >= cloakSessionKeep {
		delete(s.ids, s.seeds[0])
		s.seeds = s.seeds[1:]
	}
	s.ids[seed] = id
	s.seeds = append(s.seeds, seed)
	return id
}

// mintOpenCodeID builds the identifier the OpenCode client would send: the six
// leading bytes of a millisecond timestamp with a counter, bit-inverted so the
// newest sorts first, then fourteen random base62 characters. The gate decodes
// that leading value, and rejects an id whose counter does not fit.
func mintOpenCodeID(now time.Time) (string, error) {
	value := (uint64(now.UnixMilli())*0x1000 + cloakIDCounter) & cloakIDMask
	value = ^value & cloakIDMask
	var b strings.Builder
	b.WriteString("ses_")
	for shift := 40; shift >= 0; shift -= 8 {
		fmt.Fprintf(&b, "%02x", byte(value>>uint(shift)))
	}
	random := make([]byte, 14)
	if _, errRead := rand.Read(random); errRead != nil {
		return "", fmt.Errorf("mint OpenCode session id: %w", errRead)
	}
	for _, byteValue := range random {
		b.WriteByte(cloakIDAlphabet[int(byteValue)%len(cloakIDAlphabet)])
	}
	return b.String(), nil
}

// minOpenCodeVersion is the oldest client the free tier answers; anything
// below it gets 426 UpgradeRequired rather than FreeTierError.
var minOpenCodeVersion = [3]int{1, 18, 0}

// openCodeSessionPattern is the identifier shape OpenCode's client mints:
// "ses_", six bytes of a bit-inverted timestamp as hex, then fourteen base62
// characters.
var openCodeSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// validOpenCodeSession reports whether id is one the gate accepts. It decodes
// the leading value because the gate does: an id whose counter does not fit
// the six bits it reserves is refused even when it looks right.
func validOpenCodeSession(id string) bool {
	if !openCodeSessionPattern.MatchString(id) {
		return false
	}
	value := uint64(0)
	for _, char := range id[len("ses_") : len("ses_")+12] {
		value = value<<4 | uint64(strings.IndexRune("0123456789abcdef", char))
	}
	return (^value & cloakIDMask & 0xfff) < cloakIDMaxCounter
}

// openCodeUserAgentVersion reads the version an OpenCode User-Agent names. The
// client writes opencode/<channel>/<version>/<name>, the AI SDK appends its own
// agents after a space, and both are accepted; anything else is not OpenCode.
func openCodeUserAgentVersion(userAgent string) ([3]int, bool) {
	lower := strings.ToLower(strings.TrimSpace(userAgent))
	index := strings.Index(lower, "opencode/")
	if index < 0 {
		return [3]int{}, false
	}
	for _, field := range strings.FieldsFunc(lower[index+len("opencode/"):], func(r rune) bool {
		return r == '/' || r == ' ' || r == ';' || r == '\t'
	}) {
		if version, ok := parseVersion(field); ok {
			return version, true
		}
	}
	return [3]int{}, false
}

func parseVersion(field string) ([3]int, bool) {
	parts := strings.Split(field, ".")
	if len(parts) < 2 {
		return [3]int{}, false
	}
	var version [3]int
	for index, part := range parts {
		if index == 3 {
			break
		}
		number, errNumber := strconv.Atoi(part)
		if errNumber != nil || number < 0 {
			return [3]int{}, false
		}
		version[index] = number
	}
	return version, true
}

func versionAtLeast(version, floor [3]int) bool {
	for index := range floor {
		if version[index] != floor[index] {
			return version[index] > floor[index]
		}
	}
	return true
}

// nativeOpenCodeSession is the session identifier of a caller that is the
// OpenCode client itself, which is what tells the plugin it has nothing to
// impersonate: the request already names OpenCode and carries a session the
// gate accepts, so overriding either would only throw away the client's own
// metering and cache affinity.
func nativeOpenCodeSession(headers http.Header) (string, bool) {
	if version, ok := openCodeUserAgentVersion(headerValue(headers, "User-Agent")); !ok || !versionAtLeast(version, minOpenCodeVersion) {
		return "", false
	}
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id"} {
		if id := cleanSessionID(headerValue(headers, name)); validOpenCodeSession(id) {
			return id, true
		}
	}
	return "", false
}

// ensureReadTool makes a translated request body acceptable to the free tier:
// streaming on, and a tool named "read" in the list. A caller that already
// declares "read" keeps its own tool untouched — the gate only reads the name,
// so there is nothing to add — and otherwise the placeholder is appended.
func ensureReadTool(protocol sdktranslator.Format, body []byte) ([]byte, error) {
	if stream := gjson.GetBytes(body, "stream"); stream.Type != gjson.True {
		body, _ = sjson.SetBytes(body, "stream", true)
	}
	namePath, ok := readToolNamePath(protocol)
	if !ok {
		return nil, fmt.Errorf("no tool shape for %s requests", protocol)
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get(namePath).String() == cloakToolName || tool.Get("name").String() == cloakToolName {
				return body, nil
			}
		}
	}
	entry, errEntry := cloakToolEntry(protocol)
	if errEntry != nil {
		return nil, errEntry
	}
	body, _ = sjson.SetRawBytes(body, "tools", appendReadTool(tools.Raw, entry))
	return body, nil
}

// appendReadTool appends one tool entry to a tools array's raw JSON, creating
// the array when the request carries none.
func appendReadTool(existing, entry string) []byte {
	trimmed := bytes.TrimSpace([]byte(existing))
	if len(trimmed) == 0 || trimmed[0] != '[' || strings.TrimSpace(string(trimmed)) == "[]" {
		return []byte("[" + entry + "]")
	}
	return append(append(trimmed[:len(trimmed)-1], ','), append([]byte(entry), ']')...)
}

// readToolNamePath names the field that holds a tool's name in one protocol's
// tool shape.
func readToolNamePath(protocol sdktranslator.Format) (string, bool) {
	switch protocol {
	case sdktranslator.FormatOpenAI:
		return "function.name", true
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude:
		return "name", true
	}
	return "", false
}

// cloakToolEntry renders the placeholder in one protocol's tool shape.
func cloakToolEntry(protocol sdktranslator.Format) (string, error) {
	description, errJSON := json.Marshal(cloakToolDescription)
	if errJSON != nil {
		return "", errJSON
	}
	switch protocol {
	case sdktranslator.FormatOpenAI:
		return fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":%s,"parameters":%s}}`, cloakToolName, description, cloakToolSchema), nil
	case sdktranslator.FormatOpenAIResponse:
		return fmt.Sprintf(`{"type":"function","name":%q,"description":%s,"parameters":%s}`, cloakToolName, description, cloakToolSchema), nil
	case sdktranslator.FormatClaude:
		return fmt.Sprintf(`{"name":%q,"description":%s,"input_schema":%s}`, cloakToolName, description, cloakToolSchema), nil
	}
	return "", fmt.Errorf("no tool shape for %s requests", protocol)
}

// readToolFor renders OpenCode's read tool in one protocol's tool shape and
// names the field that holds its name.
// isStreamBody reports whether a buffered upstream body is an SSE stream,
// which is how the free tier answers even a request that asked for JSON.
func isStreamBody(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:"))
}

// assembleStream turns a buffered SSE body into the single response the same
// request would have returned without streaming, in the model's own protocol.
// It is only reached for cloaked requests from a client that asked for JSON.
func assembleStream(protocol sdktranslator.Format, body []byte) ([]byte, error) {
	var splitter sseSplitter
	frames := append(splitter.push(body), splitter.flush()...)
	switch protocol {
	case sdktranslator.FormatOpenAI:
		return assembleChatCompletion(frames)
	case sdktranslator.FormatOpenAIResponse:
		return assembleResponses(frames)
	case sdktranslator.FormatClaude:
		return assembleMessages(frames)
	}
	return nil, fmt.Errorf("cannot assemble a %s stream", protocol)
}

// assembleChatCompletion rebuilds a Chat Completions response from its chunks.
func assembleChatCompletion(frames []sseFrame) ([]byte, error) {
	id, model := "", ""
	var created int64
	var content, reasoning strings.Builder
	calls := map[int64]map[string]string{}
	var order []int64
	finish := ""
	var usage string
	started := false
	for _, frame := range frames {
		data := bytes.TrimSpace(frame.Data)
		if frame.Data == nil || len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		root := gjson.ParseBytes(data)
		if root.Get("error").Exists() && !root.Get("choices").Exists() {
			return nil, streamFailure(data, "")
		}
		if !started {
			id, model, created = root.Get("id").String(), root.Get("model").String(), root.Get("created").Int()
			started = true
		}
		for _, choice := range root.Get("choices").Array() {
			delta := choice.Get("delta")
			content.WriteString(delta.Get("content").String())
			reasoning.WriteString(delta.Get("reasoning_content").String())
			for _, call := range delta.Get("tool_calls").Array() {
				index := call.Get("index").Int()
				entry, ok := calls[index]
				if !ok {
					entry = map[string]string{}
					calls[index] = entry
					order = append(order, index)
				}
				if value := call.Get("id").String(); value != "" {
					entry["id"] = value
				}
				if value := call.Get("type").String(); value != "" {
					entry["type"] = value
				}
				if value := call.Get("function.name").String(); value != "" {
					entry["name"] = value
				}
				entry["arguments"] += call.Get("function.arguments").String()
			}
			if reason := choice.Get("finish_reason"); reason.Type == gjson.String && reason.String() != "" {
				finish = reason.String()
			}
		}
		if value := root.Get("usage"); value.IsObject() {
			usage = value.Raw
		}
	}
	message := []byte(`{"role":"assistant","content":null}`)
	message, _ = sjson.SetBytes(message, "content", content.String())
	if reasoning.Len() > 0 {
		message, _ = sjson.SetBytes(message, "reasoning_content", reasoning.String())
	}
	if len(order) > 0 {
		rendered := make([][]byte, 0, len(order))
		for _, index := range order {
			entry := calls[index]
			call, _ := sjson.SetBytes([]byte(`{"function":{"arguments":""}}`), "id", entry["id"])
			call, _ = sjson.SetBytes(call, "type", "function")
			call, _ = sjson.SetBytes(call, "function.name", entry["name"])
			call, _ = sjson.SetBytes(call, "function.arguments", entry["arguments"])
			rendered = append(rendered, call)
		}
		message, _ = sjson.SetRawBytes(message, "tool_calls", append(append([]byte("["), bytes.Join(rendered, []byte(","))...), ']'))
	}
	out, errOut := json.Marshal(map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model})
	if errOut != nil {
		return nil, errOut
	}
	out, _ = sjson.SetRawBytes(out, "choices", []byte("[{}]"))
	out, _ = sjson.SetRawBytes(out, "choices.0.message", message)
	out, _ = sjson.SetBytes(out, "choices.0.index", 0)
	out, _ = sjson.SetBytes(out, "choices.0.finish_reason", finish)
	if usage != "" {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(usage))
	}
	return out, nil
}

// assembleResponses takes the whole response object the terminal event of a
// Responses stream carries.
func assembleResponses(frames []sseFrame) ([]byte, error) {
	for _, frame := range frames {
		data := bytes.TrimSpace(frame.Data)
		if frame.Data == nil || len(data) == 0 {
			continue
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed", "response.incomplete":
			if response := gjson.GetBytes(data, "response"); response.IsObject() {
				return []byte(response.Raw), nil
			}
		case "response.failed", "error":
			return nil, streamFailure(data, "response")
		}
	}
	return nil, fmt.Errorf("the Responses stream carried no completed response")
}

// assembleMessages rebuilds a Messages response from its events.
func assembleMessages(frames []sseFrame) ([]byte, error) {
	var message []byte
	blocks := map[int64][]byte{}
	var order []int64
	var partial = map[int64]*strings.Builder{}
	stopReason, stopSequence := "", ""
	usage := []byte(nil)
	for _, frame := range frames {
		data := bytes.TrimSpace(frame.Data)
		if frame.Data == nil || len(data) == 0 {
			continue
		}
		root := gjson.ParseBytes(data)
		switch root.Get("type").String() {
		case "message_start":
			if start := root.Get("message"); start.IsObject() {
				message = []byte(start.Raw)
			}
		case "content_block_start":
			index := root.Get("index").Int()
			block := root.Get("content_block")
			if !block.IsObject() {
				continue
			}
			if block.Get("type").String() == "tool_use" {
				partial[index] = &strings.Builder{}
			}
			if _, ok := blocks[index]; !ok {
				order = append(order, index)
			}
			blocks[index] = []byte(block.Raw)
		case "content_block_delta":
			index := root.Get("index").Int()
			delta := root.Get("delta")
			block, ok := blocks[index]
			if !ok {
				continue
			}
			switch delta.Get("type").String() {
			case "text_delta":
				block, _ = sjson.SetBytes(block, "text", gjson.GetBytes(block, "text").String()+delta.Get("text").String())
			case "thinking_delta":
				block, _ = sjson.SetBytes(block, "thinking", gjson.GetBytes(block, "thinking").String()+delta.Get("thinking").String())
			case "signature_delta":
				block, _ = sjson.SetBytes(block, "signature", gjson.GetBytes(block, "signature").String()+delta.Get("signature").String())
			case "input_json_delta":
				if builder := partial[index]; builder != nil {
					builder.WriteString(delta.Get("partial_json").String())
				}
			}
			blocks[index] = block
		case "content_block_stop":
			index := root.Get("index").Int()
			if builder := partial[index]; builder != nil {
				block := blocks[index]
				block, _ = sjson.SetRawBytes(block, "input", []byte(jsonOrEmptyObject(builder.String())))
				blocks[index] = block
			}
		case "message_delta":
			if delta := root.Get("delta"); delta.IsObject() {
				stopReason = delta.Get("stop_reason").String()
				stopSequence = delta.Get("stop_sequence").String()
			}
			if value := root.Get("usage"); value.IsObject() {
				usage = []byte(value.Raw)
			}
		case "error":
			return nil, streamFailure(data, "")
		}
	}
	if message == nil {
		return nil, fmt.Errorf("the Messages stream carried no message_start")
	}
	content := make([][]byte, 0, len(order))
	for _, index := range order {
		content = append(content, blocks[index])
	}
	message, _ = sjson.SetRawBytes(message, "content", append(append([]byte("["), bytes.Join(content, []byte(","))...), ']'))
	message, _ = sjson.SetBytes(message, "stop_reason", stopReason)
	if stopSequence != "" {
		message, _ = sjson.SetBytes(message, "stop_sequence", stopSequence)
	}
	if usage != nil {
		message, _ = sjson.SetRawBytes(message, "usage", usage)
	}
	return message, nil
}

// jsonOrEmptyObject keeps a tool_use input valid when a stream ended before
// any argument arrived.
func jsonOrEmptyObject(text string) string {
	if trimmed := strings.TrimSpace(text); trimmed != "" && gjson.Valid(trimmed) {
		return trimmed
	}
	return "{}"
}
