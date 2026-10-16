package auth

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

func TestForcedBurnLosingUrgencyRevokesLeaseReadinessAndAdmission(t *testing.T) {
	m, _, b, model, id, _ := forcedQuotaFixture(t)
	lease := m.routableForcedBurn(context.Background(), model, exec.Options{})
	if lease == nil {
		t.Fatal("genuine pressure did not issue lease")
	}
	b.mu.Lock()
	b.lastTokenTPS = 100000
	b.mu.Unlock()
	a, _ := m.GetByID(id)
	if lease.permits(a) {
		t.Fatal("retained lease bypassed cap after pressure disappeared")
	}
	snap := m.SessionQuotaSnapshot(model)
	if snap.ForcedBurn.Eligible {
		t.Fatal("public readiness still advertises stale burn")
	}
	for _, bucket := range snap.Buckets {
		if bucket.Session == "waiting" && bucket.AdmissionReady {
			t.Fatal("empty session advertised as ready")
		}
	}
	_, _, reservation, err := m.admitSessionQuota(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Metadata: map[string]any{sessionquota.MetadataKey: "waiting"}})
	var exhausted *sessionquota.ExhaustedError
	if reservation != nil || !errors.As(err, &exhausted) {
		t.Fatalf("empty session admitted after urgency disappeared: %v", err)
	}
}
