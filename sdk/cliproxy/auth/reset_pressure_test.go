package auth

import (
	"context"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func pressureTestAuth(id string, now time.Time, used string, resetIn time.Duration) *Auth {
	return &Auth{ID: id, Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent":        used,
		"X-Codex-Primary-Window-Minutes":      "300",
		"X-Codex-Primary-Reset-After-Seconds": strconv.FormatInt(int64(resetIn/time.Second), 10),
	}}}
}

func TestResetPressureWeights(t *testing.T) {
	now := time.Unix(1800000000, 0)
	cases := []struct {
		name     string
		provider string
		signals  map[string]string
		want     float64
		blocked  bool
	}{
		{"unknown", "codex", nil, 1, false},
		{"5h-near", "codex", map[string]string{"x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "3600"}, 6, false},
		{"5h-far", "codex", map[string]string{"x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "14400"}, 2.25, false},
		{"week-near", "codex", map[string]string{"x-codex-secondary-used-percent": "0", "x-codex-secondary-reset-after-seconds": "86400"}, 8, false},
		{"week-over-short", "codex", map[string]string{"x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "3600", "x-codex-secondary-used-percent": "0", "x-codex-secondary-reset-after-seconds": "86400"}, 8, false},
		{"remaining-quota", "codex", map[string]string{"x-codex-primary-used-percent": "90", "x-codex-primary-reset-after-seconds": "3600"}, 1.5, false},
		{"bounded-near-week", "codex", map[string]string{"x-codex-secondary-used-percent": "0", "x-codex-secondary-reset-after-seconds": "1"}, 1001, false},
		{"exhausted-5h", "codex", map[string]string{"x-codex-primary-used-percent": "100", "x-codex-primary-reset-after-seconds": "3600"}, 0, true},
		{"exhausted-week-overrides-5h", "codex", map[string]string{"x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "60", "x-codex-secondary-used-percent": "100", "x-codex-secondary-reset-after-seconds": "86400"}, 0, true},
		{"swapped-primary-week", "codex", map[string]string{"x-codex-primary-window-minutes": "10080", "x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "86400"}, 8, false},
		{"other-window", "codex", map[string]string{"x-codex-primary-window-minutes": "60", "x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "60"}, 1, false},
		{"claude-5h", "claude", map[string]string{"anthropic-ratelimit-unified-5h-utilization": "0.5", "anthropic-ratelimit-unified-5h-reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, 3.5, false},
		{"claude-week", "claude", map[string]string{"anthropic-ratelimit-unified-7d-utilization": "0", "anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10)}, 8, false},
		{"unsupported-provider", "gemini", map[string]string{"x-codex-primary-used-percent": "0", "x-codex-primary-reset-after-seconds": "3600"}, 1, false},
		{"invalid-nan", "codex", map[string]string{"x-codex-primary-used-percent": "NaN", "x-codex-primary-reset-after-seconds": "3600"}, 1, false},
		{"invalid-inf", "codex", map[string]string{"x-codex-primary-used-percent": "Inf", "x-codex-primary-reset-after-seconds": "3600"}, 1, false},
		{"invalid-negative", "codex", map[string]string{"x-codex-primary-used-percent": "-1", "x-codex-primary-reset-after-seconds": "3600"}, 1, false},
		{"invalid-percent", "codex", map[string]string{"x-codex-primary-used-percent": "101", "x-codex-primary-reset-after-seconds": "3600"}, 1, false},
		{"missing-reset", "codex", map[string]string{"x-codex-primary-used-percent": "0"}, 1, false},
		{"expired-reset", "codex", map[string]string{"x-codex-primary-used-percent": "100", "x-codex-primary-reset-at": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), "x-codex-primary-reset-after-seconds": "3600"}, 1, false},
		{"far-future-reset", "codex", map[string]string{"x-codex-primary-used-percent": "100", "x-codex-primary-reset-at": "253402300799"}, 1, false},
		{"invalid-countdown", "codex", map[string]string{"x-codex-primary-used-percent": "100", "x-codex-primary-reset-after-seconds": "Inf"}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Provider: tc.provider, Quota: QuotaState{ObservedAt: now, Signals: tc.signals}}
			got, blocked := resetPressureWeight(auth, "mixed", now)
			if math.Abs(got-tc.want) > 1e-9 || (!blocked.IsZero()) != tc.blocked {
				t.Fatalf("weight = %v, blocked = %v; want %v, %v", got, blocked, tc.want, tc.blocked)
			}
		})
	}
}

func TestResetPressureObservationAging(t *testing.T) {
	now := time.Unix(1800000000, 0)
	auth := pressureTestAuth("a", now, "0", time.Hour)
	before, _ := resetPressureWeight(auth, "codex", now)
	after, _ := resetPressureWeight(auth, "codex", now.Add(30*time.Minute))
	if after <= before {
		t.Fatalf("pressure did not increase: before=%v after=%v", before, after)
	}
	weight, _ := resetPressureWeight(auth, "codex", now.Add(time.Hour))
	if weight != 1 {
		t.Fatalf("expired observation weight=%v, want neutral 1", weight)
	}
	auth.Quota.ObservedAt = now.Add(2 * time.Minute)
	weight, _ = resetPressureWeight(auth, "codex", now)
	if weight != 1 {
		t.Fatalf("future observation weight=%v", weight)
	}
	auth.Quota.ObservedAt = time.Time{}
	weight, _ = resetPressureWeight(auth, "codex", now)
	if weight != 1 {
		t.Fatalf("countdown without observation time weight=%v", weight)
	}
	auth.Quota.Signals["X-Codex-Primary-Reset-At"] = now.Add(time.Hour).Format(time.RFC3339)
	weight, _ = resetPressureWeight(auth, "codex", now)
	if weight != 6 {
		t.Fatalf("absolute RFC3339 weight=%v", weight)
	}
}

func TestResetPressureDistributionAndNoMutation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	sel := &ResetPressureSelector{now: func() time.Time { return now }}
	a := pressureTestAuth("a", now, "0", time.Hour)
	b := pressureTestAuth("b", now, "0", 4*time.Hour)
	counts := map[string]int{}
	for i := 0; i < 1100; i++ {
		picked, err := sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{b, a})
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	if counts["a"] != 800 || counts["b"] != 300 {
		t.Fatalf("distribution=%v, want a:800 b:300", counts)
	}
	if a.Quota.Signals["X-Codex-Primary-Used-Percent"] != "0" || len(a.Attributes) != 0 || a.Quota.Exceeded {
		t.Fatal("selection mutated credential state")
	}
	// Unknown accounts must continue receiving traffic to learn their quotas.
	unknown := &Auth{ID: "c", Provider: "codex"}
	counts = map[string]int{}
	for i := 0; i < 700; i++ {
		picked, err := sel.Pick(context.Background(), "codex", "other", cliproxyexecutor.Options{}, []*Auth{a, unknown})
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	if counts["c"] != 100 {
		t.Fatalf("unknown exploration distribution=%v", counts)
	}
}

func TestResetPressureEligibilityAndCooldown(t *testing.T) {
	now := time.Unix(1800000000, 0)
	sel := &ResetPressureSelector{now: func() time.Time { return now }}
	exhausted := pressureTestAuth("a", now, "100", time.Hour)
	ready := pressureTestAuth("b", now, "0", 4*time.Hour)
	picked, err := sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{exhausted, ready})
	if err != nil || picked.ID != "b" {
		t.Fatalf("exhausted account picked: %v %v", picked, err)
	}
	_, err = sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{exhausted})
	if _, ok := err.(*modelCooldownError); !ok {
		t.Fatalf("all exhausted error=%T %v", err, err)
	}
	// Static priorities still take precedence; pressure only balances within a tier.
	ready.Attributes = map[string]string{"priority": "10"}
	urgent := pressureTestAuth("c", now, "0", time.Minute)
	picked, err = sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{urgent, ready})
	if err != nil || picked.ID != "b" {
		t.Fatalf("explicit priority ignored: %v %v", picked, err)
	}
	ready.Unavailable = true
	ready.NextRetryAfter = now.Add(time.Hour)
	picked, err = sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{urgent, ready})
	if err != nil || picked.ID != "c" {
		t.Fatalf("cooldown ignored: %v %v", picked, err)
	}
	if _, err = sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil); err == nil {
		t.Fatal("empty pool must fail")
	}
}

func TestResetPressureRetainsCreditsAsWeightsChange(t *testing.T) {
	now := time.Unix(1800000000, 0)
	sel := &ResetPressureSelector{now: func() time.Time { return now }}
	a := pressureTestAuth("a", now, "0", time.Hour)
	b := pressureTestAuth("b", now, "0", 4*time.Hour)
	counts := map[string]int{}
	for i := 0; i < 500; i++ {
		now = now.Add(time.Second)
		picked, err := sel.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{a, b})
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	if counts["a"] <= counts["b"] || counts["b"] < 100 {
		t.Fatalf("weight drift starved an account: %v", counts)
	}
}

func TestResetPressureConcurrentAndBounded(t *testing.T) {
	sel := &ResetPressureSelector{}
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := sel.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	for i := 0; i < resetPressureMaxKeys+1; i++ {
		if _, err := sel.Pick(context.Background(), "gemini", strconv.Itoa(i), cliproxyexecutor.Options{}, auths); err != nil {
			t.Fatal(err)
		}
	}
	if len(sel.currents) > resetPressureMaxKeys {
		t.Fatalf("unbounded rotation keys: %d", len(sel.currents))
	}
}

func TestResetPressureUsesPrevalidatedAliasCandidates(t *testing.T) {
	now := time.Now()
	sel := &ResetPressureSelector{}
	a := &Auth{ID: "a", ModelStates: map[string]*ModelState{"alias": {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}}
	ctx := selectorContextForAvailableAuths(context.Background(), sel, "alias")
	picked, err := sel.Pick(ctx, "codex", "alias", cliproxyexecutor.Options{}, []*Auth{a})
	if err != nil || picked.ID != "a" {
		t.Fatalf("rechecked already validated alias candidates: %v %v", picked, err)
	}
	manager := NewManager(nil, sel, nil)
	if manager.useSchedulerFastPath() {
		t.Fatal("static scheduler fast path would bypass live reset pressure")
	}
}
