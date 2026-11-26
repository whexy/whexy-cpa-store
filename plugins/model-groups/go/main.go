// Command model-groups is a CLIProxyAPI plugin that serves groups of models
// under one model ID each, routes every request for a group to one of its
// members, and keeps a session on the member it first used.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/datadir"
	"github.com/whexy/whexy-cpa-store/pluginkit/plugin"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const (
	pluginVersion = "0.2.1"

	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 2 * time.Second
	// providerRefreshInterval keeps the cached credential view fresh enough
	// for the page without polling the host on the request path.
	providerRefreshInterval = 15 * time.Second
	// persistInterval bounds how much a crash can lose of the session
	// bindings.
	persistInterval = 30 * time.Second
)

var (
	// pluginStateRef is the process-wide state; a plugin library has exactly
	// one.
	pluginStateRef = newPluginState(datadir.Default(pluginID))
	pluginStart    = time.Now()
	loopsOnce      sync.Once
)

func init() {
	plugin.Serve(plugin.Plugin{
		ID:        pluginID,
		Handle:    handleMethod,
		Oversized: oversizedRequest,
		Shutdown: func() {
			pluginStateRef.persist()
			background.Close(shutdownGrace)
		},
	})
}

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		pluginStateRef.applyConfig(request)
		startBackgroundLoops()
		return rpc.OK(pluginRegistration())

	case pluginabi.MethodPluginQuiesce:
		return rpc.OK(struct{}{})

	case pluginabi.MethodPluginShutdown:
		pluginStateRef.persist()
		return rpc.OK(struct{}{})

	case pluginabi.MethodModelRegister:
		return rpc.OK(registerModels())

	case pluginabi.MethodExecutorIdentifier:
		return rpc.OK(rpc.IdentifierResponse{Identifier: providerKey})

	case pluginabi.MethodModelRoute:
		return routeModel(request)

	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequestBeforeAuth(request)

	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequestAfterAuth(request)

	case pluginabi.MethodRequestComplete:
		return handleRequestComplete(request)

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
			pluginapi.ConfigField{Name: "prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every group model ID, as in " + defaultPrefix + "/fast. Default " + defaultPrefix + "."},
			pluginapi.ConfigField{Name: "affinity.enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Keep a session on the member it first used, which is what keeps CLIProxyAPI on one account for that session. Default true."},
			pluginapi.ConfigField{Name: "affinity.ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long a session keeps its member, as a duration such as 1h or 24h. Default 1h, CLIProxyAPI's own default for routing.session-affinity-ttl; keep it at least that long."},
			pluginapi.ConfigField{Name: "affinity.rebind_on_failure", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Move a session to another member as soon as its member fails. Default false, so a session only moves once its member has no credentials left."},
			pluginapi.ConfigField{Name: "groups", Type: pluginapi.ConfigFieldTypeArray, Description: "The groups and their members. Manage them on the plugin page."},
		),
		Capabilities: rpc.Capabilities{
			ModelRegistrar: true,
			ModelRouter:    true,
			// CPA lists the model IDs of a plugin without an executor only
			// through credentials of its provider, and providerKey has none.
			// The executor is never asked to serve: the router answers every
			// group ID first, and a request that falls through finds no
			// credential.
			Executor:               true,
			ExecutorModelScope:     pluginapi.ExecutorModelScopeStatic,
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ManagementAPI:          true,
		},
	}
}

// oversizedRequest answers a request the ABI cannot copy into Go. Every
// request hook leaves such a request alone rather than failing it.
func oversizedRequest(method string, length uint64) ([]byte, error) {
	switch method {
	case pluginabi.MethodModelRoute:
		return rpc.OK(pluginapi.ModelRouteResponse{})
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		return rpc.OK(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestComplete:
		return rpc.OK(struct{}{})
	default:
		return nil, fmt.Errorf("%s request of %d bytes is too large to read", method, length)
	}
}

// registerModels publishes one model ID per enabled group, with the
// capabilities every member of the group shares.
func registerModels() pluginapi.ModelRegistrationResponse {
	cfg := pluginStateRef.settingsSnapshot()
	models := make([]pluginapi.ModelInfo, 0, len(cfg.Groups))
	for _, group := range cfg.Groups {
		if !group.Enabled {
			continue
		}
		model := pluginapi.ModelInfo{
			ID:          group.ID,
			Object:      "model",
			Created:     pluginStart.Unix(),
			OwnedBy:     pluginID,
			DisplayName: group.Name,
		}
		group.Capabilities.apply(&model)
		models = append(models, model)
	}
	return pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models}
}

func handleRequestComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if errUnmarshal := json.Unmarshal(raw, &completion); errUnmarshal != nil {
		return nil, fmt.Errorf("decode request completion: %w", errUnmarshal)
	}
	pluginStateRef.observeCompletion(completion)
	return rpc.OK(struct{}{})
}

func startBackgroundLoops() {
	loopsOnce.Do(func() {
		background.Go(func(ctx context.Context) {
			pluginStateRef.catalog.refresh()
			every(ctx, providerRefreshInterval, pluginStateRef.catalog.refresh)
		})
		background.Go(func(ctx context.Context) {
			every(ctx, persistInterval, pluginStateRef.persist)
		})
	})
}

// every runs fn each interval until ctx is done.
func every(ctx context.Context, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}
