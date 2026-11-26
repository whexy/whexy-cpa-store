package main

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/whexy/whexy-cpa-store/pluginkit/background"
	"github.com/whexy/whexy-cpa-store/pluginkit/config"
)

// settings are the plugin-owned values under plugins.configs.zed.
type settings struct {
	BaseURL     string
	SignInURL   string
	ModelPrefix string
	// DisablePrefix serves every model under its bare ID only.
	DisablePrefix bool
	// DisabledModels are upstream model IDs the plugin keeps out of CPA.
	DisabledModels []string
	// DashboardCookie is a dashboard.zed.dev Cookie header value. Zed has no
	// API for plan usage; this cookie is the only way to read it.
	DashboardCookie string
	// DashboardAccount is the Zed username the cookie belongs to. Usage is
	// never shown for another auth, since the dashboard endpoint has no
	// parameter naming the account.
	DashboardAccount string
	UserAgent        string
}

type rawSettings struct {
	BaseURL          string   `yaml:"base_url"`
	SignInURL        string   `yaml:"sign_in_url"`
	ModelPrefix      string   `yaml:"model_prefix"`
	DisablePrefix    bool     `yaml:"disable_prefix"`
	DisabledModels   []string `yaml:"disabled_models"`
	DashboardCookie  string   `yaml:"dashboard_cookie"`
	DashboardAccount string   `yaml:"dashboard_account"`
	UserAgent        string   `yaml:"user_agent"`
}

var (
	currentConfig atomic.Pointer[settings]
	versionOnce   sync.Once
)

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultSettings()
}

func defaultSettings() settings {
	return settings{
		BaseURL:     defaultBaseURL,
		SignInURL:   defaultSignInURL,
		ModelPrefix: defaultModelPrefix,
	}
}

// configure applies a plugin.register or plugin.reconfigure request.
func configure(request []byte) error {
	var raw rawSettings
	if errDecode := config.Decode(request, &raw); errDecode != nil {
		return errDecode
	}
	cfg, errParse := parseSettings(raw)
	if errParse != nil {
		return errParse
	}
	if currentSettings().BaseURL != cfg.BaseURL {
		// A JWT and a catalog are only valid for the cloud that issued them.
		llmTokens.clear()
		modelsByAuth.clear()
	}
	currentConfig.Store(&cfg)
	versionOnce.Do(func() { background.Go(versionLoop) })
	return nil
}

func parseSettings(raw rawSettings) (settings, error) {
	cfg := defaultSettings()
	base, errBase := config.URL("base_url", raw.BaseURL)
	if errBase != nil {
		return settings{}, errBase
	}
	if base != "" {
		cfg.BaseURL = base
	}
	signIn, errSignIn := config.URL("sign_in_url", raw.SignInURL)
	if errSignIn != nil {
		return settings{}, errSignIn
	}
	if signIn != "" {
		cfg.SignInURL = signIn
	}
	prefix, errPrefix := config.ModelPrefix("model_prefix", raw.ModelPrefix)
	if errPrefix != nil {
		return settings{}, errPrefix
	}
	if prefix != "" {
		cfg.ModelPrefix = prefix
	}
	cfg.DisablePrefix = raw.DisablePrefix
	for _, id := range raw.DisabledModels {
		if id = strings.TrimSpace(id); id != "" && !cfg.modelDisabled(id) {
			cfg.DisabledModels = append(cfg.DisabledModels, id)
		}
	}
	cfg.DashboardCookie = strings.TrimSpace(raw.DashboardCookie)
	cfg.DashboardAccount = strings.TrimSpace(raw.DashboardAccount)
	cfg.UserAgent = strings.TrimSpace(raw.UserAgent)
	return cfg, nil
}

// modelPrefix is the prefix exposed model IDs carry, empty when the page
// turned disable_prefix on.
func (s settings) modelPrefix() string {
	if s.DisablePrefix {
		return ""
	}
	return s.ModelPrefix
}

// exposedID is the model ID CLIProxyAPI lists and clients request.
func (s settings) exposedID(upstreamID string) string {
	if prefix := s.modelPrefix(); prefix != "" {
		return prefix + "/" + upstreamID
	}
	return upstreamID
}

// upstreamID turns a client model ID back into the ID Zed names, accepting
// the bare ID as well as the prefixed one.
func (s settings) upstreamID(model string) string {
	model = strings.TrimSpace(model)
	// Accept the configured prefix even while bare IDs are served, so a
	// client that kept the prefixed ID after the switch still resolves.
	prefix := s.ModelPrefix
	if prefix == "" {
		prefix = defaultModelPrefix
	}
	if trimmed, ok := strings.CutPrefix(model, prefix+"/"); ok {
		return trimmed
	}
	return model
}

// dashboardApplies reports whether the dashboard cookie belongs to this
// auth. The dashboard endpoint has no account parameter, so a cookie must be
// pinned to one username.
func (s settings) dashboardApplies(cred credential) bool {
	return s.DashboardCookie != "" && s.DashboardAccount != "" && strings.EqualFold(s.DashboardAccount, cred.Username)
}

func (s settings) modelDisabled(upstreamID string) bool {
	for _, id := range s.DisabledModels {
		if strings.EqualFold(id, upstreamID) {
			return true
		}
	}
	return false
}

// userAgent is the User-Agent sent to Zed; the edge gates on the Zed name.
func (s settings) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return "Zed/" + zedVersion() + " (linux; x86_64)"
}
