// Command zed is a CLIProxyAPI plugin that serves Zed-hosted models through
// signed-in Zed accounts: sign-in, model catalogs, request translation, plan
// usage and a Management Center page.
package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.2.0"

	providerKey = "zed"
	displayName = "Zed"
	// defaultBaseURL is Zed's cloud edge, where account calls and the LLM
	// gateway share a host.
	defaultBaseURL     = "https://cloud.zed.dev"
	defaultSignInURL   = "https://zed.dev/native_app_signin"
	defaultModelPrefix = "zed"

	// shutdownGrace bounds how long background work may observe cancellation
	// before the host unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

var executorFormats = []string{"claude", "openai", "openai-response", "gemini"}

func init() {
	plugin.Serve(plugin.Plugin{
		ID:        providerKey,
		Handle:    handleMethod,
		Oversized: oversizedRequest,
		Shutdown:  shutdown,
		Redact:    redactSecrets,
	})
}

func main() {}

func shutdown() {
	logins.closeAll()
	background.Close(shutdownGrace)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
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
		return executorCountTokens()
	case pluginabi.MethodExecutorHTTPRequest:
		return executorHTTPRequest()

	case pluginabi.MethodQuotaDescribe:
		return rpc.OK(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{providerKey}, DisplayName: displayName})
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return rpc.Fail("unsupported", "Zed usage cannot be reset", rpc.WithStatus(http.StatusBadRequest)), nil

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
			pluginapi.ConfigField{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Zed cloud base URL. Default " + defaultBaseURL + "."},
			pluginapi.ConfigField{Name: "sign_in_url", Type: pluginapi.ConfigFieldTypeString, Description: "The Zed sign-in page the OAuth Login tile opens. Default " + defaultSignInURL + "."},
			pluginapi.ConfigField{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every Zed model ID, as in " + defaultModelPrefix + "/claude-sonnet-5-5. Default " + defaultModelPrefix + ". disable_prefix overrides this setting."},
			pluginapi.ConfigField{Name: "disable_prefix", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Serve every Zed model under its bare ID only, as in claude-sonnet-5-5, and ignore model_prefix. A bare ID then shares routing by priority and weight with every other provider that serves the same ID."},
			pluginapi.ConfigField{Name: "dashboard_cookie", Type: pluginapi.ConfigFieldTypeString, Description: "The Cookie header of a signed-in dashboard.zed.dev session. Zed exposes plan usage only to the dashboard, so the Plugin tab shows spend and limit only when this and dashboard_account are set. It is sent only to the dashboard usage endpoint, not to /completions."},
			pluginapi.ConfigField{Name: "dashboard_account", Type: pluginapi.ConfigFieldTypeString, Description: "The Zed username the dashboard cookie belongs to. Usage meters are shown only for that account; the dashboard endpoint does not name an account, so another auth would otherwise see this account's spend."},
			pluginapi.ConfigField{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent sent to Zed. Default Zed/" + clientVersion + " (linux; x86_64); the edge identifies the client from it."},
		),
		Capabilities: rpc.Capabilities{
			AuthProvider:  true,
			ModelProvider: true,
			Executor:      true,
			// Models depend on the account's plan, and a static ID another
			// provider already serves is dropped by the host.
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  executorFormats,
			ExecutorOutputFormats: executorFormats,
			QuotaProvider:         true,
			ManagementAPI:         true,
		},
	}
}

// oversizedRequest answers a request too large to copy into Go. Only an
// executor call can carry a client payload that large, and the client should
// learn its request was too big rather than see a plugin failure.
func oversizedRequest(method string, length uint64) ([]byte, error) {
	switch method {
	case pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream, pluginabi.MethodExecutorCountTokens:
		return rpc.Fail("request_too_large", fmt.Sprintf("the request of %d bytes is too large for the Zed plugin", length), rpc.WithStatus(http.StatusRequestEntityTooLarge)), nil
	default:
		return nil, fmt.Errorf("%s request of %d bytes is too large to read", method, length)
	}
}
