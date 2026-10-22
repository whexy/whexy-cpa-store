package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The usage routes read the endpoints behind the Console's workspace usage
// page. The Console bundle labels them internal, so their shapes are the ones
// observed in October 2026 and may change without notice.
const (
	workspacesPath = managementBasePath + "/workspaces"

	// usageTTL and requestLogTTL are how long the Console's own page reuses
	// these answers; asking more often only spends OpenCode's rate limit.
	usageTTL      = 30 * time.Second
	requestLogTTL = 10 * time.Second
	// modelPageSize is the largest page /api/usage/models serves, and
	// maxModelPages bounds the pages read for one range.
	modelPageSize = 100
	maxModelPages = 5
	// requestLogLimit is how many requests the Console's overview lists.
	requestLogLimit = 40
	// microcentsPerUSD converts the Console's amounts, which count
	// millionths of a cent.
	microcentsPerUSD = 100_000_000
)

// Ranges of the Console's usage pages: the day so far in hours, and the last
// 7 or 30 UTC days or all time in days.
const (
	rangeToday = "today"
	range7d    = "7d"
	range30d   = "30d"
	rangeAll   = "all"
)

// workspaceReader is a login or key that can read a workspace's usage.
type workspaceReader struct {
	cred credential
	// orgID is sent as x-org-id with a login's token, which may reach
	// several workspaces. A key is bound to its workspace and sends none:
	// the Console refuses a key that names any workspace but its own.
	orgID    string
	login    bool
	disabled bool
	expired  bool
}

// rank orders a workspace's readers. Logins come first because only they
// can read the request log, and an expired login comes last, kept only to
// name what is wrong with a workspace nothing else reaches.
func (r workspaceReader) rank() int {
	rank := 0
	switch {
	case r.login && r.expired:
		rank = 4
	case !r.login:
		rank = 2
	}
	if r.disabled {
		rank++
	}
	return rank
}

// usageWorkspace is a workspace some login or key reaches, with its readers
// best first.
type usageWorkspace struct {
	ID      string
	Name    string
	readers []workspaceReader
}

type workspaceView struct {
	ID string `json:"id"`
	// Name is the workspace name a login recorded; keys cannot read it.
	Name string `json:"name,omitempty"`
	// Credential names the login or key the usage is read with, and Login
	// says it is a login: a key cannot read the request log.
	Credential string `json:"credential"`
	Login      bool   `json:"login"`
}

func (w usageWorkspace) view() workspaceView {
	reader := w.readers[0]
	return workspaceView{ID: w.ID, Name: w.Name, Credential: reader.cred.Name, Login: reader.login}
}

// listWorkspaces groups every login and key by workspace. failures name the
// keys whose workspace could not be learned.
func listWorkspaces(ctx context.Context, h host) ([]usageWorkspace, []string, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return nil, nil, errLoad
	}
	cfg := currentSettings()
	now := time.Now()
	var out []usageWorkspace
	index := map[string]int{}
	add := func(id, name string, reader workspaceReader) {
		at, seen := index[id]
		if !seen {
			at = len(out)
			index[id] = at
			out = append(out, usageWorkspace{ID: id})
		}
		if out[at].Name == "" {
			out[at].Name = name
		}
		out[at].readers = append(out[at].readers, reader)
	}
	for _, stored := range files.Logins {
		login := logins.current(stored.Login)
		add(login.OrgID, login.Workspace, workspaceReader{
			cred:     login.credential(),
			orgID:    login.OrgID,
			login:    true,
			disabled: login.Disabled,
			expired:  login.expired(now),
		})
	}
	var failures []string
	for _, file := range files.Keys {
		for _, entry := range file.File.Keys {
			cred := keyCredential(entry)
			id, errWorkspace := keyWorkspace(ctx, h, cfg, entry)
			if errWorkspace != nil {
				failures = append(failures, cred.Name+": "+errWorkspace.Error())
				continue
			}
			add(id, "", workspaceReader{cred: cred, disabled: entry.Disabled})
		}
	}
	for at := range out {
		readers := out[at].readers
		sort.SliceStable(readers, func(i, j int) bool { return readers[i].rank() < readers[j].rank() })
	}
	return out, failures, nil
}

// keyWorkspace is the workspace a key belongs to, which only the key's
// workspace config names. It is read through the catalog store, so the
// model lists and the usage share one read every catalogTTL; a fallback to
// another credential's config would name the wrong workspace, so only the
// key's own config counts.
func keyWorkspace(ctx context.Context, doer httpDoer, cfg settings, entry keyEntry) (string, error) {
	base, _ := modelsDev.cached()
	cat, source, errFetch := catalogs.forKey(ctx, doer, cfg, keyCredential(entry), base)
	if errFetch != nil && source != catalogStale {
		return "", errFetch
	}
	if cat.Workspace == "" {
		return "", errors.New("OpenCode did not say which workspace the key belongs to")
	}
	return cat.Workspace, nil
}

type workspaceList struct {
	Workspaces []workspaceView `json:"workspaces"`
	Failures   []string        `json:"failures,omitempty"`
}

func handleListWorkspaces(h host, _ pluginapi.ManagementRequest) (int, any, error) {
	found, failures, errList := listWorkspaces(context.Background(), h)
	if errList != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errList)
	}
	list := workspaceList{Workspaces: []workspaceView{}, Failures: failures}
	for _, workspace := range found {
		list.Workspaces = append(list.Workspaces, workspace.view())
	}
	return http.StatusOK, list, nil
}

// usageQuery is the range a usage read covers. Since is the start of the
// viewer's day for rangeToday, which only the browser knows.
type usageQuery struct {
	Range string
	Since time.Time
}

// parseUsageQuery reads ?range= and, for today, ?since=. Without since,
// today starts at midnight UTC, as it does on the Console.
func parseUsageQuery(query url.Values, now time.Time) (usageQuery, error) {
	out := usageQuery{Range: strings.TrimSpace(query.Get("range"))}
	rawSince := strings.TrimSpace(query.Get("since"))
	switch out.Range {
	case "":
		out.Range = rangeToday
	case rangeToday, range7d, range30d, rangeAll:
	default:
		return usageQuery{}, failf(http.StatusBadRequest, "Unknown range %q: use today, 7d, 30d or all.", out.Range)
	}
	if out.Range != rangeToday {
		if rawSince != "" {
			return usageQuery{}, failf(http.StatusBadRequest, "since applies only to the today range.")
		}
		return out, nil
	}
	if rawSince == "" {
		out.Since = now.UTC().Truncate(24 * time.Hour)
		return out, nil
	}
	since, errParse := time.Parse(time.RFC3339Nano, rawSince)
	// The start of anyone's day lies within the last 26 hours; the margins
	// allow for a browser clock that is off.
	if errParse != nil || since.Before(now.Add(-36*time.Hour)) || since.After(now.Add(time.Hour)) {
		return usageQuery{}, failf(http.StatusBadRequest, "since must be the start of today as an ISO time, such as 2026-10-21T04:00:00.000Z.")
	}
	out.Since = since.UTC()
	return out, nil
}

// consoleParams are the range parameters the Console's page sends: today is
// range 24h from since, and all time sends no range.
func (q usageQuery) consoleParams() url.Values {
	params := url.Values{}
	switch q.Range {
	case rangeToday:
		params.Set("range", "24h")
		params.Set("since", q.Since.UTC().Format("2006-01-02T15:04:05.000Z"))
	case range7d, range30d:
		params.Set("range", q.Range)
	}
	return params
}

func (q usageQuery) bucket() string {
	if q.Range == rangeToday {
		return "hour"
	}
	return "day"
}

// consoleCount is a Console count or amount, which the API sends as a
// decimal string.
type consoleCount int64

func (c *consoleCount) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		*c = 0
		return nil
	}
	value, errParse := strconv.ParseInt(text, 10, 64)
	if errParse != nil {
		return fmt.Errorf("read count %s: %w", raw, errParse)
	}
	*c = consoleCount(value)
	return nil
}

func usd(microcents consoleCount) float64 {
	return float64(microcents) / microcentsPerUSD
}

type consoleSummary struct {
	TotalRequests           consoleCount `json:"totalRequests"`
	TotalInputTokens        consoleCount `json:"totalInputTokens"`
	TotalOutputTokens       consoleCount `json:"totalOutputTokens"`
	TotalCacheReadTokens    consoleCount `json:"totalCacheReadTokens"`
	TotalCacheWrite5mTokens consoleCount `json:"totalCacheWrite5mTokens"`
	TotalCacheWrite1hTokens consoleCount `json:"totalCacheWrite1hTokens"`
	TotalCostMicroCents     consoleCount `json:"totalCostMicroCents"`
	Services                []struct {
		Service             string       `json:"service"`
		TotalRequests       consoleCount `json:"totalRequests"`
		TotalCostMicroCents consoleCount `json:"totalCostMicroCents"`
	} `json:"services"`
}

// usageSummaryView is the range's totals; Cost is in USD. Every part of the
// view carries its own Error, so one failed read leaves the others shown.
type usageSummaryView struct {
	Error            string  `json:"error,omitempty"`
	Requests         int64   `json:"requests"`
	Tokens           int64   `json:"tokens"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	Cost             float64 `json:"cost"`
	// WebSearches and WebSearchCost are the web search service's share,
	// which Requests and Cost include.
	WebSearches   int64   `json:"web_searches"`
	WebSearchCost float64 `json:"web_search_cost"`
}

func newUsageSummaryView(doc consoleSummary) usageSummaryView {
	cacheWrite := int64(doc.TotalCacheWrite5mTokens + doc.TotalCacheWrite1hTokens)
	view := usageSummaryView{
		Requests:         int64(doc.TotalRequests),
		InputTokens:      int64(doc.TotalInputTokens),
		OutputTokens:     int64(doc.TotalOutputTokens),
		CacheReadTokens:  int64(doc.TotalCacheReadTokens),
		CacheWriteTokens: cacheWrite,
		Cost:             usd(doc.TotalCostMicroCents),
	}
	view.Tokens = view.InputTokens + view.OutputTokens + view.CacheReadTokens + cacheWrite
	for _, service := range doc.Services {
		if service.Service == "web-search" {
			view.WebSearches += int64(service.TotalRequests)
			view.WebSearchCost += usd(service.TotalCostMicroCents)
		}
	}
	return view
}

type consoleBucket struct {
	Date                string       `json:"date"`
	TotalCostMicroCents consoleCount `json:"totalCostMicroCents"`
	TotalTokens         consoleCount `json:"totalTokens"`
	TotalRequests       consoleCount `json:"totalRequests"`
}

type usageChartView struct {
	Error string `json:"error,omitempty"`
	// Bucket is hour or day; Buckets holds every one of the range, empty
	// ones included.
	Bucket  string            `json:"bucket"`
	Buckets []usageBucketView `json:"buckets"`
}

type usageBucketView struct {
	// Date is an hour's start in UTC, such as 2026-10-21T14:00:00Z, or a
	// UTC day, such as 2026-10-21.
	Date     string  `json:"date"`
	Cost     float64 `json:"cost"`
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
}

// bucketStart reads a Console bucket date: an RFC 3339 hour or a bare day.
func bucketStart(date string) (time.Time, bool) {
	if parsed, errParse := time.Parse(time.RFC3339Nano, date); errParse == nil {
		return parsed.UTC(), true
	}
	if parsed, errParse := time.Parse(time.DateOnly, date); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

// fillBuckets adds the empty buckets the Console leaves out, over the span
// its page draws: the hours from the start of today to now, the range's UTC
// days up to today, and for all time the days from the first one with usage,
// but at least two weeks.
func fillBuckets(points []consoleBucket, q usageQuery, now time.Time) []usageBucketView {
	hourly := q.Range == rangeToday
	format := func(t time.Time) string {
		if hourly {
			return t.Format(time.RFC3339)
		}
		return t.Format(time.DateOnly)
	}
	byDate := make(map[string]consoleBucket, len(points))
	var first time.Time
	for _, point := range points {
		start, ok := bucketStart(point.Date)
		if !ok {
			continue
		}
		byDate[format(start)] = point
		if first.IsZero() || start.Before(first) {
			first = start
		}
	}
	var start, end time.Time
	step := func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	if hourly {
		start, end = q.Since.UTC().Truncate(time.Hour), now.UTC().Truncate(time.Hour)
		step = func(t time.Time) time.Time { return t.Add(time.Hour) }
	} else {
		end = now.UTC().Truncate(24 * time.Hour)
		switch q.Range {
		case range7d:
			start = end.AddDate(0, 0, -6)
		case range30d:
			start = end.AddDate(0, 0, -29)
		default:
			start = end.AddDate(0, 0, -13)
			if !first.IsZero() && first.Before(start) {
				start = first
			}
		}
	}
	if end.Before(start) {
		end = start
	}
	out := []usageBucketView{}
	for at := start; !at.After(end); at = step(at) {
		key := format(at)
		point := byDate[key]
		out = append(out, usageBucketView{
			Date:     key,
			Cost:     usd(point.TotalCostMicroCents),
			Tokens:   int64(point.TotalTokens),
			Requests: int64(point.TotalRequests),
		})
	}
	return out
}

type consoleModelPage struct {
	Items []struct {
		Model                   string       `json:"model"`
		Provider                string       `json:"provider"`
		TotalRequests           consoleCount `json:"totalRequests"`
		TotalInputTokens        consoleCount `json:"totalInputTokens"`
		TotalOutputTokens       consoleCount `json:"totalOutputTokens"`
		TotalCacheReadTokens    consoleCount `json:"totalCacheReadTokens"`
		TotalCacheWrite5mTokens consoleCount `json:"totalCacheWrite5mTokens"`
		TotalCacheWrite1hTokens consoleCount `json:"totalCacheWrite1hTokens"`
		TotalCostMicroCents     consoleCount `json:"totalCostMicroCents"`
	} `json:"items"`
	PageInfo struct {
		PageCount int `json:"pageCount"`
	} `json:"pageInfo"`
}

type modelUsageList struct {
	Error string `json:"error,omitempty"`
	// Rows are sorted by cost, highest first. Truncated says the workspace
	// used more models than the plugin reads.
	Rows      []modelUsageView `json:"rows"`
	Truncated bool             `json:"truncated,omitempty"`
}

type modelUsageView struct {
	Model string `json:"model"`
	// Family is go for the Go subscription and zen for pay-as-you-go
	// OpenCode Zen; another provider is passed on by name.
	Family           string  `json:"family"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	Cost             float64 `json:"cost"`
}

func providerFamily(provider string) string {
	switch provider {
	case providerKey:
		return string(familyGo)
	case zenProviderKey:
		return string(familyZen)
	}
	return provider
}

// consoleRequestLog is GET /api/request-logs, whose shape was read from the
// Console bundle: no key may read it, so it has not been observed.
type consoleRequestLog struct {
	Items []struct {
		StartedAt       float64  `json:"startedAt"`
		DurationMs      float64  `json:"durationMs"`
		Outcome         string   `json:"outcome"`
		StatusCode      *float64 `json:"statusCode"`
		RequestedModel  string   `json:"requestedModel"`
		Model           string   `json:"model"`
		Provider        string   `json:"provider"`
		Product         string   `json:"product"`
		InputTokens     float64  `json:"inputTokens"`
		OutputTokens    float64  `json:"outputTokens"`
		CacheReadTokens float64  `json:"cacheReadTokens"`
		Cost            *float64 `json:"cost"`
	} `json:"items"`
}

type requestLogView struct {
	Error string `json:"error,omitempty"`
	// NeedsLogin says the workspace is read with a key, to which the
	// Console never shows the request log.
	NeedsLogin bool                  `json:"needs_login,omitempty"`
	Entries    []requestLogEntryView `json:"entries"`
}

type requestLogEntryView struct {
	StartedAt       string `json:"started_at"`
	DurationMs      int64  `json:"duration_ms"`
	Outcome         string `json:"outcome"`
	Status          int    `json:"status,omitempty"`
	Model           string `json:"model"`
	Family          string `json:"family,omitempty"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	// Cost is in USD, absent while OpenCode has not priced the request.
	Cost *float64 `json:"cost,omitempty"`
}

// requestFamily names what billed a request: the Go subscription, OpenCode
// Zen's standard pay-as-you-go product, or another provider.
func requestFamily(product, provider string) string {
	switch product {
	case "go", "go-plus":
		return string(familyGo)
	case "standard":
		return string(familyZen)
	}
	return provider
}

type workspaceUsageView struct {
	Workspace  workspaceView    `json:"workspace"`
	Range      string           `json:"range"`
	Summary    usageSummaryView `json:"summary"`
	Chart      usageChartView   `json:"chart"`
	Models     modelUsageList   `json:"models"`
	RequestLog requestLogView   `json:"request_log"`
}

// usageStore caches Console answers per credential and URL, as the Console's
// own page does, so reloading the page or going back to a range does not ask
// OpenCode again.
type usageStore struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]usageEntry
}

type usageEntry struct {
	at   time.Time
	ttl  time.Duration
	resp pluginapi.HTTPResponse
}

func newUsageStore() *usageStore {
	return &usageStore{now: time.Now, entries: make(map[string]usageEntry)}
}

var consoleUsage = newUsageStore()

// get answers a Console GET from the cache while it is fresh. It keeps 200
// and 403 answers only: a refusal lasts as long as the credential's rights,
// and anything else is worth asking again.
func (s *usageStore) get(ctx context.Context, doer httpDoer, cfg settings, reader workspaceReader, path string, query url.Values, ttl time.Duration) (pluginapi.HTTPResponse, error) {
	target := cfg.consoleURL() + path + "?" + query.Encode()
	key := reader.cred.ID + " " + target
	s.mu.Lock()
	entry, cached := s.entries[key]
	now := s.now()
	s.mu.Unlock()
	if cached && now.Sub(entry.at) < entry.ttl {
		return entry.resp, nil
	}
	headers := goRequestHeaders(cfg, reader.cred.Bearer)
	if reader.orgID != "" {
		headers.Set("x-org-id", reader.orgID)
	}
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: target, Headers: headers})
	if errDo != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("reach OpenCode Console: %w", errDo)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusForbidden {
		s.mu.Lock()
		for name, old := range s.entries {
			if now.Sub(old.at) >= old.ttl {
				delete(s.entries, name)
			}
		}
		s.entries[key] = usageEntry{at: now, ttl: ttl, resp: resp}
		s.mu.Unlock()
	}
	return resp, nil
}

func (s *usageStore) getJSON(ctx context.Context, doer httpDoer, cfg settings, reader workspaceReader, path string, query url.Values, ttl time.Duration, out any) error {
	resp, errGet := s.get(ctx, doer, cfg, reader, path, query, ttl)
	if errGet != nil {
		return errGet
	}
	if resp.StatusCode != http.StatusOK {
		return parseConsoleError(resp.StatusCode, resp.Headers, resp.Body)
	}
	if errDecode := json.Unmarshal(resp.Body, out); errDecode != nil {
		return fmt.Errorf("decode %s: %w", path, errDecode)
	}
	return nil
}

// parseConsoleError reads the Console API's {"_tag":"Forbidden"} errors,
// falling back to the gateway's shapes.
func parseConsoleError(status int, headers http.Header, body []byte) *upstreamError {
	var tagged struct {
		Tag     string `json:"_tag"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &tagged) == nil && strings.TrimSpace(tagged.Tag) != "" {
		return &upstreamError{
			Status:  status,
			Type:    strings.TrimSpace(tagged.Tag),
			Message: redactSecrets(strings.TrimSpace(tagged.Message)),
			LogID:   strings.TrimSpace(headers.Get("x-opencode-log-id")),
		}
	}
	return parseUpstreamError(status, headers, body)
}

// describeUsageFailure says in plain words why one part of a workspace's
// usage could not be read.
func describeUsageFailure(err error, reader workspaceReader) string {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		switch {
		case upstream.Status == http.StatusUnauthorized && reader.login:
			return "OpenCode rejected the login's token: " + upstream.Error() + ". If CLIProxyAPI cannot refresh it, " + logInAgain + "."
		case upstream.Status == http.StatusUnauthorized || upstream.Status == http.StatusForbidden:
			return "OpenCode refused " + reader.cred.Name + ": " + upstream.Error()
		case upstream.Status == http.StatusTooManyRequests:
			return "OpenCode is limiting how often usage is read; try again in a minute (" + upstream.Error() + ")."
		case upstream.Status >= http.StatusInternalServerError:
			return "OpenCode is not answering: " + upstream.Error()
		}
		return "OpenCode refused the request: " + upstream.Error()
	}
	var hostErr *hostCallError
	if errors.As(err, &hostErr) {
		return "Could not reach OpenCode: " + hostErr.Message
	}
	return "OpenCode sent an unreadable answer: " + err.Error()
}

// read collects one workspace's usage; the four parts are read at once and
// each reports its own failure.
func (s *usageStore) read(ctx context.Context, doer httpDoer, cfg settings, workspace usageWorkspace, q usageQuery) workspaceUsageView {
	reader := workspace.readers[0]
	view := workspaceUsageView{Workspace: workspace.view(), Range: q.Range}
	var wg sync.WaitGroup
	wg.Go(func() { view.Summary = s.summary(ctx, doer, cfg, reader, q) })
	wg.Go(func() { view.Chart = s.chart(ctx, doer, cfg, reader, q) })
	wg.Go(func() { view.Models = s.models(ctx, doer, cfg, reader, q) })
	wg.Go(func() { view.RequestLog = s.requestLog(ctx, doer, cfg, reader) })
	wg.Wait()
	return view
}

func (s *usageStore) summary(ctx context.Context, doer httpDoer, cfg settings, reader workspaceReader, q usageQuery) usageSummaryView {
	var doc consoleSummary
	if errRead := s.getJSON(ctx, doer, cfg, reader, "/api/usage/summary", q.consoleParams(), usageTTL, &doc); errRead != nil {
		return usageSummaryView{Error: describeUsageFailure(errRead, reader)}
	}
	return newUsageSummaryView(doc)
}

func (s *usageStore) chart(ctx context.Context, doer httpDoer, cfg settings, reader workspaceReader, q usageQuery) usageChartView {
	params := q.consoleParams()
	params.Set("bucket", q.bucket())
	view := usageChartView{Bucket: q.bucket(), Buckets: []usageBucketView{}}
	var points []consoleBucket
	if errRead := s.getJSON(ctx, doer, cfg, reader, "/api/usage/cost-by-day", params, usageTTL, &points); errRead != nil {
		view.Error = describeUsageFailure(errRead, reader)
		return view
	}
	view.Buckets = fillBuckets(points, q, s.now())
	return view
}

func (s *usageStore) models(ctx context.Context, doer httpDoer, cfg settings, reader workspaceReader, q usageQuery) modelUsageList {
	out := modelUsageList{Rows: []modelUsageView{}}
	for page := 1; ; page++ {
		params := q.consoleParams()
		params.Set("page", strconv.Itoa(page))
		params.Set("pageSize", strconv.Itoa(modelPageSize))
		params.Set("costOrder", "desc")
		var doc consoleModelPage
		if errRead := s.getJSON(ctx, doer, cfg, reader, "/api/usage/models", params, usageTTL, &doc); errRead != nil {
			return modelUsageList{Rows: []modelUsageView{}, Error: describeUsageFailure(errRead, reader)}
		}
		for _, item := range doc.Items {
			out.Rows = append(out.Rows, modelUsageView{
				Model:            item.Model,
				Family:           providerFamily(item.Provider),
				Requests:         int64(item.TotalRequests),
				InputTokens:      int64(item.TotalInputTokens),
				CacheReadTokens:  int64(item.TotalCacheReadTokens),
				CacheWriteTokens: int64(item.TotalCacheWrite5mTokens + item.TotalCacheWrite1hTokens),
				OutputTokens:     int64(item.TotalOutputTokens),
				Cost:             usd(item.TotalCostMicroCents),
			})
		}
		if page >= doc.PageInfo.PageCount || len(doc.Items) == 0 {
			return out
		}
		if page == maxModelPages {
			out.Truncated = true
			return out
		}
	}
}

// requestLog reads the latest requests, whatever the range, as the
// Console's overview does.
func (s *usageStore) requestLog(ctx context.Context, doer httpDoer, cfg settings, reader workspaceReader) requestLogView {
	params := url.Values{"category": {"inference"}, "limit": {strconv.Itoa(requestLogLimit)}}
	var doc consoleRequestLog
	errRead := s.getJSON(ctx, doer, cfg, reader, "/api/request-logs", params, requestLogTTL, &doc)
	var upstream *upstreamError
	if errors.As(errRead, &upstream) && upstream.Status == http.StatusForbidden && !reader.login {
		return requestLogView{NeedsLogin: true, Entries: []requestLogEntryView{}}
	}
	if errRead != nil {
		return requestLogView{Error: describeUsageFailure(errRead, reader), Entries: []requestLogEntryView{}}
	}
	out := requestLogView{Entries: make([]requestLogEntryView, 0, len(doc.Items))}
	for _, item := range doc.Items {
		entry := requestLogEntryView{
			StartedAt:       time.UnixMilli(int64(item.StartedAt)).UTC().Format(time.RFC3339Nano),
			DurationMs:      int64(item.DurationMs),
			Outcome:         item.Outcome,
			Family:          requestFamily(item.Product, item.Provider),
			InputTokens:     int64(item.InputTokens),
			OutputTokens:    int64(item.OutputTokens),
			CacheReadTokens: int64(item.CacheReadTokens),
			Cost:            item.Cost,
		}
		if item.StatusCode != nil {
			entry.Status = int(*item.StatusCode)
		}
		// The Console shows the model a client asked for, because OpenCode
		// routes its own products to models it does not name.
		entry.Model = item.RequestedModel
		if entry.Model == "" {
			entry.Model = item.Model
		}
		out.Entries = append(out.Entries, entry)
	}
	return out
}

func handleWorkspaceUsage(h host, req pluginapi.ManagementRequest) (int, any, error) {
	id := strings.TrimSpace(req.Query.Get("id"))
	if id == "" {
		return 0, nil, failf(http.StatusBadRequest, "Missing workspace id.")
	}
	q, errQuery := parseUsageQuery(req.Query, consoleUsage.now())
	if errQuery != nil {
		return 0, nil, errQuery
	}
	found, _, errList := listWorkspaces(context.Background(), h)
	if errList != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errList)
	}
	for _, workspace := range found {
		if workspace.ID != id {
			continue
		}
		if workspace.readers[0].expired {
			return 0, nil, failf(http.StatusUnprocessableEntity, "The login's token expired; %s.", logInAgain)
		}
		return http.StatusOK, consoleUsage.read(context.Background(), h, currentSettings(), workspace, q), nil
	}
	return 0, nil, failf(http.StatusNotFound, "No login or key reaches that workspace now. Reload the list.")
}
