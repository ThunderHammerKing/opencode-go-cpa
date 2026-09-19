// Package resources embeds the plugin's browser pages. They are served from
// the unauthenticated /v0/resource/plugins/opencode-go-cpa/ prefix; anything
// state-changing goes through the state-token-bound POST on the login page.
package resources

import _ "embed"

//go:embed quota_page.html
var QuotaPage []byte

//go:embed login_page.html
var LoginPage []byte
