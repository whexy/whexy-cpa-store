// Package hosttest provides an in-process fake of CLIProxyAPI's host
// callbacks for plugin tests.
package hosttest

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// HandlerFunc answers one callback. Returning a *host.CallError fails it with
// that code, message and status; any other error fails it with
// host_call_failed.
type HandlerFunc func(payload json.RawMessage) (any, error)

// Call is one recorded callback.
type Call struct {
	Method  string
	Payload json.RawMessage
}

// LogLine is one recorded host.log callback.
type LogLine struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

// Fake answers host callbacks from registered handlers. The transport is
// process state, so tests that install a Fake must not run in parallel.
type Fake struct {
	mu         sync.Mutex
	calls      []Call
	handlers   map[string]HandlerFunc
	logs       []LogLine
	operations int
}

// Install replaces the host transport with a new Fake until the test ends.
// host.log, host.http.operation_open and host.http.cancel are answered by
// default; register a handler to override them.
func Install(t testing.TB) *Fake {
	t.Helper()
	fake := &Fake{handlers: map[string]HandlerFunc{}}
	fake.handlers[pluginabi.MethodHostLog] = fake.recordLog
	fake.handlers[pluginabi.MethodHostHTTPOperationOpen] = fake.openOperation
	fake.handlers[pluginabi.MethodHostHTTPCancel] = func(json.RawMessage) (any, error) { return struct{}{}, nil }
	t.Cleanup(host.SetTransport(fake.Call))
	return fake
}

// Handle answers method with fn.
func (f *Fake) Handle(method string, fn HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = fn
}

// HandleAuths answers host.auth.list with entries.
func (f *Fake) HandleAuths(entries ...pluginapi.HostAuthFileEntry) {
	f.Handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		return map[string]any{"files": entries}, nil
	})
}

// HandleHTTP answers host.http.do with fn, decoding the request the plugin
// sent.
func (f *Fake) HandleHTTP(fn func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)) {
	f.Handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HTTPRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return fn(req)
	})
}

// Call is the host transport the Fake installs.
func (f *Fake) Call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{Method: method, Payload: append(json.RawMessage(nil), payload...)})
	fn := f.handlers[method]
	f.mu.Unlock()
	if fn == nil {
		return rpc.Fail("host_call_failed", "unsupported host callback "+method), nil
	}
	result, errCall := fn(payload)
	if errCall != nil {
		var callErr *host.CallError
		if errors.As(errCall, &callErr) {
			return rpc.Fail(callErr.Code, callErr.Message, rpc.WithStatus(callErr.Status)), nil
		}
		return rpc.Fail("host_call_failed", errCall.Error()), nil
	}
	return rpc.OK(result)
}

// Calls returns the payloads of every recorded call to method.
func (f *Fake) Calls(method string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []json.RawMessage
	for _, call := range f.calls {
		if call.Method == method {
			out = append(out, call.Payload)
		}
	}
	return out
}

// AllCalls returns every recorded call in order.
func (f *Fake) AllCalls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Logs returns every host.log line recorded by the default handler.
func (f *Fake) Logs() []LogLine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LogLine(nil), f.logs...)
}

func (f *Fake) recordLog(payload json.RawMessage) (any, error) {
	var line LogLine
	if errUnmarshal := json.Unmarshal(payload, &line); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	f.mu.Lock()
	f.logs = append(f.logs, line)
	f.mu.Unlock()
	return struct{}{}, nil
}

func (f *Fake) openOperation(json.RawMessage) (any, error) {
	f.mu.Lock()
	f.operations++
	id := "op-" + strconv.Itoa(f.operations)
	f.mu.Unlock()
	return map[string]string{"operation_id": id}, nil
}
