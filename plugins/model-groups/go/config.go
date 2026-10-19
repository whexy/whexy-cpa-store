package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	pluginID    = "model-groups"
	displayName = "Model Groups"

	// providerKey is the provider every group model ID is registered under. It
	// deliberately has no credentials: a request that reaches CPA's own
	// model-to-provider resolution instead of this plugin's router must fail
	// instead of being served by an unrelated credential.
	providerKey = "model-groups"

	defaultPrefix      = "auto-group"
	defaultAffinityTTL = time.Hour
	minAffinityTTL     = time.Minute
	maxAffinityTTL     = 30 * 24 * time.Hour
	maxGroupMembers    = 64
	// providerObservationTTL is how long a provider list CPA reported is
	// trusted for the page's availability display.
	providerObservationTTL = 30 * time.Minute
	minMemberWeight        = 1
	maxMemberWeight        = 1000
)

const (
	policyRoundRobin = "round-robin"
	policyWeightedRR = "weighted-round-robin"
	policyFillFirst  = "fill-first"
)

// slugPattern keeps group IDs usable as model names in client configuration
// and clear of the parentheses and spaces CPA parses in model names.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// settings is the plugin's own node of config.yaml plus everything derived
// from it. It is replaced wholesale on plugin.register/plugin.reconfigure.
type settings struct {
	Prefix          string
	AffinityEnabled bool
	AffinityTTL     time.Duration
	RebindOnFailure bool
	Groups          []groupConfig
}

// groupConfig is one group: the model ID clients ask for, the models it
// routes to, and the policy that picks one of them.
type groupConfig struct {
	Name    string
	Slug    string
	ID      string
	Policy  string
	Enabled bool
	Members []memberConfig
	// Capabilities is what every member supports, worked out by the editor
	// when the group was saved.
	Capabilities capabilities
}

// memberConfig is one model inside a group. Providers is the set of provider
// keys CPA serves that model under, resolved when the group is saved; more
// than one entry means the plugin picks one of them and keeps a session on
// it.
type memberConfig struct {
	Model     string
	Providers []string
	Weight    int
}

type rawSettings struct {
	Prefix   string      `yaml:"prefix"`
	Affinity rawAffinity `yaml:"affinity"`
	Groups   []rawGroup  `yaml:"groups"`
}

type rawAffinity struct {
	Enabled         *bool  `yaml:"enabled"`
	TTL             string `yaml:"ttl"`
	RebindOnFailure *bool  `yaml:"rebind_on_failure"`
}

type rawGroup struct {
	Name         string           `yaml:"name"`
	Slug         string           `yaml:"slug"`
	Policy       string           `yaml:"policy"`
	Enabled      *bool            `yaml:"enabled"`
	Members      []rawMember      `yaml:"members"`
	Capabilities *rawCapabilities `yaml:"capabilities"`
}

type rawMember struct {
	Model     string   `yaml:"model"`
	Provider  string   `yaml:"provider"`
	Providers []string `yaml:"providers"`
	Weight    *int     `yaml:"weight"`
}

func defaultSettings() settings {
	return settings{Prefix: defaultPrefix, AffinityEnabled: true, AffinityTTL: defaultAffinityTTL}
}

func parseSettings(configYAML []byte) (settings, error) {
	cfg := defaultSettings()
	if len(configYAML) == 0 {
		return cfg, nil
	}
	var raw rawSettings
	if errUnmarshal := yaml.Unmarshal(configYAML, &raw); errUnmarshal != nil {
		return settings{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
	}

	prefix := strings.Trim(strings.TrimSpace(raw.Prefix), "/")
	if raw.Prefix != "" && prefix == "" {
		return settings{}, fmt.Errorf("prefix must not be empty")
	}
	if prefix != "" {
		if strings.ContainsAny(prefix, " \t\r\n()*") {
			return settings{}, fmt.Errorf("prefix must not contain spaces, parentheses or *, got %q", raw.Prefix)
		}
		cfg.Prefix = prefix
	}
	if errPrefix := validatePrefix(cfg.Prefix); errPrefix != nil {
		return settings{}, errPrefix
	}

	if raw.Affinity.Enabled != nil {
		cfg.AffinityEnabled = *raw.Affinity.Enabled
	}
	if ttl := strings.TrimSpace(raw.Affinity.TTL); ttl != "" {
		parsed, errParse := time.ParseDuration(ttl)
		if errParse != nil {
			return settings{}, fmt.Errorf("affinity.ttl must be a duration such as 1h, got %q", raw.Affinity.TTL)
		}
		if parsed < minAffinityTTL || parsed > maxAffinityTTL {
			return settings{}, fmt.Errorf("affinity.ttl must be between %s and %s", minAffinityTTL, maxAffinityTTL)
		}
		cfg.AffinityTTL = parsed
	}
	if raw.Affinity.RebindOnFailure != nil {
		cfg.RebindOnFailure = *raw.Affinity.RebindOnFailure
	}

	seenSlug := make(map[string]bool, len(raw.Groups))
	cfg.Groups = make([]groupConfig, 0, len(raw.Groups))
	for index, rawGroupEntry := range raw.Groups {
		group, errGroup := parseGroup(cfg.Prefix, rawGroupEntry, index)
		if errGroup != nil {
			return settings{}, errGroup
		}
		if seenSlug[group.Slug] {
			return settings{}, fmt.Errorf("groups[%d]: duplicate slug %q", index, group.Slug)
		}
		seenSlug[group.Slug] = true
		cfg.Groups = append(cfg.Groups, group)
	}
	return cfg, nil
}

func parseGroup(prefix string, raw rawGroup, index int) (groupConfig, error) {
	slug := strings.ToLower(strings.TrimSpace(raw.Slug))
	if slug == "" {
		slug = slugify(raw.Name)
	}
	if slug == "" {
		return groupConfig{}, fmt.Errorf("groups[%d]: name or slug is required", index)
	}
	if !slugPattern.MatchString(slug) {
		return groupConfig{}, fmt.Errorf("groups[%d]: slug %q must be lowercase letters, digits and single dashes", index, slug)
	}

	group := groupConfig{
		Name:    strings.TrimSpace(raw.Name),
		Slug:    slug,
		ID:      prefix + "/" + slug,
		Policy:  policyWeightedRR,
		Enabled: true,
	}
	if group.Name == "" {
		group.Name = slug
	}
	if raw.Enabled != nil {
		group.Enabled = *raw.Enabled
	}
	if policy := normalizePolicy(raw.Policy); policy != "" {
		group.Policy = policy
	} else if strings.TrimSpace(raw.Policy) != "" {
		return groupConfig{}, fmt.Errorf("groups[%d]: unknown policy %q", index, raw.Policy)
	}
	if len(raw.Members) == 0 {
		return groupConfig{}, fmt.Errorf("groups[%d] (%s): at least one member is required", index, slug)
	}
	if len(raw.Members) > maxGroupMembers {
		return groupConfig{}, fmt.Errorf("groups[%d] (%s): at most %d members", index, slug, maxGroupMembers)
	}

	caps, errCaps := parseCapabilities(raw.Capabilities)
	if errCaps != nil {
		return groupConfig{}, fmt.Errorf("groups[%d] (%s): %w", index, slug, errCaps)
	}
	group.Capabilities = caps

	seenMember := make(map[string]bool, len(raw.Members))
	for memberIndex, rawMemberEntry := range raw.Members {
		member, errMember := parseMember(group.ID, rawMemberEntry)
		if errMember != nil {
			return groupConfig{}, fmt.Errorf("groups[%d] (%s) members[%d]: %w", index, slug, memberIndex, errMember)
		}
		key := member.Model + "\x00" + strings.Join(member.Providers, ",")
		if seenMember[key] {
			return groupConfig{}, fmt.Errorf("groups[%d] (%s) members[%d]: %s is already a member", index, slug, memberIndex, member.Model)
		}
		seenMember[key] = true
		group.Members = append(group.Members, member)
	}
	return group, nil
}

func parseMember(groupID string, raw rawMember) (memberConfig, error) {
	model := strings.TrimSpace(raw.Model)
	if model == "" {
		return memberConfig{}, fmt.Errorf("model is required")
	}
	if model == groupID || strings.HasPrefix(model, groupID+"(") {
		return memberConfig{}, fmt.Errorf("a group cannot contain itself")
	}
	member := memberConfig{Model: model, Weight: minMemberWeight}
	if raw.Weight != nil {
		member.Weight = *raw.Weight
	}
	if member.Weight < minMemberWeight || member.Weight > maxMemberWeight {
		return memberConfig{}, fmt.Errorf("weight must be between %d and %d", minMemberWeight, maxMemberWeight)
	}
	providers := make([]string, 0, len(raw.Providers)+1)
	if single := providerKeyOf(raw.Provider); single != "" {
		providers = append(providers, single)
	}
	for _, provider := range raw.Providers {
		if normalized := providerKeyOf(provider); normalized != "" {
			providers = append(providers, normalized)
		}
	}
	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if provider == providerKey {
			// Routing a member back into this plugin would recurse.
			return memberConfig{}, fmt.Errorf("provider %q is this plugin's own provider key", provider)
		}
		if seen[provider] {
			continue
		}
		seen[provider] = true
		member.Providers = append(member.Providers, provider)
	}
	return member, nil
}

// normalizePolicy accepts the names the plugin documents plus the abbreviations
// a hand-written config tends to use, and reports "" for an unknown value.
func normalizePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", policyWeightedRR, "weighted", "wrr":
		return policyWeightedRR
	case policyRoundRobin, "rr", "roundrobin", "round-robin-routing":
		return policyRoundRobin
	case policyFillFirst, "ff", "fillfirst":
		return policyFillFirst
	default:
		return ""
	}
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("prefix must not be empty")
	}
	if strings.ContainsAny(prefix, " \t\r\n()*/\\") {
		return fmt.Errorf("prefix must not contain spaces, parentheses or slashes, got %q", prefix)
	}
	return nil
}

// slugify turns a group name into a slug; it returns "" when nothing usable
// is left.
func slugify(name string) string {
	var builder strings.Builder
	lastDash := false
	for _, symbol := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= '0' && symbol <= '9':
			builder.WriteRune(symbol)
			lastDash = false
		case !lastDash && builder.Len() > 0:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

// groupByID returns the configured group for a model ID without its thinking
// suffix.
func (s settings) groupByID(id string) (groupConfig, bool) {
	for _, group := range s.Groups {
		if group.ID == id {
			return group, true
		}
	}
	return groupConfig{}, false
}

// registeredModelIDs lists the model IDs the host should publish for the
// enabled groups.
func (s settings) registeredModelIDs() []string {
	ids := make([]string, 0, len(s.Groups))
	for _, group := range s.Groups {
		if group.Enabled {
			ids = append(ids, group.ID)
		}
	}
	return ids
}
