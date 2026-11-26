// Command extra-models is a CLIProxyAPI plugin that registers configured
// model IDs for one built-in provider before its model catalog lists them.
package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.1.0"

	pluginID    = "extra-models"
	displayName = "Extra Models"
)

func init() {
	plugin.Serve(plugin.Plugin{ID: pluginID, Handle: handleMethod})
}

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		return rpc.OK(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return rpc.OK(struct{}{})
	case pluginabi.MethodModelRegister:
		return rpc.OK(currentModels().registration())
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
		// CPA HTML-escapes these descriptions and Management Center shows the
		// escapes literally, so they avoid angle brackets, ampersands and
		// quotes.
		Metadata: rpc.NewMetadata(displayName, pluginVersion, router.LogoPath(),
			pluginapi.ConfigField{Name: "provider", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: providerKeys(), Description: "The built-in provider whose accounts serve the models. The plugin adds models to this one provider."},
			pluginapi.ConfigField{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Models to add, as a JSON list of entries in the format of the CLIProxyAPI model catalog, such as {id: gemini-4-argon, display_name: Gemini 4 Argon, context_length: 1048576}. Only id is required. Without thinking, the client reasoning settings pass through unchecked."},
		),
		Capabilities: rpc.Capabilities{
			ModelRegistrar: true,
			ManagementAPI:  true,
		},
	}
}
