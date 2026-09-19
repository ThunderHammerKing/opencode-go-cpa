package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/ThunderHammerKing/opencode-go-cpa/internal/catalog"
	"github.com/ThunderHammerKing/opencode-go-cpa/internal/config"
	"github.com/ThunderHammerKing/opencode-go-cpa/internal/errclass"
	"github.com/ThunderHammerKing/opencode-go-cpa/internal/usage"
)

// ProviderID is the single provider key served by this plugin (FR-001).
const ProviderID = "opencode-go"

// pluginName / pluginVersion are reported in registration metadata and used
// in resource route paths. pluginName must match the loaded library file name
// (opencode-go-cpa-v0.1.0.dylib minus version/extension).
const (
	pluginName    = "opencode-go-cpa"
	pluginVersion = "0.1.0"
	pluginAuthor  = "ThunderHammerKing"
)

// githubRepoURL satisfies the host's validPlugin gate (host.go
// validPlugin rejects empty Metadata.GitHubRepository).
const githubRepoURL = "https://github.com/ThunderHammerKing/opencode-go-cpa"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup/reconfigure for a
// full request-timeout (default 15m). On expiry registration proceeds per
// FR-002 empty/stale semantics; the ticker retries at refresh-interval with
// the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, catalog manager, refresh
// loop) and routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	mu   sync.RWMutex
	cfg  config.Config
	mgr  *catalog.Manager
	// usage is the local spend accounting backing the quota page.
	usage *usage.Tracker
	// loginKey caches the most recently pasted credential so the catalog
	// refresh can seed itself before any config key exists.
	loginKey string
	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge, usage: usage.NewTracker()}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		// Defensive: host-boundary panics are goroutine-contained (see
		// HostBridge.callWithTimeout); this guards future handler bugs.
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return m.handleModels()
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		resp, err := (authProvider{}).ParseAuth(context.Background(), req)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		debugTrace("auth parse response provider=%s id=%s attr_api_key_present=%t storage_json_bytes=%d", resp.Auth.Provider, resp.Auth.ID, strings.TrimSpace(resp.Auth.Attributes["api_key"]) != "", len(resp.Auth.StorageJSON))
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart:
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth login request body"), nil
		}
		resp, err := (authProvider{}).StartLogin(context.Background(), req.AuthLoginStartRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		debugTrace("auth login start provider=%s state_prefix=%s", ProviderID, safePrefix(resp.State, 8))
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginPoll:
		var req struct{ pluginapi.AuthLoginPollRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth poll request body"), nil
		}
		resp, err := (authProvider{}).PollLogin(context.Background(), req.AuthLoginPollRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthRefresh:
		var req struct{ pluginapi.AuthRefreshRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		resp, err := (authProvider{}).RefreshAuth(context.Background(), req.AuthRefreshRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "executor.count_tokens has no OpenCode Go equivalent",
		}), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by opencode-go", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
// The request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func registrationEnvelope() []byte {
	// Client side is chat-completions only: the host translates Claude Code
	// (/v1/messages) and Codex (/v1/responses) requests into chat-completions
	// before the executor sees them, and translates our openai responses back
	// (adapters_executors.go selectExecutorInputFormat/OutputFormat).
	formats := []string{"chat-completions"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: githubRepoURL,
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	})
}

// configFields is the Management Center settings form. Flat keys only, and
// the panel renders string / number / boolean / array-of-strings — anything
// structured stays YAML-only (see README).
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream base URL (default https://opencode.ai/zen/go/v1)."},
		{Name: "model-prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Client-facing model id prefix; empty disables prefixing."},
		{Name: "refresh-interval", Type: pluginapi.ConfigFieldTypeString, Description: "Catalog discovery cadence, min 1m (default 15m)."},
		{Name: "request-timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream request timeout (default 5m)."},
		{Name: "max-concurrent-per-key", Type: pluginapi.ConfigFieldTypeNumber, Description: "Per-credential concurrency cap; 0 = unlimited."},
		{Name: "user-agent", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream User-Agent; the docs require a client-own agent name."},
		{Name: "forward-session", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Derive/forward x-opencode-session from native client session headers."},
		{Name: "cooldown-on-429", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Mark the quota page blocked on upstream 429s."},
		{Name: "respect-retry-after", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Honour upstream Retry-After when marking quota windows."},
		{Name: "api-keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional seed keys; credentials normally live in auth files (added via 登录)."},
	}
}

func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	// The quota page is the plugin's only sidebar menu entry. The login and
	// quota-data resource paths must ALSO be registered (the host dispatches
	// resource requests by registered path); empty Menu keeps them out of the
	// sidebar. The login page is only useful with a live state token from
	// StartLogin, and the page JS fetches ./quota/data relatively.
	return okEnvelope(struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
		Resources []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		} `json:"resources"`
	}{
		// login-submit is a management route (resource routes are GET-only
		// host-side); the key page sends the management key it collected.
		Routes: []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}{{Method: "POST", Path: "/plugins/" + pluginName + "/login-submit"}},
		Resources: []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		}{
			{Path: "/quota", Menu: "OpenCode Go Quota", Description: "View OpenCode Go usage windows (local accounting)."},
			{Path: "/quota/data", Description: "Quota snapshot JSON for the quota page."},
			{Path: "/login", Description: "Paste-your-API-key page for the login flow."},
		},
	}), nil
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config, refresh the catalog once, publish state, restart the refresh
// loop. Registration succeeds even when the initial refresh fails (FR-002).
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	debugTrace("lifecycle config_loaded key_count=%d prefix=%s provider=%s", len(cfg.APIKeys), cfg.ModelPrefix, cfg.ProviderID)
	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	if err := m.materializeAuthRecords(ctx, cfg); err != nil {
		return ErrEnvelope("auth_materialization_failed", err.Error()), nil
	}
	// A nil *HostBridge must not enter the interface as a typed nil, or
	// catalog's nil-client guard never fires and Refresh panics inside Do.
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := catalog.New(cfg, client)
	refreshKey := m.catalogRefreshKey(ctx, cfg)
	refreshErr := refreshOnce(context.Background(), mgr, m.bridge, registerRefreshTimeout, cfg, refreshKey)
	debugTrace("lifecycle refresh_complete model_count=%d refresh_error=%t", len(mgr.Models()), refreshErr != nil)

	// Retire any running loop and wait for its exit outside m.mu: a mid-refresh
	// tick must never stall readers holding RLock (F4). lifeMu keeps the
	// stop-wait-install sequence atomic against other lifecycles.
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	m.mu.Lock()
	m.cfg = cfg
	// FR-002 stale-while-unavailable: a failed refresh must neither wipe a
	// non-empty previous snapshot nor keep the OLD manager, whose stored cfg
	// would pin the old base-url/catalog-url/protocols/prefix so ticks
	// fetched the old URL with the new config's keys forever (F5). The NEW
	// manager is adopted unconditionally; when a previous snapshot exists
	// AND the NEW config keeps stale-while-unavailable enabled, it is
	// seeded into the new one, so serving is unchanged until the next
	// successful tick refreshes against the NEW config. Under fail-closed
	// (stale-while-unavailable:false) no seed happens — same as the ticker
	// failure path — so a reconfigure during an outage serves nothing until
	// a refresh succeeds, honoring the operator's policy on BOTH paths.
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	interval := cfg.Catalog.RefreshInterval
	m.mu.Unlock()

	m.startRefreshLoop(cfg, mgr, interval, stop, done)
	return registrationEnvelope(), nil
}

// materializeAuthRecords makes CPA-visible auth files idempotently. Existing
// records are discovered through CPA so their host-managed metadata is never
// overwritten. The full key digest is non-secret and independent of config
// ordering. The host ABI has no delete/disable callback, so removed keys remain
// stale records and are not claimed as removed.
func (m *Manager) materializeAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return nil
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records: %w", err)
	}
	existing := make(map[string]struct{}, len(entries)*2)
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Name); name != "" {
			existing[name] = struct{}{}
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			existing[id] = struct{}{}
		}
	}
	for _, key := range cfg.APIKeys {
		digest := sha256.Sum256([]byte(key.Value))
		hash := hex.EncodeToString(digest[:])
		id := "opencode-go-key-" + hash
		name := id + ".json"
		if _, ok := existing[id]; ok {
			continue
		}
		if _, ok := existing[name]; ok {
			continue
		}
		record, err := json.Marshal(struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Label  string `json:"label"`
			APIKey string `json:"api_key"`
		}{
			Type: "opencode-go", ID: id, Label: "OpenCode Go credential " + hash, APIKey: key.Value,
		})
		if err != nil {
			return fmt.Errorf("build auth record")
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{
			Name: name, JSON: record,
		}); err != nil {
			return err
		}
		existing[id] = struct{}{}
		existing[name] = struct{}{}
		debugTrace("auth materialized id=%s file=%s", id, name)
	}
	return nil
}

// handleModels implements model.static / model.for_auth (FR-003): the last
// good catalog snapshot mapped to wire ModelInfos; empty catalog yields an
// empty slice, not an error.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if mgr != nil {
		for _, rec := range mgr.Models() {
			models = append(models, pluginapi.ModelInfo{
				ID:                        rec.PublicID,
				Object:                    "model",
				OwnedBy:                   ProviderID,
				DisplayName:               rec.DisplayName,
				ContextLength:             rec.ContextLimit,
				MaxCompletionTokens:       rec.OutputLimit,
				SupportedInputModalities:  rec.InputModes,
				SupportedOutputModalities: rec.OutputModes,
				Thinking:                  rec.Thinking,
			})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// shutdownDrainTimeout bounds how long handleShutdown waits for orphaned
// host-callback goroutines (timed-out callbacks and their abandon/drain
// cleanup) to finish before returning. Package var so tests can shrink it.
var shutdownDrainTimeout = 15 * time.Second

// handleShutdown stops the refresh loop, drains orphaned host callbacks,
// and clears state. The drain matters on Unix: the SDK loader frees host_api
// and dlclose's the plugin immediately after this export returns
// (loader_unix.go), so a goroutine still calling into the host would crash
// the process — see COMPATIBILITY.md limitations.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	oldDone := m.closeStop()
	if oldDone != nil {
		<-oldDone
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		// Non-Windows unload during this residual window can crash the
		// host; the warn makes the stall visible without blocking forever.
		_ = m.bridge.Log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.mgr = config.Config{}, nil
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu) and the served
// catalog manager. The loop captures its own cfg/mgr/bridge snapshot so
// it never contends on m.mu; reconfigure swaps state and restarts the loop.
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *catalog.Manager, interval time.Duration, stop, done chan struct{}) {
	// F4: tick refresh contexts derive from stopCtx so close(stop) aborts
	// an in-flight Refresh immediately instead of leaving lifeMu held until
	// the old config's request-timeout expires. The watcher goroutine is
	// required because the loop body blocks inside refreshOnce while a tick
	// runs and cannot select on stop itself.
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						// CGO bridge calls can panic; a tick must never
						// take the host process down.
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					refreshOnce(stopCtx, mgr, m.bridge, cfg.RequestTimeout, cfg, m.catalogRefreshKey(stopCtx, cfg))
				}()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the stop/done pair.
// It returns the loop's done channel — nil when no loop was running — and
// the CALLER waits on it after this returns, never while holding m.mu (F4).
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

// refreshOnce runs one bounded catalog refresh. Catalog refresh is not a
// client request, so the key comes from catalogRefreshKey (config seed first,
// then an auth-file credential) and carries no client selection, rotation,
// cooldown, or retry state. parent bounds-and-cancels the attempt: the
// lifecycle path passes context.Background() plus registerRefreshTimeout;
// ticker ticks pass the loop's stop-derived context plus the full
// request-timeout, so close(stop) aborts an in-flight tick (F4). On failure
// it logs a warn via host.log — error text is a redacted category label from
// the catalog package, never key material (FR-011).
func refreshOnce(parent context.Context, mgr *catalog.Manager, bridge *HostBridge, timeout time.Duration, cfg config.Config, apiKey string) error {
	if apiKey == "" {
		// No credential anywhere yet (fresh install, login not completed):
		// fail soft so registration and reconfigure still succeed.
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh skipped: no opencode-go credential available yet", nil)
		}
		return fmt.Errorf("no credential available for catalog refresh")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	err := mgr.Refresh(ctx, apiKey)
	if err != nil {
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	// FR-010: surface normalization diagnostics with the post-refresh log.
	warnings := mgr.Warnings()
	if uns := mgr.Unsupported(); len(uns) > 0 {
		parts := make([]string, len(uns))
		for i, u := range uns {
			parts[i] = fmt.Sprintf("%q: %s", u.UpstreamID, u.Reason)
		}
		fields := map[string]any{"models": strings.Join(parts, ", ")}
		if len(warnings) > 0 {
			fields["warnings"] = strings.Join(warnings, "; ")
		}
		_ = bridge.Log("warn", "unsupported models excluded from routable catalog", fields)
	} else if len(warnings) > 0 {
		_ = bridge.Log("warn", "catalog normalization warnings",
			map[string]any{"warnings": strings.Join(warnings, "; ")})
	}
	return nil
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// catalogRefreshKey picks the credential for catalog discovery: the first
// config seed key when present, else a key harvested from an existing auth
// file (credentials added through the login flow live only in auth files).
// The harvested key is cached so repeat ticks cost no host calls.
func (m *Manager) catalogRefreshKey(ctx context.Context, cfg config.Config) string {
	if len(cfg.APIKeys) > 0 {
		return cfg.APIKeys[0].Value
	}
	m.mu.RLock()
	cached := m.loginKey
	m.mu.RUnlock()
	if cached != "" {
		return cached
	}
	if m.bridge == nil {
		return ""
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if (e.Type != "" && e.Type != ProviderID) || (e.Provider != "" && e.Provider != ProviderID) {
			continue
		}
		idx := e.AuthIndex
		if idx == "" {
			continue
		}
		raw, err := m.bridge.AuthGet(ctx, idx)
		if err != nil {
			continue
		}
		var rec struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal(raw, &rec) != nil || strings.TrimSpace(rec.APIKey) == "" {
			continue
		}
		m.mu.Lock()
		m.loginKey = rec.APIKey
		m.mu.Unlock()
		return rec.APIKey
	}
	return ""
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
