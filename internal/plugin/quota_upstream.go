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

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// upstreamQuotaTTL bounds how often a credential's usage is re-read. The page
// can force a refresh; routine page loads reuse the snapshot.
const upstreamQuotaTTL = 60 * time.Second

type upstreamWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// upstreamQuota is the flat view of GET {base}/usage. The json tags are the
// wire shape the page reads, so they must stay lowercase.
type upstreamQuota struct {
	Rolling upstreamWindow `json:"rolling"`
	Weekly  upstreamWindow `json:"weekly"`
	Monthly upstreamWindow `json:"monthly"`
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
	hint string
}

// quotaHint turns an upstream failure into a short operator-facing hint. The
// console is the only place a key can be replaced, so say so explicitly
// instead of leaving the page in a silent "no data" state.
func quotaHint(status int, body []byte) string {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	switch {
	case env.Error.Code == "api_key_expired":
		return "凭证已过期：请到 OpenCode 控制台创建新的 API key 后重新登录"
	case strings.EqualFold(env.Error.Type, "CreditsError") || env.Error.Code == "insufficient_balance":
		return "上游余额不足：请检查 OpenCode 订阅或余额"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Sprintf("凭证被上游拒绝（HTTP %d）：请重新登录或更换 API key", status)
	case status > 0:
		return fmt.Sprintf("上游用量读取失败（HTTP %d）", status)
	default:
		return "上游用量读取失败"
	}
}

// fetchUpstreamQuota reads the live usage for one credential's key. Exactly
// one of the results is non-empty: data on success, a hint on failure.
func (m *Manager) fetchUpstreamQuota(ctx context.Context, key string) (*upstreamQuota, string) {
	if m.bridge == nil {
		return nil, "插件未连接到宿主，无法读取上游用量"
	}
	if strings.TrimSpace(key) == "" {
		return nil, "凭证没有 API key"
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
		return nil, "网络错误：读取上游用量失败"
	}
	if resp.StatusCode != http.StatusOK {
		return nil, quotaHint(resp.StatusCode, resp.Body)
	}
	var env upstreamUsageEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, "上游用量响应格式异常"
	}
	return &upstreamQuota{
		Rolling: env.Usage.Rolling,
		Weekly:  env.Usage.Weekly,
		Monthly: env.Usage.Monthly,
	}, ""
}

// cachedUpstreamQuota serves the usage from a short-lived cache. A failed
// refresh keeps serving the last good snapshot (stale-while-unavailable) but
// returns the failure hint so the page can explain what is wrong. Failures
// are cached for the same TTL as successes so a dead credential is not
// re-probed on every page load.
func (m *Manager) cachedUpstreamQuota(ctx context.Context, authID, key string, refresh bool) (*upstreamQuota, string) {
	m.quotaMu.Lock()
	cached := m.quotaCache[authID]
	m.quotaMu.Unlock()
	if !refresh && time.Since(cached.at) < upstreamQuotaTTL {
		return cached.data, cached.hint
	}
	data, hint := m.fetchUpstreamQuota(ctx, key)
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	if hint != "" {
		m.quotaCache[authID] = cachedQuota{at: time.Now(), data: cached.data, hint: hint}
		return cached.data, hint
	}
	m.quotaCache[authID] = cachedQuota{at: time.Now(), data: data}
	return data, ""
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
	data, hint := m.fetchUpstreamQuota(ctx, key)
	if hint != "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("%s", hint)
	}
	if data == nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota unavailable")
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
