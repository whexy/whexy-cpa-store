// Command usage-dashboard is a CLIProxyAPI plugin that records token usage
// and provider quota on disk and charts them on a Management Center page.
package main

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.8.0"

	pluginID    = "usage-dashboard"
	displayName = "Usage Dashboard"
	menuLabel   = "Usage"

	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

func init() {
	plugin.Serve(plugin.Plugin{ID: pluginID, Handle: handleMethod, Shutdown: shutdown})
}

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return rpc.OK(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		if active := activeStore(); active != nil {
			active.quiesce()
		}
		return rpc.OK(struct{}{})
	case pluginabi.MethodPluginShutdown:
		shutdown()
		return rpc.OK(struct{}{})

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
		Metadata: rpc.NewMetadata(displayName, pluginVersion, router.LogoPath(),
			pluginapi.ConfigField{Name: "data_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory for the usage files. Default: data/usage-dashboard beside the plugins directory that holds this plugin, so it lands on the same volume. Relative paths resolve from CLIProxyAPI's working directory."},
			pluginapi.ConfigField{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: fmt.Sprintf("Days of usage to keep, 1 to %d. Default %d. The page offers the 90-day range only when this is at least 90.", maxRetentionDays, defaultRetentionDays)},
			pluginapi.ConfigField{Name: "pricing", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Estimate cost from models.dev prices. Default true."},
			pluginapi.ConfigField{Name: "pricing_url", Type: pluginapi.ConfigFieldTypeString, Description: "models.dev-format price catalog. Default " + defaultPricingURL + "."},
			pluginapi.ConfigField{Name: "quota_poll_interval", Type: pluginapi.ConfigFieldTypeString, Description: "How often to read quota from the usage endpoints of providers that send none in response headers (Grok, Meta, OpenCode Go), as a duration such as 15m. Default 15m, at least 1m; off records only the quota Claude and Codex send with responses."},
			pluginapi.ConfigField{Name: "model_families", Type: pluginapi.ConfigFieldTypeObject, Description: "Extra model family rules: a glob over the model ID (without any provider prefix) mapped to a family name, e.g. {\"my-coder-*\": \"Qwen\"}. Checked before the built-in rules."},
		),
		Capabilities: rpc.Capabilities{
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}
