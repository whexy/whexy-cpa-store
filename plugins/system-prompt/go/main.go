// Command system-prompt is a CLIProxyAPI plugin that appends configured text
// to the system prompt of requests for the models a rule names.
package main

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.2.0"

	pluginID    = "system-prompt"
	displayName = "System Prompts"
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

	// The host calls both phases of a request interceptor. Rules match the
	// model the request resolved to, which is known only after credential
	// selection; both phases still precede the executor's translation and
	// Claude cloaking.
	case pluginabi.MethodRequestInterceptBefore:
		return rpc.OK(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequest(request)

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
			pluginapi.ConfigField{Name: "rules", Type: pluginapi.ConfigFieldTypeArray, Description: "Rules of {models, prompt}: the prompt is appended to the system prompt of requests whose model matches one of the models. Edit them on the System Prompts page in Management Center."},
		),
		Capabilities: rpc.Capabilities{
			// The host sends every model request through request
			// interceptors, so this is declared only while a rule applies.
			RequestInterceptor: len(currentRules().rules) > 0,
			ManagementAPI:      true,
		},
	}
}

// oversizedRequest answers a request too large to read. An interceptor lets
// it through unchanged rather than failing it over a prompt it cannot add.
func oversizedRequest(method string, length uint64) ([]byte, error) {
	switch method {
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		return rpc.OK(pluginapi.RequestInterceptResponse{})
	default:
		return nil, fmt.Errorf("%s request of %d bytes is too large to read", method, length)
	}
}
