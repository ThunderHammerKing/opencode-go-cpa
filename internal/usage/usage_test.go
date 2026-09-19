package usage

import (
	"testing"
	"time"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func TestWindowPercentages(t *testing.T) {
	tr := NewTracker()
	// glm-5.3: $1.40/M input, $0.26/M cached, $4.40/M output, $15 monthly.
	// 5h limit = 15*0.2 = 3; weekly = 7.5; monthly = 15.
	tr.Observe("a", "glm-5.3", 1_000_000, 0, 1_000_000, base)          // 1.40 + 4.40 = 5.80
	tr.Observe("a", "glm-5.3", 2_000_000, 1_000_000, 0, base.Add(time.Minute)) // (2-1)*1.40 = 1.40
	snap := tr.Snapshot("a", "", base.Add(2*time.Minute))
	if len(snap.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(snap.Models))
	}
	want := map[string]struct {
		spent, limit, pct float64
	}{
		Window5h:      {7.20, 3.0, 240.0},
		WindowWeekly:  {7.20, 7.5, 96.0},
		WindowMonthly: {7.20, 15.0, 48.0},
	}
	for _, w := range snap.Models[0].Windows {
		e := want[w.Name]
		if w.SpentUSD != e.spent || w.LimitUSD != e.limit || w.Percent != e.pct {
			t.Fatalf("%s = spent %.2f limit %.2f pct %.2f, want %.2f/%.2f/%.2f", w.Name, w.SpentUSD, w.LimitUSD, w.Percent, e.spent, e.limit, e.pct)
		}
	}
}

func TestFiveHourWindowExpires(t *testing.T) {
	tr := NewTracker()
	tr.Observe("a", "kimi-k3", 1_000_000, 0, 0, base) // $3.00
	snap := tr.Snapshot("a", "", base.Add(4*time.Hour))
	if got := snap.Total[0].SpentUSD; got != 3.00 {
		t.Fatalf("5h spent before expiry = %.2f, want 3.00", got)
	}
	snap = tr.Snapshot("a", "", base.Add(5*time.Hour+time.Second))
	if got := snap.Total[0].SpentUSD; got != 0 {
		t.Fatalf("5h spent after expiry = %.2f, want 0", got)
	}
	// Weekly still holds the event.
	if got := snap.Total[1].SpentUSD; got != 3.00 {
		t.Fatalf("weekly spent = %.2f, want 3.00", got)
	}
}

func TestBlockedClearsAfterReset(t *testing.T) {
	tr := NewTracker()
	reset := base.Add(30 * time.Minute)
	tr.MarkLimited("a", "glm-5.3", Window5h, reset, base)
	snap := tr.Snapshot("a", "", base.Add(time.Minute))
	if !snap.Models[0].Windows[0].Blocked {
		t.Fatal("5h window should be blocked before reset")
	}
	snap = tr.Snapshot("a", "", reset.Add(time.Second))
	if snap.Models[0].Windows[0].Blocked {
		t.Fatal("5h window should clear after reset")
	}
}

func TestUnknownModelZeroPriced(t *testing.T) {
	tr := NewTracker()
	tr.Observe("a", "brand-new-model", 1_000_000, 0, 1_000_000, base)
	snap := tr.Snapshot("a", "", base)
	if got := snap.Models[0].Windows[2].LimitUSD; got != 0 {
		t.Fatalf("unknown model limit = %.2f, want 0", got)
	}
	if got := snap.Models[0].Windows[2].SpentUSD; got != 0 {
		t.Fatalf("unknown model spend = %.2f, want 0 (no pricing)", got)
	}
}
