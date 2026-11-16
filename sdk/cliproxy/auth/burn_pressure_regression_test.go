package auth

import (
	"math"
	"testing"
	"time"
)

func TestBurnFiveHourProjectionDoesNotTurnWeeklyScarcityIntoUrgency(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := &Auth{ID: "weekly-limited", Provider: "codex"}
	a.Quota = burnTestQuota(now, 44, 66, now.Add(279*time.Minute), now.Add(6*24*time.Hour))
	b.Observe(a, a.Quota, now)
	state := b.account(a.ID)
	state.Windows["five_hour"].PercentPerToken = 0.00005356814397639636
	state.Windows["weekly"].PercentPerToken = 0.000036892755849331846
	for _, w := range state.Windows {
		w.CalibrationSamples = 2
	}
	b.lastTokenTPS = 1795.3662384717431
	want := b.lastTokenTPS * (279 * time.Minute).Seconds()
	if got := b.projectedTokens(state, "five_hour", b.lastTokenTPS, now); math.Abs(got-want) > 1e-6 {
		t.Fatalf("weekly budget distorted five-hour throughput: got %g, want %g", got, want)
	}
	if got := b.chooseForcedLocked([]*Auth{a}, now); got != "" {
		t.Fatalf("forced %q hours early despite enough throughput to finish in about ten minutes", got)
	}
}

func TestBurnForcedTargetRevalidatesCurrentPressure(t *testing.T) {
	for _, change := range []string{"faster throughput", "less quota remaining", "calibration correction", "unknown throughput", "invalid throughput"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			b := burnTestController(now)
			a := &Auth{ID: "pressure", Provider: "codex"}
			a.Quota = burnTestQuota(now, 10, 10, now.Add(10*time.Minute), now.Add(24*time.Hour))
			b.Observe(a, a.Quota, now)
			w := b.account(a.ID).Windows["five_hour"]
			w.PercentPerToken, w.CalibrationSamples = .1, 2
			b.lastTokenTPS = 1
			if got := b.chooseForcedLocked([]*Auth{a}, now); got != a.ID {
				t.Fatalf("genuine pressure did not force: %q", got)
			}
			switch change {
			case "faster throughput":
				b.lastTokenTPS = 100
			case "less quota remaining":
				w.Used = 99
			case "calibration correction":
				w.PercentPerToken = 10
			case "unknown throughput":
				b.lastTokenTPS = 0
			case "invalid throughput":
				b.lastTokenTPS = math.NaN()
			}
			if got := b.forcedEligibleLocked([]*Auth{a}, now); got != "" {
				t.Fatalf("old target remains bypass authority after %s: %q", change, got)
			}
			if got := b.chooseForcedLocked([]*Auth{a}, now); got != "" {
				t.Fatalf("reselected nonurgent target after %s: %q", change, got)
			}
		})
	}
}

func TestBurnExhaustedTargetIsNeverForced(t *testing.T) {
	// An account whose five-hour window is fully consumed must not be chosen as
	// the forced burn target: reset-pressure would refuse it and routing would
	// starve while other accounts still have headroom.
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	exhausted := &Auth{ID: "exhausted", Provider: "codex"}
	exhausted.Quota = burnTestQuota(now, 100, 10, now.Add(3*time.Hour), now.Add(24*time.Hour))
	b.Observe(exhausted, exhausted.Quota, now)
	healthy := &Auth{ID: "healthy", Provider: "codex"}
	healthy.Quota = burnTestQuota(now, 20, 10, now.Add(3*time.Hour), now.Add(24*time.Hour))
	b.Observe(healthy, healthy.Quota, now)
	b.lastTokenTPS = 100
	if got := b.chooseForcedLocked([]*Auth{exhausted, healthy}, now); got == exhausted.ID {
		t.Fatal("exhausted account was selected as the forced burn target")
	}
}

func TestBurnStaleLatchDoesNotSurviveLostUrgency(t *testing.T) {
	// A latched forced target that later exhausts its window must be revoked:
	// chooseForcedLocked previously fell through to the stale latch even after
	// forcedEligibleLocked revalidation failed, starving routing onto the one
	// account reset-pressure refuses.
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	target := &Auth{ID: "latch-target", Provider: "codex"}
	target.Quota = burnTestQuota(now, 10, 10, now.Add(10*time.Minute), now.Add(24*time.Hour))
	b.Observe(target, target.Quota, now)
	w := b.account(target.ID).Windows["five_hour"]
	w.PercentPerToken, w.CalibrationSamples = .1, 2
	b.lastTokenTPS = 1
	if got := b.chooseForcedLocked([]*Auth{target}, now); got != target.ID {
		t.Fatalf("genuine pressure did not latch the target: %q", got)
	}
	// The window exhausts before its reset; the latch must not survive.
	later := now.Add(time.Minute)
	target.Quota = burnTestQuota(later, 100, 10, later.Add(9*time.Minute), later.Add(24*time.Hour))
	b.Observe(target, target.Quota, later)
	if got := b.chooseForcedLocked([]*Auth{target}, later); got != "" {
		t.Fatalf("stale latch survived lost urgency: %q", got)
	}
}
