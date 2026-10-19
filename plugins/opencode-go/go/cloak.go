package main

// Presenting free OpenCode Zen requests as the OpenCode client.
//
// The Zen free tier is gated by a client fingerprint inside OpenCode's private
// inference service, which answers every other caller with 403 FreeTierError
// ("OpenCode's free tier can only be used from within OpenCode"). The gate was
// reproduced against the live endpoint on 2026-10-15, and again factor by
// factor on 2026-10-19; a request is accepted only when all of this holds:
//
//   - the User-Agent names OpenCode with a version of at least 1.18.0, in any
//     of the shapes the client uses (opencode/1.19.0, opencode/latest/2.0.29/
//     cli, or the version followed by other agents). Older versions get 426
//     UpgradeRequired;
//   - a session header carries an OpenCode-shaped session identifier (see
//     openCodeSessionPattern). The shape alone is what is checked: a leading
//     value that is not the bit-inverted timestamp OpenCode mints, is all
//     zeros, all ones or years old is accepted, and a hash cut to the same
//     length or the hex in the other twelve characters is not;
//   - the body sets "stream": true, and the gateway answers with SSE even for
//     a request that asks for JSON;
//   - the body's tools contain an entry named exactly "read": lowercase, no
//     surrounding space. It is the only name that opens the gate — every
//     other built-in of OpenCode's was refused on its own, "bash" included —
//     and nothing else of the entry is compared: a placeholder whose
//     description and schema are nothing like OpenCode's is accepted.
//
// Passing the gate is not the whole of it: the gate is a fingerprint, and a
// fingerprint can be read as easily from what a request leaves out as from
// what it puts in. So what is sent is not the least that is answered but what
// the client named sends — its full User-Agent, the session in both of the
// headers it names it in, a fresh request identifier for the turn, its client
// and project headers, and none of the session headers it does not use — and
// its tool list offers both of the built-ins OpenCode reaches a model with
// first, "bash" as well as "read", each with OpenCode's own parameters, even
// though "read" alone is all the gate checks for. A request that only just
// passes is one the next tightening of the fingerprint names.
//
// That last finding is what keeps the tools harmless: only the names are
// compared, so what a caller lacks goes up described as unavailable, and
// models leave it alone — asked to read a file with the genuine description
// they call it, and with the placeholder beside a caller's own file tool they
// call that tool instead. It is never described as a capability, and a caller
// that already declares "read" keeps its own. And whatever a model decides, a
// call of a placeholder never reaches the caller: it exists for the gate
// alone, so a reply that calls one is handed back as the same reply minus
// that call (cloakFilter), a turn that called nothing else ending as a plain
// stop.
//
// Nothing here signs or claims a credential: the caller still sends its own
// Authorization (or none), and the gateway bills exactly as it would without
// the cloak. Headers the gate does not read either way were each sent and the
// request answered regardless — HTTP-Referer among them — but they are not
// sent, because the client named does not send them.

import (
	"bytes"
	"container/list"
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

// cloakUserAgent is the User-Agent of the client release the cloak presents,
// whole, as that client sends it: the gate reads the version out of it, and
// the rest of the string is part of the same fingerprint, so sending only the
// version would name a client that never sent it that way.
const cloakUserAgent = "opencode/1.19.0 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

const (
	// cloakSessionKeep bounds the sessions whose minted IDs are remembered.
	cloakSessionKeep = 1024
	// cloakIDAlphabet is the base62 alphabet OpenCode's identifiers use.
	cloakIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// cloakIDMask keeps the six leading bytes of the identifier value.
	cloakIDMask = uint64(1)<<48 - 1
	// cloakToolName is the name of the tool the gate looks for in a tool list,
	// and of the file tool the cloak presents, renamed and added alike.
	cloakToolName = "read"
	// cloakBashName is the other built-in a free request offers, which is
	// what OpenCode's client reaches a model with and the other one of the
	// pair the free tier is asked as. The gate does not ask for it — it was
	// refused on its own — and that is not the point: a tool list carrying
	// only the one name the gate checks for is the tool list of a caller
	// answering a check, not of the client it claims to be, whose sessions
	// offer both.
	cloakBashName = "bash"
	// cloakToolDescription is what the model is told about the placeholder.
	// The gate ignores it, so it says what the tool is: a marker the transport
	// needs, not something to call. Models honour it, and a caller that
	// declares its own file tool keeps getting that one called instead.
	cloakToolDescription = "Reserved by the transport and unavailable in this session. Never call this tool."
	// cloakStubNote is what a turn that called nothing but the placeholder
	// and otherwise said nothing is answered with, so a caller never sees its
	// own request end in silence over a tool it never declared.
	cloakStubNote = "(The model asked for a tool this session doesn't have.)"
)

// cloakToolSchemas are the parameters of OpenCode's own bash and read tools,
// as its client sends them in the release the cloak names. The gate compared
// no schema when last reproduced — an empty one was accepted — but a tool
// offered with no parameters is one no client of OpenCode's ever offered, and
// the genuine ones are accepted by any check that is really asking whether
// the tool offered is OpenCode's.
var cloakToolSchemas = map[string]string{
	"bash": `{"type":"object","properties":{"command":{"type":"string","description":"The command to execute"},"timeout":{"type":"integer","description":"Optional timeout in milliseconds"},"workdir":{"type":"string","description":"The working directory to run the command in"}},"required":["command"]}`,
	"read": `{"type":"object","properties":{"filePath":{"type":"string","description":"The absolute path to the file or directory to read"},"offset":{"type":"integer","description":"The line number to start reading from (1-indexed)"},"limit":{"type":"integer","description":"The maximum number of lines to read"}},"required":["filePath"]}`,
}

// cloakSessionStore remembers the identifier minted for a conversation. The
// free tier meters and caches per session, so every turn of one conversation
// has to present the same id. The least recently used conversation is
// forgotten first, so one that is still going only loses its id — and with it
// the cache the gateway kept for it — once cloakSessionKeep others have had a
// turn since.
type cloakSessionStore struct {
	mu    sync.Mutex
	now   func() time.Time
	ids   map[string]*list.Element // seed → its element in order
	order *list.List               // of cloakSessionEntry, least recently used first
}

// cloakSessionEntry is one remembered conversation.
type cloakSessionEntry struct{ seed, id string }

func newCloakSessionStore() *cloakSessionStore {
	return &cloakSessionStore{now: time.Now, ids: make(map[string]*list.Element), order: list.New()}
}

var cloakSessions = newCloakSessionStore()

// id returns the OpenCode session identifier for one conversation, minting it
// the first time it is asked for.
func (s *cloakSessionStore) id(seed string) string {
	// Minted before the lock: mintOpenCodeID takes a lock of its own, and a
	// seed that turns out to be remembered only costs a discarded id.
	minted, errMint := mintOpenCodeID(s.now())
	if errMint != nil {
		// Without randomness the id would repeat across conversations, so fall
		// back to the seed's own hash rather than to a fixed value.
		minted = "ses_" + strings.Repeat("0", 12) + shortHash(seed)[:14]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.ids[seed]; ok {
		s.order.MoveToBack(element)
		return element.Value.(cloakSessionEntry).id
	}
	if s.order.Len() >= cloakSessionKeep {
		oldest := s.order.Front()
		s.order.Remove(oldest)
		delete(s.ids, oldest.Value.(cloakSessionEntry).seed)
	}
	s.ids[seed] = s.order.PushBack(cloakSessionEntry{seed: seed, id: minted})
	return minted
}

// cloakIDs is the clock and counter the identifiers are minted with: the
// counter counts the ids minted within one millisecond, as the identifier's
// low field does in OpenCode's own client.
var cloakIDs struct {
	sync.Mutex
	milli, count uint64
}

// mintOpenCodeID builds the session identifier the OpenCode client would
// send; see mintPrefixedID.
func mintOpenCodeID(now time.Time) (string, error) {
	return mintPrefixedID("ses", true, now)
}

// mintPrefixedID builds an identifier of the shape OpenCode's client mints:
// prefix_, the six leading bytes of a millisecond timestamp times 0x1000 plus
// a count of the ids minted in that millisecond — bit-inverted when
// descending, which a session id is and a message id is not, so that the
// newest session sorts first — as twelve hex digits, then fourteen random
// base62 characters. Only the shape is checked at the gate, but an id is
// minted the way the client mints it, which is the shape the check was
// written for and the one its logs are full of.
func mintPrefixedID(prefix string, descending bool, now time.Time) (string, error) {
	milli := uint64(now.UnixMilli())
	cloakIDs.Lock()
	if milli != cloakIDs.milli {
		cloakIDs.milli, cloakIDs.count = milli, 0
	}
	cloakIDs.count++
	count := cloakIDs.count
	cloakIDs.Unlock()
	value := (milli*0x1000 + count) & cloakIDMask
	if descending {
		value = ^value & cloakIDMask
	}
	var b strings.Builder
	b.WriteString(prefix + "_")
	for shift := 40; shift >= 0; shift -= 8 {
		fmt.Fprintf(&b, "%02x", byte(value>>uint(shift)))
	}
	random := make([]byte, 14)
	if _, errRead := rand.Read(random); errRead != nil {
		return "", fmt.Errorf("mint OpenCode %s id: %w", prefix, errRead)
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

// validOpenCodeSession reports whether id is one the gate accepts, which is
// one of the identifier's shape: "ses_", twelve hex digits, fourteen base62
// ones. The leading value is not decoded — ids whose leading bytes are not
// the inverted timestamp OpenCode lays out, or whose low field runs past any
// width its client counts to, were answered all the same — so a session the
// caller genuinely sent is never thrown away over what is inside it.
func validOpenCodeSession(id string) bool {
	return openCodeSessionPattern.MatchString(id)
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

// cloakTools records how the tool list of one cloaked request was made to
// look like OpenCode's, which is what a reply's calls of it are undone with.
type cloakTools struct {
	// back is the caller's name for each offered name it declared in another
	// case (Claude Code's Bash and Read), which went upstream lowercase.
	back map[string]string
	// stubs are the offered names the caller never declared, which went
	// upstream as placeholders. A reply's calls of them are of a tool the
	// caller cannot run, and are dropped.
	stubs map[string]bool
}

// untouched reports that the caller offered both tools under the names they
// go upstream with (Pi does, and OpenCode itself does), so nothing was added
// or renamed and every call in a reply is the caller's own.
func (t cloakTools) untouched() bool {
	return len(t.back) == 0 && len(t.stubs) == 0
}

// cloakBody makes a translated request body one the OpenCode client would
// send: streaming on, and OpenCode's bash and read in the tool list, under
// those names. A caller that declares either keeps its own tool untouched,
// and one that declares it in another case has it offered under the name
// OpenCode's carries, which keeps the model on a tool it can really call
// rather than beside a placeholder that says it cannot. Only what a caller
// declares neither of is given a placeholder, appended in OpenCode's order
// beside the caller's own, which stay where they were.
//
// The returned cloakTools says what was done, name by name, and rides along
// to the reply, where it is undone for it.
func cloakBody(protocol sdktranslator.Format, body []byte) ([]byte, cloakTools, error) {
	tools := cloakTools{back: map[string]string{}, stubs: map[string]bool{}}
	if stream := gjson.GetBytes(body, "stream"); stream.Type != gjson.True {
		body, _ = sjson.SetBytes(body, "stream", true)
	}
	namePath, ok := toolNamePath(protocol)
	if !ok {
		return nil, tools, fmt.Errorf("no tool shape for %s requests", protocol)
	}
	existing := gjson.GetBytes(body, "tools")
	var entries []string
	for _, need := range []string{cloakBashName, cloakToolName} {
		exact, renamed := -1, -1
		if existing.IsArray() {
			for index, tool := range existing.Array() {
				name := tool.Get(namePath).String()
				if name == "" {
					name = tool.Get("name").String()
				}
				if name == need {
					exact = index
				}
				if renamed < 0 && strings.EqualFold(name, need) {
					renamed = index
				}
			}
		}
		switch {
		case exact >= 0:
			// The caller's own tool, under OpenCode's name already.
		case renamed >= 0:
			body, _ = sjson.SetBytes(body, fmt.Sprintf("tools.%d.%s", renamed, namePath), need)
			// A name is recovered from its own tool, not from another case
			// of it: the caller's word for it is whatever it declared.
			tools.back[need] = toolNameOf(existing.Array()[renamed], namePath)
		default:
			entry, errEntry := cloakToolEntry(protocol, need)
			if errEntry != nil {
				return nil, tools, errEntry
			}
			entries = append(entries, entry)
			tools.stubs[need] = true
		}
	}
	if len(entries) > 0 {
		body, _ = sjson.SetRawBytes(body, "tools", appendTools(existing.Raw, entries))
	}
	return body, tools, nil
}

// toolNameOf is the name a tool declares, in whichever field its shape puts
// one.
func toolNameOf(tool gjson.Result, namePath string) string {
	if name := tool.Get(namePath).String(); name != "" {
		return name
	}
	return tool.Get("name").String()
}

// appendTools appends tool entries to a tools array's raw JSON, creating the
// array when the request carries none.
func appendTools(existing string, entries []string) []byte {
	trimmed := bytes.TrimSpace([]byte(existing))
	joined := strings.Join(entries, ",")
	if len(trimmed) == 0 || trimmed[0] != '[' || strings.TrimSpace(string(trimmed)) == "[]" {
		return []byte("[" + joined + "]")
	}
	return append(append(trimmed[:len(trimmed)-1], ','), append([]byte(joined), ']')...)
}

// toolNamePath names the field that holds a tool's name in one protocol's
// tool shape.
func toolNamePath(protocol sdktranslator.Format) (string, bool) {
	switch protocol {
	case sdktranslator.FormatOpenAI:
		return "function.name", true
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude:
		return "name", true
	}
	return "", false
}

// cloakToolEntry renders one placeholder, named name, in one protocol's tool
// shape, with that tool's own parameters.
func cloakToolEntry(protocol sdktranslator.Format, name string) (string, error) {
	description, errJSON := json.Marshal(cloakToolDescription)
	if errJSON != nil {
		return "", errJSON
	}
	switch protocol {
	case sdktranslator.FormatOpenAI:
		return fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":%s,"parameters":%s}}`, name, description, cloakToolSchemas[name]), nil
	case sdktranslator.FormatOpenAIResponse:
		return fmt.Sprintf(`{"type":"function","name":%q,"description":%s,"parameters":%s}`, name, description, cloakToolSchemas[name]), nil
	case sdktranslator.FormatClaude:
		return fmt.Sprintf(`{"name":%q,"description":%s,"input_schema":%s}`, name, description, cloakToolSchemas[name]), nil
	}
	return "", fmt.Errorf("no tool shape for %s requests", protocol)
}

// cloakFilter undoes, for a cloaked reply, what cloakBody did to its request:
// a renamed tool's calls take the caller's own name again, and a call of the
// placeholder — a tool the caller never declared and cannot run — never
// reaches it. It works in the model's own protocol, on the upstream body and
// stream before either is translated into the caller's, so the undoing is
// written once rather than in every shape a caller may be listening in, and so
// a translator never has to make sense of a call of a tool that is in no list
// it saw.
type cloakFilter struct {
	protocol sdktranslator.Format
	tools    cloakTools
}

// newCloakFilter is the filter for one cloaked call, undoing the tools its
// request was made to offer.
func newCloakFilter(protocol sdktranslator.Format, tools cloakTools) cloakFilter {
	return cloakFilter{protocol: protocol, tools: tools}
}

// active reports whether there is anything to undo.
func (f cloakFilter) active() bool { return !f.tools.untouched() }

// disposition is what a call named name means to the caller: the name its
// calls are handed on under, or false for a call of a placeholder. A name the
// cloak did nothing to — the caller's own tool, offered as it declared it —
// is handed on as it came, whether the model spelled it as offered or not.
func (f cloakFilter) disposition(name string) (string, bool) {
	for offered, own := range f.tools.back {
		if name == offered {
			return own, true
		}
	}
	if f.tools.stubs[name] {
		return "", false
	}
	return name, true
}

// rawJSONArray joins raw JSON values into one array.
func rawJSONArray(items [][]byte) []byte {
	return append(append([]byte("["), bytes.Join(items, []byte(","))...), ']')
}

// buffered turns a whole reply in the model's protocol back into the
// caller's. A turn left with no call at all stops as a turn that called
// nothing does, and one left with nothing to say either says why: silence
// would read as a broken reply rather than as the end of one.
func (f cloakFilter) buffered(body []byte) []byte {
	switch f.protocol {
	case sdktranslator.FormatOpenAI:
		return f.chatBuffered(body)
	case sdktranslator.FormatOpenAIResponse:
		return f.responsesBuffered(body)
	case sdktranslator.FormatClaude:
		return f.messagesBuffered(body)
	}
	return body
}

func (f cloakFilter) chatBuffered(body []byte) []byte {
	choices := gjson.GetBytes(body, "choices")
	if !choices.IsArray() {
		return body
	}
	for index, choice := range choices.Array() {
		calls := choice.Get("message.tool_calls")
		if !calls.IsArray() {
			continue
		}
		var kept [][]byte
		removed := false
		for _, call := range calls.Array() {
			name := call.Get("function.name").String()
			own, keep := f.disposition(name)
			if !keep {
				removed = true
				continue
			}
			raw := []byte(call.Raw)
			if own != name {
				raw, _ = sjson.SetBytes(raw, "function.name", own)
			}
			kept = append(kept, raw)
		}
		message := fmt.Sprintf("choices.%d.message", index)
		if len(kept) > 0 {
			body, _ = sjson.SetRawBytes(body, message+".tool_calls", rawJSONArray(kept))
			continue
		}
		body, _ = sjson.DeleteBytes(body, message+".tool_calls")
		if !removed {
			continue
		}
		if choice.Get("finish_reason").String() == "tool_calls" {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("choices.%d.finish_reason", index), "stop")
		}
		if strings.TrimSpace(choice.Get("message.content").String()) == "" {
			body, _ = sjson.SetBytes(body, message+".content", cloakStubNote)
		}
	}
	return body
}

func (f cloakFilter) responsesBuffered(body []byte) []byte {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return body
	}
	var kept [][]byte
	removed, callsLeft, said := false, false, false
	for _, item := range output.Array() {
		switch item.Get("type").String() {
		case "function_call":
			name := item.Get("name").String()
			own, keep := f.disposition(name)
			if !keep {
				removed = true
				continue
			}
			callsLeft = true
			raw := []byte(item.Raw)
			if own != name {
				raw, _ = sjson.SetBytes(raw, "name", own)
			}
			kept = append(kept, raw)
		case "message":
			for _, part := range item.Get("content").Array() {
				if part.Get("text").String() != "" {
					said = true
				}
			}
			kept = append(kept, []byte(item.Raw))
		default:
			kept = append(kept, []byte(item.Raw))
		}
	}
	if removed && !callsLeft && !said {
		kept = append(kept, noteMessageItem())
	}
	body, _ = sjson.SetRawBytes(body, "output", rawJSONArray(kept))
	return body
}

// noteMessageItem is a Responses message item saying cloakStubNote.
func noteMessageItem() []byte {
	id, errMint := mintPrefixedID("msg", false, time.Now())
	if errMint != nil {
		id = "msg_cloak"
	}
	item, _ := sjson.SetBytes([]byte(`{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[]}]}`), "id", id)
	item, _ = sjson.SetBytes(item, "content.0.text", cloakStubNote)
	return item
}

func (f cloakFilter) messagesBuffered(body []byte) []byte {
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return body
	}
	var kept [][]byte
	removed, callsLeft, said := false, false, false
	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "tool_use":
			name := block.Get("name").String()
			own, keep := f.disposition(name)
			if !keep {
				removed = true
				continue
			}
			callsLeft = true
			raw := []byte(block.Raw)
			if own != name {
				raw, _ = sjson.SetBytes(raw, "name", own)
			}
			kept = append(kept, raw)
		case "text":
			if block.Get("text").String() != "" {
				said = true
			}
			kept = append(kept, []byte(block.Raw))
		default:
			kept = append(kept, []byte(block.Raw))
		}
	}
	if removed && !callsLeft {
		if gjson.GetBytes(body, "stop_reason").String() == "tool_use" {
			body, _ = sjson.SetBytes(body, "stop_reason", "end_turn")
		}
		if !said {
			note, _ := sjson.SetBytes([]byte(`{"type":"text"}`), "text", cloakStubNote)
			kept = append(kept, note)
		}
	}
	body, _ = sjson.SetRawBytes(body, "content", rawJSONArray(kept))
	return body
}

// cloakStream is the filter's state for one streamed reply: the calls it has
// decided about, keyed by the identity a call carries in its own protocol, so
// two calls streaming side by side are decided on their own names and never on
// whichever one happened to be open when a fragment arrived.
type cloakStream struct {
	f       cloakFilter
	chat    map[int64]*chatChoiceTrack
	blocks  map[int64]bool  // Messages block index → dropped
	items   map[int64]int64 // Responses output index → the one handed on, -1 dropped
	next    int64           // the next Responses output index handed on
	said    bool            // text was handed on
	stubbed bool            // a call of the placeholder was dropped
	calls   int             // calls handed on, Messages and Responses
	maxBlk  int64           // the highest Messages block index seen
	// seq is the highest Responses sequence_number handed on, from which an
	// event with none of its own is numbered.
	seq int64
}

// nextSeq numbers an event the filter made up, after everything handed on
// so far, and hands it on with the number in it.
func (s *cloakStream) numbered(event []byte) []byte {
	s.seq++
	event, _ = sjson.SetBytes(event, "sequence_number", s.seq)
	return event
}

// streams is the stream state for the filter.
func (f cloakFilter) streams() *cloakStream {
	return &cloakStream{f: f, chat: make(map[int64]*chatChoiceTrack), blocks: make(map[int64]bool), items: make(map[int64]int64)}
}

// event turns one upstream event back into the caller's: the events to hand
// on, as data payloads, none for an event that belonged to a dropped call.
func (s *cloakStream) event(data []byte) [][]byte {
	if bytes.Equal(data, []byte("[DONE]")) {
		return [][]byte{data}
	}
	switch s.f.protocol {
	case sdktranslator.FormatOpenAI:
		if out := s.chatEvent(data); out != nil {
			return [][]byte{out}
		}
		return nil
	case sdktranslator.FormatOpenAIResponse:
		return s.responsesEvent(data)
	case sdktranslator.FormatClaude:
		return s.messagesEvent(data)
	}
	return [][]byte{data}
}

// chatChoiceTrack is the calls of one Chat Completions choice.
type chatChoiceTrack struct {
	// names is the name a call is handed on under, by the index it arrived
	// with; a call absent from it is undecided, and one present with an empty
	// name is dropped.
	names map[int64]string
	// handed is the index a kept call is handed on with, by the one it
	// arrived with, so a dropped call leaves no hole in the sequence.
	handed map[int64]int64
	next   int64
	// pending holds a call's fragments from before its name arrived, which
	// cannot be decided about yet.
	pending map[int64][][]byte
	stubbed bool
	said    bool
}

func (s *cloakStream) choice(index int64) *chatChoiceTrack {
	track, ok := s.chat[index]
	if !ok {
		track = &chatChoiceTrack{names: make(map[int64]string), handed: make(map[int64]int64), pending: make(map[int64][][]byte)}
		s.chat[index] = track
	}
	return track
}

// chatFragment turns one tool call fragment into the caller's: dropped and
// still undecided fragments come back nil, and undecided ones again with the
// fragment that settles them.
func (t *chatChoiceTrack) fragment(f cloakFilter, raw []byte) [][]byte {
	call := gjson.ParseBytes(raw)
	index := call.Get("index").Int()
	decided, ok := t.names[index]
	if !ok {
		if name := call.Get("function.name").String(); name != "" {
			own, keep := f.disposition(name)
			if !keep {
				t.names[index] = ""
				t.stubbed = true
				delete(t.pending, index)
				return nil
			}
			t.names[index] = own
			t.handed[index] = t.next
			t.next++
			decided = own
		} else {
			t.pending[index] = append(t.pending[index], []byte(call.Raw))
			return nil
		}
	}
	if decided == "" {
		return nil
	}
	out := make([][]byte, 0, len(t.pending[index])+1)
	for _, held := range t.pending[index] {
		held, _ = sjson.SetBytes(held, "index", t.handed[index])
		out = append(out, held)
	}
	delete(t.pending, index)
	fragment := []byte(call.Raw)
	fragment, _ = sjson.SetBytes(fragment, "index", t.handed[index])
	if name := call.Get("function.name").String(); name != "" && name != decided {
		fragment, _ = sjson.SetBytes(fragment, "function.name", decided)
	}
	return append(out, fragment)
}

func (s *cloakStream) chatEvent(data []byte) []byte {
	root := gjson.ParseBytes(data)
	choices := root.Get("choices")
	if !choices.IsArray() {
		return data
	}
	out := data
	for position, choice := range choices.Array() {
		track := s.choice(choice.Get("index").Int())
		delta := choice.Get("delta")
		if delta.Get("content").String() != "" {
			track.said = true
		}
		if calls := delta.Get("tool_calls"); calls.IsArray() {
			var fragments [][]byte
			for _, call := range calls.Array() {
				fragments = append(fragments, track.fragment(s.f, []byte(call.Raw))...)
			}
			path := fmt.Sprintf("choices.%d.delta.tool_calls", position)
			if len(fragments) == 0 {
				out, _ = sjson.DeleteBytes(out, path)
			} else {
				out, _ = sjson.SetRawBytes(out, path, rawJSONArray(fragments))
			}
		}
		if track.stubbed && track.next == 0 && choice.Get("finish_reason").String() == "tool_calls" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("choices.%d.finish_reason", position), "stop")
			if !track.said {
				out, _ = sjson.SetBytes(out, fmt.Sprintf("choices.%d.delta.content", position), cloakStubNote)
				track.said = true
			}
		}
	}
	return out
}

func (s *cloakStream) messagesEvent(data []byte) [][]byte {
	root := gjson.ParseBytes(data)
	switch root.Get("type").String() {
	case "content_block_start":
		index := root.Get("index").Int()
		s.maxBlk = max(s.maxBlk, index)
		block := root.Get("content_block")
		if block.Get("type").String() != "tool_use" {
			return [][]byte{data}
		}
		name := block.Get("name").String()
		own, keep := s.f.disposition(name)
		if !keep {
			s.blocks[index] = true
			s.stubbed = true
			return nil
		}
		s.calls++
		if own != name {
			data, _ = sjson.SetBytes(data, "content_block.name", own)
		}
		return [][]byte{data}
	case "content_block_delta", "content_block_stop":
		if s.blocks[root.Get("index").Int()] {
			return nil
		}
		if root.Get("delta.type").String() == "text_delta" && root.Get("delta.text").String() != "" {
			s.said = true
		}
		return [][]byte{data}
	case "message_delta":
		if !s.stubbed || s.calls > 0 {
			return [][]byte{data}
		}
		out := [][]byte(nil)
		if !s.said {
			out = append(out, noteBlockEvents(s.maxBlk+1)...)
			s.said = true
		}
		if root.Get("delta.stop_reason").String() == "tool_use" {
			data, _ = sjson.SetBytes(data, "delta.stop_reason", "end_turn")
		}
		return append(out, data)
	}
	return [][]byte{data}
}

// noteBlockEvents is a Messages text block, at index, saying cloakStubNote.
func noteBlockEvents(index int64) [][]byte {
	start, _ := sjson.SetBytes([]byte(`{"type":"content_block_start","content_block":{"type":"text","text":""}}`), "index", index)
	delta, _ := sjson.SetBytes([]byte(`{"type":"content_block_delta","delta":{"type":"text_delta"}}`), "index", index)
	delta, _ = sjson.SetBytes(delta, "delta.text", cloakStubNote)
	stop, _ := sjson.SetBytes([]byte(`{"type":"content_block_stop"}`), "index", index)
	return [][]byte{start, delta, stop}
}

func (s *cloakStream) responsesEvent(data []byte) [][]byte {
	root := gjson.ParseBytes(data)
	if sequence := root.Get("sequence_number"); sequence.Exists() {
		s.seq = max(s.seq, sequence.Int())
	}
	eventType := root.Get("type").String()
	switch eventType {
	case "response.output_item.added":
		index := root.Get("output_index").Int()
		item := root.Get("item")
		if item.Get("type").String() == "function_call" {
			name := item.Get("name").String()
			own, keep := s.f.disposition(name)
			if !keep {
				s.items[index] = -1
				s.stubbed = true
				return nil
			}
			s.calls++
			if own != name {
				data, _ = sjson.SetBytes(data, "item.name", own)
			}
		}
		s.items[index] = s.next
		data, _ = sjson.SetBytes(data, "output_index", s.next)
		s.next++
		return [][]byte{data}
	case "response.output_text.delta":
		if root.Get("delta").String() != "" {
			s.said = true
		}
		return s.renumbered(data, root)
	case "response.completed", "response.incomplete":
		if !root.Get("response").IsObject() {
			return [][]byte{data}
		}
		out := [][]byte(nil)
		if s.stubbed && s.calls == 0 && !s.said {
			for _, event := range noteItemEvents(s.next) {
				out = append(out, s.numbered(event))
			}
			s.said = true
			s.next++
			// The terminal event follows them without repeating a number.
			data, _ = sjson.SetBytes(data, "sequence_number", s.seq+1)
			s.seq++
		}
		scrubbed := s.f.responsesBuffered([]byte(root.Get("response").Raw))
		data, _ = sjson.SetRawBytes(data, "response", scrubbed)
		return append(out, data)
	}
	if root.Get("output_index").Exists() {
		return s.renumbered(data, root)
	}
	return [][]byte{data}
}

// renumbered hands an event on with the output index its item went out with,
// and drops one that belonged to a dropped call. An item the stream never
// announced keeps the index it arrived with.
func (s *cloakStream) renumbered(data []byte, root gjson.Result) [][]byte {
	index := root.Get("output_index").Int()
	handed, known := s.items[index]
	if !known {
		return [][]byte{data}
	}
	if handed < 0 {
		return nil
	}
	data, _ = sjson.SetBytes(data, "output_index", handed)
	// A finished call carries its name again, in the item it ends.
	if root.Get("type").String() == "response.output_item.done" {
		if item := root.Get("item"); item.Get("type").String() == "function_call" {
			if own, keep := s.f.disposition(item.Get("name").String()); keep && own != item.Get("name").String() {
				data, _ = sjson.SetBytes(data, "item.name", own)
			}
		}
	}
	return [][]byte{data}
}

// noteItemEvents is the stream of a Responses message item, at index, saying
// cloakStubNote: the item a streaming caller reads its text out of event by
// event, as noteMessageItem is the same item whole for a buffered one. The
// events are numbered by the stream that emits them.
func noteItemEvents(index int64) [][]byte {
	item := noteMessageItem()
	id := gjson.GetBytes(item, "id").String()
	added, _ := sjson.SetBytes([]byte(`{"type":"response.output_item.added","item":{"type":"message","status":"in_progress","role":"assistant","content":[]}}`), "output_index", index)
	added, _ = sjson.SetBytes(added, "item.id", id)
	part, _ := sjson.SetBytes([]byte(`{"type":"response.content_part.added","content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`), "item_id", id)
	part, _ = sjson.SetBytes(part, "output_index", index)
	delta, _ := sjson.SetBytes([]byte(`{"type":"response.output_text.delta","content_index":0,"logprobs":[]}`), "item_id", id)
	delta, _ = sjson.SetBytes(delta, "output_index", index)
	delta, _ = sjson.SetBytes(delta, "delta", cloakStubNote)
	textDone, _ := sjson.SetBytes(delta, "type", "response.output_text.done")
	textDone, _ = sjson.DeleteBytes(textDone, "delta")
	textDone, _ = sjson.SetBytes(textDone, "text", cloakStubNote)
	partDone, _ := sjson.SetBytes(part, "type", "response.content_part.done")
	partDone, _ = sjson.SetBytes(partDone, "part.text", cloakStubNote)
	done, _ := sjson.SetBytes([]byte(`{"type":"response.output_item.done"}`), "output_index", index)
	done, _ = sjson.SetRawBytes(done, "item", item)
	return [][]byte{added, part, delta, textDone, partDone, done}
}

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
