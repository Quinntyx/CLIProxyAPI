package auth

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaTestAuth(id string, caps bool) *Auth {
	a := &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"account_id": id, "plan_type": "plus"}}
	if caps {
		a.Metadata["weekly_cap_percent"] = 50
		a.Metadata["five_hour_cap_percent"] = 50
	}
	return a
}
func quotaAssert(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > .001 {
		t.Fatalf("percentage=%v want=%v", got, want)
	}
}
func TestRemainingQuotaCapAdjustedTotalsAndCurrent(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	shared, personal, third := quotaTestAuth("shared", true), quotaTestAuth("personal", false), quotaTestAuth("third", false)
	for _, a := range []*Auth{shared, personal, third} {
		b.Observe(a, burnTestQuota(now, 0, 0, now.Add(time.Hour), now.Add(24*time.Hour)), now)
	}
	full := b.RemainingQuota([]*Auth{shared, personal, third}, nil)
	quotaAssert(t, full.FiveHour.TotalPercent, 250)
	quotaAssert(t, full.Weekly.TotalPercent, 250)
	if full.FiveHour.CurrentPercent != nil {
		t.Fatal("unbound account guessed")
	}
	b.Observe(personal, burnTestQuota(now.Add(time.Second), 25, 72, now.Add(time.Hour), now.Add(24*time.Hour)), now.Add(time.Second))
	b.Observe(shared, burnTestQuota(now.Add(time.Second), 0, 13, now.Add(time.Hour), now.Add(24*time.Hour)), now.Add(time.Second))
	before, _ := json.Marshal(burnDisk{Accounts: b.accounts, Aliases: b.aliases, Weeks: b.history})
	result := b.RemainingQuota([]*Auth{shared, personal, third}, personal)
	quotaAssert(t, result.FiveHour.TotalPercent, 225)
	quotaAssert(t, result.FiveHour.CurrentPercent, 75)
	quotaAssert(t, result.Weekly.TotalPercent, 165)
	quotaAssert(t, result.Weekly.CurrentPercent, 28)
	if result.FiveHour.Accounts != 3 || result.FiveHour.UnknownAccounts != 0 {
		t.Fatal(result)
	}
	after, _ := json.Marshal(burnDisk{Accounts: b.accounts, Aliases: b.aliases, Weeks: b.history})
	if string(before) != string(after) {
		t.Fatal("read-only quota inspection mutated accounting")
	}
	b.Observe(shared, burnTestQuota(now.Add(2*time.Second), 60, 55, now.Add(time.Hour), now.Add(24*time.Hour)), now.Add(2*time.Second))
	blocked := b.RemainingQuota([]*Auth{shared, personal, third}, shared)
	quotaAssert(t, blocked.FiveHour.CurrentPercent, 0)
	quotaAssert(t, blocked.Weekly.CurrentPercent, 0)
}
func TestRemainingQuotaDeduplicatesSeatsAndIgnoresDisabled(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	first := quotaTestAuth("one", true)
	first.Metadata["account_id"] = "workspace"
	first.Metadata["plan_type"] = "team"
	first.Metadata["quota_user_id"] = "seat-one"
	second := quotaTestAuth("two", false)
	second.Metadata["account_id"] = "workspace"
	second.Metadata["plan_type"] = "team"
	second.Metadata["quota_user_id"] = "seat-two"
	duplicate := first.Clone()
	duplicate.ID = "dup"
	delete(duplicate.Metadata, "weekly_cap_percent")
	delete(duplicate.Metadata, "five_hour_cap_percent")
	disabled := quotaTestAuth("disabled", false)
	disabled.Disabled = true
	other := quotaTestAuth("other", false)
	other.Provider = "claude"
	for _, a := range []*Auth{first, second, disabled} {
		b.Observe(a, burnTestQuota(now, 10, 20, now.Add(time.Hour), now.Add(24*time.Hour)), now)
	}
	result := b.RemainingQuota([]*Auth{first, duplicate, second, disabled, other, nil}, duplicate)
	quotaAssert(t, result.FiveHour.TotalPercent, 130)
	quotaAssert(t, result.Weekly.TotalPercent, 110)
	quotaAssert(t, result.Weekly.CurrentPercent, 30)
	if result.Weekly.Accounts != 2 {
		t.Fatal("duplicate or disabled credential counted", result)
	}
}
func TestRemainingQuotaUnknownStaleResetAndZeroCaps(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	a := quotaTestAuth("one", false)
	if b.RemainingQuota([]*Auth{a}, a).Weekly.TotalPercent != nil {
		t.Fatal("missing observation guessed")
	}
	b.Observe(a, burnTestQuota(now, 10, 20, now.Add(time.Hour), now.Add(24*time.Hour)), now)
	for _, kind := range []string{"five_hour", "weekly"} {
		w := b.accounts[codexQuotaIdentity(a)].Windows[kind]
		fresh := *w
		for _, mutate := range []func(*BurnWindow){func(w *BurnWindow) { w.Observed = now.Add(-b.maxAge() - time.Second) }, func(w *BurnWindow) { w.Reset = now }, func(w *BurnWindow) { w.Closed = true }, func(w *BurnWindow) { w.Used = math.NaN() }} {
			mutate(w)
			result := b.RemainingQuota([]*Auth{a}, a)
			window := result.Weekly
			if kind == "five_hour" {
				window = result.FiveHour
			}
			if window.TotalPercent != nil || window.CurrentPercent != nil || window.UnknownAccounts != 1 {
				t.Fatal("stale/invalid observation displayed", window)
			}
			*w = fresh
		}
	}
	a.Metadata["weekly_cap_percent"] = 0
	result := b.RemainingQuota([]*Auth{a}, a)
	quotaAssert(t, result.Weekly.TotalPercent, 0)
	a.Metadata["weekly_cap_percent"] = "invalid"
	quotaAssert(t, b.RemainingQuota([]*Auth{a}, a).Weekly.TotalPercent, 0)
	empty := b.RemainingQuota(nil, nil)
	quotaAssert(t, empty.Weekly.TotalPercent, 0)
}
func TestBurnWrapperPreservesActualAffinityInspection(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	a := quotaTestAuth("account", false)
	b.Observe(a, burnTestQuota(now, 0, 0, now.Add(time.Hour), now.Add(24*time.Hour)), now)
	affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
	wrapper := &BurnDeadlineSelector{Fallback: affinity, Controller: b}
	manager := NewManager(nil, wrapper, nil)
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	// Production attaches a plugin host even with no active scheduler.
	inactive := &inactivePluginScheduler{}
	manager.SetPluginScheduler(inactive)
	model := "gpt-6.1-sol"
	session := "footer-session"
	got, status := manager.LookupSessionAffinity("codex", model, session)
	if got != nil || status != "unbound" {
		t.Fatal("inspection created binding", status)
	}
	_, err := wrapper.Pick(context.Background(), "mixed", model, exec.Options{Headers: http.Header{"Session-Id": []string{session}}}, []*Auth{a})
	if err != nil {
		t.Fatal(err)
	}
	got, status = manager.LookupSessionAffinity("codex", model, session)
	if got == nil || got.ID != a.ID || status != "bound" {
		t.Fatal("burn wrapper lost binding", status)
	}
	got, status = manager.LookupSessionAffinity("codex", "another-model", session)
	if got != nil || status != "unbound" {
		t.Fatal("model-specific binding leaked", status)
	}
	if inactive.calls != 0 {
		t.Fatal("read-only inspection invoked a scheduler")
	}
	manager.SetPluginScheduler(&fakePluginScheduler{})
	if got, status = manager.LookupSessionAffinity("codex", model, session); got != nil || status != "unsupported" {
		t.Fatal("active custom scheduler was treated as built-in affinity", status)
	}
}

func TestRemainingQuotaReportsStrandedBudgetAndRoutingSeparately(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	shared, exhausted, cooldown := quotaTestAuth("shared", true), quotaTestAuth("exhausted", false), quotaTestAuth("cooldown", false)
	for _, tc := range []struct {
		a    *Auth
		h, w float64
	}{{shared, 4, 50}, {exhausted, 100, 71}, {cooldown, 0, 72}} {
		b.Observe(tc.a, burnTestQuota(now, tc.h, tc.w, now.Add(time.Hour), now.Add(24*time.Hour)), now)
	}
	auths := []*Auth{shared, exhausted, cooldown}
	before, _ := json.Marshal(burnDisk{Accounts: b.accounts, Aliases: b.aliases, Weeks: b.history})
	// Shared is hard-capped by its weekly window. Another account is on a
	// five-hour cooldown, even if passive quota observations still show headroom.
	result := b.remainingQuota(auths, cooldown, map[string]bool{shared.ID: true, exhausted.ID: true, cooldown.ID: false})
	quotaAssert(t, result.FiveHour.TotalPercent, 146)
	quotaAssert(t, result.Weekly.TotalPercent, 57)
	quotaAssert(t, result.FiveHour.CurrentPercent, 100)
	if result.RoutingAvailable == nil || *result.RoutingAvailable || result.CurrentAvailable == nil || *result.CurrentAvailable || result.AvailableAccounts != 0 {
		t.Fatal("stranded budgets reported as routable", result)
	}
	quotaAssert(t, result.FiveHour.AvailablePercent, 0)
	quotaAssert(t, result.Weekly.AvailablePercent, 0)
	// Recovery must update availability without losing the unspent budgets.
	ready := b.remainingQuota(auths, cooldown, map[string]bool{shared.ID: true, exhausted.ID: true, cooldown.ID: true})
	if ready.RoutingAvailable == nil || !*ready.RoutingAvailable || ready.AvailableAccounts != 1 {
		t.Fatal("recovered route still blocked", ready)
	}
	quotaAssert(t, ready.FiveHour.AvailablePercent, 100)
	quotaAssert(t, ready.Weekly.AvailablePercent, 28)
	after, _ := json.Marshal(burnDisk{Accounts: b.accounts, Aliases: b.aliases, Weeks: b.history})
	if string(before) != string(after) {
		t.Fatal("telemetry changed weekly accounting")
	}
}

func TestRemainingQuotaRoutingRequiresFreshCapsAndDeduplicates(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	a := quotaTestAuth("shared", true)
	b.Observe(a, burnTestQuota(now, 10, 20, now.Add(time.Hour), now.Add(24*time.Hour)), now)
	duplicate := a.Clone()
	duplicate.ID = "duplicate"
	delete(duplicate.Metadata, "five_hour_cap_percent")
	delete(duplicate.Metadata, "weekly_cap_percent")
	auths := []*Auth{a, duplicate}
	eligible := map[string]bool{a.ID: true, duplicate.ID: true}
	got := b.remainingQuota(auths, duplicate, eligible)
	if got.AvailableAccounts != 1 {
		t.Fatal("duplicate seats double-counted", got)
	}
	quotaAssert(t, got.FiveHour.AvailablePercent, 40)
	w := b.accounts[codexQuotaIdentity(a)].Windows["weekly"]
	w.Observed = now.Add(-b.maxAge() - time.Second)
	stale := b.remainingQuota(auths, duplicate, eligible)
	if *stale.RoutingAvailable || stale.AvailableAccounts != 0 {
		t.Fatal("uncapped alias bypassed stale seat cap", stale)
	}
	quotaAssert(t, stale.FiveHour.AvailablePercent, 0)
	if stale.Weekly.TotalPercent != nil {
		t.Fatal("stale remaining quota guessed")
	}
	w.Observed = now
	b.capFailures = map[string]time.Time{codexQuotaIdentity(a): now.Add(time.Second)}
	failed := b.remainingQuota(auths, duplicate, eligible)
	if *failed.RoutingAvailable {
		t.Fatal("quota poll failure ignored")
	}
}

func TestManagerRemainingQuotaUsesLiveModelEligibilityAndActualBinding(t *testing.T) {
	now := time.Now()
	b := burnTestController(now)
	previous := DefaultBurnController
	DefaultBurnController = b
	t.Cleanup(func() { DefaultBurnController = previous })
	affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
	manager := NewManager(nil, &BurnDeadlineSelector{Fallback: affinity, Controller: b}, nil)
	manager.SetPluginScheduler(&inactivePluginScheduler{})
	a := quotaTestAuth("remaining-model-test", false)
	a.Quota = burnTestQuota(now, 25, 72, now.Add(time.Hour), now.Add(24*time.Hour))
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b.Observe(a, a.Quota, now)
	model := "gpt-6.1-sol"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	session := "pi-current-session"
	if _, err := affinity.Pick(context.Background(), "mixed", model, exec.Options{Headers: http.Header{"Session-Id": []string{session}}}, []*Auth{a}); err != nil {
		t.Fatal(err)
	}
	got := manager.RemainingQuotaStatus(model, session)
	if got.CurrentBinding != "bound" || got.RoutingAvailable == nil || !*got.RoutingAvailable {
		t.Fatal("production-shaped manager lost binding or routing", got)
	}
	quotaAssert(t, got.FiveHour.CurrentPercent, 75)
	quotaAssert(t, got.Weekly.CurrentPercent, 28)
	// Passive percent readings need not reflect the latest model-specific 429.
	a.ModelStates = map[string]*ModelState{model: {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}
	if _, err := manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	got = manager.RemainingQuotaStatus(model, session)
	if got.RoutingAvailable == nil || *got.RoutingAvailable {
		t.Fatal("model-specific cooldown ignored", got)
	}
	quotaAssert(t, got.FiveHour.TotalPercent, 75)
	quotaAssert(t, got.FiveHour.AvailablePercent, 0)
	// Other windows/models must not inherit the wrong model's eligibility.
	unsupported := manager.RemainingQuotaStatus("not-registered", session)
	if unsupported.RoutingAvailable == nil || *unsupported.RoutingAvailable {
		t.Fatal("unregistered model advertised as available", unsupported)
	}
	unscoped := manager.RemainingQuotaStatus("", "")
	if unscoped.RoutingAvailable != nil {
		t.Fatal("unscoped quota invented routing state", unscoped)
	}
}
