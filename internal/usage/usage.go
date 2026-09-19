// Package usage accounts OpenCode Go spend locally. Upstream exposes no
// usage API (the docs point at the console), so the executor reports every
// completed request's token counts here and they are priced against the
// official per-model table into three windows: 5-hour = 20% of the model's
// monthly dollar limit, weekly = 50%, monthly = 100%.
package usage

import (
	"sync"
	"time"
)

// Window names, in display order.
const (
	Window5h      = "5h"
	WindowWeekly  = "weekly"
	WindowMonthly = "monthly"
)

// windowFractions per the docs: 5h = 20%, weekly = 50%, monthly = 100% of the
// model's monthly dollar limit.
var windowFractions = map[string]float64{
	Window5h:      0.2,
	WindowWeekly:  0.5,
	WindowMonthly: 1.0,
}

var windowOrder = []string{Window5h, WindowWeekly, WindowMonthly}

type Window struct {
	Name     string    `json:"name"`
	SpentUSD float64   `json:"spent_usd"`
	LimitUSD float64   `json:"limit_usd"` // 0 = unknown pricing
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resets_at"`
	Blocked  bool      `json:"blocked"`
}

type ModelUsage struct {
	Model   string   `json:"model"`
	Windows []Window `json:"windows"`
}

type Snapshot struct {
	AuthID string       `json:"auth_id"`
	Label  string       `json:"label,omitempty"`
	Total  []Window     `json:"total"`
	Models []ModelUsage `json:"models"`
}

// price is USD per 1M tokens plus the model's monthly dollar limit.
type price struct {
	Input  float64
	Output float64
	Cached float64
	Monthly float64
}

// prices transcribed from https://opencode.ai/docs/go/ (usage limits table).
// ponytail: DeepSeek peak pricing (exactly 2x input/output, 01:00-04:00 and
// 06:00-10:00 UTC Mon-Fri) is not modelled; off-peak is billed year-round.
// Upgrade path: add a peak table + a UTC clock check in cost().
var prices = map[string]price{
	"glm-5.3-flash":                {0.15, 0.50, 0.03, 60},
	"glm-5.3":                      {1.40, 4.40, 0.26, 15},
	"glm-5.2":                      {1.40, 4.40, 0.26, 60},
	"glm-5.1":                      {1.40, 4.40, 0.26, 60},
	"kimi-k3":                      {3.00, 15.00, 0.30, 15},
	"kimi-k2.7-code":               {0.95, 4.00, 0.19, 60},
	"kimi-k2.6":                    {0.95, 4.00, 0.16, 60},
	"longcat-2.0":                  {0.30, 1.20, 0.006, 60},
	"mimo-v2.5":                    {0.14, 0.28, 0.0028, 60},
	"mimo-v2.5-pro":                {0.435, 0.87, 0.003625, 15},
	"minimax-m3":                   {0.30, 1.20, 0.06, 60},
	"minimax-m2.7":                 {0.30, 1.20, 0.06, 60},
	"minimax-m2.5":                 {0.30, 1.20, 0.06, 60},
	"muse-spark-1.3-contributor":   {0.10, 0.20, 0.002, 60},
	"muse-spark-1.2-contributor":   {0.10, 0.20, 0.002, 60},
	"qwen3.8-max":                  {2.00, 6.00, 0.25, 15},
	"qwen3.8-flash":                {0.15, 0.47, 0.016, 30},
	"qwen3.7-max":                  {2.50, 7.50, 0.50, 30},
	"qwen3.7-plus":                 {0.40, 1.60, 0.04, 60},
	"qwen3.6-plus":                 {0.50, 3.00, 0.05, 60},
	"deepseek-v4.1-flash":          {0.30, 1.20, 0.006, 60},
	"deepseek-v4-pro":              {1.32, 3.96, 0.044, 15},
	"deepseek-v4-flash":            {0.30, 1.20, 0.006, 30},
	"deepseek-v4-flash-vision-exp": {0.30, 1.20, 0.006, 15},
	"hy4-preview":                  {0.834, 2.501, 0.042, 30},
	"hy3":                          {0.14, 0.58, 0.035, 60},
	"grok-4.6":                     {4.00, 12.00, 1.00, 15},
	"gpt-5.6-luna":                 {0.20, 1.20, 0.02, 15},
}

// lookup resolves a model id case-insensitively, ignoring any "provider/"
// prefix, so catalog public ids and upstream ids both resolve.
func lookup(model string) (price, bool) {
	id := model
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			id = id[i+1:]
		}
	}
	for name, p := range prices {
		if len(name) == len(id) {
			match := true
			for i := 0; i < len(name); i++ {
				a, b := name[i], id[i]
				if 'A' <= a && a <= 'Z' {
					a += 32
				}
				if 'A' <= b && b <= 'Z' {
					b += 32
				}
				if a != b {
					match = false
					break
				}
			}
			if match {
				return p, true
			}
		}
	}
	return price{}, false
}

// event is one billed request.
type event struct {
	at    time.Time
	cost  float64
}

// limitState tracks an upstream 429 signal for one window.
type limitState struct {
	until time.Time
}

type keyModel struct {
	events     []event
	limitedAt  map[string]time.Time // window name -> blocked until
}

// Tracker aggregates spend per (credential, model). Safe for concurrent use.
type Tracker struct {
	mu     sync.Mutex
	byAuth map[string]map[string]*keyModel // auth id -> upstream model -> data
	labels map[string]string
}

func NewTracker() *Tracker {
	return &Tracker{byAuth: make(map[string]map[string]*keyModel), labels: make(map[string]string)}
}

// Observe records one completed request.
func (t *Tracker) Observe(authID, model string, inputTokens, cachedTokens, outputTokens int64, at time.Time) {
	if t == nil || (inputTokens <= 0 && outputTokens <= 0) {
		return
	}
	p, known := lookup(model)
	cached := cachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > inputTokens {
		cached = inputTokens
	}
	var cost float64
	if known {
		rest := float64(inputTokens - cached)
		cost = (rest*p.Input + float64(cached)*p.Cached + float64(outputTokens)*p.Output) / 1e6
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.labels[authID] == "" {
		t.labels[authID] = "OpenCode Go credential"
	}
	km := t.model(authID, model)
	km.events = append(km.events, event{at: at, cost: cost})
}

// MarkLimited records an upstream 429 for the named window ("" = all three)
// so the quota page can show the block until it resets. Advisory only: it
// never affects routing.
func (t *Tracker) MarkLimited(authID, model, window string, resetsAt time.Time, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	km := t.model(authID, model)
	if window == "" {
		for _, name := range windowOrder {
			km.limitedAt[name] = resetsAt
		}
		return
	}
	km.limitedAt[window] = resetsAt
}

// Snapshot renders one credential's three windows per model plus a summed
// total. Cheap enough to call per page load.
func (t *Tracker) Snapshot(authID, label string, at time.Time) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := Snapshot{AuthID: authID, Label: label}
	if lbl := t.labels[authID]; snap.Label == "" && lbl != "" {
		snap.Label = lbl
	}
	models := t.byAuth[authID]
	totals := map[string]float64{}
	limits := map[string]float64{}
	blocked := map[string]time.Time{}
	for model, km := range models {
		mu := ModelUsage{Model: model, Windows: make([]Window, 0, 3)}
		p, known := lookup(model)
		var oldest5h, oldestWeek time.Time
		for _, name := range windowOrder {
			w := Window{Name: name}
			cutoff := windowCutoff(name, at)
			var spent float64
			for _, e := range km.events {
				if e.at.Before(cutoff) {
					continue
				}
				spent += e.cost
				if name == Window5h && (oldest5h.IsZero() || e.at.Before(oldest5h)) {
					oldest5h = e.at
				}
				if name == WindowWeekly && (oldestWeek.IsZero() || e.at.Before(oldestWeek)) {
					oldestWeek = e.at
				}
			}
			w.SpentUSD = round2(spent)
			if known {
				w.LimitUSD = round2(p.Monthly * windowFractions[name])
				w.Percent = round2(spent / w.LimitUSD * 100)
			}
			w.ResetsAt = windowReset(name, at, oldest5h, oldestWeek)
			if until, ok := km.limitedAt[name]; ok {
				if at.Before(until) {
					w.Blocked = true
					w.ResetsAt = until
				}
			}
			mu.Windows = append(mu.Windows, w)
			totals[name] += w.SpentUSD
			if known {
				limits[name] += w.LimitUSD
			}
			if w.Blocked {
				if cur, ok := blocked[name]; !ok || until2(w.ResetsAt) > until2(cur) {
					blocked[name] = w.ResetsAt
				}
			}
		}
		snap.Models = append(snap.Models, mu)
	}
	for _, name := range windowOrder {
		w := Window{Name: name, SpentUSD: round2(totals[name]), LimitUSD: round2(limits[name])}
		if w.LimitUSD > 0 {
			w.Percent = round2(w.SpentUSD / w.LimitUSD * 100)
		}
		w.ResetsAt = nextBoundary(name, at)
		if until, ok := blocked[name]; ok && at.Before(until) {
			w.Blocked = true
			w.ResetsAt = until
		}
		snap.Total = append(snap.Total, w)
	}
	return snap
}

// KnownModels lists models that have at least one recorded event.
func (t *Tracker) KnownModels() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.byAuth))
	for _, models := range t.byAuth {
		for model, km := range models {
			if len(km.events) > 0 {
				out = append(out, model)
			}
		}
	}
	return out
}

// Credentials lists auth ids that have recorded activity, with their labels.
func (t *Tracker) Credentials() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]string, len(t.byAuth))
	for id := range t.byAuth {
		out[id] = t.labels[id]
	}
	return out
}

// Prune drops events older than the widest window plus slack (31 days).
func (t *Tracker) Prune(before time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, models := range t.byAuth {
		for _, km := range models {
			kept := km.events[:0]
			for _, e := range km.events {
				if e.at.After(before) {
					kept = append(kept, e)
				}
			}
			km.events = kept
		}
	}
}

func (t *Tracker) model(authID, model string) *keyModel {
	if t.byAuth[authID] == nil {
		t.byAuth[authID] = make(map[string]*keyModel)
	}
	if t.byAuth[authID][model] == nil {
		t.byAuth[authID][model] = &keyModel{limitedAt: make(map[string]time.Time)}
	}
	return t.byAuth[authID][model]
}

// windowCutoff is the instant before which events leave the named window.
func windowCutoff(name string, at time.Time) time.Time {
	switch name {
	case Window5h:
		return at.Add(-5 * time.Hour)
	case WindowWeekly:
		return at.Add(-7 * 24 * time.Hour)
	default:
		return time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}

// windowReset is when the named window next clears.
func windowReset(name string, at, oldest5h, oldestWeek time.Time) time.Time {
	if name == Window5h && !oldest5h.IsZero() {
		return oldest5h.Add(5 * time.Hour)
	}
	if name == WindowWeekly && !oldestWeek.IsZero() {
		return oldestWeek.Add(7 * 24 * time.Hour)
	}
	return nextBoundary(name, at)
}

func nextBoundary(name string, at time.Time) time.Time {
	switch name {
	case Window5h:
		return at.Add(5 * time.Hour)
	case WindowWeekly:
		return at.Add(7 * 24 * time.Hour)
	default:
		y, m, _ := at.UTC().Date()
		if m == 12 {
			return time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	}
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

// until2 keeps map comparisons readable without shadowing time.Time semantics.
func until2(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
