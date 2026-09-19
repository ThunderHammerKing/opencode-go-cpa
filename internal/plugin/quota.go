package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/ThunderHammerKing/opencode-go-cpa/internal/config"
	"github.com/ThunderHammerKing/opencode-go-cpa/internal/usage"
	"github.com/ThunderHammerKing/opencode-go-cpa/resources"
)

// Resource routes served by this plugin. The quota page reads its data from
// the sibling /quota/data endpoint; both are unauthenticated resource routes,
// so /quota/data stays strictly read-only and /login binds every write to a
// live state token minted by StartLogin.
const (
	quotaPagePath = "/v0/resource/plugins/" + pluginName + "/quota"
	quotaDataPath = "/v0/resource/plugins/" + pluginName + "/quota/data"
	loginPagePath = "/v0/resource/plugins/" + pluginName + "/login"
)

// quotaIdentity keeps the reference's stable, non-secret credential naming so
// config-seeded keys and login-added keys are distinguishable but consistent.
func quotaIdentity(key string) (id, label string) {
	digest := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(digest[:])
	return "opencode-go-key-" + hash, "OpenCode Go credential " + hash[:12]
}

// HandleManagement serves the plugin's resource routes plus the login-submit
// management route. No management-route quota API is registered: the
// Management Center's per-credential quota buttons are hardcoded to native
// providers, so the page is the UI.
func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	switch {
	case req.Method == http.MethodGet && req.Path == quotaPagePath:
		return htmlResponse(resources.QuotaPage), nil
	case req.Method == http.MethodGet && req.Path == quotaDataPath:
		return m.quotaData(ctx)
	case req.Method == http.MethodGet && req.Path == loginPagePath:
		return htmlResponse(resources.LoginPage), nil
	case req.Method == http.MethodPost && req.Path == loginPagePath:
		return m.handleLoginSubmit(ctx, req.Body)
	case req.Method == http.MethodPost && req.Path == "/v0/management/plugins/"+pluginName+"/login-submit":
		// The page posts here because the host only dispatches GET on
		// resource routes; the management key travels in the Authorization
		// header and is validated by the host before we see the request.
		return m.handleLoginSubmit(ctx, req.Body)
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)}, nil
}

// quotaData renders every known credential's local accounting snapshot.
func (m *Manager) quotaData(ctx context.Context) (pluginapi.ManagementResponse, error) {
	now := time.Now()
	defer m.usage.Prune(now.Add(-31 * 24 * time.Hour))

	credentials := make([]usage.Snapshot, 0, 4)
	seen := make(map[string]bool, 8)
	if m.bridge != nil {
		if entries, err := m.bridge.AuthList(ctx); err == nil {
			for _, e := range entries {
				if (e.Type != "" && e.Type != ProviderID) || (e.Provider != "" && e.Provider != ProviderID) {
					continue
				}
				id := e.ID
				if id == "" {
					id = e.AuthIndex
				}
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				credentials = append(credentials, m.usage.Snapshot(id, e.Label, now))
			}
		}
	}
	m.mu.RLock()
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()
	for _, k := range keys {
		id, label := quotaIdentity(k.Value)
		if seen[id] {
			continue
		}
		seen[id] = true
		credentials = append(credentials, m.usage.Snapshot(id, label, now))
	}
	if len(credentials) == 0 {
		// Nothing enumerable yet — still surface whatever the login flow has
		// recorded so a just-added credential is visible without a restart.
		for authID, label := range m.usage.Credentials() {
			if !seen[authID] {
				credentials = append(credentials, m.usage.Snapshot(authID, label, now))
			}
		}
	}
	return quotaJSON(map[string]any{"credentials": credentials})
}

// handleLoginSubmit accepts the key page's POST: validate the state, record
// the credential for PollLogin, and persist the auth record through the host
// with the same idempotent dedup as config-seeded keys.
func (m *Manager) handleLoginSubmit(ctx context.Context, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		State  string `json:"state"`
		APIKey string `json:"api_key"`
		Label  string `json:"label"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"invalid request"}`)}, nil
		}
	}
	if err := SubmitLogin(req.State, req.APIKey, req.Label); err != nil {
		return quotaJSON(map[string]any{"status": "error", "error": err.Error()})
	}
	auth := buildAuthData(strings.TrimSpace(req.APIKey), strings.TrimSpace(req.Label))
	if m.bridge != nil && len(auth.StorageJSON) > 0 {
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: auth.ID + ".json", JSON: auth.StorageJSON}); err != nil {
			// PollLogin still reports success from the in-memory session, but
			// the record must persist or the credential dies with the process.
			return quotaJSON(map[string]any{"status": "error", "error": "failed to persist credential"})
		}
	}
	m.mu.Lock()
	m.loginKey = req.APIKey
	m.mu.Unlock()
	statePrefix := req.State
	if len(statePrefix) > 8 {
		statePrefix = statePrefix[:8]
	}
	debugTrace("login submitted state_prefix=%s auth_id=%s", statePrefix, auth.ID)
	return quotaJSON(map[string]any{"status": "ok"})
}

func htmlResponse(page []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:    page,
	}
}

func quotaJSON(v any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("quota response encoding failed")
	}
	return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
}
