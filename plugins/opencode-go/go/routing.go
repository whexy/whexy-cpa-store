package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Each login and key file keeps a priority and a weight per family, which
// auth.parse gives that family's auths. CPA applies a file's top-level
// priority and weight to every auth the file yields, over the plugin's
// values, so those route Go and Zen alike; saving from the page moves them
// into the family fields. The OpenCode Free file yields one auth, so its
// routing is the top-level fields themselves.
const (
	priorityField = "priority"
	weightField   = "weight"
	// maxWeight is CPA's upper bound for a credential weight.
	maxWeight = 1_000_000
)

type routingFieldNames struct {
	priority, weight string
}

var familyRoutingFields = map[modelFamily]routingFieldNames{
	familyGo:   {priority: "go_priority", weight: "go_weight"},
	familyZen:  {priority: "zen_priority", weight: "zen_weight"},
	familyFree: {priority: priorityField, weight: weightField},
}

// credentialFamilies are the families a login or key file routes.
var credentialFamilies = []modelFamily{familyGo, familyZen}

// routingValues is one family's routing; nil means CPA's default.
type routingValues struct {
	Priority *int64 `json:"priority"`
	Weight   *int64 `json:"weight"`
}

// routingFileView is one auth file's routing as CPA applies it: Go and Zen
// for a login or key file, Free for the OpenCode Free file.
type routingFileView struct {
	File string         `json:"file"`
	Go   *routingValues `json:"go,omitempty"`
	Zen  *routingValues `json:"zen,omitempty"`
	Free *routingValues `json:"free,omitempty"`
	// Shared says a login or key file holds a top-level priority or weight,
	// which CPA applies to both families.
	Shared bool `json:"shared"`
}

// familyRoutingView is the value every file shares for one family, or nil
// when unset everywhere or Mixed.
type familyRoutingView struct {
	routingValues
	Mixed bool `json:"mixed"`
}

// routingView is the routing of every file; Free is nil until the OpenCode
// Free file exists.
type routingView struct {
	Files []routingFileView  `json:"files"`
	Go    familyRoutingView  `json:"go"`
	Zen   familyRoutingView  `json:"zen"`
	Free  *familyRoutingView `json:"free,omitempty"`
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

// effectiveRouting is the routing CPA applies to a file's auths of one
// family: a top-level field when present, the family field otherwise.
func effectiveRouting(extra map[string]json.RawMessage, family modelFamily) routingValues {
	names := familyRoutingFields[family]
	pick := func(shared, own string) *int64 {
		if value := extraInt(extra, shared); value != nil {
			return value
		}
		return extraInt(extra, own)
	}
	return routingValues{Priority: pick(priorityField, names.priority), Weight: pick(weightField, names.weight)}
}

// routingAttributes are a family's routing fields as auth attributes. A
// value CPA would reject is left out, because one invalid weight makes CPA
// refuse every auth of the file.
func routingAttributes(extra map[string]json.RawMessage, family modelFamily) map[string]string {
	names := familyRoutingFields[family]
	out := map[string]string{}
	if priority := extraInt(extra, names.priority); priority != nil && *priority >= math.MinInt32 && *priority <= math.MaxInt32 {
		out[priorityField] = strconv.FormatInt(*priority, 10)
	}
	if weight := extraInt(extra, names.weight); weight != nil && *weight >= 0 && *weight <= maxWeight {
		out[weightField] = strconv.FormatInt(*weight, 10)
	}
	return out
}

func readRouting(files storedAuthFiles) routingView {
	view := routingView{Files: []routingFileView{}}
	var credentialFiles []routingFileView
	add := func(name string, extra map[string]json.RawMessage) {
		goValues, zenValues := effectiveRouting(extra, familyGo), effectiveRouting(extra, familyZen)
		file := routingFileView{
			File:   name,
			Go:     &goValues,
			Zen:    &zenValues,
			Shared: extraInt(extra, priorityField) != nil || extraInt(extra, weightField) != nil,
		}
		view.Files = append(view.Files, file)
		credentialFiles = append(credentialFiles, file)
	}
	for _, stored := range files.Logins {
		add(stored.Name, stored.Login.extra)
	}
	for _, stored := range files.Keys {
		add(stored.Name, stored.File.extra)
	}
	view.Go = sharedRouting(credentialFiles, func(file routingFileView) routingValues { return *file.Go })
	view.Zen = sharedRouting(credentialFiles, func(file routingFileView) routingValues { return *file.Zen })
	if files.Free != nil {
		values := effectiveRouting(files.Free.File.extra, familyFree)
		view.Files = append(view.Files, routingFileView{File: files.Free.Name, Free: &values})
		view.Free = &familyRoutingView{routingValues: values}
	}
	return view
}

func sharedRouting(files []routingFileView, family func(routingFileView) routingValues) familyRoutingView {
	same := func(pick func(routingValues) *int64) (*int64, bool) {
		var first *int64
		for index, file := range files {
			value := pick(family(file))
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
	priority, samePriority := same(func(values routingValues) *int64 { return values.Priority })
	weight, sameWeight := same(func(values routingValues) *int64 { return values.Weight })
	return familyRoutingView{routingValues: routingValues{Priority: priority, Weight: weight}, Mixed: !samePriority || !sameWeight}
}

func handleGetRouting(host, pluginapi.ManagementRequest) (int, any, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	return http.StatusOK, readRouting(files), nil
}

// routingChange is one family field to write; a nil value removes it,
// which restores CPA's default.
type routingChange struct {
	family modelFamily
	field  string
	value  json.RawMessage
}

// handleSetRouting writes the families' priority and weight into the
// OpenCode Go auth files, as {"go": {"priority": 5, "weight": 2}, "zen":
// {...}, "free": {...}}: Go and Zen into every login and key file, Free
// into the OpenCode Free file. A field sent as null is removed; a field
// left out keeps the value CPA applies now, which moves a top-level field
// of a login or key file into the family fields.
func handleSetRouting(_ host, req pluginapi.ManagementRequest) (int, any, error) {
	var body map[string]json.RawMessage
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	var changes []routingChange
	for _, family := range families {
		raw, present := body[string(family)]
		if !present {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return 0, nil, failf(http.StatusBadRequest, "%s must be an object of priority and weight.", family)
		}
		names := familyRoutingFields[family]
		for _, name := range []string{priorityField, weightField} {
			value, present := fields[name]
			if !present {
				continue
			}
			checked, errValue := routingValue(name, value)
			if errValue != nil {
				return 0, nil, errValue
			}
			field := names.priority
			if name == weightField {
				field = names.weight
			}
			changes = append(changes, routingChange{family: family, field: field, value: checked})
		}
	}
	if len(changes) == 0 {
		return 0, nil, failf(http.StatusBadRequest, "Nothing to change: send go, zen or free with priority or weight.")
	}
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	if len(files.Keys) == 0 && len(files.Logins) == 0 && files.Free == nil {
		return 0, nil, failf(http.StatusConflict, "No OpenCode Go login or key is stored yet, and OpenCode Free is off.")
	}
	credentialEdit := func(extra map[string]json.RawMessage) map[string]json.RawMessage {
		return applyRouting(extra, credentialFamilies, changes)
	}
	var failures []string
	save := func(name string, kind fileKind, edit func(map[string]json.RawMessage) map[string]json.RawMessage) {
		if errSave := editFileFields(name, kind, edit); errSave != nil {
			failures = append(failures, name+": "+errSave.Error())
		}
	}
	if changesFor(changes, credentialFamilies) {
		for _, stored := range files.Logins {
			save(stored.Name, loginFileKind, credentialEdit)
		}
		for _, stored := range files.Keys {
			save(stored.Name, keyFileKind, credentialEdit)
		}
	}
	if files.Free != nil && changesFor(changes, []modelFamily{familyFree}) {
		save(files.Free.Name, freeFileKind, func(extra map[string]json.RawMessage) map[string]json.RawMessage {
			return applyRouting(extra, []modelFamily{familyFree}, changes)
		})
	}
	if len(failures) > 0 {
		return 0, nil, failf(http.StatusInternalServerError, "Could not save %s", strings.Join(failures, "; "))
	}
	return handleGetRouting(host{}, req)
}

func changesFor(changes []routingChange, families []modelFamily) bool {
	for _, change := range changes {
		if slices.Contains(families, change.family) {
			return true
		}
	}
	return false
}

// applyRouting writes the fields of the file's families, the changed ones
// from changes and the others as CPA applies them now. A login or key file
// loses its top-level fields, which would route both families alike.
func applyRouting(extra map[string]json.RawMessage, families []modelFamily, changes []routingChange) map[string]json.RawMessage {
	fields := fileFields{}
	if !slices.Contains(families, familyFree) {
		fields[priorityField], fields[weightField] = nil, nil
	}
	encode := func(value *int64) json.RawMessage {
		if value == nil {
			return nil
		}
		return json.RawMessage(strconv.FormatInt(*value, 10))
	}
	for _, family := range families {
		current := effectiveRouting(extra, family)
		names := familyRoutingFields[family]
		fields[names.priority] = encode(current.Priority)
		fields[names.weight] = encode(current.Weight)
	}
	for _, change := range changes {
		if slices.Contains(families, change.family) {
			fields[change.field] = change.value
		}
	}
	return fields.apply(extra)
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
