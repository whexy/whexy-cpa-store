module github.com/whexy/whexy-cpa-store/plugins/system-prompt

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.27
	github.com/tidwall/gjson v1.18.0
	github.com/tidwall/sjson v1.2.5
	github.com/whexy/whexy-cpa-store/pluginkit v0.0.0
)

require (
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/whexy/whexy-cpa-store/pluginkit => ../../../pluginkit
