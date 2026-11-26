package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// CPA lists a plugin's model IDs without a credential only when the plugin
// declares an executor for the provider those IDs are registered under.
func TestGroupIDsAreRegisteredUnderTheExecutorsProvider(t *testing.T) {
	caps := pluginRegistration().Capabilities
	if !caps.Executor || caps.ExecutorModelScope != pluginapi.ExecutorModelScopeStatic {
		t.Fatalf("registration declares executor=%v scope=%q, want a static-model executor", caps.Executor, caps.ExecutorModelScope)
	}

	raw, errHandle := handleMethod(pluginabi.MethodExecutorIdentifier, nil)
	if errHandle != nil {
		t.Fatalf("executor.identifier: %v", errHandle)
	}
	var identifier rpc.IdentifierResponse
	decodeResult(t, raw, &identifier)

	newFakeHost(t)
	withPluginState(t, newTestState(t, roundRobinConfig))
	if provider := registerModels().Provider; identifier.Identifier != provider {
		t.Fatalf("executor serves %q, group IDs are registered under %q", identifier.Identifier, provider)
	}
}
