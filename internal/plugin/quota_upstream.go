package plugin

// Upstream quota. OpenCode Go exposes the authoritative usage windows at
// GET {base}/usage — rolling (5h) / weekly / monthly, each with a percent and
// a reset timestamp. That is the same data the console shows, so it is the
// primary source for the quota page; the local token accounting in
// internal/usage stays as the per-model breakdown.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// upstreamQuotaTTL bounds how often a credential's usage is re-read. The page
// can force a refresh; routine page loads reuse the snapshot.
const upstreamQuotaTTL = 60 * time.Second

type upstreamWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// upstreamQuota is the flat view of GET {base}/usage.
type upstreamQuota struct {
	Rolling upstreamWindow
	Weekly  upstreamWindow
	Monthly upstreamWindow
}

// upstreamUsageEnvelope mirrors the wire shape: the three windows live under
// a top-level "usage" object.
type upstreamUsageEnvelope struct {
	Usage struct {
		Rolling upstreamWindow `json:"rolling"`
		Weekly  upstreamWindow `json:"weekly"`
		Monthly upstreamWindow `json:"monthly"`
	} `json:"usage"`
}

type cachedQuota struct {
	at   time.Time
	data *upstreamQuota
}

// fetchUpstreamQuota reads the live usage for one credential's key.
func (m *Manager) fetchUpstreamQuota(ctx context.Context, key string) (*upstreamQuota, error) {
	if m.bridge == nil {
		return nil, fmt.Errorf("quota bridge unavailable")
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("quota credential has no key")
	}
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	m.mu.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     strings.TrimRight(baseURL, "/") + "/usage",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}, "Accept": []string{"application/json"}},
	})
	if err != nil {
		return nil, fmt.Errorf("quota request failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota upstream status %d", resp.StatusCode)
	}
	var env upstreamUsageEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("quota response invalid")
	}
	return &upstreamQuota{
		Rolling: env.Usage.Rolling,
		Weekly:  env.Usage.Weekly,
		Monthly: env.Usage.Monthly,
	}, nil
}

// cachedUpstreamQuota serves the usage from a short-lived cache. A failed
// refresh keeps serving the last good snapshot (stale-while-unavailable),
// because a usage hiccup must not blank the page.
func (m *Manager) cachedUpstreamQuota(ctx context.Context, authID, key string, refresh bool) *upstreamQuota {
	m.quotaMu.Lock()
	cached := m.quotaCache[authID]
	m.quotaMu.Unlock()
	if !refresh && cached.data != nil && time.Since(cached.at) < upstreamQuotaTTL {
		return cached.data
	}
	data, err := m.fetchUpstreamQuota(ctx, key)
	if err != nil {
		return cached.data
	}
	m.quotaMu.Lock()
	m.quotaCache[authID] = cachedQuota{at: time.Now(), data: data}
	m.quotaMu.Unlock()
	return data
}

// authKeyOf extracts the credential's API key for an upstream call: the
// parsed attribute when present, else the raw storage JSON.
func authKeyOf(attrs map[string]string, storage []byte) string {
	if strings.TrimSpace(attrs["api_key"]) != "" {
		return strings.TrimSpace(attrs["api_key"])
	}
	var raw struct {
		APIKey string `json:"api_key"`
	}
	if len(storage) > 0 && json.Unmarshal(storage, &raw) == nil {
		return strings.TrimSpace(raw.APIKey)
	}
	return ""
}

// quotaWindowFraction converts a used percentage into the ABI's remaining
// fraction, clamped so a malformed upstream cannot produce a negative bucket.
func quotaWindowFraction(percent int) float64 {
	f := 1 - float64(percent)/100
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// --- QuotaProvider (quota.identifier / quota.describe / quota.fetch / quota.reset)

func (m *Manager) quotaIdentifier() string { return ProviderID }

func (m *Manager) quotaDescribe(context.Context) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderID},
		DisplayName:        "OpenCode Go",
		SupportsReset:      false,
	}, nil
}

// quotaFetch serves one credential's real usage to the host's quota API.
func (m *Manager) quotaFetch(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	key := authKeyOf(req.Attributes, req.StorageJSON)
	if key == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("credential has no api key")
	}
	data, err := m.fetchUpstreamQuota(ctx, key)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	buckets := []pluginapi.QuotaBucket{
		{Window: "5h", RemainingFraction: quotaWindowFraction(data.Rolling.Percent), ResetTime: data.Rolling.ResetsAt},
		{Window: "weekly", RemainingFraction: quotaWindowFraction(data.Weekly.Percent), ResetTime: data.Weekly.ResetsAt},
		{Window: "monthly", RemainingFraction: quotaWindowFraction(data.Monthly.Percent), ResetTime: data.Monthly.ResetsAt},
	}
	return pluginapi.QuotaFetchResponse{
		Groups: []pluginapi.QuotaGroup{{DisplayName: "OpenCode Go", Buckets: buckets}},
	}, nil
}

func (m *Manager) quotaReset(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{}, fmt.Errorf("opencode-go quota reset is not supported")
}
