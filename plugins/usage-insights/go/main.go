package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginVersion = "0.3.0"
	// Usage and Management API capabilities have used the same RPC shape since
	// schema v1; advertising the SDK's latest schema would unnecessarily reject older hosts.
	pluginSchemaVersion uint32 = 1
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

var configureMu sync.Mutex

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	configureMu.Lock()
	old := activeStore.Swap(nil)
	configureMu.Unlock()
	_ = old.close()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		if store := activeStore.Load(); store != nil {
			store.mu.Lock()
			if store.writer != nil {
				_ = store.writer.Flush()
			}
			store.mu.Unlock()
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	cfg := defaultPluginConfig()
	if len(request.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(request.ConfigYAML, &cfg); errUnmarshal != nil {
			return fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	cfg = normalizePluginConfig(cfg)

	configureMu.Lock()
	defer configureMu.Unlock()
	if !cfg.Enabled {
		old := activeStore.Swap(nil)
		return old.close()
	}
	next, errStore := newUsageStore(cfg)
	if errStore != nil {
		return errStore
	}
	old := activeStore.Swap(next)
	if errClose := old.close(); errClose != nil {
		_ = next.close()
		activeStore.CompareAndSwap(next, nil)
		return fmt.Errorf("close previous usage store: %w", errClose)
	}
	return nil
}

func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage record: %w", errUnmarshal)
	}
	store := activeStore.Load()
	if store == nil {
		return okEnvelope(struct{}{})
	}
	if errAppend := store.append(record); errAppend != nil {
		return nil, errAppend
	}
	return okEnvelope(struct{}{})
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Usage Insights",
			Version:          pluginVersion,
			Author:           "Whexy",
			GitHubRepository: "https://github.com/whexy/whexy-cpa-store",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Collect completed model-call usage records."},
				{Name: "data_file", Type: pluginapi.ConfigFieldTypeString, Description: "Append-only JSONL data file. Relative paths resolve from the CLIProxyAPI working directory."},
				{Name: "recent_records", Type: pluginapi.ConfigFieldTypeInteger, Description: "Number of recent calls kept in memory for the dashboard (0-1000)."},
				{Name: "include_api_key", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Persist the frontend API key identifier. Disabled by default to reduce sensitive data retention."},
				{Name: "pricing_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Fetch live per-million-token prices from models.dev and estimate API-equivalent cost."},
				{Name: "pricing_url", Type: pluginapi.ConfigFieldTypeString, Description: "models.dev-compatible pricing catalog URL."},
				{Name: "pricing_refresh_hours", Type: pluginapi.ConfigFieldTypeInteger, Description: "Background pricing refresh interval in hours."},
				{Name: "pricing_cache_file", Type: pluginapi.ConfigFieldTypeString, Description: "Optional local cache path for the models.dev catalog."},
				{Name: "pricing_provider_map", Type: pluginapi.ConfigFieldTypeObject, Description: "Optional CLIProxyAPI provider to models.dev provider overrides."},
				{Name: "pricing_model_map", Type: pluginapi.ConfigFieldTypeObject, Description: "Optional provider/model to models.dev provider/model overrides."},
			},
		},
		Capabilities: registrationCapability{UsagePlugin: true, ManagementAPI: true},
	}
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/plugins/usage-insights/dashboard"},
			{Method: http.MethodGet, Path: "/plugins/usage-insights/summary"},
			{Method: http.MethodGet, Path: "/plugins/usage-insights/models"},
			{Method: http.MethodGet, Path: "/plugins/usage-insights/export.csv"},
			{Method: http.MethodPost, Path: "/plugins/usage-insights/pricing/refresh"},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var request pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	store := activeStore.Load()
	if store == nil {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusServiceUnavailable,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       []byte(`{"error":"usage collection is disabled"}`),
		})
	}
	if strings.HasSuffix(request.Path, "/pricing/refresh") {
		if store.pricing == nil || !store.pricing.enabled {
			return okEnvelope(pluginapi.ManagementResponse{
				StatusCode: http.StatusServiceUnavailable,
				Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
				Body:       []byte(`{"error":"live pricing is disabled"}`),
			})
		}
		if errRefresh := store.pricing.refresh(nil); errRefresh != nil {
			store.pricing.setError(errRefresh)
			body, errMarshal := json.Marshal(map[string]string{"error": errRefresh.Error()})
			if errMarshal != nil {
				return nil, errMarshal
			}
			return okEnvelope(pluginapi.ManagementResponse{
				StatusCode: http.StatusBadGateway,
				Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
				Body:       body,
			})
		}
		body, errMarshal := json.MarshalIndent(store.pricing.status(), "", "  ")
		if errMarshal != nil {
			return nil, errMarshal
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       append(body, '\n'),
		})
	}
	if strings.HasSuffix(request.Path, "/models") {
		from, to, errPeriod := modelUsagePeriodFromQuery(request.Query, time.Now().UTC())
		if errPeriod != nil {
			return managementJSONError(http.StatusBadRequest, errPeriod.Error())
		}
		report, errReport := store.modelUsage(from, to)
		if errReport != nil {
			return nil, errReport
		}
		body, errMarshal := json.MarshalIndent(report, "", "  ")
		if errMarshal != nil {
			return nil, errMarshal
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       append(body, '\n'),
		})
	}

	snapshot := store.snapshot()
	switch {
	case strings.HasSuffix(request.Path, "/dashboard"):
		body, errRender := renderDashboard(snapshot)
		if errRender != nil {
			return nil, errRender
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       body,
		})
	case strings.HasSuffix(request.Path, "/summary"):
		body, errMarshal := json.MarshalIndent(snapshot, "", "  ")
		if errMarshal != nil {
			return nil, errMarshal
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       append(body, '\n'),
		})
	case strings.HasSuffix(request.Path, "/export.csv"):
		body, errCSV := renderCSV(snapshot.Recent)
		if errCSV != nil {
			return nil, errCSV
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":        []string{"text/csv; charset=utf-8"},
				"Content-Disposition": []string{`attachment; filename="usage-insights-recent.csv"`},
			},
			Body: body,
		})
	default:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte("not found\n")})
	}
}

func modelUsagePeriodFromQuery(query map[string][]string, now time.Time) (time.Time, time.Time, error) {
	from := time.Unix(0, 0).UTC()
	to := now.UTC()
	var err error
	if value := strings.TrimSpace(firstQueryValue(query, "from")); value != "" {
		from, err = parseReportTime(value)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from: %w", err)
		}
	}
	if value := strings.TrimSpace(firstQueryValue(query, "to")); value != "" {
		to, err = parseReportTime(value)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to: %w", err)
		}
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be earlier than to")
	}
	return from, to, nil
}

func firstQueryValue(query map[string][]string, key string) string {
	values := query[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func parseReportTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("use RFC3339 or YYYY-MM-DD")
}

func managementJSONError(statusCode int, message string) ([]byte, error) {
	body, errMarshal := json.Marshal(map[string]string{"error": message})
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       append(body, '\n'),
	})
}

func renderCSV(records []persistedRecord) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	header := []string{"requested_at", "provider", "model", "alias", "credential", "failed", "latency_ms", "ttft_ms", "uncached_input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "cache_ratio", "pricing_provider", "pricing_model", "estimated_api_cost_usd"}
	if errWrite := writer.Write(header); errWrite != nil {
		return nil, errWrite
	}
	for _, record := range records {
		pricingProvider := ""
		pricingModel := ""
		estimatedCost := ""
		if record.APICost != nil {
			pricingProvider = record.APICost.Provider
			pricingModel = record.APICost.Model
			estimatedCost = strconv.FormatFloat(record.APICost.TotalCostUSD, 'f', 9, 64)
		}
		row := []string{
			record.RequestedAt.Format(time.RFC3339Nano),
			record.Provider,
			record.Model,
			record.Alias,
			credentialKey(record),
			strconv.FormatBool(record.Failed),
			strconv.FormatFloat(record.LatencyMS, 'f', 3, 64),
			strconv.FormatFloat(record.TTFTMS, 'f', 3, 64),
			strconv.FormatInt(record.UncachedInput, 10),
			strconv.FormatInt(record.CacheReadTokens, 10),
			strconv.FormatInt(record.CacheWriteTokens, 10),
			strconv.FormatInt(record.OutputTokens, 10),
			strconv.FormatInt(record.ReasoningTokens, 10),
			strconv.FormatInt(record.TotalTokens, 10),
			strconv.FormatFloat(record.CacheRatio, 'f', 6, 64),
			pricingProvider,
			pricingModel,
			estimatedCost,
		}
		if errWrite := writer.Write(row); errWrite != nil {
			return nil, errWrite
		}
	}
	writer.Flush()
	if errCSV := writer.Error(); errCSV != nil {
		return nil, errCSV
	}
	return buffer.Bytes(), nil
}

func renderDashboard(data snapshot) ([]byte, error) {
	var buffer bytes.Buffer
	if errExecute := dashboardTemplate.Execute(&buffer, data); errExecute != nil {
		return nil, fmt.Errorf("render dashboard: %w", errExecute)
	}
	return buffer.Bytes(), nil
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"integer":    func(value int64) string { return strconv.FormatInt(value, 10) },
	"percent":    func(value float64) string { return strconv.FormatFloat(value*100, 'f', 1, 64) + "%" },
	"millis":     func(value float64) string { return strconv.FormatFloat(value, 'f', 1, 64) },
	"money":      func(value float64) string { return strconv.FormatFloat(value, 'f', 6, 64) },
	"credential": credentialKey,
	"dict": func(values ...any) map[string]any {
		out := make(map[string]any, len(values)/2)
		for index := 0; index+1 < len(values); index += 2 {
			key, _ := values[index].(string)
			out[key] = values[index+1]
		}
		return out
	},
	"timefmt": func(value time.Time) string {
		if value.IsZero() {
			return "—"
		}
		return value.Local().Format("2006-01-02 15:04:05")
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Usage Insights</title>
<style>
:root{color-scheme:light dark;font-family:ui-sans-serif,system-ui,sans-serif}body{margin:0;background:#0b1020;color:#e5e7eb}.page{max-width:1500px;margin:auto;padding:28px}.muted{color:#94a3b8}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px;margin:20px 0}.card,.panel{background:#11182d;border:1px solid #26314d;border-radius:12px;padding:16px}.value{font-size:28px;font-weight:700;margin-top:6px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(420px,1fr));gap:16px}table{width:100%;border-collapse:collapse;font-size:14px}th,td{text-align:left;padding:9px;border-bottom:1px solid #26314d;white-space:nowrap}th{color:#93c5fd}h1,h2{margin:0 0 10px}.scroll{overflow:auto}.bad{color:#fca5a5}.good{color:#86efac}code{color:#c4b5fd}@media(max-width:600px){.page{padding:14px}.grid{grid-template-columns:1fr}}
</style></head><body><main class="page">
<h1>Usage Insights</h1><div class="muted">Generated {{timefmt .GeneratedAt}} · Data file <code>{{.DataFile}}</code></div><div class="muted">Pricing: {{if .Pricing.Enabled}}models.dev fetched {{timefmt .Pricing.FetchedAt}}{{if .Pricing.LastError}} · last refresh error: {{.Pricing.LastError}}{{end}}{{else}}disabled{{end}}</div>
<section class="cards">
<div class="card"><div class="muted">Requests</div><div class="value">{{integer .Overall.Requests}}</div></div>
<div class="card"><div class="muted">Total tokens</div><div class="value">{{integer .Overall.TotalTokens}}</div></div>
<div class="card"><div class="muted">Uncached input</div><div class="value">{{integer .Overall.UncachedInput}}</div></div>
<div class="card"><div class="muted">Cache read</div><div class="value">{{percent .Overall.CacheReadRatio}}</div><div class="muted">{{integer .Overall.CacheReadTokens}} tokens</div></div>
<div class="card"><div class="muted">Cache write</div><div class="value">{{percent .Overall.CacheWriteRatio}}</div><div class="muted">{{integer .Overall.CacheWriteTokens}} tokens</div></div>
<div class="card"><div class="muted">Output / reasoning</div><div class="value">{{integer .Overall.OutputTokens}}</div><div class="muted">{{integer .Overall.ReasoningTokens}} reasoning</div></div>
<div class="card"><div class="muted">Average latency</div><div class="value">{{millis .Overall.AverageLatencyMS}} ms</div><div class="muted">TTFT {{millis .Overall.AverageTTFTMS}} ms</div></div>
<div class="card"><div class="muted">Estimated API cost</div><div class="value">${{money .Overall.EstimatedAPICostUSD}}</div><div class="muted">{{integer .Overall.PricedRequests}} priced · {{integer .Overall.UnpricedRequests}} unpriced</div></div>
<div class="card"><div class="muted">Failures</div><div class="value {{if .Overall.FailedRequests}}bad{{else}}good{{end}}">{{integer .Overall.FailedRequests}}</div></div>
</section>
<section class="grid">
{{template "group" (dict "Title" "By provider" "Rows" .ByProvider)}}
{{template "group" (dict "Title" "By model" "Rows" .ByModel)}}
{{template "group" (dict "Title" "By credential" "Rows" .ByCredential)}}
</section>
<section class="panel" style="margin-top:16px"><h2>Recent calls</h2><div class="scroll"><table><thead><tr><th>Time</th><th>Provider</th><th>Model</th><th>Credential</th><th>Status</th><th>Latency</th><th>Uncached</th><th>Cache read</th><th>Cache write</th><th>Output</th><th>Total</th><th>Cache ratio</th><th>API cost</th></tr></thead><tbody>
{{range .Recent}}<tr><td>{{timefmt .RequestedAt}}</td><td>{{.Provider}}</td><td>{{.Model}}</td><td>{{credential .}}</td><td>{{if .Failed}}<span class="bad">failed {{.FailureStatusCode}}</span>{{else}}<span class="good">ok</span>{{end}}</td><td>{{millis .LatencyMS}} ms</td><td>{{integer .UncachedInput}}</td><td>{{integer .CacheReadTokens}}</td><td>{{integer .CacheWriteTokens}}</td><td>{{integer .OutputTokens}}</td><td>{{integer .TotalTokens}}</td><td>{{percent .CacheRatio}}</td><td>{{if .APICost}}${{money .APICost.TotalCostUSD}}{{else}}—{{end}}</td></tr>{{else}}<tr><td colspan="13" class="muted">No calls collected yet.</td></tr>{{end}}
</tbody></table></div></section>
</main></body></html>
{{define "group"}}<section class="panel"><h2>{{.Title}}</h2><div class="scroll"><table><thead><tr><th>Name</th><th>Requests</th><th>Tokens</th><th>Cache ratio</th><th>API cost</th><th>Failures</th></tr></thead><tbody>{{range .Rows}}<tr><td>{{.Key}}</td><td>{{integer .Requests}}</td><td>{{integer .TotalTokens}}</td><td>{{percent .CacheRatio}}</td><td>${{money .EstimatedAPICostUSD}}</td><td>{{integer .FailedRequests}}</td></tr>{{else}}<tr><td colspan="6" class="muted">No data.</td></tr>{{end}}</tbody></table></div></section>{{end}}`))

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
