package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestMintOpenCodeID(t *testing.T) {
	now := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	id, errMint := mintOpenCodeID(now)
	if errMint != nil {
		t.Fatal(errMint)
	}
	if !strings.HasPrefix(id, "ses_") || len(id) != len("ses_")+26 {
		t.Fatalf("id = %q", id)
	}
	// The leading value is the bit-inverted timestamp, as OpenCode lays one
	// out: inverted back, its high field reads as the millisecond.
	value := leadingIDValue(t, id)
	if stamp := int64(value >> 12); stamp != now.UnixMilli()&(1<<36-1) {
		t.Fatalf("timestamp = %d, want %d", stamp, now.UnixMilli()&(1<<36-1))
	}
	for _, char := range id[len("ses_")+12:] {
		if !strings.ContainsRune(cloakIDAlphabet, char) {
			t.Fatalf("id %q has a character outside the alphabet: %q", id, char)
		}
	}
	// The low field counts the ids minted in that millisecond, so the next
	// one for the same instant leads with the next value, and is a different
	// id besides.
	second, _ := mintOpenCodeID(now)
	if second == id {
		t.Fatal("two ids for one millisecond are identical")
	}
	if next := leadingIDValue(t, second); next != value+1 {
		t.Fatalf("the count did not advance: %d then %d", value&0xfff, next&0xfff)
	}
	// The third, on a clock that has moved, counts from the start again.
	next, _ := mintOpenCodeID(now.Add(time.Second))
	if counter := leadingIDValue(t, next) & 0xfff; counter != 1 {
		t.Fatalf("a new millisecond counts from %d", counter)
	}
}

// leadingIDValue reads an identifier's twelve leading hex digits back into
// the value they were minted from.
func leadingIDValue(t *testing.T, id string) uint64 {
	t.Helper()
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("id %q has no ses_ prefix", id)
	}
	value := uint64(0)
	for _, char := range id[len("ses_") : len("ses_")+12] {
		digit := strings.IndexRune("0123456789abcdef", char)
		if digit < 0 {
			t.Fatalf("id %q has a non-hex leading byte %q", id, char)
		}
		value = value<<4 | uint64(digit)
	}
	return ^value & cloakIDMask
}

func TestCloakSessionsAreStableAndBounded(t *testing.T) {
	store := newCloakSessionStore()
	now := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	first := store.id("conversation-a")
	if first != store.id("conversation-a") {
		t.Fatal("one conversation got two session ids")
	}
	if first == store.id("conversation-b") {
		t.Fatal("two conversations share a session id")
	}
	for index := 0; index < cloakSessionKeep*2; index++ {
		store.id(strings.Repeat("x", index+1))
	}
	if len(store.ids) > cloakSessionKeep {
		t.Fatalf("the store kept %d sessions, want at most %d", len(store.ids), cloakSessionKeep)
	}
	if store.order.Len() != len(store.ids) {
		t.Fatalf("the store's order holds %d of its %d sessions", store.order.Len(), len(store.ids))
	}
}

// A conversation that is still having turns keeps the id it began with even
// as far more than cloakSessionKeep others come and go: what is forgotten is
// the least recently used, not the longest standing.
func TestCloakSessionsForgetTheLeastRecentlyUsed(t *testing.T) {
	store := newCloakSessionStore()
	busy := store.id("busy")
	quiet := store.id("quiet")
	for index := 0; index < cloakSessionKeep*2; index++ {
		store.id(strings.Repeat("y", index+1))
		if got := store.id("busy"); got != busy {
			t.Fatalf("the busy conversation's session changed at %d: %q, was %q", index, got, busy)
		}
	}
	if got := store.id("busy"); got != busy {
		t.Fatalf("the busy conversation's session is %q, was %q", got, busy)
	}
	// One that stopped talking is gone by now, and comes back with a new one.
	if got := store.id("quiet"); got == quiet {
		t.Fatal("a conversation that stopped talking kept its session")
	}
}

// A caller that offers only one of OpenCode's tools is offered the other as
// a placeholder: its own tool untouched, the placeholder carrying OpenCode's
// own parameters for the tool it stands in for rather than an empty schema,
// which the gate accepted when it was last reproduced but no client of
// OpenCode's ever sent.
func TestCloakBodyPerProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol sdktranslator.Format
		body     string
		namePath string
		schema   string
	}{
		{"chat", sdktranslator.FormatOpenAI, `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"bash","description":"d","parameters":{}}}]}`, "tools.1.function.name", "tools.1.function.parameters"},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"function","name":"bash","description":"d","parameters":{}}]}`, "tools.1.name", "tools.1.parameters"},
		{"messages", sdktranslator.FormatClaude, `{"model":"m","messages":[],"tools":[{"name":"bash","description":"d","input_schema":{}}]}`, "tools.1.name", "tools.1.input_schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, tools, errTool := cloakBody(tt.protocol, []byte(tt.body))
			if errTool != nil {
				t.Fatal(errTool)
			}
			// Its own bash is the caller's to keep; only read was missing.
			if !tools.stubs[cloakToolName] || tools.stubs[cloakBashName] || len(tools.back) != 0 {
				t.Fatalf("a caller with bash and no read was recorded as %v", tools)
			}
			if got := gjson.GetBytes(out, "tools.0.function.name").String() + gjson.GetBytes(out, "tools.0.name").String(); got != "bash" {
				t.Fatalf("the client tool was disturbed: %s", out)
			}
			if got := gjson.GetBytes(out, tt.namePath).String(); got != cloakToolName {
				t.Fatalf("%s = %q in %s", tt.namePath, got, out)
			}
			// The placeholder tells the model what it is, and declares the
			// parameters of OpenCode's own read.
			if got := gjson.GetBytes(out, tt.namePath[:strings.LastIndex(tt.namePath, ".")]+".description").String(); got != cloakToolDescription {
				t.Fatalf("description = %q", got)
			}
			if got := gjson.GetBytes(out, tt.schema+".properties.filePath.type").String(); got != "string" {
				t.Fatalf("%s = %s", tt.schema, gjson.GetBytes(out, tt.schema).Raw)
			}
			if gjson.GetBytes(out, "stream").Type != gjson.True {
				t.Fatalf("stream was not turned on: %s", out)
			}
			// A body already offering both is left byte for byte alone.
			again, tools, errAgain := cloakBody(tt.protocol, out)
			if errAgain != nil || string(again) != string(out) || !tools.untouched() {
				t.Fatalf("second pass changed the body:\n%s\n%s (%v)", out, again, tools)
			}
		})
	}
}

// A caller with no tool at all is offered both of OpenCode's, in its order —
// the pair a session of its client opens with, of which read alone is what
// the gate checks for — each with that tool's own parameters and each
// described as not to be called, since neither is the caller's to run.
func TestCloakBodyOffersBashAndRead(t *testing.T) {
	tests := []struct {
		name     string
		protocol sdktranslator.Format
		body     string
		names    string
		schemas  [2]string
	}{
		{"chat", sdktranslator.FormatOpenAI, `{"model":"m","messages":[]}`, "bash,read", [2]string{"tools.0.function.parameters", "tools.1.function.parameters"}},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi"}`, "bash,read", [2]string{"tools.0.parameters", "tools.1.parameters"}},
		{"messages", sdktranslator.FormatClaude, `{"model":"m","messages":[]}`, "bash,read", [2]string{"tools.0.input_schema", "tools.1.input_schema"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, tools, errTool := cloakBody(tt.protocol, []byte(tt.body))
			if errTool != nil {
				t.Fatal(errTool)
			}
			if !tools.stubs[cloakBashName] || !tools.stubs[cloakToolName] {
				t.Fatalf("a caller with no tools was recorded as %v", tools)
			}
			if got := toolsOf(t, out); got != tt.names {
				t.Fatalf("tools = %s in %s", got, out)
			}
			if got := gjson.GetBytes(out, tt.schemas[0]+".properties.command.type").String(); got != "string" {
				t.Fatalf("bash takes no command: %s", gjson.GetBytes(out, tt.schemas[0]).Raw)
			}
			if got := gjson.GetBytes(out, tt.schemas[1]+".properties.filePath.type").String(); got != "string" {
				t.Fatalf("read takes no filePath: %s", gjson.GetBytes(out, tt.schemas[1]).Raw)
			}
			for index := range 2 {
				base := tt.schemas[index][:strings.LastIndex(tt.schemas[index], ".")]
				if got := gjson.GetBytes(out, base+".description").String(); got != cloakToolDescription {
					t.Fatalf("placeholder %d is described as a capability: %q", index, got)
				}
			}
		})
	}
}

// A caller that already declares both tools under OpenCode's names, as Pi and
// OpenCode itself do, keeps them exactly as it sent them: the gate reads the
// names alone, so there is nothing to add and nothing to rename.
func TestCloakBodyKeepsAClientRead(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"stream":true,"tools":[{"type":"function","function":{"name":"read","description":"mine","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}},{"type":"function","function":{"name":"bash","description":"mine too","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`)
	out, tools, errTool := cloakBody(sdktranslator.FormatOpenAI, body)
	if errTool != nil {
		t.Fatal(errTool)
	}
	if string(out) != string(body) || !tools.untouched() {
		t.Fatalf("the client tools were rewritten:\n%s\n%s (%v)", body, out, tools)
	}
}

// A caller that declares the same tools in another case, as Claude Code's
// Bash and Read are, has each offered under the name OpenCode's carries —
// renamed, not joined by a placeholder that tells the model the tool cannot
// be called — and nothing else of either tool, or of the tools around them,
// is touched.
func TestCloakBodyRenamesACapitalizedRead(t *testing.T) {
	tests := []struct {
		name     string
		protocol sdktranslator.Format
		body     string
		names    string
	}{
		{"chat", sdktranslator.FormatOpenAI, `{"model":"m","messages":[],"stream":true,"tools":[{"type":"function","function":{"name":"Read","description":"read a file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}},{"type":"function","function":{"name":"Bash","description":"run","parameters":{}}}]}`, "read,bash"},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","stream":true,"tools":[{"type":"function","name":"READ","description":"read a file","parameters":{"type":"object"}},{"type":"function","name":"BASH","description":"run","parameters":{}}]}`, "read,bash"},
		{"messages", sdktranslator.FormatClaude, `{"model":"m","messages":[],"stream":true,"tools":[{"name":"Bash","description":"run","input_schema":{}},{"name":"Read","description":"read a file","input_schema":{"type":"object"}}]}`, "bash,read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, tools, errTool := cloakBody(tt.protocol, []byte(tt.body))
			if errTool != nil {
				t.Fatal(errTool)
			}
			if len(tools.back) != 2 || len(tools.stubs) != 0 {
				t.Fatalf("two capitalized tools were recorded as %v, want both renamed", tools)
			}
			if got := toolsOf(t, out); got != tt.names {
				t.Fatalf("tools = %s in %s", got, out)
			}
			// The caller's word for each is the one it declared, so a reply's
			// calls of it go back under it.
			for offered, own := range tools.back {
				if own == offered {
					t.Fatalf("%s was recorded as renamed to itself", offered)
				}
			}
			if got := tools.back[cloakToolName]; !strings.EqualFold(got, "read") || got == cloakToolName {
				t.Fatalf("the caller's read is recorded as %q", got)
			}
			for _, tool := range gjson.GetBytes(out, "tools").Array() {
				if tool.Get("function.description").String()+tool.Get("description").String() == cloakToolDescription {
					t.Fatalf("a placeholder joined tools the caller has: %s", tool.Raw)
				}
			}
		})
	}
	// A tool that is already "read" wins over another case of the same name,
	// which is left alone, and only the absent bash is added.
	body := []byte(`{"model":"m","messages":[],"stream":true,"tools":[{"name":"Read","description":"a","input_schema":{}},{"name":"read","description":"b","input_schema":{}}]}`)
	out, tools, errTool := cloakBody(sdktranslator.FormatClaude, body)
	if errTool != nil {
		t.Fatal(errTool)
	}
	if got := toolsOf(t, out); got != "Read,read,bash" {
		t.Fatalf("tools = %s in %s", got, out)
	}
	if len(tools.back) != 0 || !tools.stubs[cloakBashName] || tools.stubs[cloakToolName] {
		t.Fatalf("recorded as %v", tools)
	}
}

func TestCloakBodyTurnsStreamingOn(t *testing.T) {
	out, tools, errTool := cloakBody(sdktranslator.FormatOpenAI, []byte(`{"model":"m","messages":[],"stream":false}`))
	if errTool != nil {
		t.Fatal(errTool)
	}
	if !tools.stubs[cloakBashName] || !tools.stubs[cloakToolName] {
		t.Fatalf("a request with no tools at all was recorded as %v", tools)
	}
	if gjson.GetBytes(out, "stream").Type != gjson.True {
		t.Fatalf("stream = %s", gjson.GetBytes(out, "stream").Raw)
	}
	if got := toolsOf(t, out); got != "bash,read" {
		t.Fatalf("tools = %s in %s", got, out)
	}
}

func TestOpenCodeUserAgentVersion(t *testing.T) {
	tests := []struct {
		userAgent string
		ok        bool
		atLeast   bool
	}{
		{"opencode/1.19.0", true, true},
		{"opencode/latest/2.0.29/cli", true, true},
		{"OpenCode/1.18.0", true, true},
		{"opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/node.js/24", true, true},
		{"opencode/1.17.9", true, false},
		{"curl/8.0", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		version, ok := openCodeUserAgentVersion(tt.userAgent)
		if ok != tt.ok {
			t.Fatalf("%q parsed = %v", tt.userAgent, ok)
		}
		if ok && versionAtLeast(version, minOpenCodeVersion) != tt.atLeast {
			t.Fatalf("%q version = %v", tt.userAgent, version)
		}
	}
}

func TestValidOpenCodeSession(t *testing.T) {
	now := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	minted, errMint := mintOpenCodeID(now)
	if errMint != nil {
		t.Fatal(errMint)
	}
	if !validOpenCodeSession(minted) {
		t.Fatalf("a minted id was refused: %q", minted)
	}
	// The gate checks the shape and nothing inside it: a leading value that
	// is not the inverted timestamp OpenCode lays out, is all zeros, or
	// decodes to a count wider than its client ever counts to was answered
	// live, so a session that is shaped right is kept rather than replaced,
	// which is what keeps a native caller's metering and cache affinity.
	base62tail := strings.Repeat("A", 14)
	for _, id := range []string{
		"ses_" + strings.Repeat("0", 12) + base62tail,
		"ses_" + strings.Repeat("f", 12) + base62tail,
		minted[:len("ses_")+9] + "000" + minted[len("ses_")+12:],
	} {
		if !validOpenCodeSession(id) {
			t.Fatalf("a well-shaped id was refused: %q", id)
		}
	}
	// What is not shaped right still fails: hex in the other twelve
	// characters and a client hash among them.
	for _, id := range []string{"", "ses_short", "cpa-0123456789abcdef", "ses_" + strings.Repeat("z", 26), minted[:len(minted)-1], "ses_" + minted[len(minted)-14:] + minted[len("ses_"):len("ses_")+12]} {
		if validOpenCodeSession(id) {
			t.Fatalf("%q passed", id)
		}
	}
}

// A caller that is OpenCode itself keeps its own user agent and session.
func TestNativeOpenCodeSession(t *testing.T) {
	now := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	minted, _ := mintOpenCodeID(now)
	headers := http.Header{}
	headers.Set("User-Agent", "opencode/latest/2.0.29/cli")
	headers.Set("x-opencode-session", minted)
	if id, ok := nativeOpenCodeSession(headers); !ok || id != minted {
		t.Fatalf("native = %q %v", id, ok)
	}
	headers.Set("User-Agent", "curl/8.0")
	if _, ok := nativeOpenCodeSession(headers); ok {
		t.Fatal("a non-OpenCode user agent was treated as native")
	}
	headers.Set("User-Agent", "opencode/1.19.0")
	headers.Set("x-opencode-session", "cpa-not-an-id")
	if _, ok := nativeOpenCodeSession(headers); ok {
		t.Fatal("a hashed session was treated as native")
	}
	// The client may name the session with either header.
	headers.Del("x-opencode-session")
	headers.Set("x-opencode-session-id", minted)
	if id, ok := nativeOpenCodeSession(headers); !ok || id != minted {
		t.Fatalf("session-id header = %q %v", id, ok)
	}
}

func TestAssembleStream(t *testing.T) {
	chat, errChat := assembleStream(sdktranslator.FormatOpenAI, readFixture(t, "chat_stream.txt"))
	if errChat != nil {
		t.Fatal(errChat)
	}
	if got := gjson.GetBytes(chat, "object").String(); got != "chat.completion" {
		t.Fatalf("chat object = %q", got)
	}
	if got := gjson.GetBytes(chat, "choices.0.message.content").String(); got != "OK" {
		t.Fatalf("chat content = %q", got)
	}
	if gjson.GetBytes(chat, "choices.0.message.reasoning_content").String() == "" {
		t.Fatal("chat reasoning was dropped")
	}
	if got := gjson.GetBytes(chat, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("chat finish = %q", got)
	}
	if got := gjson.GetBytes(chat, "usage.total_tokens").Int(); got != 48 {
		t.Fatalf("chat usage = %s", gjson.GetBytes(chat, "usage").Raw)
	}

	responses, errResponses := assembleStream(sdktranslator.FormatOpenAIResponse, readFixture(t, "responses_stream.txt"))
	if errResponses != nil {
		t.Fatal(errResponses)
	}
	if got := gjson.GetBytes(responses, "status").String(); got != "completed" {
		t.Fatalf("responses status = %q", got)
	}
	if got := gjson.GetBytes(responses, "usage.total_tokens").Int(); got != 25 {
		t.Fatalf("responses usage = %s", gjson.GetBytes(responses, "usage").Raw)
	}
	if !strings.Contains(string(responses), `"OK"`) {
		t.Fatalf("responses lost its text: %s", responses)
	}

	messages, errMessages := assembleStream(sdktranslator.FormatClaude, readFixture(t, "messages_stream.txt"))
	if errMessages != nil {
		t.Fatal(errMessages)
	}
	if got := gjson.GetBytes(messages, "type").String(); got != "message" {
		t.Fatalf("messages type = %q", got)
	}
	if got := gjson.GetBytes(messages, "stop_reason").String(); got != "max_tokens" {
		t.Fatalf("messages stop reason = %q", got)
	}
	if got := gjson.GetBytes(messages, "content.0.type").String(); got != "thinking" || gjson.GetBytes(messages, "content.0.thinking").String() == "" {
		t.Fatalf("messages content = %s", gjson.GetBytes(messages, "content").Raw)
	}
	if got := gjson.GetBytes(messages, "usage.output_tokens").Int(); got != 32 {
		t.Fatalf("messages usage = %s", gjson.GetBytes(messages, "usage").Raw)
	}
}

// A free Zen model reaches the gateway as the OpenCode client, and its SSE
// answer is folded back into the JSON a non-streaming client asked for.
func TestExecutorCloaksFreeZenModels(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"cat-jam-free", "kimi-k3"}
	useSettings(t, cfg)
	useTestCatalog(t)
	stream := readFixture(t, "chat_stream.txt")
	gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply {
		return gatewayReply{Status: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}
	})

	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode/cat-jam-free", Payload: clientPayload("openai", "opencode/cat-jam-free", false)}.raw(t))
	resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if got := parseClientResponse(t, "openai", resp.Payload); got.Text != "OK" {
		t.Fatalf("folded response = %+v", got)
	}
	sent := gateway.inferenceRequests()
	if len(sent) != 1 {
		t.Fatalf("requests = %d", len(sent))
	}
	free := sent[0]
	if free.Headers.Get("User-Agent") != cloakUserAgent {
		t.Fatalf("user agent = %q", free.Headers.Get("User-Agent"))
	}
	session := free.Headers.Get("x-opencode-session")
	if !strings.HasPrefix(session, "ses_") || len(session) != len("ses_")+26 {
		t.Fatalf("session = %q", session)
	}
	if gjson.GetBytes(free.Body, "stream").Type != gjson.True {
		t.Fatalf("stream = %s", gjson.GetBytes(free.Body, "stream").Raw)
	}
	// Both of OpenCode's tools, in its order, and the identity whole: the
	// session named twice, a request identifier for the turn, its client and
	// project — and no header the client does not send, which a request
	// carrying only the least that is answered would be marked by.
	if got := toolsOf(t, free.Body); got != "bash,read" {
		t.Fatalf("tools = %s in %s", got, free.Body)
	}
	if free.Headers.Get("x-opencode-session-id") != session {
		t.Fatalf("session-id = %q, want the session %q", free.Headers.Get("x-opencode-session-id"), session)
	}
	if request := free.Headers.Get("x-opencode-request"); !strings.HasPrefix(request, "msg_") || len(request) != len("msg_")+26 {
		t.Fatalf("request id = %q", request)
	}
	if free.Headers.Get("x-opencode-client") != "cli" || free.Headers.Get("x-opencode-project") != "global" {
		t.Fatalf("client/project = %q/%q", free.Headers.Get("x-opencode-client"), free.Headers.Get("x-opencode-project"))
	}
	if free.Headers.Get("X-Session-Affinity") != "" || free.Headers.Get("X-Session-Id") != "" || free.Headers.Get("Referer") != "" {
		t.Fatalf("headers the client does not send went upstream: %v", free.Headers)
	}
	if got := gjson.GetBytes(free.Body, "tools.0.function.description").String(); got != cloakToolDescription {
		t.Fatalf("the placeholder is described as a capability: %q", got)
	}
	if got := gjson.GetBytes(free.Body, "model").String(); got != "cat-jam-free" {
		t.Fatalf("model = %q", got)
	}

	// A paid Zen model is not cloaked: it is served to any caller, and the
	// cloak would only add a tool the client never asked for.
	raw, _ = executorExecute(testCall{Source: "openai", Model: "opencode/kimi-k3", Payload: clientPayload("openai", "opencode/kimi-k3", false)}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	paid := gateway.inferenceRequests()[1]
	if paid.Headers.Get("User-Agent") == cloakUserAgent {
		t.Fatalf("a paid model was cloaked: %v", paid.Headers)
	}
	if gjson.GetBytes(paid.Body, "tools").Exists() {
		t.Fatalf("a paid request gained tools: %s", paid.Body)
	}
}

// Turning the cloak off leaves free models exactly as the gateway sees any
// other client.
func TestExecutorCloakCanBeTurnedOff(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"cat-jam-free"}
	cfg.CloakFreeModels = false
	useSettings(t, cfg)
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode/cat-jam-free", Payload: clientPayload("openai", "opencode/cat-jam-free", false)}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	sent := gateway.inferenceRequests()
	if len(sent) != 1 || sent[0].Headers.Get("User-Agent") == cloakUserAgent || gjson.GetBytes(sent[0].Body, "tools").Exists() {
		t.Fatalf("request = %+v", sent)
	}
}

// The placeholder never displaces a caller's own tools: a Codex request keeps
// its shell and apply_patch, and a harness that declares its own file reader
// keeps that too. Only the name the gate looks for is added.
func TestExecutorCloakKeepsCallerTools(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"cat-jam-free"}
	useSettings(t, cfg)
	useTestCatalog(t)
	gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply {
		return gatewayReply{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: readFixture(t, "chat_response.json")}
	})
	caller := `[{"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}},{"type":"function","name":"apply_patch","description":"Edit a file","parameters":{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"]}}]`
	payload := `{"model":"opencode/cat-jam-free","instructions":"be brief","input":"read /etc/hostname","tools":` + caller + `}`
	raw, _ := executorExecute(testCall{Source: "openai-response", Model: "opencode/cat-jam-free", Payload: payload}.raw(t))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("error %d: %s", env.Error.HTTPStatus, env.Error.Message)
	}
	sent := gateway.inferenceRequests()
	if len(sent) != 1 {
		t.Fatalf("requests = %d", len(sent))
	}
	tools := gjson.GetBytes(sent[0].Body, "tools").Array()
	if len(tools) != 4 {
		t.Fatalf("tools = %s", gjson.GetBytes(sent[0].Body, "tools").Raw)
	}
	for index, want := range []string{"shell", "apply_patch", cloakBashName, cloakToolName} {
		if got := tools[index].Get("function.name").String(); got != want {
			t.Fatalf("tool %d = %q, want %q", index, got, want)
		}
	}
	if got := tools[0].Get("function.description").String(); got != "Run a command" {
		t.Fatalf("the caller's tool was rewritten: %s", tools[0].Raw)
	}
}

// A caller that is OpenCode keeps its own user agent and session: the gate
// already accepts them, and minting a new session would throw away the
// client's metering and cache affinity.
func TestExecutorKeepsNativeOpenCodeIdentity(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"cat-jam-free"}
	useSettings(t, cfg)
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	native, errMint := mintOpenCodeID(time.Now())
	if errMint != nil {
		t.Fatal(errMint)
	}
	headers := http.Header{}
	headers.Set("User-Agent", "opencode/latest/2.0.29/cli")
	headers.Set("x-opencode-session", native)
	raw, _ := executorExecute(testCall{
		Source:  "openai",
		Model:   "opencode/cat-jam-free",
		Payload: clientPayload("openai", "opencode/cat-jam-free", false),
		Headers: headers,
	}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	sent := gateway.inferenceRequests()
	if len(sent) != 1 {
		t.Fatalf("requests = %d", len(sent))
	}
	if got := sent[0].Headers.Get("User-Agent"); got != "opencode/latest/2.0.29/cli" {
		t.Fatalf("user agent = %q", got)
	}
	if got := sent[0].Headers.Get("x-opencode-session"); got != native {
		t.Fatalf("session = %q, want the client's own %q", got, native)
	}
	// The gate still needs the tools, and the client sent none.
	if got := toolsOf(t, sent[0].Body); got != "bash,read" {
		t.Fatalf("tools = %s in %s", got, sent[0].Body)
	}
	// Its identity is its own, so nothing of the cloak's is put on it.
	if sent[0].Headers.Get("x-opencode-request") != "" || sent[0].Headers.Get("x-opencode-client") != "" {
		t.Fatalf("a native caller was given the cloak's identity: %v", sent[0].Headers)
	}
}
