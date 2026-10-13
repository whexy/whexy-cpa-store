package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// CPA reads these top-level auth file fields for every auth the file
// yields, virtual ones included, and the plugin's rewrites keep them.
const (
	priorityField = "priority"
	weightField   = "weight"
	// maxWeight is CPA's upper bound for a credential weight.
	maxWeight = 1_000_000
)

// routingFileView is one auth file's routing fields; nil means unset.
type routingFileView struct {
	File     string `json:"file"`
	Priority *int64 `json:"priority"`
	Weight   *int64 `json:"weight"`
}

// routingView reports the files' routing fields. Priority and Weight are
// the value every file shares, or nil when unset everywhere or Mixed.
type routingView struct {
	Files    []routingFileView `json:"files"`
	Priority *int64            `json:"priority"`
	Weight   *int64            `json:"weight"`
	Mixed    bool              `json:"mixed"`
}

// extraInt reads an integer field the way CPA does: a JSON number or a
// numeric string.
func extraInt(extra map[string]json.RawMessage, name string) *int64 {
	raw := bytes.TrimSpace(extra[name])
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number == math.Trunc(number) {
		value := int64(number)
		return &value
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if value, errParse := strconv.ParseInt(strings.TrimSpace(text), 10, 64); errParse == nil {
			return &value
		}
	}
	return nil
}

func readRouting(files storedAuthFiles) routingView {
	view := routingView{Files: []routingFileView{}}
	add := func(name string, extra map[string]json.RawMessage) {
		view.Files = append(view.Files, routingFileView{File: name, Priority: extraInt(extra, priorityField), Weight: extraInt(extra, weightField)})
	}
	for _, stored := range files.Logins {
		add(stored.Name, stored.Login.extra)
	}
	for _, stored := range files.Keys {
		add(stored.Name, stored.File.extra)
	}
	same := func(pick func(routingFileView) *int64) (*int64, bool) {
		var first *int64
		for index, file := range view.Files {
			value := pick(file)
			if index == 0 {
				first = value
				continue
			}
			if (first == nil) != (value == nil) || (first != nil && *first != *value) {
				return nil, false
			}
		}
		return first, true
	}
	priority, samePriority := same(func(file routingFileView) *int64 { return file.Priority })
	weight, sameWeight := same(func(file routingFileView) *int64 { return file.Weight })
	view.Priority, view.Weight = priority, weight
	view.Mixed = !samePriority || !sameWeight
	return view
}

func handleGetRouting(host, pluginapi.ManagementRequest) (int, any, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	return http.StatusOK, readRouting(files), nil
}

// handleSetRouting writes priority and weight into every opencode-go auth
// file. A field sent as null is removed, which restores CPA's default.
func handleSetRouting(_ host, req pluginapi.ManagementRequest) (int, any, error) {
	var body map[string]json.RawMessage
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	fields := fileFields{}
	for _, name := range []string{priorityField, weightField} {
		raw, present := body[name]
		if !present {
			continue
		}
		value, errValue := routingValue(name, raw)
		if errValue != nil {
			return 0, nil, errValue
		}
		fields[name] = value
	}
	if len(fields) == 0 {
		return 0, nil, failf(http.StatusBadRequest, "Nothing to change: send priority or weight.")
	}
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	if len(files.Keys) == 0 && len(files.Logins) == 0 {
		return 0, nil, failf(http.StatusConflict, "No OpenCode Go login or key is stored yet.")
	}
	var failures []string
	for _, stored := range files.Logins {
		if errSave := setFileFields(stored.Name, true, fields); errSave != nil {
			failures = append(failures, stored.Name+": "+errSave.Error())
		}
	}
	for _, stored := range files.Keys {
		if errSave := setFileFields(stored.Name, false, fields); errSave != nil {
			failures = append(failures, stored.Name+": "+errSave.Error())
		}
	}
	if len(failures) > 0 {
		return 0, nil, failf(http.StatusInternalServerError, "Could not save %s", strings.Join(failures, "; "))
	}
	return handleGetRouting(host{}, req)
}

// routingValue validates one routing field; nil removes it.
func routingValue(name string, raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if string(raw) == "null" {
		return nil, nil
	}
	var number float64
	if json.Unmarshal(raw, &number) != nil || number != math.Trunc(number) {
		return nil, failf(http.StatusBadRequest, "%s must be a whole number.", name)
	}
	switch {
	case name == weightField && (number < 0 || number > maxWeight):
		return nil, failf(http.StatusBadRequest, "weight must be between 0 and %d.", maxWeight)
	case name == priorityField && (number < math.MinInt32 || number > math.MaxInt32):
		return nil, failf(http.StatusBadRequest, "priority must fit in 32 bits.")
	}
	return json.RawMessage(strconv.FormatInt(int64(number), 10)), nil
}
