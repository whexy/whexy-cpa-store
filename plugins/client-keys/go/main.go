// Command client-keys is a CLIProxyAPI plugin that enforces model and
// Responses API policies per client API key and filters each key's model
// lists to match.
package main

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.3.0"

	pluginID    = "client-keys"
	displayName = "Client keys"
)

func init() {
	plugin.Serve(plugin.Plugin{ID: pluginID, Handle: handleMethod, Oversized: oversizedRequest})
}

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		return rpc.OK(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return rpc.OK(struct{}{})

	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequest(request, decide)
	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequest(request, decideAfterAuth)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(request)

	case pluginabi.MethodManagementRegister:
		return router.Register()
	case pluginabi.MethodManagementHandle:
		return router.Handle(request)
	default:
		return rpc.Fail("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() rpc.Registration {
	return rpc.Registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: rpc.NewMetadata(displayName, pluginVersion, router.LogoPath(),
			pluginapi.ConfigField{Name: "keys", Type: pluginapi.ConfigFieldTypeObject, Description: "Policies per client API key, keyed by the key's caller scope (64 hex characters). Edit them on the Client keys page in Management Center."},
		),
		Capabilities: rpc.Capabilities{
			RequestInterceptor: true,
			// The host sends every successful non-streaming response through
			// response interceptors, so this is declared only while some key
			// has a model list to filter.
			ResponseInterceptor: currentRules().filtersModelLists,
			ManagementAPI:       true,
		},
	}
}

// oversizedRequest answers a request too large to read. Interceptors refuse
// it, because an error would let it through unchecked.
func oversizedRequest(method string, length uint64) ([]byte, error) {
	switch method {
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		return rpc.OK(requestTooLarge.response())
	default:
		return nil, fmt.Errorf("%s request of %d bytes is too large to read", method, length)
	}
}
