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
