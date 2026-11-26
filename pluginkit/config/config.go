// Package config reads a plugin's settings from plugin.register and
// plugin.reconfigure.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
	"gopkg.in/yaml.v3"
)

// Decode reads the plugin's node of config.yaml from a lifecycle request
// into out. An empty request or node leaves out unchanged.
func Decode(request []byte, out any) error {
	var lifecycle rpc.LifecycleRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &lifecycle); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	if len(lifecycle.ConfigYAML) == 0 {
		return nil
	}
	if errUnmarshal := yaml.Unmarshal(lifecycle.ConfigYAML, out); errUnmarshal != nil {
		return fmt.Errorf("decode plugin config: %w", errUnmarshal)
	}
	return nil
}

// URL reads an optional absolute http(s) URL setting without its trailing
// slash.
func URL(name, value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "", nil
	}
	parsed, errParse := url.Parse(value)
	if errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute http(s) URL, got %q", name, value)
	}
	return value, nil
}

// ModelPrefix reads an optional model-ID prefix without its surrounding
// slashes, rejecting the characters a model ID can never carry.
func ModelPrefix(name, value string) (string, error) {
	prefix := strings.Trim(strings.TrimSpace(value), "/")
	if prefix == "" {
		return "", nil
	}
	if strings.ContainsAny(prefix, " \t\r\n()*") {
		return "", fmt.Errorf("%s must not contain spaces, parentheses or *, got %q", name, prefix)
	}
	return prefix, nil
}
