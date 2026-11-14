package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	resetPressureMaxKeys  = 4096
	resetPressureMaxBoost = 1000.0
)

// ResetPressureSelector distributes requests in proportion to the unused quota
// that must be spent before each five-hour or weekly reset. Unknown observations
// retain a baseline share, allowing passive upstream telemetry to populate them.
// Cooldowns, explicit credential priorities and websocket eligibility still apply.
// The zero value is ready to use.
type ResetPressureSelector struct {
	mu       sync.Mutex
	currents map[string]map[string]float64
	now      func() time.Time // Optional fixed clock for tests; set before first use.
}

func (s *ResetPressureSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	weights := make(map[string]float64, len(available))
	total := 0.0
	var earliest time.Time
	for _, candidate := range available {
		weight, blockedUntil := resetPressureWeight(candidate, provider, now)
		if weight > 0 {
			weights[candidate.ID] = weight
			total += weight
		} else if !blockedUntil.IsZero() && (earliest.IsZero() || blockedUntil.Before(earliest)) {
			earliest = blockedUntil
		}
	}
	if total == 0 {
		if !earliest.IsZero() {
			if provider == "mixed" {
				provider = ""
			}
			return nil, newModelCooldownError(model, provider, earliest.Sub(now))
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available with remaining quota"}
	}

	key := provider + ":" + canonicalModelKey(weightedSelectorStateModel(ctx, model))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currents == nil {
		s.currents = make(map[string]map[string]float64)
	}
	current := s.currents[key]
	if current == nil {
		if len(s.currents) >= resetPressureMaxKeys {
			s.currents = make(map[string]map[string]float64)
		}
		current = make(map[string]float64)
		s.currents[key] = current
	}
	// Preserve credits through changing pressure and transient retry exclusions.
	// Resetting credits whenever a weight changes would starve weaker accounts.
	if len(current) > maxSmoothWeightedStateEntries {
		for id := range current {
			if _, present := weights[id]; !present {
				delete(current, id)
			}
		}
	}
	var picked *Auth
	for _, candidate := range available {
		weight := weights[candidate.ID]
		if weight <= 0 {
			continue
		}
		current[candidate.ID] += weight / total
		if picked == nil || current[candidate.ID] > current[picked.ID] {
			picked = candidate
		}
	}
	current[picked.ID] -= 1
	return picked, nil
}

// resetPressureWeight returns a neutral weight for unavailable telemetry. A known
// exhausted window blocks this credential until that window resets, irrespective
// of pressure in another window. Passive observations never mutate cooldown state.
func resetPressureWeight(auth *Auth, provider string, now time.Time) (float64, time.Time) {
	if auth == nil {
		return 0, time.Time{}
	}
	windows, ok := observedQuotaWindows(auth, provider, now)
	if !ok {
		return 1, time.Time{}
	}
	pressure := 0.0
	var blockedUntil time.Time
	for _, window := range windows {
		if window.used == 1 {
			if window.reset.After(blockedUntil) {
				blockedUntil = window.reset
			}
			continue
		}
		// Normalize by window length so weekly pressure is not dwarfed by 5h.
		// A one-minute floor and bounded boost avoid singularities near reset.
		remaining := math.Max(window.reset.Sub(now).Seconds(), 60)
		windowPressure := (1 - window.used) * window.period.Seconds() / remaining
		pressure = math.Max(pressure, windowPressure)
	}
	if !blockedUntil.IsZero() {
		return 0, blockedUntil
	}
	return 1 + math.Min(pressure, resetPressureMaxBoost), time.Time{}
}

// quotaWindowObservation is one valid five-hour or weekly observation.
type quotaWindowObservation struct {
	period time.Duration
	used   float64 // Consumed fraction in [0, 1].
	reset  time.Time
}

// observedQuotaWindows parses the passive Codex/Claude quota snapshot. ok is
// false when the provider is unsupported or no usable snapshot exists. Invalid,
// expired or implausible windows are omitted rather than guessed.
func observedQuotaWindows(auth *Auth, provider string, now time.Time) ([]quotaWindowObservation, bool) {
	if auth == nil {
		return nil, false
	}
	if auth.Provider != "" {
		provider = auth.Provider
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "codex" && provider != "claude" {
		return nil, false
	}
	q := auth.Quota
	if len(q.Signals) == 0 || q.ObservedAt.After(now.Add(time.Minute)) {
		return nil, false
	}
	headers := make(map[string]string, len(q.Signals))
	for name, value := range q.Signals {
		headers[strings.ToLower(name)] = strings.TrimSpace(value)
	}
	var windows []quotaWindowObservation
	for index, period := range []time.Duration{5 * time.Hour, 7 * 24 * time.Hour} {
		var prefix, usedKey string
		scale := 100.0
		switch provider {
		case "codex":
			prefix = "x-codex-primary-"
			if index == 1 {
				prefix = "x-codex-secondary-"
			}
			if minutes := headers[prefix+"window-minutes"]; minutes != "" {
				n, err := strconv.ParseFloat(minutes, 64)
				if err != nil || (n != 300 && n != 10080) {
					continue
				}
				period = time.Duration(n) * time.Minute
			}
			usedKey = prefix + "used-percent"
		case "claude":
			prefix = "anthropic-ratelimit-unified-5h-"
			if index == 1 {
				prefix = "anthropic-ratelimit-unified-7d-"
			}
			usedKey = prefix + "utilization"
			scale = 1
		}
		used, err := strconv.ParseFloat(headers[usedKey], 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > scale {
			continue
		}
		reset, ok := resetPressureReset(headers, prefix, q.ObservedAt, now)
		if !ok || reset.Sub(now) > period+time.Minute {
			continue
		}
		windows = append(windows, quotaWindowObservation{period: period, used: used / scale, reset: reset})
	}
	return windows, true
}

// ObservedQuotaWindow is a read-only view of a passively observed subscription window.
type ObservedQuotaWindow struct {
	// Period is the window length (five hours or seven days).
	Period time.Duration
	// UsedPercent is the consumed share in percentage points.
	UsedPercent float64
	// ResetsAt is when the window resets.
	ResetsAt time.Time
}

// ObservedQuotaWindows reports the valid passive quota windows of a Codex or
// Claude credential. supported is false for providers without observations.
// It never issues requests or mutates scheduling state.
func ObservedQuotaWindows(auth *Auth, now time.Time) (windows []ObservedQuotaWindow, supported bool) {
	if auth == nil || !ProviderSupportsQuotaWindows(auth.Provider) {
		return nil, false
	}
	observed, _ := observedQuotaWindows(auth, auth.Provider, now)
	for _, window := range observed {
		windows = append(windows, ObservedQuotaWindow{Period: window.period, UsedPercent: window.used * 100, ResetsAt: window.reset})
	}
	return windows, true
}

// ProviderSupportsQuotaWindows reports whether ObservedQuotaWindows can read the provider.
func ProviderSupportsQuotaWindows(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "claude":
		return true
	default:
		return false
	}
}

func resetPressureReset(headers map[string]string, prefix string, observedAt, now time.Time) (time.Time, bool) {
	// Absolute timestamps take precedence; an expired timestamp must not be
	// resurrected by a relative countdown captured on an older response.
	absolute := headers[prefix+"reset-at"]
	if absolute == "" {
		absolute = headers[prefix+"reset"] // Anthropic's Unix-second watermark.
	}
	if absolute != "" {
		if reset, err := time.Parse(time.RFC3339, absolute); err == nil {
			return reset, reset.After(now)
		}
		seconds, err := strconv.ParseFloat(absolute, 64)
		if err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds > 0 && seconds < 253402300800 {
			reset := time.Unix(int64(seconds), 0)
			return reset, reset.After(now)
		}
	}
	seconds, err := strconv.ParseFloat(headers[prefix+"reset-after-seconds"], 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > (365*24*time.Hour).Seconds() || observedAt.IsZero() || observedAt.After(now) {
		return time.Time{}, false
	}
	reset := observedAt.Add(time.Duration(seconds * float64(time.Second)))
	return reset, reset.After(now)
}
