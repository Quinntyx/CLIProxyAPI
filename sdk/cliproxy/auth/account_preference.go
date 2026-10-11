package auth

import "strings"

func codexAccountPlanType(a *Auth) string {
	if a == nil || !strings.EqualFold(a.Provider, "codex") {
		return ""
	}
	plan, _ := a.Metadata["plan_type"].(string)
	if strings.TrimSpace(plan) == "" {
		plan = a.Attributes["plan_type"]
	}
	return strings.ToLower(strings.TrimSpace(plan))
}

// codexIdlePreference preserves the uncapped Business subscription for interactive
// use outside the proxy. BurnDeadlineSelector applies this only when it has no
// eligible burn target, after quota, cooldown and explicit-priority filtering.
// Unknown plans retain the baseline tier rather than guessing their identity.
func codexIdlePreference(a *Auth) int {
	switch codexAccountPlanType(a) {
	case "team", "business":
		fiveHour, _ := capValue(a, "five_hour_cap_percent")
		weekly, _ := capValue(a, "weekly_cap_percent")
		if fiveHour == 50 || weekly == 50 {
			return 1
		}
		if fiveHour == 100 && weekly == 100 {
			return 2
		}
	}
	return 0
}

func preferIdleCodexAccounts(auths []*Auth) []*Auth {
	if len(auths) < 2 {
		return auths
	}
	best := codexIdlePreference(auths[0])
	for _, a := range auths[1:] {
		best = min(best, codexIdlePreference(a))
	}
	preferred := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if codexIdlePreference(a) == best {
			preferred = append(preferred, a)
		}
	}
	return preferred
}
