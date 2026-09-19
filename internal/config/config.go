// Package config loads and validates the opencode-go-cpa plugin
// configuration. Credentials primarily live in CPA auth files (added through
// the plugin login flow); config api-keys are an optional seed list, so an
// empty key list is valid.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults. DefaultUserAgent must be kept in sync with pluginVersion in
// internal/plugin until the plugin injects its real version at runtime.
const (
	DefaultBaseURL          = "https://opencode.ai/zen/go/v1"
	DefaultProviderID       = "opencode-go"
	DefaultModelPrefix      = "opencode-go"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
	DefaultMaxConcurrent    = 4               // per credential
	DefaultUserAgent        = "opencode-go-cpa/0.1.2"
)

// DefaultSessionHeaders is the ordered list of downstream-native conversation
// headers consulted when deriving the upstream x-opencode-session. CPA's own
// affinity header comes first, then Codex and Claude Code native headers —
// OpenCode Go recognises those natively, so preserving them keeps session
// affinity and prompt caching intact.
var DefaultSessionHeaders = []string{
	"X-Session-Affinity", "Session-Id", "X-Claude-Code-Session-Id", "X-OpenCode-Session", "X-Session-Id",
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

// Identity controls how the plugin presents itself upstream. The OpenCode Go
// docs ask clients to identify with their OWN user agent and to send a stable
// x-opencode-session per conversation — not to impersonate the official CLI.
type Identity struct {
	UserAgent      string
	ForwardSession bool
	SessionHeaders []string
}

// Pool bounds per-credential concurrency and upstream rate-limit discipline.
type Pool struct {
	MaxConcurrentPerKey int
	CooldownOn429       bool
	RespectRetryAfter   bool
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"` // "chat-completions" | "messages" | "responses"
	Endpoint string `yaml:"endpoint"` // must start with "/"
}

type APIKey struct{ Value string }

type Config struct {
	BaseURL          string
	CatalogURL       string
	ProviderID       string
	ModelPrefix      string // "" disables prefixing
	APIKeys          []APIKey
	Catalog          Catalog
	Identity         Identity
	Pool             Pool
	RouteOverrides   map[string]RouteOverride
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// rawConfig mirrors the flat YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys such as enabled /
// priority / store).
type rawConfig struct {
	BaseURL              *string                  `yaml:"base-url"`
	CatalogURL           *string                  `yaml:"catalog-url"`
	ProviderID           *string                  `yaml:"provider-id"`
	ModelPrefix          *string                  `yaml:"model-prefix"`
	APIKeys              flexKeys                 `yaml:"api-keys"`
	RefreshInterval      *string                  `yaml:"refresh-interval"`
	RequestTimeout       *string                  `yaml:"request-timeout"`
	MaxResponseBytes     *int64                   `yaml:"max-response-bytes"`
	MaxConcurrentPerKey  *int                     `yaml:"max-concurrent-per-key"`
	UserAgent            *string                  `yaml:"user-agent"`
	ForwardSession       *bool                    `yaml:"forward-session"`
	CooldownOn429        *bool                    `yaml:"cooldown-on-429"`
	RespectRetryAfter    *bool                    `yaml:"respect-retry-after"`
	StaleWhileUnavailable *bool                   `yaml:"stale-while-unavailable"`
	AllowHTTP            bool                     `yaml:"allow-http"`
	RouteOverrides       map[string]RouteOverride `yaml:"route-overrides"`
}

// flexKeys accepts both "- sk-..." scalars and the legacy "- value: sk-..."
// mapping form so hand-edited config and management-panel output both load.
type flexKeys []APIKey

func (f *flexKeys) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("api-keys: must be a list")
	}
	out := make(flexKeys, 0, len(node.Content))
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			var s string
			if err := item.Decode(&s); err != nil {
				return fmt.Errorf("api-keys: invalid entry")
			}
			out = append(out, APIKey{Value: s})
		case yaml.MappingNode:
			var raw struct {
				Value string `yaml:"value"`
			}
			if err := item.Decode(&raw); err != nil {
				return fmt.Errorf("api-keys: invalid entry")
			}
			out = append(out, APIKey{Value: raw.Value})
		default:
			return fmt.Errorf("api-keys: invalid entry")
		}
	}
	*f = out
	return nil
}

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates. Decode errors never echo decoded node
// values — a malformed entry (e.g. a bare-scalar API key) must not leak into
// the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value)}
	}
	refreshInterval, err := parseDuration("refresh-interval", raw.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	identity := Identity{
		UserAgent:      DefaultUserAgent,
		ForwardSession: true,
		SessionHeaders: append([]string(nil), DefaultSessionHeaders...),
	}
	if raw.UserAgent != nil {
		if v := strings.TrimSpace(*raw.UserAgent); v != "" {
			identity.UserAgent = v
		}
	}
	if raw.ForwardSession != nil {
		identity.ForwardSession = *raw.ForwardSession
	}
	c := Config{
		BaseURL:     orDefault(raw.BaseURL, DefaultBaseURL),
		ProviderID:  orDefault(raw.ProviderID, DefaultProviderID),
		ModelPrefix: orDefault(raw.ModelPrefix, DefaultModelPrefix),
		APIKeys:     keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.StaleWhileUnavailable, true),
		},
		Identity: identity,
		Pool: Pool{
			MaxConcurrentPerKey: orDefault(raw.MaxConcurrentPerKey, DefaultMaxConcurrent),
			CooldownOn429:       orDefault(raw.CooldownOn429, true),
			RespectRetryAfter:   orDefault(raw.RespectRetryAfter, true),
		},
		RouteOverrides:   raw.RouteOverrides,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	if c.Pool.MaxConcurrentPerKey < 0 {
		c.Pool.MaxConcurrentPerKey = 0 // 0 = unlimited
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// PublicID returns the client-facing model ID.
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix != "" {
		return c.ModelPrefix + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) Validate() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	// Credentials primarily live in CPA auth files, so an empty api-keys
	// list is valid; only malformed entries are rejected.
	seen := make(map[string]bool, len(c.APIKeys))
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.ProviderID == "" {
		return fmt.Errorf("provider-id: must not be empty")
	}
	if !validPrefix(c.ProviderID) {
		return fmt.Errorf("provider-id: invalid provider-ID characters %q", c.ProviderID)
	}
	if c.ModelPrefix != "" && !validPrefix(c.ModelPrefix) {
		return fmt.Errorf("model-prefix: invalid provider-ID characters %q", c.ModelPrefix)
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	if c.Identity.UserAgent == "" {
		return fmt.Errorf("user-agent: must not be empty")
	}
	for name, o := range c.RouteOverrides {
		switch o.Protocol {
		case "chat-completions", "messages", "responses":
		default:
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist: https always; http only behind allow-http; anything
	// else (ftp://, file://, custom schemes) is rejected at load instead of
	// failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
