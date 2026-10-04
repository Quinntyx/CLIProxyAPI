package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func burnTestQuota(now time.Time, used5, usedW float64, reset5, resetW time.Time) QuotaState {
	h := http.Header{}
	for _, p := range []struct {
		prefix string
		used   float64
		reset  time.Time
		mins   int
	}{{"x-codex-primary-", used5, reset5, 300}, {"x-codex-secondary-", usedW, resetW, 10080}} {
		h.Set(p.prefix+"used-percent", strconv.FormatFloat(p.used, 'f', -1, 64))
		h.Set(p.prefix+"reset-at", strconv.FormatInt(p.reset.Unix(), 10))
		h.Set(p.prefix+"window-minutes", strconv.Itoa(p.mins))
	}
	var q QuotaState
	q.ObserveResponseHeadersForProvider("codex", h, now)
	return q
}
func burnTestController(now time.Time) *BurnController {
	b := NewBurnController()
	b.now = func() time.Time { return now }
	b.configure(config.BurnDeadlineConfig{Enabled: true, Threshold: .75})
	return b
}
func TestBurnCapsFailClosedAndReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := &Auth{ID: "cap-test", Provider: "codex", Metadata: map[string]any{"weekly_cap_percent": 50.0, "five_hour_cap_percent": 50.0, "exclude_burn_metrics": true}}
	if b.CapPermits(a, now) {
		t.Fatal("unknown quota must be blocked")
	}
	a.Quota = burnTestQuota(now, 49, 49, now.Add(time.Hour), now.Add(24*time.Hour))
	if !b.CapPermits(a, now) {
		t.Fatal("fresh quota below caps must be usable")
	}
	for _, tc := range []struct {
		name         string
		five, weekly float64
	}{{"five", 50, 49}, {"weekly", 49, 50}, {"external", 60, 55}} {
		t.Run(tc.name, func(t *testing.T) {
			b := burnTestController(now)
			a.Quota = burnTestQuota(now, tc.five, tc.weekly, now.Add(time.Hour), now.Add(24*time.Hour))
			if b.CapPermits(a, now) {
				t.Fatal("cap reached must block")
			}
		})
	}
	a.Quota = burnTestQuota(now, 49, 49, now.Add(time.Hour), now.Add(24*time.Hour))
	if b.CapPermits(a, now.Add(3*time.Minute)) {
		t.Fatal("stale quota must block")
	}
	if b.CapPermits(a, now.Add(2*time.Hour)) {
		t.Fatal("expired window must block until confirmed refill")
	}
	a.Quota = burnTestQuota(now.Add(2*time.Hour), 0, 49, now.Add(7*time.Hour), now.Add(24*time.Hour))
	if !b.CapPermits(a, now.Add(2*time.Hour)) {
		t.Fatal("confirmed refill must unblock")
	}
	a.Metadata["weekly_cap_percent"] = "invalid"
	if b.CapPermits(a, now.Add(2*time.Hour)) {
		t.Fatal("invalid cap must fail closed")
	}
}
func TestBurnWeeklyAccounting(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	for _, tc := range []struct {
		id              string
		used            float64
		active, exclude bool
	}{{"active", 80, true, false}, {"idle", 0, false, false}, {"shared", 50, true, true}} {
		a := &Auth{ID: tc.id, Provider: "codex", Metadata: map[string]any{"exclude_burn_metrics": tc.exclude}}
		q := burnTestQuota(now, 20, tc.used, now.Add(time.Hour), now.Add(time.Minute))
		b.Observe(a, q, now)
		b.accounts[a.ID].ActiveWeek = tc.active
		b.closeCrossedWeek(a.ID, b.accounts[a.ID], now.Add(2*time.Minute))
		b.closeCrossedWeek(a.ID, b.accounts[a.ID], now.Add(3*time.Minute))
	}
	s := b.Snapshot()
	if s.ActiveWeeks != 1 || s.CompletedWeeks != 2 || s.Inefficiency == nil || *s.Inefficiency != 20 || s.TotalWaste == nil || *s.TotalWaste != 60 {
		t.Fatalf("unexpected metrics: %+v", s)
	}
	if s.EstimatedWeeks != 2 {
		t.Fatal("pre-boundary observations must be labelled estimates")
	}
	if len(b.history) != 3 {
		t.Fatal("resets must not double count")
	}
	empty := burnTestController(now).Snapshot()
	if empty.Inefficiency != nil || empty.TotalWaste != nil {
		t.Fatal("empty averages must be null")
	}
}
func TestBurnCalibrationAndTPS(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := &Auth{ID: "calibration", Provider: "codex"}
	q := burnTestQuota(now, 0, 0, now.Add(time.Hour), now.Add(24*time.Hour))
	b.Observe(a, q, now)
	for i := 0; i < 2; i++ {
		b.HandleUsage(context.Background(), usage.Record{Provider: "codex", AuthID: a.ID, Model: "gpt-5.5", RequestedAt: now, Latency: 10 * time.Second, Detail: usage.Detail{OutputTokens: 100}})
	}
	if v := b.Snapshot().OutputTPS; math.Abs(v-20) > 1e-9 {
		t.Fatalf("overlapping intervals: TPS=%v, want20", v)
	}
	q = burnTestQuota(now.Add(time.Second), 2, 1, now.Add(time.Hour), now.Add(24*time.Hour))
	b.Observe(a, q, now.Add(time.Second))
	w := b.accounts[a.ID].Windows["weekly"]
	if w.CalibrationSamples != 1 || math.Abs(w.PercentPerToken-.005) > 1e-9 {
		t.Fatalf("bad calibration: %+v", w)
	}
	// No token use: external use must not calibrate our throughput.
	q = burnTestQuota(now.Add(2*time.Second), 3, 2, now.Add(time.Hour), now.Add(24*time.Hour))
	b.Observe(a, q, now.Add(2*time.Second))
	if b.accounts[a.ID].Windows["weekly"].CalibrationSamples != 1 {
		t.Fatal("external usage contaminated calibration")
	}
	// Idle time does not turn active throughput into zero.
	b.now = func() time.Time { return now.Add(time.Hour) }
	if b.Snapshot().OutputTPS != 20 {
		t.Fatal("active rate should survive idle time")
	}
}
func TestBurnDeadlineOverridesStickyThenLatches(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := &Auth{ID: "burn-A", Provider: "codex"}
	z := &Auth{ID: "burn-Z", Provider: "codex"}
	for _, auth := range []*Auth{a, z} {
		auth.Quota = burnTestQuota(now, 0, 50, now.Add(time.Hour), now.Add(time.Hour))
		b.Observe(auth, auth.Quota, now)
	}
	b.lastTokenTPS = 10
	b.accounts[z.ID].Windows["weekly"].Reset = now.Add(60 * time.Second)
	b.accounts[z.ID].Windows["weekly"].PercentPerToken = .1
	b.accounts[z.ID].Windows["weekly"].CalibrationSamples = 2
	sticky := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	opts := exec.Options{Headers: http.Header{"Session-Id": []string{"burn-session"}}}
	first, err := sticky.Pick(context.Background(), "codex", "", opts, []*Auth{a, z})
	if err != nil || first.ID != a.ID {
		t.Fatalf("cold sticky binding failed: %v", err)
	}
	selector := &BurnDeadlineSelector{Fallback: sticky, Controller: b}
	chosen, err := selector.Pick(context.Background(), "codex", "", opts, []*Auth{a, z})
	if err != nil || chosen.ID != z.ID {
		t.Fatalf("urgent plan did not override sticky: %v, %+v", err, chosen)
	}
	b.lastTokenTPS = 100
	chosen, err = selector.Pick(context.Background(), "codex", "", opts, []*Auth{a, z})
	if err != nil || chosen.ID != z.ID {
		t.Fatal("burn target must latch, not oscillate")
	}
	z.Disabled = true
	chosen, err = selector.Pick(context.Background(), "codex", "", opts, []*Auth{a, z})
	if err != nil || chosen.ID != a.ID {
		t.Fatal("disabled burn target must fail over")
	}
}
func TestBurnUnknownRatesDoNotForce(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	a := &Auth{ID: "unknown", Provider: "codex"}
	a.Quota = burnTestQuota(now, 0, 0, now.Add(time.Minute), now.Add(time.Minute))
	b.Observe(a, a.Quota, now)
	if b.chooseForcedLocked([]*Auth{a}, now) != "" {
		t.Fatal("uncalibrated rate must not force")
	}
}
func TestBurnStatePrivateAndPersistent(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	b.cfg.StateFile = filepath.Join(t.TempDir(), "private", "burn.json")
	b.lastTokenTPS = 50
	b.history = []BurnWeek{{AuthID: "one", Reset: now, RemainingPercent: 25, Active: true}}
	if err := b.save(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(b.cfg.StateFile)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("private state permissions: %v %v", stat, err)
	}
	raw, _ := os.ReadFile(b.cfg.StateFile)
	if strings.Contains(string(raw), "access_token") || strings.Contains(string(raw), "prompt") {
		t.Fatal("private telemetry must not contain tokens or prompts")
	}
	restored := burnTestController(now)
	restored.cfg.StateFile = b.cfg.StateFile
	if err := restored.load(); err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().TotalWaste == nil || *restored.Snapshot().TotalWaste != 25 || restored.Snapshot().ActiveTokenTPS != 50 {
		t.Fatal("state/averages must survive restart")
	}
}
func TestBurnConcurrentTelemetry(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	a := &Auth{ID: "concurrent", Provider: "codex"}
	q := burnTestQuota(now, 0, 0, now.Add(time.Hour), now.Add(24*time.Hour))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Observe(a, q, now)
			b.HandleUsage(context.Background(), usage.Record{Provider: "codex", AuthID: a.ID, Latency: time.Second, Detail: usage.Detail{OutputTokens: 1}})
			_ = b.Snapshot()
		}()
	}
	wg.Wait()
}

func TestBurnIdentityAndWeeklyRateSurviveReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := &Auth{ID: "old-file", Provider: "codex", Metadata: map[string]any{"account_id": "same-subscription"}}
	q := burnTestQuota(now, 0, 80, now.Add(time.Hour), now.Add(time.Minute))
	b.Observe(a, q, now)
	state := b.account(a.ID)
	state.ActiveWeek = true
	state.Windows["weekly"].PercentPerToken = .005
	state.Windows["weekly"].CalibrationSamples = 3
	b.closeCrossedWeek(a.ID, state, now.Add(2*time.Minute))
	b.closeCrossedWeek(a.ID, state, now.Add(3*time.Minute))
	replacement := &Auth{ID: "new-file", Provider: "codex", Metadata: map[string]any{"account_id": "same-subscription"}}
	q = burnTestQuota(now.Add(2*time.Minute), 0, 0, now.Add(time.Hour), now.Add(7*24*time.Hour+time.Minute))
	b.Observe(replacement, q, now.Add(2*time.Minute))
	if len(b.accounts) != 1 || len(b.history) != 1 {
		t.Fatalf("re-auth duplicated subscription: accounts=%d weeks=%d", len(b.accounts), len(b.history))
	}
	if w := b.account(replacement.ID).Windows["weekly"]; w.PercentPerToken != .005 || w.CalibrationSamples != 3 {
		t.Fatal("reset discarded calibration")
	}
	if *b.Snapshot().TotalWaste != 20 {
		t.Fatal("weekly ledger lost pre-reset remaining percent")
	}
}

func TestBurnWeeklyProjectionRespectsFiveHourCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := b.account("capacity")
	a.Windows["weekly"] = &BurnWindow{Used: 20, Reset: now.Add(time.Hour), Observed: now, PercentPerToken: .01, CalibrationSamples: 2}
	a.Windows["five_hour"] = &BurnWindow{Used: 90, Reset: now.Add(2 * time.Hour), Observed: now, PercentPerToken: .1, CalibrationSamples: 2}
	if got := b.projectedTokens(a, "weekly", 100, now); got != 100 {
		t.Fatalf("forecast ignores five-hour bottleneck: %v", got)
	}
	a.Windows["five_hour"].Reset = now.Add(30 * time.Minute)
	if got := b.projectedTokens(a, "weekly", 100, now); got != 1100 {
		t.Fatalf("forecast does not include refill: %v", got)
	}
}
func TestBurnCompletionAfterResetAttributedToOldWeek(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	a := &Auth{ID: "late", Provider: "codex"}
	q := burnTestQuota(now.Add(-2*time.Minute), 0, 25, now.Add(time.Hour), now.Add(-time.Minute))
	b.account(a.ID).Windows["weekly"] = &BurnWindow{Used: 25, Reset: now.Add(-time.Minute), Observed: now.Add(-2 * time.Minute)}
	b.HandleUsage(context.Background(), usage.Record{Provider: "codex", AuthID: a.ID, RequestedAt: now.Add(-2 * time.Minute), Latency: 2 * time.Minute, Detail: usage.Detail{OutputTokens: 10}, ResponseHeaders: http.Header{}})
	_ = q
	if b.account(a.ID).ActiveWeek || len(b.history) != 1 || !b.history[0].Active {
		t.Fatal("late completion was assigned to wrong week")
	}
}

func TestBurnCapPreflightFailsClosedEvenWithFreshSnapshot(t *testing.T) {
	b := NewBurnController()
	now := time.Now()
	b.now = func() time.Time { return now }
	a := &Auth{ID: "shared-preflight", Provider: "codex", Quota: burnTestQuota(now, 20, 20, now.Add(time.Hour), now.Add(24*time.Hour))}
	a.Metadata = map[string]any{"weekly_cap_percent": 50, "five_hour_cap_percent": 50}
	b.Observe(a, a.Quota, now)
	if !b.CapPermits(a, now) {
		t.Fatal("fresh under-cap snapshot should initially permit")
	}
	if b.RefreshCaps(context.Background(), a) {
		t.Fatal("missing live credentials must fail preflight")
	}
	if b.CapPermits(a, now) {
		t.Fatal("failed preflight must invalidate the older snapshot")
	}
	now = now.Add(time.Second)
	a.Quota.ObservedAt = now
	b.Observe(a, a.Quota, now)
	if !b.CapPermits(a, now) {
		t.Fatal("new authoritative observation should recover eligibility")
	}
}
func TestBurnUncappedPreflightNeedsNoCredentials(t *testing.T) {
	b := NewBurnController()
	if !b.RefreshCaps(context.Background(), &Auth{ID: "solo"}) {
		t.Fatal("uncapped accounts need no preflight")
	}
}

func TestBurnWebSocketRebindsUnavailableAccount(t *testing.T) {
	b := NewBurnController()
	a := &Auth{ID: "retired", Provider: "codex", Disabled: true}
	if !b.WantsSwitch(a, []*Auth{a}, time.Now(), "gpt-5.5") {
		t.Fatal("retired websocket account must rebind")
	}
}

func TestBurnTeamSeatsHaveIndependentQuotaIdentities(t *testing.T) {
	a := &Auth{ID: "seat-a", Provider: "codex", Metadata: map[string]any{"account_id": "workspace", "plan_type": "team", "quota_user_id": "user-a"}}
	b := &Auth{ID: "seat-b", Provider: "codex", Metadata: map[string]any{"account_id": "workspace", "plan_type": "team", "quota_user_id": "user-b"}}
	if codexQuotaIdentity(a) == codexQuotaIdentity(b) {
		t.Fatal("different Team seats collided")
	}
	duplicate := a.Clone()
	duplicate.ID = "refreshed"
	if codexQuotaIdentity(a) != codexQuotaIdentity(duplicate) {
		t.Fatal("credential rotation changed seat identity")
	}
	claims := map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user-a"}}
	raw, _ := json.Marshal(claims)
	token := a.Clone()
	delete(token.Metadata, "quota_user_id")
	token.Metadata["id_token"] = "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
	if codexQuotaIdentity(a) != codexQuotaIdentity(token) {
		t.Fatal("JWT seat identity differs from staged identity")
	}
}
