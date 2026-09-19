package plugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// OpenCode Go is API-key only — there is no OAuth dance to perform. The
// login flow therefore hands the Management Center a plugin-hosted page
// where the user pastes the key they copied from opencode.ai/auth, and
// reports success once that page has delivered a credential (reference
// issue #6: the old unconditional stub left the panel with a dead button).

const loginSessionTTL = 15 * time.Minute

type loginSession struct {
	expiresAt time.Time
	apiKey    string
	label     string
	submitted bool
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

type authProvider struct{}

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

func (authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	var raw struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Label    string `json:"label"`
		APIKey   string `json:"api_key"`
	}
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if strings.TrimSpace(raw.APIKey) == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has no api key")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s api_key_present=%t api_key_length=%d", req.Provider, req.FileName, raw.ID, strings.TrimSpace(raw.APIKey) != "", len(raw.APIKey))
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: ProviderID, ID: raw.ID, FileName: req.FileName, Label: raw.Label, StorageJSON: req.RawJSON,
		Attributes: map[string]string{"api_key": raw.APIKey},
	}}, nil
}

// StartLogin mints a short-lived state and points the Management Center at
// the plugin-hosted key page. The host opens resp.URL in the browser and
// polls PollLogin until the flow resolves.
func (authProvider) StartLogin(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	state, err := newLoginState()
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("generate login state: %w", err)
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderID,
		State:     state,
		URL:       loginPageURL(req.BaseURL, state),
		ExpiresAt: time.Now().Add(loginSessionTTL),
	}, nil
}

// PollLogin resolves the flow: pending until the key page POSTs a credential,
// success once it has, error when the state is unknown or expired.
func (authProvider) PollLogin(_ context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	loginMu.Lock()
	defer loginMu.Unlock()
	pruneLoginLocked(time.Now())
	sess, ok := loginSessions[req.State]
	if !ok {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login session expired; start again"}, nil
	}
	if !sess.submitted {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	return pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   buildAuthData(sess.apiKey, sess.label),
	}, nil
}

// SubmitLogin records a credential pasted on the key page against a live
// state. Unknown or expired states are refused so the unauthenticated
// resource route cannot be used to plant arbitrary records.
func SubmitLogin(state, apiKey, label string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return fmt.Errorf("api key is required")
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	pruneLoginLocked(time.Now())
	sess, ok := loginSessions[state]
	if !ok || time.Now().After(sess.expiresAt) {
		return fmt.Errorf("login session expired; start again")
	}
	sess.apiKey, sess.label, sess.submitted = apiKey, strings.TrimSpace(label), true
	return nil
}

func (authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d attr_names=%v metadata_names=%v", req.AuthProvider, req.AuthID, len(req.StorageJSON), mapKeys(req.Attributes), mapKeys(req.Metadata))
	// API keys never expire; hand the record back untouched.
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
}

// newLoginState returns a hex state token. Hex keeps the host's OAuth state
// validation (alphanumerics only) satisfied without needing to import the
// host's validator.
func newLoginState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	state := hex.EncodeToString(buf)
	loginMu.Lock()
	defer loginMu.Unlock()
	pruneLoginLocked(time.Now())
	loginSessions[state] = &loginSession{expiresAt: time.Now().Add(loginSessionTTL)}
	return state, nil
}

func pruneLoginLocked(now time.Time) {
	for state, sess := range loginSessions {
		if now.After(sess.expiresAt) {
			delete(loginSessions, state)
		}
	}
}

// loginPageURL derives the plugin key-page URL from the host's management
// callback URL (BaseURL carries scheme://host plus the callback path).
func loginPageURL(baseURL, state string) string {
	scheme, host := "http", "127.0.0.1:8317"
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && u.Host != "" {
		if u.Scheme != "" {
			scheme = u.Scheme
		}
		host = u.Host
	}
	return scheme + "://" + host + "/v0/resource/plugins/" + pluginName + "/login?state=" + url.QueryEscape(state)
}

// buildAuthData renders the pasted key as the canonical auth record shape —
// the same JSON materializeAuthRecords writes for config-seeded keys.
func buildAuthData(apiKey, label string) pluginapi.AuthData {
	digest := sha256.Sum256([]byte(apiKey))
	hash := hex.EncodeToString(digest[:])
	id := "opencode-go-key-" + hash
	if label == "" {
		label = "OpenCode Go credential " + hash[:12]
	}
	record, err := json.Marshal(struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
	}{Type: ProviderID, ID: id, Label: label, APIKey: apiKey})
	if err != nil {
		record = nil
	}
	return pluginapi.AuthData{
		Provider:   ProviderID,
		ID:         id,
		Label:      label,
		StorageJSON: record,
		Attributes: map[string]string{"api_key": apiKey},
	}
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
