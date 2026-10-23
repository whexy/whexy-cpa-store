package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("executor.identifier answered %s", string(raw))
	}
	var identifier identifierResponse
	if errUnmarshal := json.Unmarshal(env.Result, &identifier); errUnmarshal != nil {
		t.Fatalf("decode identifier: %v", errUnmarshal)
	}

	withPluginState(t, newTestState(t, roundRobinConfig, newFakeHost()))
	if provider := registerModels().Provider; identifier.Identifier != provider {
		t.Fatalf("executor serves %q, group IDs are registered under %q", identifier.Identifier, provider)
	}
}
