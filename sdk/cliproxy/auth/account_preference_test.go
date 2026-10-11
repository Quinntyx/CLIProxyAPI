package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func idlePreferenceAccounts(now time.Time) []*Auth {
	personal := quotaTestAuth("z-personal", false)
	david := quotaTestAuth("m-business-david", true)
	david.Metadata["plan_type"] = "team"
	business := quotaTestAuth("a-business", false)
	business.Metadata["plan_type"] = "team"
	for _, a := range []*Auth{business, david, personal} {
		a.Quota = burnTestQuota(now, 0, 0, now.Add(time.Hour), now.Add(24*time.Hour))
	}
	return []*Auth{business, david, personal}
}

func TestIdlePreferencePersonalThenCappedBusinessThenUncapped(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	auths := idlePreferenceAccounts(now)
	selector := &BurnDeadlineSelector{Controller: b, Fallback: &RoundRobinSelector{}}
	pick := func(want string) {
		t.Helper()
		chosen, err := selector.Pick(context.Background(), "codex", "", exec.Options{}, auths)
		if err != nil || chosen == nil || chosen.ID != want {
			t.Fatalf("got %+v, %v; want %s", chosen, err, want)
		}
	}
	for range 5 {
		pick("z-personal")
	}
	auths[2].Disabled = true
	pick("m-business-david")
	// A depleted donor must not win just because its preference is higher.
	b.now = func() time.Time { return now.Add(time.Second) }
	auths[1].Quota = burnTestQuota(now.Add(time.Second), 0, 50, now.Add(time.Hour), now.Add(24*time.Hour))
	pick("a-business")
}

func TestIdlePreferenceOverridesLowerPreferenceStickyBinding(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	auths := idlePreferenceAccounts(now)
	sticky := NewSessionAffinitySelector(&RoundRobinSelector{})
	opts := exec.Options{Headers: http.Header{"Session-Id": []string{"idle-session"}}}
	if _, err := sticky.Pick(context.Background(), "codex", "", opts, auths[:1]); err != nil {
		t.Fatal(err)
	}
	selector := &BurnDeadlineSelector{Controller: b, Fallback: sticky}
	chosen, err := selector.Pick(context.Background(), "codex", "", opts, auths)
	if err != nil || chosen == nil || chosen.ID != "z-personal" {
		t.Fatalf("idle preference did not override phone account binding: %+v, %v", chosen, err)
	}
}

func TestBurnPressureOverridesIdlePreferenceAndRetainsLatch(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	auths := idlePreferenceAccounts(now)
	phone := auths[0]
	phone.Quota = burnTestQuota(now, 0, 50, now.Add(time.Hour), now.Add(time.Minute))
	for _, a := range auths {
		b.Observe(a, a.Quota, now)
	}
	b.lastTokenTPS = 10
	w := b.accounts[codexQuotaIdentity(phone)].Windows["weekly"]
	w.PercentPerToken, w.CalibrationSamples = .1, 2
	selector := &BurnDeadlineSelector{Controller: b, Fallback: NewSessionAffinitySelector(&RoundRobinSelector{})}
	opts := exec.Options{Headers: http.Header{"Session-Id": []string{"urgent-session"}}}
	for range 3 {
		chosen, err := selector.Pick(context.Background(), "codex", "", opts, auths)
		if err != nil || chosen == nil || chosen.ID != phone.ID {
			t.Fatalf("burn pressure lost to idle preference: %+v, %v", chosen, err)
		}
	}
	phone.Disabled = true
	chosen, err := selector.Pick(context.Background(), "codex", "", opts, auths)
	if err != nil || chosen == nil || chosen.ID != "z-personal" {
		t.Fatalf("inactive burn target did not restore idle preference: %+v, %v", chosen, err)
	}
}

func TestIdlePreferenceRespectsExplicitPriorityAndLeavesUnknownPlansUnchanged(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auths := idlePreferenceAccounts(now)
	auths[0].Attributes = map[string]string{"priority": "10"}
	selector := &BurnDeadlineSelector{Controller: burnTestController(now), Fallback: &RoundRobinSelector{}}
	chosen, err := selector.Pick(context.Background(), "codex", "", exec.Options{}, auths)
	if err != nil || chosen == nil || chosen.ID != auths[0].ID {
		t.Fatalf("explicit priority was overridden: %+v, %v", chosen, err)
	}
	unknown := []*Auth{{ID: "x", Provider: "codex"}, {ID: "y", Provider: "codex"}}
	if len(preferIdleCodexAccounts(unknown)) != 2 {
		t.Fatal("unknown accounts lost baseline selection")
	}
}

func TestRemainingQuotaDescribesOnlyActualBindingWithoutSecrets(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	auths := idlePreferenceAccounts(now)
	for _, a := range auths {
		a.Label = "private-email@example.test"
		a.Metadata["access_token"] = "private-access-token"
		b.Observe(a, a.Quota, now)
	}
	if result := b.RemainingQuota(auths, nil); result.CurrentAccount != nil {
		t.Fatal("unbound inspection guessed account", result.CurrentAccount)
	}
	for _, a := range auths {
		result := b.RemainingQuota(auths, a)
		account := result.CurrentAccount
		cap, _ := capValue(a, "weekly_cap_percent")
		if account == nil || account.PlanType != codexAccountPlanType(a) || account.WeeklyCapPercent != cap {
			t.Fatalf("wrong routed account descriptor: %+v", account)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{a.ID, a.Label, "private-access-token"} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("public quota response leaked identity or credentials")
			}
		}
	}
	auths[0].Disabled = true
	if b.RemainingQuota(auths, auths[0]).CurrentAccount != nil {
		t.Fatal("disabled account advertised as current")
	}
	duplicate := auths[1].Clone()
	duplicate.ID = "duplicate"
	delete(duplicate.Metadata, "weekly_cap_percent")
	delete(duplicate.Metadata, "five_hour_cap_percent")
	result := b.RemainingQuota(append(auths, duplicate), duplicate)
	if result.CurrentAccount == nil || result.CurrentAccount.WeeklyCapPercent != 50 {
		t.Fatal("duplicate credential hid effective seat cap", result.CurrentAccount)
	}
}
