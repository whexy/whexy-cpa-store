package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testOtherOrgID = "wrk_TESTOTHER0000000000000000"
	usageLoginFile = "opencode-go-oauth-" + testOrgID + ".json"
)

var (
	usageNow   = time.Date(2026, 10, 22, 2, 30, 0, 0, time.UTC)
	usageSince = time.Date(2026, 10, 21, 5, 0, 0, 0, time.UTC)
)

// useUsageStore gives the test an empty usage cache whose clock reads *now.
func useUsageStore(t *testing.T, now *time.Time) {
	t.Helper()
	previous := consoleUsage
	store := newUsageStore()
	store.now = func() time.Time { return *now }
	consoleUsage = store
	t.Cleanup(func() { consoleUsage = previous })
}

// usageReplies answers Console usage paths; a key absent from it fails the
// test when requested.
type usageReplies map[string]func(req hostHTTPRequest) (int, string)

const (
	summaryJSON = `{"totalRequests":"145","totalInputTokens":"1061732","totalOutputTokens":"94839","totalCacheReadTokens":"8269440","totalCacheWrite5mTokens":"100","totalCacheWrite1hTokens":"20","totalCostMicroCents":"24305490","services":[{"service":"web-search","totalRequests":"3","totalCostMicroCents":"3000000"}]}`
	hourlyJSON  = `[{"date":"2026-10-21T12:00:00Z","totalCostMicroCents":"2733269","totalTokens":"900851","totalRequests":"30"},{"date":"2026-10-22T02:00:00Z","totalCostMicroCents":"62167","totalTokens":"1317","totalRequests":"1"}]`
	modelsJSON  = `{"items":[{"model":"deepseek-v4.1-flash","provider":"opencode-go","totalRequests":"140","totalInputTokens":"1061000","totalOutputTokens":"94000","totalCacheReadTokens":"8269440","totalCacheWrite5mTokens":"100","totalCacheWrite1hTokens":"20","totalCostMicroCents":"24305490"},{"model":"deepseek-v4.1-flash-free","provider":"opencode","totalRequests":"5","totalInputTokens":"732","totalOutputTokens":"839","totalCacheReadTokens":"0","totalCacheWrite5mTokens":"0","totalCacheWrite1hTokens":"0","totalCostMicroCents":"0"}],"pageInfo":{"page":1,"pageSize":100,"total":2,"pageCount":1}}`
)

func requestLogJSON(t *testing.T) string {
	return string(mustJSON(t, map[string]any{
		"items": []map[string]any{{
			"id": "log_1", "requestID": "req_1", "startedAt": time.Date(2026, 10, 22, 2, 20, 0, 0, time.UTC).UnixMilli(),
			"finishedAt": time.Date(2026, 10, 22, 2, 20, 1, 0, time.UTC).UnixMilli(), "durationMs": 1834, "outcome": "succeeded",
			"category": "inference", "protocol": "openai", "statusCode": 200, "requestedModel": "deepseek-v4.1-flash",
			"model": "accounts/upstream/deepseek", "provider": "upstream", "product": "go",
			"inputTokens": 1200, "outputTokens": 80, "cacheReadTokens": 7168, "cost": 0.0043,
		}, {
			"id": "log_2", "requestID": "req_2", "startedAt": time.Date(2026, 10, 22, 2, 10, 0, 0, time.UTC).UnixMilli(),
			"finishedAt": time.Date(2026, 10, 22, 2, 10, 0, 0, time.UTC).UnixMilli(), "durationMs": 40, "outcome": "rejected",
			"category": "inference", "protocol": "openai", "statusCode": 429, "model": "gpt-5.5", "provider": "opencode", "product": "standard",
		}},
		"nextCursor": nil, "until": usageNow.UnixMilli(), "retentionDays": 30,
	}))
}

func defaultUsageReplies(t *testing.T) usageReplies {
	return usageReplies{
		"/console/api/usage/summary":     func(hostHTTPRequest) (int, string) { return 200, summaryJSON },
		"/console/api/usage/cost-by-day": func(hostHTTPRequest) (int, string) { return 200, hourlyJSON },
		"/console/api/usage/models":      func(hostHTTPRequest) (int, string) { return 200, modelsJSON },
		"/console/api/request-logs": func(req hostHTTPRequest) (int, string) {
			if req.Headers.Get("X-Org-Id") == "" {
				return 403, `{"_tag":"Forbidden"}`
			}
			return 200, requestLogJSON(t)
		},
	}
}

// serveUsage answers workspace config reads with the workspace orgs names
// for each bearer, and usage reads from replies.
func (fx *managementFixture) serveUsage(orgs map[string]string, replies usageReplies) {
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		if req.URL == configURL(defaultSettings()) {
			org, ok := orgs[strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer ")]
			if !ok {
				return upstreamJSON(401, `{"error":{"type":"AuthError","message":"Invalid API key."}}`)
			}
			return upstreamJSON(200, `{"providers":{"opencode":{"headers":{"x-opencode-org-id":"`+org+`"},"models":{}},"opencode-go":{"headers":{"x-opencode-org-id":"`+org+`"},"models":{}}}}`)
		}
		parsed, errParse := url.Parse(req.URL)
		if errParse == nil {
			if reply, ok := replies[parsed.Path]; ok {
				status, body := reply(req)
				return upstreamJSON(status, body)
			}
		}
		fx.t.Errorf("unexpected upstream request %s", req.URL)
		return upstreamJSON(404, `{}`)
	}
}

// usageCalls are the Console usage requests made so far, by path.
func (fx *managementFixture) usageCalls(path string) []hostHTTPRequest {
	var out []hostHTTPRequest
	for _, req := range fx.httpCalls() {
		if parsed, errParse := url.Parse(req.URL); errParse == nil && parsed.Path == "/console"+path {
			out = append(out, req)
		}
	}
	return out
}

func (fx *managementFixture) usage(query url.Values) (managementResult, workspaceUsageView) {
	fx.t.Helper()
	resp := fx.call(http.MethodGet, workspacesPath+"/usage", query, nil)
	if resp.Status != http.StatusOK {
		return resp, workspaceUsageView{}
	}
	return resp, decodeBodyAs[workspaceUsageView](fx.t, resp)
}

func todayQuery(id string) url.Values {
	return url.Values{"id": {id}, "range": {"today"}, "since": {usageSince.Format("2006-01-02T15:04:05.000Z")}}
}

func TestUsageSummaryConvertsCountsAndMicrocents(t *testing.T) {
	var doc consoleSummary
	if errDecode := json.Unmarshal([]byte(summaryJSON), &doc); errDecode != nil {
		t.Fatal(errDecode)
	}
	got := newUsageSummaryView(doc)
	want := usageSummaryView{
		Requests: 145, InputTokens: 1061732, OutputTokens: 94839, CacheReadTokens: 8269440, CacheWriteTokens: 120,
		Tokens: 1061732 + 94839 + 8269440 + 120, Cost: 0.2430549, WebSearches: 3, WebSearchCost: 0.03,
	}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
	var counts []consoleCount
	if errDecode := json.Unmarshal([]byte(`["12", 7, null, ""]`), &counts); errDecode != nil || counts[0] != 12 || counts[1] != 7 || counts[2] != 0 || counts[3] != 0 {
		t.Fatalf("counts = %v, %v", counts, errDecode)
	}
	if errDecode := json.Unmarshal([]byte(`["1.5"]`), &counts); errDecode == nil {
		t.Fatal("a fractional count decoded")
	}
	if got := usd(100_000_000); got != 1 {
		t.Fatalf("usd = %v", got)
	}
}

func TestFillBucketsCoversTheRange(t *testing.T) {
	var hourly, daily []consoleBucket
	if errDecode := json.Unmarshal([]byte(hourlyJSON), &hourly); errDecode != nil {
		t.Fatal(errDecode)
	}
	if errDecode := json.Unmarshal([]byte(`[{"date":"2026-08-10","totalCostMicroCents":"470708","totalTokens":"25109","totalRequests":"10"},{"date":"2026-10-20","totalCostMicroCents":"100000000","totalTokens":"5","totalRequests":"2"}]`), &daily); errDecode != nil {
		t.Fatal(errDecode)
	}
	recent := daily[1:]
	for _, tc := range []struct {
		name        string
		points      []consoleBucket
		q           usageQuery
		count       int
		first, last string
		at          int
		atCost      float64
	}{
		// The day so far, in UTC hours from the hour the viewer's day starts.
		{"today", hourly, usageQuery{Range: rangeToday, Since: usageSince}, 22, "2026-10-21T05:00:00Z", "2026-10-22T02:00:00Z", 7, 0.02733269},
		{"today from a half hour", nil, usageQuery{Range: rangeToday, Since: time.Date(2026, 10, 21, 18, 30, 0, 0, time.UTC)}, 9, "2026-10-21T18:00:00Z", "2026-10-22T02:00:00Z", 0, 0},
		{"7d", recent, usageQuery{Range: range7d}, 7, "2026-10-16", "2026-10-22", 4, 1},
		{"30d", recent, usageQuery{Range: range30d}, 30, "2026-09-23", "2026-10-22", 27, 1},
		{"all from the first day used", daily, usageQuery{Range: rangeAll}, 74, "2026-08-10", "2026-10-22", 0, 0.00470708},
		{"all is at least two weeks", recent, usageQuery{Range: rangeAll}, 14, "2026-10-09", "2026-10-22", 11, 1},
		{"all with no usage", nil, usageQuery{Range: rangeAll}, 14, "2026-10-09", "2026-10-22", 0, 0},
	} {
		got := fillBuckets(tc.points, tc.q, usageNow)
		if len(got) != tc.count || got[0].Date != tc.first || got[len(got)-1].Date != tc.last || got[tc.at].Cost != tc.atCost {
			t.Fatalf("%s: %d buckets %s..%s, [%d] = %+v", tc.name, len(got), got[0].Date, got[len(got)-1].Date, tc.at, got[tc.at])
		}
		var requests int64
		for _, bucket := range got {
			requests += bucket.Requests
		}
		var want int64
		for _, point := range tc.points {
			want += int64(point.TotalRequests)
		}
		if requests != want {
			t.Fatalf("%s: %d requests in the buckets, %d returned", tc.name, requests, want)
		}
	}
}

func TestParseUsageQuery(t *testing.T) {
	got, errQuery := parseUsageQuery(url.Values{}, usageNow)
	if errQuery != nil || got.Range != rangeToday || !got.Since.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("default = %+v, %v", got, errQuery)
	}
	got, errQuery = parseUsageQuery(url.Values{"range": {"today"}, "since": {"2026-10-21T05:00:00.000Z"}}, usageNow)
	if errQuery != nil || got.consoleParams().Encode() != "range=24h&since=2026-10-21T05%3A00%3A00.000Z" || got.bucket() != "hour" {
		t.Fatalf("today = %+v %s, %v", got, got.consoleParams().Encode(), errQuery)
	}
	for name, want := range map[string]string{"7d": "range=7d", "30d": "range=30d", "all": ""} {
		got, errQuery := parseUsageQuery(url.Values{"range": {name}}, usageNow)
		if errQuery != nil || got.consoleParams().Encode() != want || got.bucket() != "day" {
			t.Fatalf("%s = %+v, %v", name, got, errQuery)
		}
	}
	for _, query := range []url.Values{
		{"range": {"24h"}},
		{"range": {"7d"}, "since": {"2026-10-21T05:00:00.000Z"}},
		{"range": {"today"}, "since": {"2026-10-19T05:00:00.000Z"}},
		{"range": {"today"}, "since": {"2026-10-22T05:00:00.000Z"}},
		{"range": {"today"}, "since": {"yesterday"}},
	} {
		if _, errQuery := parseUsageQuery(query, usageNow); errQuery == nil {
			t.Fatalf("%v was accepted", query)
		}
	}
}

func TestManagementWorkspacesGroupCredentials(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		usageLoginFile: string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)),
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"},` +
			`{"label":"Other","api_key":"` + testKeyB + `"},{"label":"Broken","api_key":"` + testKeyC + `"}]}`,
	})
	useLoginStore(t, time.Now())
	now := usageNow
	useUsageStore(t, &now)
	orgs := map[string]string{testKeyA: testOrgID, testKeyB: testOtherOrgID}
	fx.serveUsage(orgs, nil)

	resp := fx.call(http.MethodGet, workspacesPath, nil, nil)
	list := decodeBodyAs[workspaceList](t, resp)
	if resp.Status != http.StatusOK || len(list.Workspaces) != 2 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	// The login's workspace is listed once, read with the login although a
	// key reaches it too.
	if got := list.Workspaces[0]; got != (workspaceView{ID: testOrgID, Name: "Acme", Credential: "Acme · dev@example.test", Login: true}) {
		t.Fatalf("login workspace = %+v", got)
	}
	if got := list.Workspaces[1]; got != (workspaceView{ID: testOtherOrgID, Credential: "Other · " + maskKey(testKeyB)}) {
		t.Fatalf("key workspace = %+v", got)
	}
	// The key whose config cannot be read is reported, never placed in the
	// workspace of the config another key read.
	if len(list.Failures) != 1 || !strings.HasPrefix(list.Failures[0], "Broken · ") || !strings.Contains(list.Failures[0], "401") {
		t.Fatalf("failures = %v", list.Failures)
	}

	// The catalog store's config cache answers a second listing; only the
	// failed key is read again.
	configReads := len(fx.httpCalls())
	fx.call(http.MethodGet, workspacesPath, nil, nil)
	if got := len(fx.httpCalls()) - configReads; got != 1 {
		t.Fatalf("second listing made %d requests, want the failed key's alone", got)
	}
	if cat, ok := catalogs.ownCatalog(keyIDFor(testKeyB)); !ok || cat.Workspace != testOtherOrgID {
		t.Fatalf("catalog store holds %+v, %v for the key", cat, ok)
	}

	// Once the cache is due, a key whose config read fails keeps the
	// workspace its last config named.
	catalogs.now = func() time.Time { return time.Now().Add(catalogTTL + time.Minute) }
	delete(orgs, testKeyB)
	list = decodeBodyAs[workspaceList](t, fx.call(http.MethodGet, workspacesPath, nil, nil))
	if len(list.Workspaces) != 2 || list.Workspaces[1].ID != testOtherOrgID || len(list.Failures) != 1 {
		t.Fatalf("after a failed read: %+v", list)
	}
}

func TestManagementWorkspacesPreferAnUnexpiredLogin(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		usageLoginFile:     string(loginFileJSON(t, testAccessToken, testRefreshToken, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), nil)),
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `","disabled":true}]}`,
	})
	useLoginStore(t, time.Now())
	now := usageNow
	useUsageStore(t, &now)
	fx.serveUsage(map[string]string{testKeyA: testOrgID}, nil)
	list := decodeBodyAs[workspaceList](t, fx.call(http.MethodGet, workspacesPath, nil, nil))
	if len(list.Workspaces) != 1 || list.Workspaces[0] != (workspaceView{ID: testOrgID, Name: "Acme", Credential: "Work · " + maskKey(testKeyA)}) {
		t.Fatalf("workspaces = %+v", list.Workspaces)
	}

	// With only the expired login left, the workspace is still listed, and
	// reading it says what to do.
	fx.files["opencode-go.json"] = `{"type":"opencode-go","keys":[]}`
	list = decodeBodyAs[workspaceList](t, fx.call(http.MethodGet, workspacesPath, nil, nil))
	if len(list.Workspaces) != 1 || !list.Workspaces[0].Login {
		t.Fatalf("workspaces = %+v", list.Workspaces)
	}
	resp, _ := fx.usage(todayQuery(testOrgID))
	if resp.Status != http.StatusUnprocessableEntity || !strings.Contains(resp.errorText(t), "log in again") || len(fx.usageCalls("/api/usage/summary")) != 0 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
}

func TestManagementWorkspaceUsageWithLogin(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		usageLoginFile: string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)),
	})
	useLoginStore(t, time.Now())
	now := usageNow
	useUsageStore(t, &now)
	fx.serveUsage(nil, defaultUsageReplies(t))

	resp, got := fx.usage(todayQuery(testOrgID))
	if resp.Status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if got.Workspace.ID != testOrgID || !got.Workspace.Login || got.Range != rangeToday {
		t.Fatalf("workspace = %+v", got.Workspace)
	}
	if got.Summary.Error != "" || got.Summary.Requests != 145 || got.Summary.Cost != 0.2430549 || got.Summary.WebSearches != 3 {
		t.Fatalf("summary = %+v", got.Summary)
	}
	if got.Chart.Error != "" || got.Chart.Bucket != "hour" || len(got.Chart.Buckets) != 22 || got.Chart.Buckets[7].Requests != 30 {
		t.Fatalf("chart = %+v", got.Chart)
	}
	models := got.Models.Rows
	if got.Models.Error != "" || got.Models.Truncated || len(models) != 2 ||
		models[0] != (modelUsageView{Model: "deepseek-v4.1-flash", Family: "go", Requests: 140, InputTokens: 1061000, CacheReadTokens: 8269440, CacheWriteTokens: 120, OutputTokens: 94000, Cost: 0.2430549}) ||
		models[1].Family != "zen" || models[1].Cost != 0 {
		t.Fatalf("models = %+v", got.Models)
	}
	entries := got.RequestLog.Entries
	if got.RequestLog.Error != "" || got.RequestLog.NeedsLogin || len(entries) != 2 {
		t.Fatalf("request log = %+v", got.RequestLog)
	}
	if first := entries[0]; first.StartedAt != "2026-10-22T02:20:00Z" || first.DurationMs != 1834 || first.Status != 200 ||
		first.Model != "deepseek-v4.1-flash" || first.Family != "go" || first.InputTokens != 1200 || first.OutputTokens != 80 || first.Cost == nil || *first.Cost != 0.0043 {
		t.Fatalf("first entry = %+v", first)
	}
	if second := entries[1]; second.Outcome != "rejected" || second.Status != 429 || second.Model != "gpt-5.5" || second.Family != "zen" || second.Cost != nil {
		t.Fatalf("second entry = %+v", second)
	}

	// The login names its workspace, and every read sends the Console page's
	// own parameters.
	for path, want := range map[string]string{
		"/api/usage/summary":     "range=24h&since=2026-10-21T05%3A00%3A00.000Z",
		"/api/usage/cost-by-day": "bucket=hour&range=24h&since=2026-10-21T05%3A00%3A00.000Z",
		"/api/usage/models":      "costOrder=desc&page=1&pageSize=100&range=24h&since=2026-10-21T05%3A00%3A00.000Z",
		"/api/request-logs":      "category=inference&limit=40",
	} {
		calls := fx.usageCalls(path)
		if len(calls) != 1 {
			t.Fatalf("%s: %d requests", path, len(calls))
		}
		parsed, _ := url.Parse(calls[0].URL)
		if parsed.RawQuery != want || calls[0].Headers.Get("X-Org-Id") != testOrgID || calls[0].Headers.Get("Authorization") != "Bearer "+testAccessToken {
			t.Fatalf("%s: %s %v", path, calls[0].URL, calls[0].Headers)
		}
	}
}

func TestManagementWorkspaceUsageWithKey(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"}]}`,
	})
	now := usageNow
	useUsageStore(t, &now)
	replies := defaultUsageReplies(t)
	replies["/console/api/usage/summary"] = func(hostHTTPRequest) (int, string) {
		return 503, `{"_tag":"ServiceUnavailable"}`
	}
	replies["/console/api/usage/cost-by-day"] = func(hostHTTPRequest) (int, string) {
		return 200, `[{"date":"2026-10-20","totalCostMicroCents":"100000000","totalTokens":"5","totalRequests":"2"}]`
	}
	// Two pages of models: the second is read, and the rows keep the
	// Console's cost order.
	replies["/console/api/usage/models"] = func(req hostHTTPRequest) (int, string) {
		parsed, _ := url.Parse(req.URL)
		if parsed.Query().Get("page") == "2" {
			return 200, `{"items":[{"model":"glm-5.3-flash","provider":"opencode-go","totalRequests":"1","totalCostMicroCents":"10"}],"pageInfo":{"page":2,"pageSize":100,"total":101,"pageCount":2}}`
		}
		return 200, strings.Replace(modelsJSON, `"pageCount":1`, `"pageCount":2`, 1)
	}
	fx.serveUsage(map[string]string{testKeyA: testOrgID}, replies)

	resp, got := fx.usage(url.Values{"id": {testOrgID}, "range": {"7d"}})
	if resp.Status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if got.Workspace.Login || got.Workspace.Credential != "Work · "+maskKey(testKeyA) {
		t.Fatalf("workspace = %+v", got.Workspace)
	}
	// A key's 403 on the request log is its normal state, not an error.
	if !got.RequestLog.NeedsLogin || got.RequestLog.Error != "" || got.RequestLog.Entries == nil {
		t.Fatalf("request log = %+v", got.RequestLog)
	}
	if !strings.Contains(got.Summary.Error, "not answering") || !strings.Contains(got.Summary.Error, "503 ServiceUnavailable") {
		t.Fatalf("summary = %+v", got.Summary)
	}
	if got.Chart.Error != "" || got.Chart.Bucket != "day" || len(got.Chart.Buckets) != 7 || got.Chart.Buckets[4].Cost != 1 {
		t.Fatalf("chart = %+v", got.Chart)
	}
	if rows := got.Models.Rows; got.Models.Error != "" || len(rows) != 3 || rows[2].Model != "glm-5.3-flash" || got.Models.Truncated {
		t.Fatalf("models = %+v", got.Models)
	}
	for _, req := range fx.usageCalls("/api/usage/summary") {
		parsed, _ := url.Parse(req.URL)
		if parsed.RawQuery != "range=7d" || req.Headers.Get("X-Org-Id") != "" || req.Headers.Get("Authorization") != "Bearer "+testKeyA {
			t.Fatalf("summary request = %s %v", req.URL, req.Headers)
		}
	}
	if calls := fx.usageCalls("/api/usage/models"); len(calls) != 2 {
		t.Fatalf("%d model pages read", len(calls))
	}
}

func TestManagementWorkspaceUsageCapsModelPages(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"}]}`,
	})
	now := usageNow
	useUsageStore(t, &now)
	replies := defaultUsageReplies(t)
	replies["/console/api/usage/models"] = func(hostHTTPRequest) (int, string) {
		return 200, strings.Replace(modelsJSON, `"pageCount":1`, `"pageCount":9`, 1)
	}
	fx.serveUsage(map[string]string{testKeyA: testOrgID}, replies)
	_, got := fx.usage(url.Values{"id": {testOrgID}, "range": {"all"}})
	if !got.Models.Truncated || len(got.Models.Rows) != 2*maxModelPages || len(fx.usageCalls("/api/usage/models")) != maxModelPages {
		t.Fatalf("models = %d rows, truncated %v", len(got.Models.Rows), got.Models.Truncated)
	}
}

func TestManagementWorkspaceUsageCache(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"}]}`,
	})
	now := usageNow
	useUsageStore(t, &now)
	failing := true
	replies := defaultUsageReplies(t)
	replies["/console/api/usage/models"] = func(hostHTTPRequest) (int, string) {
		if failing {
			return 429, `{"_tag":"TooManyRequests"}`
		}
		return 200, modelsJSON
	}
	fx.serveUsage(map[string]string{testKeyA: testOrgID}, replies)
	count := func(path string) int { return len(fx.usageCalls(path)) }

	_, got := fx.usage(todayQuery(testOrgID))
	if !strings.Contains(got.Models.Error, "try again in a minute") {
		t.Fatalf("models = %+v", got.Models)
	}
	// Within the TTL only the failed read is asked again; the request log's
	// 403 is kept like an answer.
	failing = false
	now = now.Add(5 * time.Second)
	_, got = fx.usage(todayQuery(testOrgID))
	if got.Models.Error != "" || len(got.Models.Rows) != 2 || !got.RequestLog.NeedsLogin {
		t.Fatalf("second read = %+v", got)
	}
	if count("/api/usage/summary") != 1 || count("/api/usage/cost-by-day") != 1 || count("/api/usage/models") != 2 || count("/api/request-logs") != 1 {
		t.Fatalf("requests after 5s: summary %d, chart %d, models %d, log %d", count("/api/usage/summary"), count("/api/usage/cost-by-day"), count("/api/usage/models"), count("/api/request-logs"))
	}
	// Another range is another query.
	fx.usage(url.Values{"id": {testOrgID}, "range": {"30d"}})
	if count("/api/usage/summary") != 2 || count("/api/request-logs") != 1 {
		t.Fatalf("a new range read summary %d times, log %d times", count("/api/usage/summary"), count("/api/request-logs"))
	}
	// The request log expires after 10 seconds, the rest after 30.
	now = now.Add(6 * time.Second)
	fx.usage(todayQuery(testOrgID))
	if count("/api/usage/summary") != 2 || count("/api/request-logs") != 2 {
		t.Fatalf("after 11s: summary %d, log %d", count("/api/usage/summary"), count("/api/request-logs"))
	}
	now = now.Add(20 * time.Second)
	fx.usage(todayQuery(testOrgID))
	if count("/api/usage/summary") != 3 || count("/api/usage/cost-by-day") != 3 || count("/api/request-logs") != 3 {
		t.Fatalf("after 31s: summary %d, chart %d, log %d", count("/api/usage/summary"), count("/api/usage/cost-by-day"), count("/api/request-logs"))
	}
	// Writing an answer drops the expired ones, the other range's included.
	now = now.Add(time.Minute)
	fx.usage(todayQuery(testOrgID))
	if len(consoleUsage.entries) != 4 {
		t.Fatalf("cache holds %d entries, want the 4 just read", len(consoleUsage.entries))
	}
}

func TestManagementWorkspaceUsageRejectsBadRequests(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"}]}`,
	})
	now := usageNow
	useUsageStore(t, &now)
	fx.serveUsage(map[string]string{testKeyA: testOrgID}, nil)
	for _, tc := range []struct {
		query  url.Values
		status int
	}{
		{url.Values{"range": {"today"}}, http.StatusBadRequest},
		{url.Values{"id": {testOrgID}, "range": {"1h"}}, http.StatusBadRequest},
		{url.Values{"id": {testOrgID}, "range": {"30d"}, "since": {"2026-10-21T05:00:00Z"}}, http.StatusBadRequest},
		{url.Values{"id": {testOrgID}, "range": {"today"}, "since": {"2026-10-01T05:00:00Z"}}, http.StatusBadRequest},
		{url.Values{"id": {testOtherOrgID}, "range": {"today"}}, http.StatusNotFound},
	} {
		if resp, _ := fx.usage(tc.query); resp.Status != tc.status || resp.errorText(t) == "" {
			t.Fatalf("%v: status %d: %s", tc.query, resp.Status, resp.Body)
		}
	}
	if len(fx.usageCalls("/api/usage/summary")) != 0 {
		t.Fatal("a rejected request reached the Console")
	}
}
