package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// What the cloak did to a request's tool list is undone in its reply. These
// drive the filter directly, in the model's own protocol, buffered and
// streamed; cloak_test.go drives the whole executor with them.

// stubbedTools is the cloakTools of a request whose caller declared none of
// the named tools, so each went upstream as a placeholder.
func stubbedTools(names ...string) cloakTools {
	tools := cloakTools{back: map[string]string{}, stubs: map[string]bool{}}
	for _, name := range names {
		tools.stubs[name] = true
	}
	return tools
}

// renamedTools is the cloakTools of a request whose caller declared the named
// tool as own, which went upstream under the name before it.
func renamedTools(pairs ...string) cloakTools {
	tools := cloakTools{back: map[string]string{}, stubs: map[string]bool{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		tools.back[pairs[i]] = pairs[i+1]
	}
	return tools
}

// cloakReply is what a reply amounts to: what it said, and the calls it made
// as "name arguments" pairs, in order.
type cloakReply struct {
	text  string
	calls []string
	// stop is the turn's finish_reason or stop_reason.
	stop string
}

// bufferedReply reads a whole reply in one protocol.
func bufferedReply(t *testing.T, protocol sdktranslator.Format, body []byte) cloakReply {
	t.Helper()
	var out cloakReply
	switch protocol {
	case sdktranslator.FormatOpenAI:
		choice := gjson.GetBytes(body, "choices.0")
		out.text = choice.Get("message.content").String()
		out.stop = choice.Get("finish_reason").String()
		for _, call := range choice.Get("message.tool_calls").Array() {
			out.calls = append(out.calls, call.Get("function.name").String()+" "+call.Get("function.arguments").String())
		}
	case sdktranslator.FormatOpenAIResponse:
		for _, item := range gjson.GetBytes(body, "output").Array() {
			switch item.Get("type").String() {
			case "function_call":
				out.calls = append(out.calls, item.Get("name").String()+" "+item.Get("arguments").String())
			case "message":
				for _, part := range item.Get("content").Array() {
					out.text += part.Get("text").String()
				}
			}
		}
	case sdktranslator.FormatClaude:
		out.stop = gjson.GetBytes(body, "stop_reason").String()
		for _, block := range gjson.GetBytes(body, "content").Array() {
			switch block.Get("type").String() {
			case "tool_use":
				out.calls = append(out.calls, block.Get("name").String()+" "+block.Get("input").Raw)
			case "text":
				out.text += block.Get("text").String()
			}
		}
	default:
		t.Fatalf("no reader for %s replies", protocol)
	}
	return out
}

// streamedReply folds a filtered event stream in one protocol with the same
// assembler a buffered reply is rebuilt by, so a stream is read the way a
// client that asked for JSON would read it.
func streamedReply(t *testing.T, protocol sdktranslator.Format, events [][]byte) cloakReply {
	t.Helper()
	frames := make([]sseFrame, 0, len(events))
	for _, data := range events {
		frames = append(frames, sseFrame{Event: gjson.GetBytes(data, "type").String(), Data: data})
	}
	var body []byte
	var errAssemble error
	switch protocol {
	case sdktranslator.FormatOpenAI:
		body, errAssemble = assembleChatCompletion(frames)
	case sdktranslator.FormatOpenAIResponse:
		body, errAssemble = assembleResponses(frames)
	case sdktranslator.FormatClaude:
		body, errAssemble = assembleMessages(frames)
	default:
		t.Fatalf("no assembler for %s replies", protocol)
	}
	if errAssemble != nil {
		t.Fatalf("assemble the %s stream: %v", protocol, errAssemble)
	}
	return bufferedReply(t, protocol, body)
}

func checkReply(t *testing.T, got cloakReply, text, stop string, calls ...string) {
	t.Helper()
	if got.text != text {
		t.Errorf("text = %q, want %q", got.text, text)
	}
	if got.stop != stop {
		t.Errorf("stop = %q, want %q", got.stop, stop)
	}
	if strings.Join(got.calls, "|") != strings.Join(calls, "|") {
		t.Errorf("calls = %q, want %q", got.calls, calls)
	}
}

// chatReply is a Chat Completions body, whole and streamed, with text and
// calls interleaved as a gateway sends them: an unseen call is announced with
// its name and its arguments then arrive in fragments.
func chatReply(text string, calls ...[2]string) (body string, events []string) {
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	var rendered []string
	for index, call := range calls {
		rendered = append(rendered, fmt.Sprintf(`{"id":"call-%d","type":"function","function":{"name":%q,"arguments":%q}}`, index, call[0], call[1]))
	}
	toolCalls := ""
	if len(rendered) > 0 {
		toolCalls = `,"tool_calls":[` + strings.Join(rendered, ",") + `]`
	}
	body = fmt.Sprintf(`{"id":"chat-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q%s}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, finish, text, toolCalls)

	chunk := func(delta, finish string) string {
		reason := "null"
		if finish != "" {
			reason = fmt.Sprintf("%q", finish)
		}
		return fmt.Sprintf(`{"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":%s,"delta":%s}]}`, reason, delta)
	}
	events = append(events, chunk(`{"role":"assistant","content":""}`, ""))
	if text != "" {
		events = append(events, chunk(fmt.Sprintf(`{"content":%q}`, text), ""))
	}
	for index, call := range calls {
		events = append(events, chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"call-%d","type":"function","function":{"name":%q,"arguments":""}}]}`, index, index, call[0]), ""))
		events = append(events, chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]}`, index, call[1]), ""))
	}
	return body, append(events, chunk(`{}`, finish), "[DONE]")
}

// responsesReply is a Responses body, whole and streamed, whose items are the
// ones given as raw JSON.
func responsesReply(items ...string) (body string, events []string) {
	body = `{"id":"resp-1","object":"response","status":"completed","model":"m","output":[` + strings.Join(items, ",") + `],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	events = append(events, `{"type":"response.created","sequence_number":0,"response":{"id":"resp-1","object":"response","status":"in_progress","model":"m","output":[]}}`)
	sequence := 1
	for index, item := range items {
		root := gjson.Parse(item)
		events = append(events, fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":%s}`, sequence, index, item))
		sequence++
		switch root.Get("type").String() {
		case "function_call":
			events = append(events, fmt.Sprintf(`{"type":"response.function_call_arguments.delta","sequence_number":%d,"output_index":%d,"item_id":%q,"call_id":%q,"name":%q,"delta":%q}`, sequence, index, root.Get("id").String(), root.Get("call_id").String(), root.Get("name").String(), root.Get("arguments").String()))
			sequence++
			events = append(events, fmt.Sprintf(`{"type":"response.function_call_arguments.done","sequence_number":%d,"output_index":%d,"item_id":%q,"call_id":%q,"name":%q,"arguments":%q}`, sequence, index, root.Get("id").String(), root.Get("call_id").String(), root.Get("name").String(), root.Get("arguments").String()))
			sequence++
		case "message":
			for _, part := range root.Get("content").Array() {
				events = append(events, fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,"output_index":%d,"content_index":0,"item_id":%q,"delta":%q}`, sequence, index, root.Get("id").String(), part.Get("text").String()))
				sequence++
			}
		}
		events = append(events, fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":%s}`, sequence, index, item))
		sequence++
	}
	return body, append(events, fmt.Sprintf(`{"type":"response.completed","sequence_number":%d,"response":%s}`, sequence, body))
}

func responsesCall(id, name, arguments string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function_call","call_id":%q,"name":%q,"arguments":%q,"status":"completed"}`, id, id, name, arguments)
}

func responsesText(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[],"logprobs":[]}]}`, id, text)
}

// messagesReply is a Messages body, whole and streamed, whose blocks are the
// ones given as raw JSON.
func messagesReply(stop string, blocks ...string) (body string, events []string) {
	body = `{"id":"msg-1","type":"message","role":"assistant","model":"m","stop_reason":"` + stop + `","content":[` + strings.Join(blocks, ",") + `],"usage":{"input_tokens":1,"output_tokens":1}}`
	events = append(events, `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
	for index, block := range blocks {
		root := gjson.Parse(block)
		switch root.Get("type").String() {
		case "tool_use":
			events = append(events, fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, root.Get("id").String(), root.Get("name").String()))
			events = append(events, fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, index, root.Get("input").Raw))
		default:
			events = append(events, fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":%q,"text":""}}`, index, root.Get("type").String()))
			events = append(events, fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, root.Get("text").String()))
		}
		events = append(events, fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
	}
	events = append(events,
		`{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"type":"message_stop"}`)
	return body, events
}

func messagesCall(id, name, input string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, id, name, input)
}

func messagesText(text string) string {
	return fmt.Sprintf(`{"type":"text","text":%q}`, text)
}

// filter both shapes of one reply and check each.
func checkFiltered(t *testing.T, filter cloakFilter, body string, events []string, text, stop string, calls ...string) {
	t.Helper()
	checkReply(t, bufferedReply(t, filter.protocol, filter.buffered([]byte(body))), text, stop, calls...)
	stream := filter.streams()
	var out [][]byte
	for _, event := range events {
		out = append(out, stream.event([]byte(event))...)
	}
	checkReply(t, streamedReply(t, filter.protocol, out), text, stop, calls...)
}

// A caller with no read tool of its own was given the placeholder, so a call
// of it is of a tool it never declared: it is dropped from every protocol's
// reply, buffered and streamed, and a turn that called nothing else ends as
// a plain stop on what it said.
func TestCloakFilterDropsThePlaceholder(t *testing.T) {
	chat := newCloakFilter(sdktranslator.FormatOpenAI, stubbedTools("bash", "read"))
	body, events := chatReply("Let me look.", [2]string{"read", `{"filePath":"/etc/hostname"}`})
	checkFiltered(t, chat, body, events, "Let me look.", "stop")

	// Nothing said either: the client is told why rather than handed silence.
	body, events = chatReply("", [2]string{"read", `{}`})
	checkFiltered(t, chat, body, events, cloakStubNote, "stop")

	// Its own tools survive around it: the call is dropped, the turn's other
	// call and its stop stay as they were.
	body, events = chatReply("Running it.", [2]string{"read", `{"filePath":"/a"}`}, [2]string{"shell", `{"command":"ls"}`})
	checkFiltered(t, chat, body, events, "Running it.", "tool_calls", `shell {"command":"ls"}`)

	responses := newCloakFilter(sdktranslator.FormatOpenAIResponse, stubbedTools("bash", "read"))
	body, events = responsesReply(
		responsesText("msg-1", "Looking now."),
		responsesCall("call-1", "read", `{"filePath":"/a"}`),
		responsesCall("call-2", "apply_patch", `{"patch":"x"}`),
	)
	checkFiltered(t, responses, body, events, "Looking now.", "", `apply_patch {"patch":"x"}`)

	body, events = responsesReply(responsesCall("call-1", "read", `{}`))
	checkFiltered(t, responses, body, events, cloakStubNote, "")

	messages := newCloakFilter(sdktranslator.FormatClaude, stubbedTools("bash", "read"))
	body, events = messagesReply("tool_use",
		messagesText("Reading it."),
		messagesCall("toolu-1", "read", `{"filePath":"/a"}`),
	)
	checkFiltered(t, messages, body, events, "Reading it.", "end_turn")

	body, events = messagesReply("tool_use", messagesCall("toolu-1", "read", `{}`))
	checkFiltered(t, messages, body, events, cloakStubNote, "end_turn")
}

// A caller's file tool offered under another case was renamed for the gate,
// so its calls come back under the caller's own name, and the turn still
// stops as a tool turn.
func TestCloakFilterRestoresARenamedTool(t *testing.T) {
	chat := newCloakFilter(sdktranslator.FormatOpenAI, renamedTools("read", "Read"))
	body, events := chatReply("Let me look.", [2]string{"read", `{"filePath":"/a"}`}, [2]string{"bash", `{"command":"ls"}`})
	checkFiltered(t, chat, body, events, "Let me look.", "tool_calls", `Read {"filePath":"/a"}`, `bash {"command":"ls"}`)

	responses := newCloakFilter(sdktranslator.FormatOpenAIResponse, renamedTools("read", "READ"))
	body, events = responsesReply(responsesCall("call-1", "read", `{"filePath":"/a"}`))
	checkFiltered(t, responses, body, events, "", "", `READ {"filePath":"/a"}`)

	messages := newCloakFilter(sdktranslator.FormatClaude, renamedTools("read", "Read"))
	body, events = messagesReply("tool_use", messagesCall("toolu-1", "read", `{"file_path":"/a"}`))
	checkFiltered(t, messages, body, events, "", "tool_use", `Read {"file_path":"/a"}`)
}

// Calls stream in interleaved, not in order: a kept call's fragments arrive
// between the placeholder's, and a call's first fragment can carry no name at
// all. Each is decided on the index it carries, so the kept one keeps its
// whole argument and the dropped one loses nothing of the other.
func TestCloakFilterChatStreamInterleaved(t *testing.T) {
	filter := newCloakFilter(sdktranslator.FormatOpenAI, stubbedTools("bash", "read"))
	stream := filter.streams()
	events := []string{
		`{"id":"chat-1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"chat-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-0","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"id":"chat-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"comm"}}]}}]}`,
		`{"id":"chat-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"/a\"}"}}]}}]}`,
		`{"id":"chat-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-1","type":"function","function":{"name":"shell","arguments":"and\":\"ls\"}"}}]}}]}`,
		`{"id":"chat-1","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"[DONE]",
	}
	var out [][]byte
	for _, event := range events {
		out = append(out, stream.event([]byte(event))...)
	}
	if strings.Contains(fmt.Sprint(out), `"read"`) {
		t.Fatalf("the placeholder reached the caller: %s", out)
	}
	// The kept call is renumbered into first place, and its argument began
	// arriving before its name did, so the fragments have to meet.
	frames := make([]sseFrame, 0, len(out))
	for _, event := range out {
		for _, choice := range gjson.GetBytes(event, "choices").Array() {
			for _, call := range choice.Get("delta.tool_calls").Array() {
				if call.Get("index").Int() != 0 {
					t.Fatalf("the kept call kept index %d: %s", call.Get("index").Int(), event)
				}
			}
		}
		frames = append(frames, sseFrame{Data: event})
	}
	body, errAssemble := assembleChatCompletion(frames)
	if errAssemble != nil {
		t.Fatal(errAssemble)
	}
	checkReply(t, bufferedReply(t, sdktranslator.FormatOpenAI, body), "", "tool_calls", `shell {"command":"ls"}`)
}

// A Responses stream numbers its items, and a dropped one leaves a hole in
// that sequence that a client reads its items by: the items after it move
// up, in their added, delta and done events alike, and the terminal event's
// output is scrubbed to match.
func TestCloakFilterResponsesStreamRenumbers(t *testing.T) {
	filter := newCloakFilter(sdktranslator.FormatOpenAIResponse, stubbedTools("bash", "read"))
	_, events := responsesReply(
		responsesCall("call-1", "read", `{"filePath":"/a"}`),
		responsesText("msg-1", "Left behind."),
		responsesCall("call-2", "shell", `{"command":"ls"}`),
	)
	stream := filter.streams()
	var out [][]byte
	for _, event := range events {
		out = append(out, stream.event([]byte(event))...)
	}
	indexes := map[int64][]string{}
	for _, event := range out {
		root := gjson.ParseBytes(event)
		index := root.Get("output_index")
		if !index.Exists() {
			continue
		}
		indexes[index.Int()] = append(indexes[index.Int()], root.Get("type").String())
		if strings.Contains(string(event), `"read"`) {
			t.Fatalf("the placeholder reached the caller: %s", event)
		}
	}
	if len(indexes) != 2 || len(indexes[0]) == 0 || len(indexes[1]) == 0 {
		t.Fatalf("output indexes = %v, want 0 and 1", indexes)
	}
	checkReply(t, streamedReply(t, sdktranslator.FormatOpenAIResponse, out), "Left behind.", "", `shell {"command":"ls"}`)
}

// The events a dropped Responses call is replaced by are numbered after the
// ones already handed on, and the terminal event after them: a streaming
// caller reads a stream by its sequence numbers, and an unnumbered or
// repeated one would leave it out of order.
func TestCloakFilterResponsesNumbersTheNote(t *testing.T) {
	filter := newCloakFilter(sdktranslator.FormatOpenAIResponse, stubbedTools("bash", "read"))
	_, events := responsesReply(responsesCall("call-1", "read", `{}`))
	stream := filter.streams()
	var out [][]byte
	for _, event := range events {
		out = append(out, stream.event([]byte(event))...)
	}
	previous := int64(-1)
	for _, event := range out {
		sequence := gjson.GetBytes(event, "sequence_number")
		if !sequence.Exists() {
			t.Fatalf("an event with no sequence_number: %s", event)
		}
		if sequence.Int() <= previous {
			t.Fatalf("sequence went %d then %d: %s", previous, sequence.Int(), out)
		}
		previous = sequence.Int()
	}
	checkReply(t, streamedReply(t, sdktranslator.FormatOpenAIResponse, out), cloakStubNote, "")
}

// A filter for a request that needed nothing from the cloak — the caller
// declared its own bash and read, or the model was not free at all — is
// inactive, and a call of any tool it did nothing to is handed on untouched:
// a caller's own read is its own tool and is never dropped just for sharing a
// name with the one the gate wants.
func TestCloakFilterInactivePassesThrough(t *testing.T) {
	if filter := newCloakFilter(sdktranslator.FormatOpenAI, cloakTools{}); filter.active() {
		t.Fatal("a request the cloak left alone has an active filter")
	}
	// Only bash was the caller's to rename: its own read, offered as it
	// declared it, keeps going by that name.
	filter := newCloakFilter(sdktranslator.FormatOpenAI, renamedTools("bash", "Bash"))
	body, events := chatReply("Hi", [2]string{"read", `{"filePath":"/a"}`}, [2]string{"bash", `{"command":"ls"}`})
	checkFiltered(t, filter, body, events, "Hi", "tool_calls", `read {"filePath":"/a"}`, `Bash {"command":"ls"}`)
}

// Both of the tools a free request offers are undone the same way: a call of
// a caller's bash comes back under its own name, and a call of a bash the
// caller never had is dropped just as a call of a read it never had is —
// with an argument built to run a command, which is the worse of the two to
// hand a caller that might run it.
func TestCloakFilterBashIsUndoneLikeRead(t *testing.T) {
	renamed := newCloakFilter(sdktranslator.FormatOpenAI, renamedTools("bash", "Bash", "read", "Read"))
	body, events := chatReply("On it.", [2]string{"bash", `{"command":"ls"}`}, [2]string{"read", `{"filePath":"/a"}`})
	checkFiltered(t, renamed, body, events, "On it.", "tool_calls", `Bash {"command":"ls"}`, `Read {"filePath":"/a"}`)

	stubbed := newCloakFilter(sdktranslator.FormatOpenAI, stubbedTools("bash", "read"))
	body, events = chatReply("Looking.", [2]string{"bash", `{"command":"cat /etc/hostname"}`}, [2]string{"read", `{"filePath":"/a"}`}, [2]string{"shell", `{"command":"ls"}`})
	checkFiltered(t, stubbed, body, events, "Looking.", "tool_calls", `shell {"command":"ls"}`)

	body, events = chatReply("", [2]string{"bash", `{"command":"ls"}`})
	checkFiltered(t, stubbed, body, events, cloakStubNote, "stop")
}

// A cloaked stream through the whole executor: Claude Code's Bash and Read
// go upstream in the model's Chat Completions with the renamed one as
// "read", and the model's call of it comes back as the caller's Read, in its
// own format, with the rest of the reply untouched.
func TestExecutorUndoesTheCloakInStreams(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"cat-jam-free"}
	useSettings(t, cfg)
	useTestCatalog(t)

	payload := `{"model":"opencode/cat-jam-free","max_tokens":100,"stream":true,` +
		`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},` +
		`{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],` +
		`"messages":[{"role":"user","content":"list files"}]}`

	// The upstream stream answers in the model's Chat Completions: the request
	// is translated into it with both tools renamed to OpenCode's names, so
	// what comes back under them has to be handed to the caller under its
	// own.
	gateway := installFakeGateway(t, streamGateway(chatStreamOf("Running it.", [2]string{"bash", `{"command":"ls"}`})))
	decodeResult[rpcExecutorStreamResponse](t, streamRaw(t, payload))
	sent := gateway.inferenceRequests()
	if len(sent) != 1 {
		t.Fatalf("requests = %d", len(sent))
	}
	if names := toolsOf(t, sent[0].Body); names != "bash,read" {
		t.Fatalf("upstream tools = %s in %s", names, sent[0].Body)
	}
	chunks, errText := gateway.waitClosed("exec-1")
	if errText != "" {
		t.Fatalf("stream closed with %q", errText)
	}
	if got := toolUses(t, chunks); got != "Bash" {
		t.Fatalf("tool uses = %q in %s", got, chunks)
	}
	if got := parseClientStream(t, "claude", chunks); got.Text != "Running it." || !got.Terminal {
		t.Fatalf("stream = %+v in %s", got, chunks)
	}

	// The model answers through the other tool, the renamed one, which comes
	// back as the caller's Read rather than as a lowercase stranger.
	gateway = installFakeGateway(t, streamGateway(chatStreamOf("Reading it.", [2]string{"read", `{"filePath":"/a"}`})))
	decodeResult[rpcExecutorStreamResponse](t, streamRaw(t, payload))
	chunks, errText = gateway.waitClosed("exec-1")
	if errText != "" {
		t.Fatalf("stream closed with %q", errText)
	}
	if got := toolUses(t, chunks); got != "Read" {
		t.Fatalf("tool uses = %q in %s", got, chunks)
	}
}

// chatStreamOf is the streamed shape of chatReply.
func chatStreamOf(text string, calls ...[2]string) []string {
	_, events := chatReply(text, calls...)
	return events
}

// streamRaw runs one streamed cat-jam-free call from a Messages client.
func streamRaw(t *testing.T, payload string) []byte {
	t.Helper()
	raw, _ := executorExecuteStream(testCall{Source: "claude", Model: "opencode/cat-jam-free", Stream: true, Payload: payload}.raw(t))
	return raw
}

// A request from a caller with no file tool (Codex's shell and apply_patch)
// is cloaked with the placeholder, and a model that calls it anyway has the
// call dropped before the reply reaches the caller, buffered or streamed.
func TestExecutorDropsPlaceholderCalls(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"cat-jam-free"}
	useSettings(t, cfg)
	useTestCatalog(t)

	payload := `{"model":"opencode/cat-jam-free","instructions":"be brief","input":"read /etc/hostname","stream":false,` +
		`"tools":[{"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}},{"type":"function","name":"apply_patch","description":"Edit a file","parameters":{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"]}}]}`

	// Buffered: the free tier answers with SSE, which is folded into the JSON
	// the caller asked for, and the placeholder's call is gone from it while
	// the caller's own call of shell keeps its argument.
	_, upstream := chatReply("Let me look.", [2]string{"read", `{"filePath":"/etc/hostname"}`}, [2]string{"shell", `{"command":"cat /etc/hostname"}`})
	gateway := installFakeGateway(t, streamGateway(upstream))
	raw, _ := executorExecute(testCall{Source: "openai-response", Model: "opencode/cat-jam-free", Payload: payload}.raw(t))
	if sent := gateway.inferenceRequests(); len(sent) != 1 || toolsOf(t, sent[0].Body) != "shell,apply_patch,bash,read" {
		t.Fatalf("upstream tools = %s", sent[0].Body)
	}
	resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if got := responseCalls(t, resp.Payload); got != `shell {"command":"cat /etc/hostname"}` {
		t.Fatalf("calls = %q in %s", got, resp.Payload)
	}
	if got := parseClientResponse(t, "openai-response", resp.Payload); got.Text != "Let me look." {
		t.Fatalf("response = %+v", got)
	}

	// A paid Zen model on the same auth is not cloaked, so nothing about its
	// tools or its reply is rewritten either: a call named read is the
	// caller's to have, and it keeps it.
	cfg.ZenModels = []string{"cat-jam-free", "kimi-k3"}
	useSettings(t, cfg)
	paidBody, _ := chatReply("Paid.", [2]string{"read", `{"filePath":"/a"}`})
	gateway = installFakeGateway(t, func(hostHTTPRequest) gatewayReply {
		return gatewayReply{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(paidBody)}
	})
	raw, _ = executorExecute(testCall{Source: "openai-response", Model: "opencode/kimi-k3", Payload: strings.Replace(payload, "cat-jam-free", "kimi-k3", 1)}.raw(t))
	resp = decodeResult[pluginapi.ExecutorResponse](t, raw)
	if got := responseCalls(t, resp.Payload); got != `read {"filePath":"/a"}` {
		t.Fatalf("a paid reply was rewritten: calls = %q in %s", got, resp.Payload)
	}
}

// streamGateway answers one upstream request with the given Chat Completions
// events as an SSE body, as the free tier does.
func streamGateway(events []string) func(hostHTTPRequest) gatewayReply {
	var body strings.Builder
	for _, event := range events {
		body.WriteString("data: " + event + "\n\n")
	}
	return func(hostHTTPRequest) gatewayReply {
		return gatewayReply{Status: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte(body.String())}
	}
}

// toolsOf names the tools of one upstream body, in order.
func toolsOf(t *testing.T, body []byte) string {
	t.Helper()
	var names []string
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		name := tool.Get("function.name").String()
		if name == "" {
			name = tool.Get("name").String()
		}
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

// toolUses names the tools a Messages stream called, in order.
func toolUses(t *testing.T, chunks [][]byte) string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(strings.Join(toStrings(chunks), ""), "\n") {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		event := gjson.Parse(strings.TrimSpace(data))
		if event.Get("type").String() == "content_block_start" && event.Get("content_block.type").String() == "tool_use" {
			names = append(names, event.Get("content_block.name").String())
		}
	}
	return strings.Join(names, ",")
}

func toStrings(chunks [][]byte) []string {
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, string(chunk))
	}
	return out
}

// responseCalls names the calls of a Responses body, with their arguments.
func responseCalls(t *testing.T, body []byte) string {
	t.Helper()
	var calls []string
	for _, item := range gjson.GetBytes(body, "output").Array() {
		if item.Get("type").String() == "function_call" {
			calls = append(calls, item.Get("name").String()+" "+item.Get("arguments").String())
		}
	}
	return strings.Join(calls, "|")
}

// A filtered stream through the converter: what the filter drops is not
// translated, and what it keeps is handed on in the model's own protocol for
// a caller that speaks it, so a placeholder call leaves no fragment and no
// tool stop behind it while the caller's own call keeps its argument.
func TestStreamConverterFiltersBeforeTranslating(t *testing.T) {
	plan := routePlan{Source: sdktranslator.FormatOpenAI, Response: sdktranslator.FormatOpenAI, Upstream: sdktranslator.FormatOpenAI}
	converter := newStreamConverter(context.Background(), plan, "m", nil, nil)
	converter.filter = newCloakFilter(sdktranslator.FormatOpenAI, stubbedTools("bash", "read"))
	_, events := chatReply("Reading it.", [2]string{"read", `{"filePath":"/a"}`}, [2]string{"shell", `{"command":"ls"}`})
	var chunks [][]byte
	for _, event := range events {
		out, errConvert := converter.convert(sseFrame{Data: []byte(event), Raw: []byte("data: " + event + "\n\n")})
		if errConvert != nil {
			t.Fatal(errConvert)
		}
		chunks = append(chunks, out...)
	}
	finish, errFinish := converter.finish()
	if errFinish != nil {
		t.Fatal(errFinish)
	}
	chunks = append(chunks, finish...)
	if joined := strings.Join(toStrings(chunks), ""); strings.Contains(joined, `"read"`) {
		t.Fatalf("the placeholder reached the caller: %s", joined)
	}
	if got := parseClientStream(t, "openai", chunks); got.Text != "Reading it." || !got.Terminal {
		t.Fatalf("stream = %+v", got)
	}

	// The renaming is undone on the same path, before any translation out of
	// the model's protocol would see the call.
	converter = newStreamConverter(context.Background(), plan, "m", nil, nil)
	converter.filter = newCloakFilter(sdktranslator.FormatOpenAI, renamedTools("read", "Read"))
	chunks = chunks[:0]
	for _, event := range events {
		out, errConvert := converter.convert(sseFrame{Data: []byte(event), Raw: []byte("data: " + event + "\n\n")})
		if errConvert != nil {
			t.Fatal(errConvert)
		}
		chunks = append(chunks, out...)
	}
	var names []string
	for _, chunk := range chunks {
		for _, choice := range gjson.GetBytes(chunk, "choices").Array() {
			for _, call := range choice.Get("delta.tool_calls").Array() {
				if name := call.Get("function.name").String(); name != "" {
					names = append(names, name)
				}
			}
		}
	}
	if strings.Join(names, ",") != "Read,shell" {
		t.Fatalf("streamed call names = %q in %s", names, chunks)
	}
}
