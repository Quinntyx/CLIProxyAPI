package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// ReconcilePolledQuota restores quota-blocked routing after an authoritative
// refill. It does not clear authentication errors, unrelated rate limits, caps,
// disabled credentials, newer failures, or model restrictions.
func (m *Manager) ReconcilePolledQuota(ctx context.Context, polled *Auth, q QuotaState) {
	if m == nil || polled == nil || polled.Provider != "codex" {
		return
	}
	now := time.Now()
	h := map[string]string{}
	for k, v := range q.Signals {
		h[strings.ToLower(k)] = v
	}
	if q.ObservedAt.IsZero() || q.ObservedAt.Before(now.Add(-time.Minute)) || q.ObservedAt.After(now.Add(time.Minute)) || h["x-codex-allowed"] == "false" || h["x-codex-limit-reached"] == "true" {
		return
	}
	_, _, err := m.resetQuotaGuarded(ctx, polled.ID, func(current *Auth) bool {
		if current.Disabled || current.Status == StatusDisabled || current.Generation != polled.Generation || current.RegistrationEpoch != polled.RegistrationEpoch || hasUnauthorizedAuthFailure(current) {
			return false
		}
		quotaError := func(e *Error) bool {
			return e != nil && e.HTTPStatus == 429 && (strings.Contains(e.Message, "usage_limit_reached") || e.Code == "usage_limit_reached")
		}
		hasQuotaFailure := quotaError(current.LastError)
		if current.LastError != nil && !quotaError(current.LastError) {
			return false
		}
		for _, s := range current.ModelStates {
			if s == nil {
				continue
			}
			if s.Status == StatusDisabled || (s.LastError != nil && !quotaError(s.LastError)) {
				return false
			}
			if (s.Unavailable || s.Quota.Exceeded) && !quotaError(s.LastError) {
				return false
			}
			hasQuotaFailure = hasQuotaFailure || quotaError(s.LastError)
		}
		if !hasQuotaFailure {
			return false
		}
		// Both windows must be explicitly observed, below their operator cap, and
		// valid. A genuine refill must have advanced the failed window's deadline.
		refill := false
		seen := map[int]bool{}
		oldHeaders := map[string]string{}
		for k, v := range current.Quota.Signals {
			oldHeaders[strings.ToLower(k)] = v
		}
		for _, prefix := range []string{"x-codex-primary-", "x-codex-secondary-"} {
			p := struct{ prefix string }{prefix}
			minutes, e := strconv.Atoi(h[p.prefix+"window-minutes"])
			if e != nil {
				return false
			}
			if seen[minutes] {
				return false
			}
			seen[minutes] = true
			capKey := ""
			switch minutes {
			case 300:
				capKey = "five_hour_cap_percent"
			case 10080:
				capKey = "weekly_cap_percent"
			default:
				return false
			}
			used, e := strconv.ParseFloat(h[p.prefix+"used-percent"], 64)
			if e != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
				return false
			}
			cap, _ := capValue(current, capKey)
			if used >= cap {
				return false
			}
			reset, e := strconv.ParseInt(h[p.prefix+"reset-at"], 10, 64)
			if e != nil || !time.Unix(reset, 0).After(now) {
				return false
			}
			deadline := time.Unix(reset, 0)
			previousReset, parseErr := strconv.ParseInt(oldHeaders[p.prefix+"reset-at"], 10, 64)
			if parseErr != nil || previousReset <= 0 {
				continue
			}
			confirmed := func(recoverAt time.Time) bool {
				return !recoverAt.IsZero() && math.Abs(float64(recoverAt.Unix()-previousReset)) <= 2 && deadline.After(time.Unix(previousReset, 0).Add(2*time.Second))
			}
			if confirmed(current.Quota.NextRecoverAt) {
				refill = true
			}
			for _, s := range current.ModelStates {
				if s != nil && quotaError(s.LastError) && confirmed(s.Quota.NextRecoverAt) {
					refill = true
				}
			}
		}
		if !refill || !seen[300] || !seen[10080] {
			return false
		}
		current.Quota.Signals = q.Clone().Signals
		current.Quota.ObservedAt = q.ObservedAt
		return true
	})
	if err != nil {
		log.WithError(err).Warn("failed to persist confirmed quota refill")
	}
}
