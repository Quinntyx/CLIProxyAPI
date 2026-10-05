package auth

import (
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// RemainingQuotaWindow reports unspent quota in percentage points of a full
// subscription, not as a percentage of an operator cap. AvailablePercent is
// the subset usable by currently routable accounts. Nil means unknown.
type RemainingQuotaWindow struct {
	TotalPercent     *float64 `json:"total_percent"`
	AvailablePercent *float64 `json:"available_percent,omitempty"`
	CurrentPercent   *float64 `json:"current_percent"`
	Accounts         int      `json:"accounts"`
	UnknownAccounts  int      `json:"unknown_accounts"`
}

type RemainingQuotaStatus struct {
	FiveHour          RemainingQuotaWindow `json:"five_hour"`
	Weekly            RemainingQuotaWindow `json:"weekly"`
	CurrentBinding    string               `json:"current_binding"`
	RoutingAvailable  *bool                `json:"routing_available,omitempty"`
	CurrentAvailable  *bool                `json:"current_available,omitempty"`
	AvailableAccounts int                  `json:"available_accounts"`
}

// RemainingQuota is read-only: it never selects an account, changes policies,
// refreshes affinity TTLs, or advances weekly accounting. Only enabled Codex
// subscriptions are counted; duplicate credentials for one seat count once.
func (b *BurnController) RemainingQuota(auths []*Auth, current *Auth) RemainingQuotaStatus {
	return b.remainingQuota(auths, current, nil)
}

// RemainingQuotaStatus distinguishes unused budget from the accounts currently
// routable for a requested model. It never selects an account or calls Pick.
func (m *Manager) RemainingQuotaStatus(model, session string) RemainingQuotaStatus {
	auths := m.List()
	var current *Auth
	binding := "unbound"
	if model != "" && session != "" {
		current, binding = m.LookupSessionAffinity("codex", model, session)
	}
	var eligible map[string]bool
	if model != "" {
		eligible = make(map[string]bool)
		now := time.Now()
		models := registry.GetGlobalRegistry()
		for _, a := range auths {
			if a == nil || a.Provider != "codex" || !m.authSupportsRouteModel(models, a, model) {
				continue
			}
			checkModel := m.selectionModelKeyForAuth(a, model)
			blocked, _, _ := isAuthBlockedForModelState(a, checkModel, now)
			eligible[a.ID] = !blocked
		}
	}
	result := DefaultBurnController.remainingQuota(auths, current, eligible)
	result.CurrentBinding = binding
	return result
}

func (b *BurnController) remainingQuota(auths []*Auth, current *Auth, eligible map[string]bool) RemainingQuotaStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	type subscription struct {
		caps    map[string]float64
		account *BurnAccount
		ready   bool
		capped  map[string]bool
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
			entry = &subscription{caps: map[string]float64{"five_hour": 100, "weekly": 100}, account: b.accounts[key], capped: map[string]bool{}}
			accounts[key] = entry
		}
		for _, policy := range []struct{ kind, metadata string }{{"five_hour", "five_hour_cap_percent"}, {"weekly", "weekly_cap_percent"}} {
			cap, present := capValue(a, policy.metadata)
			entry.capped[policy.kind] = entry.capped[policy.kind] || present
			// Most restrictive live policy wins if duplicate credentials differ.
			entry.caps[policy.kind] = math.Min(entry.caps[policy.kind], cap)
		}
		entry.ready = entry.ready || eligible[a.ID]
		if current != nil && current.ID == a.ID {
			currentKey = key
		}
	}
	valid := func(w *BurnWindow) bool {
		return w != nil && !w.Closed && !w.Observed.IsZero() && !w.Observed.After(now.Add(time.Minute)) && w.Reset.After(now) && now.Sub(w.Observed) <= b.maxAge() && !math.IsNaN(w.Used) && !math.IsInf(w.Used, 0) && w.Used >= 0 && w.Used <= 100
	}
	for key, entry := range accounts {
		for kind, cap := range entry.caps {
			var w *BurnWindow
			if entry.account != nil {
				w = entry.account.Windows[kind]
			}
			if cap == 0 || (valid(w) && w.Used >= cap) {
				entry.ready = false
			}
			// Capped credentials fail closed on missing/stale observations or poll
			// failures, exactly as routing does. Uncapped unknown quota is not zero.
			if entry.capped[kind] && (!valid(w) || (!b.capFailures[key].IsZero() && !w.Observed.After(b.capFailures[key]))) {
				entry.ready = false
			}
		}
	}
	window := func(kind string) RemainingQuotaWindow {
		result := RemainingQuotaWindow{Accounts: len(accounts)}
		total, available := 0.0, 0.0
		unknownAvailable := false
		for key, entry := range accounts {
			var value *float64
			cap := entry.caps[kind]
			// A zero cap is known to offer no quota even without an observation.
			if cap == 0 {
				zero := 0.0
				value = &zero
			} else if entry.account != nil {
				w := entry.account.Windows[kind]
				if valid(w) {
					remaining := math.Max(0, cap-w.Used)
					value = &remaining
				}
			}
			if value == nil {
				result.UnknownAccounts++
			} else {
				total += *value
			}
			if entry.ready {
				if value == nil {
					unknownAvailable = true
				} else {
					available += *value
				}
			}
			if key == currentKey {
				result.CurrentPercent = value
			}
		}
		if result.UnknownAccounts == 0 {
			result.TotalPercent = &total
		}
		if eligible != nil && !unknownAvailable {
			result.AvailablePercent = &available
		}
		return result
	}
	result := RemainingQuotaStatus{FiveHour: window("five_hour"), Weekly: window("weekly")}
	if eligible != nil {
		for key, entry := range accounts {
			if entry.ready {
				result.AvailableAccounts++
			}
			if key == currentKey {
				ready := entry.ready
				result.CurrentAvailable = &ready
			}
		}
		ready := result.AvailableAccounts > 0
		result.RoutingAvailable = &ready
	}
	return result
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
