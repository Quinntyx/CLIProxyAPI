package auth

import (
	"math"
	"strings"
)

// RemainingQuotaWindow reports usable quota in percentage points of a full
// subscription, not as a percentage of an operator cap. Nil means unknown.
type RemainingQuotaWindow struct {
	TotalPercent    *float64 `json:"total_percent"`
	CurrentPercent  *float64 `json:"current_percent"`
	Accounts        int      `json:"accounts"`
	UnknownAccounts int      `json:"unknown_accounts"`
}

type RemainingQuotaStatus struct {
	FiveHour       RemainingQuotaWindow `json:"five_hour"`
	Weekly         RemainingQuotaWindow `json:"weekly"`
	CurrentBinding string               `json:"current_binding"`
}

// RemainingQuota is read-only: it never selects an account, changes policies,
// refreshes affinity TTLs, or advances weekly accounting. Only enabled Codex
// subscriptions are counted; duplicate credentials for one seat count once.
func (b *BurnController) RemainingQuota(auths []*Auth, current *Auth) RemainingQuotaStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	type subscription struct {
		caps    map[string]float64
		account *BurnAccount
	}
	accounts := make(map[string]*subscription)
	currentKey := ""
	for _, a := range auths {
		if a == nil || !strings.EqualFold(a.Provider, "codex") || a.Disabled || a.Status == StatusDisabled {
			continue
		}
		key := codexQuotaIdentity(a)
		if key == "" {
			key = b.key(a.ID)
		}
		entry := accounts[key]
		if entry == nil {
			entry = &subscription{caps: map[string]float64{"five_hour": 100, "weekly": 100}, account: b.accounts[key]}
			accounts[key] = entry
		}
		for _, policy := range []struct{ kind, metadata string }{{"five_hour", "five_hour_cap_percent"}, {"weekly", "weekly_cap_percent"}} {
			cap, _ := capValue(a, policy.metadata)
			// Most restrictive live policy wins if duplicate credentials differ.
			entry.caps[policy.kind] = math.Min(entry.caps[policy.kind], cap)
		}
		if current != nil && current.ID == a.ID {
			currentKey = key
		}
	}
	window := func(kind string) RemainingQuotaWindow {
		result := RemainingQuotaWindow{Accounts: len(accounts)}
		total := 0.0
		for key, entry := range accounts {
			var value *float64
			cap := entry.caps[kind]
			// A zero cap is known to offer no quota even without an observation.
			if cap == 0 {
				zero := 0.0
				value = &zero
			} else if entry.account != nil {
				w := entry.account.Windows[kind]
				if w != nil && !w.Closed && !w.Observed.IsZero() && w.Reset.After(now) && now.Sub(w.Observed) <= b.maxAge() && !math.IsNaN(w.Used) && !math.IsInf(w.Used, 0) && w.Used >= 0 && w.Used <= 100 {
					remaining := math.Max(0, cap-w.Used)
					value = &remaining
				}
			}
			if value == nil {
				result.UnknownAccounts++
			} else {
				total += *value
			}
			if key == currentKey {
				result.CurrentPercent = value
			}
		}
		if result.UnknownAccounts == 0 {
			result.TotalPercent = &total
		}
		return result
	}
	return RemainingQuotaStatus{FiveHour: window("five_hour"), Weekly: window("weekly")}
}

// Preserve read-only affinity inspection through the burn-routing wrapper. In
// particular, inspecting a footer must not call Pick or activate a burn override.
func (s *BurnDeadlineSelector) LookupAffinity(provider, model, sessionID string, filters ...func(string) bool) (string, string) {
	if s == nil || s.Fallback == nil {
		return "", "unsupported"
	}
	if observer, ok := s.Fallback.(interface {
		LookupAffinity(string, string, string, ...func(string) bool) (string, string)
	}); ok {
		return observer.LookupAffinity(provider, model, sessionID, filters...)
	}
	if observer, ok := s.Fallback.(interface {
		LookupAffinity(string, string, string) (string, string)
	}); ok {
		return observer.LookupAffinity(provider, model, sessionID)
	}
	return "", "unsupported"
}
