package auth

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

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
	model := "gpt-6.1-sol"
	session := "footer-session"
	got, status := manager.LookupSessionAffinity("codex", model, session)
	if got != nil || status != "unbound" {
		t.Fatal("inspection created binding", status)
	}
	_, err := wrapper.Pick(context.Background(), "codex", model, exec.Options{Headers: http.Header{"Session-Id": []string{session}}}, []*Auth{a})
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
}
