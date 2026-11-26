// Command reset-first is a CLIProxyAPI scheduler plugin that routes each
// request to the offered account whose quota resets soonest.
package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.1.1"

	pluginID    = "reset-first"
	displayName = "Reset First"
)

func init() {
	plugin.Serve(plugin.Plugin{ID: pluginID, Handle: handleMethod, Shutdown: quiesce})
}

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return rpc.OK(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		quiesce()
		return rpc.OK(struct{}{})

	case pluginabi.MethodSchedulerPick:
		return handlePick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)

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
		Metadata:      rpc.NewMetadata(displayName, pluginVersion, router.LogoPath(), configFields()...),
		// The scheduler sees only the highest priority tier that has a ready
		// credential, so priorities set in CLIProxyAPI still decide first.
		Capabilities: rpc.Capabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}
