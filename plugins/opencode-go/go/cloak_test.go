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
	value := ^uint64(0) // the gate inverts the leading bytes back
	value = 0
	for _, char := range id[len("ses_") : len("ses_")+12] {
		digit := strings.IndexRune("0123456789abcdef", char)
		if digit < 0 {
			t.Fatalf("id %q has a non-hex leading byte %q", id, char)
		}
		value = value<<4 | uint64(digit)
	}
	value = ^value & cloakIDMask
	if counter := value & 0xfff; counter != cloakIDCounter {
		t.Fatalf("counter = %d, want %d", counter, cloakIDCounter)
	}
	// The identifier keeps only six bytes of the value, so the gate can read
	// the timestamp back modulo that width.
	if stamp := int64(value >> 12); stamp != now.UnixMilli()&(1<<36-1) {
		t.Fatalf("timestamp = %d, want %d", stamp, now.UnixMilli()&(1<<36-1))
	}
	for _, char := range id[len("ses_")+12:] {
		if !strings.ContainsRune(cloakIDAlphabet, char) {
			t.Fatalf("id %q has a character outside the alphabet: %q", id, char)
		}
	}
	second, _ := mintOpenCodeID(now)
	if second == id {
		t.Fatal("two ids for one millisecond are identical")
	}
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
}

func TestEnsureReadToolPerProtocol(t *testing.T) {
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
			out, errTool := ensureReadTool(tt.protocol, []byte(tt.body))
			if errTool != nil {
				t.Fatal(errTool)
			}
			if got := gjson.GetBytes(out, "tools.0.function.name").String() + gjson.GetBytes(out, "tools.0.name").String(); got != "bash" {
				t.Fatalf("the client tool was disturbed: %s", out)
			}
			if got := gjson.GetBytes(out, tt.namePath).String(); got != cloakToolName {
				t.Fatalf("%s = %q in %s", tt.namePath, got, out)
			}
			// The placeholder tells the model what it is, and declares the
			// least schema a backend will validate.
			if got := gjson.GetBytes(out, tt.namePath[:strings.LastIndex(tt.namePath, ".")]+".description").String(); got != cloakToolDescription {
				t.Fatalf("description = %q", got)
			}
			if !strings.Contains(gjson.GetBytes(out, tt.schema).Raw, `"type":"object"`) {
				t.Fatalf("%s = %s", tt.schema, gjson.GetBytes(out, tt.schema).Raw)
			}
			if gjson.GetBytes(out, "stream").Type != gjson.True {
				t.Fatalf("stream was not turned on: %s", out)
			}
			// A body that already carries the tool is left byte for byte alone.
			again, errAgain := ensureReadTool(tt.protocol, out)
			if errAgain != nil || string(again) != string(out) {
				t.Fatalf("second pass changed the body:\n%s\n%s", out, again)
			}
		})
	}
}

// A caller that already declares "read" keeps it: the gate reads the name
// alone, so the caller's own tool is left exactly as it sent it.
func TestEnsureReadToolKeepsAClientRead(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"stream":true,"tools":[{"type":"function","function":{"name":"read","description":"mine","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}}]}`)
	out, errTool := ensureReadTool(sdktranslator.FormatOpenAI, body)
	if errTool != nil {
		t.Fatal(errTool)
	}
	if string(out) != string(body) {
		t.Fatalf("the client tool was rewritten:\n%s\n%s", body, out)
	}
}

func TestEnsureReadToolTurnsStreamingOn(t *testing.T) {
	out, errTool := ensureReadTool(sdktranslator.FormatOpenAI, []byte(`{"model":"m","messages":[],"stream":false}`))
	if errTool != nil {
		t.Fatal(errTool)
	}
	if gjson.GetBytes(out, "stream").Type != gjson.True {
		t.Fatalf("stream = %s", gjson.GetBytes(out, "stream").Raw)
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != cloakToolName {
		t.Fatalf("tools = %s", gjson.GetBytes(out, "tools").Raw)
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
	// The gate decodes the leading value and refuses a counter that does not
	// fit its six bits, so the check does too: an id whose inverted low bits
	// are zero decodes to the largest counter.
	over := minted[:len("ses_")+9] + "000" + minted[len("ses_")+12:]
	if validOpenCodeSession(over) {
		t.Fatalf("an oversized counter passed: %q", over)
	}
	for _, id := range []string{"", "ses_short", "cpa-0123456789abcdef", "ses_" + strings.Repeat("z", 26), minted[:len(minted)-1]} {
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
	if got := gjson.GetBytes(free.Body, "tools.0.function.name").String(); got != cloakToolName {
		t.Fatalf("tools = %s", gjson.GetBytes(free.Body, "tools").Raw)
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
	if len(tools) != 3 {
		t.Fatalf("tools = %s", gjson.GetBytes(sent[0].Body, "tools").Raw)
	}
	for index, want := range []string{"shell", "apply_patch", cloakToolName} {
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
	// The gate still needs the tool, and the client sent none.
	if got := gjson.GetBytes(sent[0].Body, "tools.0.function.name").String(); got != cloakToolName {
		t.Fatalf("tools = %s", gjson.GetBytes(sent[0].Body, "tools").Raw)
	}
}
