// Command opencode-go is a CLIProxyAPI plugin that serves the models of an
// OpenCode Go subscription, the pay-as-you-go OpenCode Zen models of the same
// workspace and OpenCode's free models, and manages its logins and keys from
// a Management Center page.
package main

import (
	"net/http"
	"regexp"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.9.0"

	providerKey = "opencode-go"
	displayName = "OpenCode Go"
	// zenProviderKey is the Console provider that holds the pay-as-you-go
	// (OpenCode Zen) models of the same workspace, next to providerKey's
	// subscription models in one config document.
	zenProviderKey = "opencode"
	defaultBaseURL = "https://opencode.ai/inference/go"
	// defaultZenBaseURL is the root of the pay-as-you-go inference paths; the
	// Go gateway is its /go subtree.
	defaultZenBaseURL     = "https://opencode.ai/inference"
	defaultConsoleURL     = "https://opencode.ai/console"
	defaultModelPrefix    = "opencode-go"
	defaultZenModelPrefix = "opencode"

	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

var executorFormats = []string{"openai", "claude", "openai-response"}

func init() {
	plugin.Serve(plugin.Plugin{
		ID:       providerKey,
		Handle:   handleMethod,
		Shutdown: func() { background.Close(shutdownGrace) },
		Redact:   redactSecrets,
	})
}

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		startCatalogWatch()
		startLoginRefresh()
		return rpc.OK(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return rpc.OK(struct{}{})

	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return rpc.OK(rpc.IdentifierResponse{Identifier: providerKey})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return handleLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)

	case pluginabi.MethodModelStatic:
		return rpc.OK(pluginapi.ModelResponse{Provider: providerKey})
	case pluginabi.MethodModelForAuth:
		return handleModelsForAuth(request)

	case pluginabi.MethodExecutorExecute:
		return executorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return executorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return executorHTTPRequest(request)

	case pluginabi.MethodQuotaDescribe:
		return rpc.OK(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{providerKey}, DisplayName: displayName})
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return rpc.Fail("unsupported", "OpenCode Go usage cannot be reset", rpc.WithStatus(http.StatusBadRequest)), nil

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
		// escapes literally, so they avoid < > & and quotes.
		Metadata: rpc.NewMetadata(displayName, pluginVersion, router.LogoPath(),
			pluginapi.ConfigField{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenCode Go gateway base URL. Default " + defaultBaseURL + "."},
			pluginapi.ConfigField{Name: "console_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenCode Console base URL, used for login, token refresh and the workspace config the model lists apply. Default: the base_url host followed by /console, which is " + defaultConsoleURL + " for the default base_url."},
			pluginapi.ConfigField{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every OpenCode Go model ID, as in " + defaultModelPrefix + "/kimi-k3. Default " + defaultModelPrefix + ". disable_prefix overrides this setting."},
			pluginapi.ConfigField{Name: "zen_model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every OpenCode Zen model ID, as in " + defaultZenModelPrefix + "/gpt-5.5. Default " + defaultZenModelPrefix + ". disable_prefix overrides this setting."},
			pluginapi.ConfigField{Name: "cloak_free_models", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Present free OpenCode Zen models as the OpenCode client, which is the only caller the free tier answers. The request carries the identity of that client and offers its bash and read tools. A caller that declares them keeps them, one that declares them under another case has them offered under the name that client uses and their calls come back under its own, and one that declares neither gets placeholders the model is told not to call, whose calls are dropped before they reach the caller. No credential is added. Default on; turning it off makes those models fail the way any other non-OpenCode client fails."},
			pluginapi.ConfigField{Name: "disable_prefix", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Serve every OpenCode Go, Zen and Free model under its bare ID only, as in kimi-k3, and ignore model_prefix and zen_model_prefix; the prefixed IDs are then not listed. A bare ID shares routing by priority and weight with every other provider and OpenCode family that serves the same ID, so disable the models you do not want on the OpenCode Go page."},
			pluginapi.ConfigField{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent sent upstream when the client sends none. Default whexy-cpa-opencode-go/" + pluginVersion + "."},
		),
		Capabilities: rpc.Capabilities{
			AuthProvider:  true,
			ModelProvider: true,
			Executor:      true,
			// Only per-auth models: the catalog depends on the key, and static
			// models whose ID a native provider serves are dropped by the host.
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  executorFormats,
			ExecutorOutputFormats: executorFormats,
			QuotaProvider:         true,
			ManagementAPI:         true,
		},
	}
}

var (
	secretPattern = regexp.MustCompile(`oc_sk_[A-Za-z0-9_\-]+`)
	// loginTokenPattern matches OpenCode Console access (st_) and refresh
	// (rt_) tokens.
	loginTokenPattern = regexp.MustCompile(`\b(st|rt)_[A-Za-z0-9_\-]{16,}`)
)

// redactSecrets removes OpenCode keys and login tokens from text the plugin
// returns or logs; upstream error bodies may echo them back.
func redactSecrets(text string) string {
	text = secretPattern.ReplaceAllString(text, "oc_sk_[redacted]")
	return loginTokenPattern.ReplaceAllString(text, "${1}_[redacted]")
}
