package plugin

// identity.go — how this plugin presents itself to the OpenCode Go upstream.
//
// The official client guidance asks third-party clients to (a) identify with
// their OWN user agent rather than a generic SDK or HTTP-library name, and
// (b) send one stable x-opencode-session per conversation so the upstream can
// optimise routing and prompt caching. OpenCode Go additionally recognises the
// native session headers of Codex and Claude Code, so those are consulted for
// the session id and forwarded verbatim. Impersonating the official opencode
// CLI is deliberately NOT done.

import (
	"net/http"
	"strings"

	"github.com/ThunderHammerKing/opencode-go-cpa/internal/config"
)

// identityUserAgent returns the configured upstream user agent.
func identityUserAgent(cfg config.Config) string {
	if ua := strings.TrimSpace(cfg.Identity.UserAgent); ua != "" {
		return ua
	}
	return config.DefaultUserAgent
}

// identitySessionHeaders returns the ordered downstream-native conversation
// headers consulted when deriving the upstream session id.
func identitySessionHeaders(cfg config.Config) []string {
	if len(cfg.Identity.SessionHeaders) > 0 {
		return cfg.Identity.SessionHeaders
	}
	return config.DefaultSessionHeaders
}

// applyIdentityHeaders stamps the client identity onto an upstream request:
// the plugin's own user agent plus the stable conversation session id. The
// bearer/x-api-key auth headers are set by the route adapters before this.
func applyIdentityHeaders(h http.Header, cfg config.Config, sessionID string) {
	h.Set("User-Agent", identityUserAgent(cfg))
	if sessionID != "" && cfg.Identity.ForwardSession {
		h.Set("x-opencode-session", sessionID)
	}
}
