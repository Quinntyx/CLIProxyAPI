package auth

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type sessionQuotaContextKey struct{}

func (m *Manager) SetSessionQuota(manager *sessionquota.Manager) {
	m.mu.Lock()
	m.sessionQuota = manager
	m.mu.Unlock()
}
func (m *Manager) SessionQuota() *sessionquota.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionQuota
}
func quotaScalar(signals map[string]string, name string) (float64, bool) {
	value, err := strconv.ParseFloat(signals[http.CanonicalHeaderKey(name)], 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func quotaReset(signals map[string]string, prefix string, observed time.Time) time.Time {
	if seconds, ok := quotaScalar(signals, prefix+"Reset-At"); ok && seconds > 0 {
		return time.Unix(int64(seconds), 0)
	}
	if seconds, ok := quotaScalar(signals, prefix+"Reset-After-Seconds"); ok && seconds >= 0 && !observed.IsZero() {
		return observed.Add(time.Duration(seconds) * time.Second)
	}
	return time.Time{}
}

func sessionQuotaAccountID(a *Auth) string {
	if a == nil {
		return ""
	}
	if key := codexQuotaIdentity(a); key != "" {
		return key
	}
	return a.ID
}

// SyncSessionQuota reuses both passive response watermarks and the existing
// subscription poller. Credential caps are absolute upstream percentage points,
// not a scale factor on the reported remaining percentage.
func (m *Manager) SyncSessionQuota() {
	q := m.SessionQuota()
	if q == nil {
		return
	}
	now := time.Now()
	byAccount := make(map[string]sessionquota.Observation)
	for _, a := range m.List() {
		if a == nil || a.Disabled || a.Status == StatusDisabled || canonicalSchedulingProvider(a.Provider) != "codex" || a.AuthKind() == AuthKindAPIKey {
			continue
		}
		fullCapacity := 100.0
		if value, err := strconv.ParseFloat(a.Attributes["session_quota_capacity"], 64); err == nil && value > 0 && value <= sessionquota.Capacity {
			fullCapacity = value
		}
		fiveCap, _ := capValue(a, "five_hour_cap_percent")
		weekCap, _ := capValue(a, "weekly_cap_percent")
		capacity := fullCapacity * fiveCap / 100
		o := sessionquota.Observation{ID: sessionQuotaAccountID(a), Capacity: capacity, CapacityConfigured: true, ObservedAt: a.Quota.ObservedAt}
		fiveObserved, weekObserved := a.Quota.ObservedAt, a.Quota.ObservedAt
		fiveUsed, weekUsed := 0.0, 0.0
		fiveKnown, weekKnown := false, false
		for _, window := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + window + "-"
			used, ok := quotaScalar(a.Quota.Signals, prefix+"Used-Percent")
			if !ok || used < 0 || used > 100 {
				continue
			}
			minutes, hasMinutes := quotaScalar(a.Quota.Signals, prefix+"Window-Minutes")
			if !hasMinutes {
				minutes = 300
				if window == "Secondary" {
					minutes = 10080
				}
			}
			if minutes == 300 {
				fiveUsed, fiveKnown = used, true
				o.ResetAt = quotaReset(a.Quota.Signals, prefix, a.Quota.ObservedAt)
			}
			if minutes >= 10080 {
				weekUsed, weekKnown = used, true
				o.WeeklyResetAt = quotaReset(a.Quota.Signals, prefix, a.Quota.ObservedAt)
			}
		}
		// The poller also observes accounts while their workflows are paused.
		b := DefaultBurnController
		if b != nil {
			b.mu.Lock()
			if account := b.accounts[b.key(a.ID)]; account != nil {
				for kind, w := range account.Windows {
					if w == nil || w.Closed || !w.Reset.After(now) || w.Observed.IsZero() || w.Observed.After(now.Add(time.Minute)) || now.Sub(w.Observed) > b.maxAge() || w.Used < 0 || w.Used > 100 {
						continue
					}
					if kind == "five_hour" && !w.Observed.Before(fiveObserved) {
						fiveUsed, fiveKnown, fiveObserved, o.ResetAt = w.Used, true, w.Observed, w.Reset
					}
					if kind == "weekly" && !w.Observed.Before(weekObserved) {
						weekUsed, weekKnown, weekObserved, o.WeeklyResetAt = w.Used, true, w.Observed, w.Reset
					}
				}
			}
			b.mu.Unlock()
		}
		o.Known = fiveKnown || fiveCap == 0
		if fiveKnown {
			o.Remaining = fullCapacity * math.Max(0, fiveCap-fiveUsed) / 100
		}
		o.WeeklyExhausted = weekCap == 0 || (weekKnown && weekUsed >= weekCap)
		if fiveObserved.After(o.ObservedAt) {
			o.ObservedAt = fiveObserved
		}
		if weekObserved.After(o.ObservedAt) {
			o.ObservedAt = weekObserved
		}
		if o.WeeklyExhausted {
			o.Known = true
			o.Remaining = 0
		}
		// Quota errors without fresh watermarks remove inferred unusable capacity;
		// inferred zero is deliberately not a calibration sample.
		if a.Quota.Exceeded && a.Quota.Reason == "credential_quota" && a.Quota.NextRecoverAt.After(now) {
			o.Known, o.Estimated, o.Remaining, o.ResetAt = true, true, 0, a.Quota.NextRecoverAt
			if a.UpdatedAt.After(o.ObservedAt) {
				o.ObservedAt = a.UpdatedAt
			}
			message := ""
			if a.LastError != nil {
				message = strings.ToLower(a.LastError.Message)
			}
			if strings.Contains(message, "weekly") || strings.Contains(message, "7-day") || a.Quota.NextRecoverAt.Sub(now) > 5*time.Hour+time.Minute {
				o.WeeklyExhausted = true
				o.WeeklyResetAt = a.Quota.NextRecoverAt
			}
		}
		if previous, exists := byAccount[o.ID]; exists {
			cap := math.Min(previous.Capacity, o.Capacity)
			weekly := previous.WeeklyExhausted || o.WeeklyExhausted
			if previous.ObservedAt.After(o.ObservedAt) {
				o = previous
			}
			o.Remaining = math.Max(0, o.Remaining-(o.Capacity-cap))
			o.Capacity, o.WeeklyExhausted = cap, weekly
			if weekly {
				o.Known = true
				o.Remaining = 0
			}
		}
		byAccount[o.ID] = o
	}
	accounts := make([]sessionquota.Observation, 0, len(byAccount))
	for _, o := range byAccount {
		accounts = append(accounts, o)
	}
	q.Sync(accounts)
	lease := m.routableForcedBurn(context.Background(), "", cliproxyexecutor.Options{})
	burn := sessionquota.ForcedBurn{}
	if lease != nil {
		burn.Eligible, burn.ExpiresAt = true, lease.reset
	}
	q.SetForcedBurn(burn)
}
func codexQuotaProviders(providers []string) bool {
	for _, provider := range providers {
		if canonicalSchedulingProvider(provider) == "codex" {
			return true
		}
	}
	return false
}
func (m *Manager) admitSessionQuota(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, cliproxyexecutor.Options, *sessionquota.Reservation, error) {
	// Capture and remove attribution and obsolete hint headers even on non-budgeted
	// routes. They are proxy-local metadata, not part of the upstream API.
	session, _ := opts.Metadata[sessionquota.MetadataKey].(string)
	if value := opts.Headers.Get(sessionquota.Header); value != "" {
		session = value
	}
	if opts.Headers != nil {
		opts.Headers = opts.Headers.Clone()
		opts.Headers.Del(sessionquota.Header)
		opts.Headers.Del(sessionquota.InputTokensHeader)
		opts.Headers.Del(sessionquota.CachedTokensHeader)
	}
	q := m.SessionQuota()
	if q == nil || !codexQuotaProviders(providers) {
		return ctx, opts, nil, nil
	}
	m.SyncSessionQuota()
	if !q.HasAccounts() {
		return ctx, opts, nil, nil
	} // API-key Codex routes have no subscription pool.
	lease := m.routableForcedBurn(ctx, req.Model, opts)
	reservation, err := q.ReserveRequest(session, lease != nil)
	if err != nil {
		return ctx, opts, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, sessionQuotaContextKey{}, q)
	if lease != nil {
		ctx = context.WithValue(ctx, sessionQuotaBurnKey{}, lease)
		metadata := make(map[string]any, len(opts.Metadata)+1)
		for key, value := range opts.Metadata {
			metadata[key] = value
		}
		metadata[cliproxyexecutor.PinnedAuthMetadataKey] = lease.id
		opts.Metadata = metadata
	}
	ctx = usage.WithRecordObserver(ctx, func(_ context.Context, record usage.Record) {
		if canonicalSchedulingProvider(record.Provider) != "codex" {
			reservation.Consume("", 0, true)
			return
		}
		detail := record.Detail
		tokens := sessionquota.WeightedTokens(detail.InputTokens, detail.OutputTokens, detail.CachedTokens, detail.CacheReadTokens, detail.CacheCreationTokens, detail.ReasoningTokens)
		noCharge := record.Failed && noQuotaChargeStatus(record.Fail.StatusCode)
		accountID := record.AuthID
		if auth, ok := m.GetByID(record.AuthID); ok {
			accountID = sessionQuotaAccountID(auth)
		}
		reservation.ConsumeRecord(record.RequestID, accountID, tokens, noCharge)
		// A successful usage record is the terminal model response, even if
		// transport draining/EOF happens later. Release the request slot
		// before the client starts tools; a tool phase is not an API request.
		if !record.Failed {
			reservation.Close(false)
		}
	})
	return ctx, opts, reservation, nil
}
func (m *Manager) sessionQuotaError(err error) error {
	if err == nil {
		return nil
	}
	var exhausted *sessionquota.ExhaustedError
	if errors.As(err, &exhausted) || isRequestInvalidError(err) {
		return err
	}
	q := m.SessionQuota()
	if q == nil {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	m.SyncSessionQuota()
	snapshot := q.Snapshot()
	if snapshot.AllWeeklyExhausted {
		return q.Error()
	}
	// Do not convert arbitrary 429s (throughput throttles), authentication failures,
	// transport errors, or weekly errors into provisioned-quota waits.
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "weekly") || strings.Contains(text, "7-day") || strings.Contains(text, "7 day") {
		return err
	}
	if (clienterror.HTTPStatusFromError(err) == http.StatusTooManyRequests || snapshot.TotalRemaining <= 0) && (strings.Contains(text, "usage_limit_reached") || strings.Contains(text, "5-hour") || strings.Contains(text, "5 hour") || strings.Contains(text, "five-hour") || strings.Contains(text, "auth_quota_exceeded") || strings.Contains(text, "auth_unavailable") || strings.Contains(text, "auth_cooldown")) {
		return q.Error()
	}
	return err
}
func noQuotaCharge(err error) bool {
	if err == nil {
		return false
	}
	var exhausted *sessionquota.ExhaustedError
	if errors.As(err, &exhausted) {
		return true
	}
	return noQuotaChargeStatus(clienterror.HTTPStatusFromError(err))
}

func noQuotaChargeStatus(status int) bool {
	return status == 400 || status == 401 || status == 403 || status == 404 || status == 413 || status == 422 || status == 429
}

// Execute admits exactly once around all credential attempts and upstream retries.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx, opts, reservation, err := m.admitSessionQuota(ctx, providers, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if reservation == nil {
		return m.executeWithoutSessionQuota(ctx, providers, req, opts)
	}
	stopCancel := context.AfterFunc(ctx, func() { reservation.Close(false) })
	defer stopCancel()
	defer m.SyncSessionQuota()
	response, err := m.executeWithoutSessionQuota(ctx, providers, req, opts)
	reservation.Close(noQuotaCharge(err))
	return response, m.sessionQuotaError(err)
}

// ExecuteStream releases admission on cancellation or terminal usage/EOF.
// Late usage remains billable even if the upstream producer never closes.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ctx, opts, reservation, err := m.admitSessionQuota(ctx, providers, req, opts)
	if err != nil {
		return nil, err
	}
	if reservation == nil {
		return m.executeStreamWithoutSessionQuota(ctx, providers, req, opts)
	}
	stopCancel := context.AfterFunc(ctx, func() { reservation.Close(false) })
	result, err := m.executeStreamWithoutSessionQuota(ctx, providers, req, opts)
	if err != nil || result == nil || result.Chunks == nil {
		stopCancel()
		reservation.Close(noQuotaCharge(err))
		m.SyncSessionQuota()
		return result, m.sessionQuotaError(err)
	}
	output := make(chan cliproxyexecutor.StreamChunk)
	wrapped := *result
	wrapped.Chunks = output
	go func() {
		var terminal error
		defer close(output)
		defer stopCancel()
		defer func() { reservation.Close(noQuotaCharge(terminal)); m.SyncSessionQuota() }()
		for {
			var chunk cliproxyexecutor.StreamChunk
			select {
			case <-ctx.Done():
				return
			case value, ok := <-result.Chunks:
				if !ok {
					return
				}
				chunk = value
			}
			if chunk.Err != nil {
				terminal = chunk.Err
				chunk.Err = m.sessionQuotaError(chunk.Err)
			}
			select {
			case output <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &wrapped, nil
}

// A burn lease is request-local authority, not a user-controlled flag. Selection
// must revalidate it; pinning forbids fallback to any other credential.
type sessionQuotaBurnKey struct{}
type sessionQuotaBurnLease struct {
	controller *BurnController
	id         string
	reset      time.Time
}

func (l *sessionQuotaBurnLease) permits(a *Auth) bool {
	if l == nil || a == nil || a.ID != l.id {
		return false
	}
	b := l.controller
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	return b.cfg.Enabled && b.forced == l.id && b.forcedReset.Equal(l.reset) && l.reset.After(now) && b.forcedEligibleLocked([]*Auth{a}, now) == l.id
}

// routableForcedBurn chooses a genuine burn target over the currently routable
// subscription pool. Model/options-specific admission validates that SAME
// target; it does not select a replacement merely to bypass a session budget.
func (m *Manager) routableForcedBurn(ctx context.Context, model string, opts cliproxyexecutor.Options) *sessionQuotaBurnLease {
	m.mu.RLock()
	selector, ok := m.selector.(*BurnDeadlineSelector)
	_, executorOK := m.executorLocked("codex")
	m.mu.RUnlock()
	if !ok || !executorOK {
		return nil
	}
	b := selector.Controller
	if b == nil {
		b = DefaultBurnController
	}
	q := m.SessionQuota()
	if q == nil {
		return nil
	}
	models := registry.GetGlobalRegistry()
	now := b.now()
	var candidates []*Auth
	for _, a := range m.List() {
		if a == nil || a.Provider != "codex" || a.Disabled || a.Status == StatusDisabled || a.AuthKind() == AuthKindAPIKey || !q.EligibleAccount(sessionQuotaAccountID(a)) {
			continue
		}
		// The public capability requires at least one registered, usable route.
		for _, info := range models.GetModelsForClient(a.ID) {
			if info == nil {
				continue
			}
			if blocked, _, _ := isAuthBlockedForModelState(a, m.selectionModelForAuth(a, info.ID), now); !blocked {
				candidates = append(candidates, a)
				break
			}
		}
	}
	b.mu.Lock()
	for _, a := range candidates {
		b.ensureIdentity(a)
		state := b.account(a.ID)
		b.setPolicy(a, state)
		b.observeLocked(a.ID, state, a.Quota, now)
	}
	id := b.chooseForcedLocked(candidates, now)
	reset := b.forcedReset
	b.mu.Unlock()
	if id == "" || !reset.After(now) {
		return nil
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	for _, a := range candidates {
		if a.ID != id || (pinned != "" && pinned != id) || !eligibility.allows(a) {
			continue
		}
		if model != "" {
			if !m.authSupportsRouteModel(models, a, model) {
				return nil
			}
			if blocked, _, _ := isAuthBlockedForModelState(a, m.selectionModelForAuth(a, model), now); blocked {
				return nil
			}
		}
		lease := &sessionQuotaBurnLease{controller: b, id: id, reset: reset}
		if lease.permits(a) {
			return lease
		}
	}
	return nil
}

// SessionQuotaSnapshot optionally scopes forced-burn readiness to a request model.
// Public clients should query their actual model; omitting it describes whether
// any route can use the forced target, not a promise about every model.
func (m *Manager) SessionQuotaSnapshot(model string) sessionquota.Snapshot {
	q := m.SessionQuota()
	if q == nil {
		return sessionquota.Snapshot{}
	}
	m.SyncSessionQuota()
	snapshot := q.Snapshot()
	snapshot.ForcedBurn = sessionquota.ForcedBurn{}
	if lease := m.routableForcedBurn(context.Background(), model, cliproxyexecutor.Options{}); lease != nil {
		snapshot.ForcedBurn = sessionquota.ForcedBurn{Eligible: true, ExpiresAt: lease.reset}
	}
	for i := range snapshot.Buckets {
		b := &snapshot.Buckets[i]
		b.AdmissionReady = !snapshot.AllWeeklyExhausted && (snapshot.ForcedBurn.Eligible || (b.Available > 0 && (b.Available > sessionquota.NearEmptyPoints || b.InFlight == 0)))
	}
	return snapshot
}
