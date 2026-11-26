// Package plugin exports the CLIProxyAPI native plugin ABI from a Go
// c-shared library and dispatches the host's calls to one Plugin.
//
// A plugin's main package registers itself from init:
//
//	func init() {
//		plugin.Serve(plugin.Plugin{ID: "my-plugin", Handle: handleMethod})
//	}
//
//	func main() {}
package plugin

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// Handler answers one host call with an encoded envelope. An error becomes a
// plugin_error envelope.
type Handler func(method string, request []byte) ([]byte, error)

// Plugin is what a library serves.
type Plugin struct {
	// ID is the plugin ID; host.Log tags every line with it.
	ID string
	// Handle answers every call the host makes.
	Handle Handler
	// Oversized answers a request too large for the ABI to copy into Go.
	// Nil fails such requests.
	Oversized func(method string, length uint64) ([]byte, error)
	// Shutdown runs once, when the host unloads the library.
	Shutdown func()
	// Redact strips secrets from messages the plugin returns or logs.
	Redact func(string) string
}

var (
	served       atomic.Pointer[Plugin]
	shutdownOnce sync.Once
)

// Serve installs p as the library's plugin. Call it once, from init.
func Serve(p Plugin) {
	if p.Handle == nil {
		panic("plugin: Serve requires a Handle function")
	}
	host.SetPluginID(p.ID)
	rpc.SetRedactor(p.Redact)
	served.Store(&p)
}

// Dispatch answers one call. A panic is returned as an error, because an
// unrecovered panic in a c-shared library aborts the whole host process.
func Dispatch(method string, request []byte) (raw []byte, err error) {
	p := served.Load()
	if p == nil {
		return rpc.Fail("plugin_unavailable", "no plugin is served"), nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			raw = nil
			err = fmt.Errorf("%s panicked: %v", method, recovered)
		}
	}()
	return p.Handle(method, request)
}

// dispatchOversized answers a request the ABI cannot copy into Go.
func dispatchOversized(method string, length uint64) (raw []byte, err error) {
	p := served.Load()
	if p == nil || p.Oversized == nil {
		return nil, fmt.Errorf("%s request of %d bytes is too large to read", method, length)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			raw = nil
			err = fmt.Errorf("%s panicked: %v", method, recovered)
		}
	}()
	return p.Oversized(method, length)
}

// shutdown runs the plugin's Shutdown from the C shutdown hook. The
// plugin.shutdown method reaches Handle like any other call.
func shutdown() {
	shutdownOnce.Do(func() {
		p := served.Load()
		if p == nil || p.Shutdown == nil {
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				host.Log("error", fmt.Sprintf("shutdown panicked: %v", recovered), nil)
			}
		}()
		p.Shutdown()
	})
}
