package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func forcedQuotaFixture(t *testing.T) (*Manager, *sessionquota.Manager, *BurnController, string, string, *time.Time) {
	t.Helper()
	m, q, model := quotaManager(t)
	id := "quota-auth-" + t.Name()
	now := time.Now().Truncate(time.Second)
	b := burnTestController(now)
	b.now = func() time.Time { return now }
	a, _ := m.GetByID(id)
	a.Quota = burnTestQuota(now, 10, 10, now.Add(10*time.Minute), now.Add(24*time.Hour))
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b.Observe(a, a.Quota, now)
	b.mu.Lock()
	b.lastTokenTPS = 1
	b.account(id).Windows["five_hour"].PercentPerToken = .1
	b.account(id).Windows["five_hour"].CalibrationSamples = 2
	b.mu.Unlock()
	m.SetSelector(&BurnDeadlineSelector{Fallback: &FillFirstSelector{}, Controller: b})
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, nil
	}})
	m.SyncSessionQuota()
	_ = q.Set("waiting", 100)
	_ = q.Set("protected", 150)
	r, err := q.ReserveRequest("waiting", false)
	if err != nil {
		t.Fatal(err)
	}
	r.ConsumeRecord("previous", sessionQuotaAccountID(a), 1000000, false)
	r.Close(false)
	return m, q, b, model, id, &now
}

func TestForcedBurnAdmitsWaitingSessionsOnlyOnEligiblePinnedCredential(t *testing.T) {
	for _, session := range []string{"waiting", "interactive-unregistered"} {
		t.Run(session, func(t *testing.T) {
			m, q, b, model, id, _ := forcedQuotaFixture(t)
			// The setting alone is never bypass authority.
			b.mu.Lock()
			b.forced = ""
			b.lastTokenTPS = 0
			b.mu.Unlock()
			calls := 0
			m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				if a.ID != id || pinnedAuthIDFromMetadata(opts.Metadata) != id {
					t.Fatal("burn fell back to wrong account", a.ID, opts.Metadata)
				}
				usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Detail: usage.Detail{InputTokens: 1000}})
				return cliproxyexecutor.Response{}, nil
			}})
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi"}`)}
			opts := cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: session}}
			_, err := m.Execute(context.Background(), []string{"codex"}, req, opts)
			var exhausted *sessionquota.ExhaustedError
			if !errors.As(err, &exhausted) || calls != 0 || m.SessionQuotaSnapshot(model).ForcedBurn.Eligible {
				t.Fatal("enabled flag bypassed budget", err, calls)
			}
			protected := quotaBucket(t, q, "protected").Remaining
			b.mu.Lock()
			b.lastTokenTPS = 1
			b.mu.Unlock()
			snapshot := m.SessionQuotaSnapshot(model)
			if !snapshot.ForcedBurn.Eligible || snapshot.ForcedBurn.ExpiresAt.IsZero() {
				t.Fatal("forced routable target not public", snapshot)
			}
			for _, bucket := range snapshot.Buckets {
				if bucket.Session == "waiting" && !bucket.AdmissionReady {
					t.Fatal(bucket)
				}
			}
			raw, _ := json.Marshal(snapshot)
			if strings.Contains(string(raw), id) {
				t.Fatal("credential identity leaked", string(raw))
			}
			_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
			if err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
			if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
				t.Fatal("mutated caller metadata")
			}
			if math.Abs(quotaBucket(t, q, "protected").Remaining-protected) > 1e-6 {
				t.Fatal("burn borrowed protected allocation")
			}
			bucketName := session
			if session != "waiting" {
				bucketName = ""
			}
			if quotaBucket(t, q, bucketName).Debt <= 0 {
				t.Fatal("burn failed to debit owner")
			}
		})
	}
}

func TestForcedBurnPublicReadinessAndAdmissionRespectModelAndPinnedAuth(t *testing.T) {
	m, q, _, model, id, _ := forcedQuotaFixture(t)
	wrongModel := model + "-other"
	otherID := id + "-other"
	registry.GetGlobalRegistry().RegisterClient(otherID, "codex", []*registry.ModelInfo{{ID: wrongModel}, {ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(otherID)
	_, err := m.Register(context.Background(), &Auth{ID: otherID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, "session_quota_capacity": "100"}})
	if err != nil {
		t.Fatal(err)
	}
	m.SyncSessionQuota()
	if credit := quotaBucket(t, q, "waiting").Remaining; credit > 0 {
		r, err := q.ReserveRequest("waiting", false)
		if err != nil {
			t.Fatal(err)
		}
		r.ConsumeRecord("drain-added-capacity", id, credit*sessionquota.DefaultTokensPerPoint, false)
		r.Close(false)
	}
	if !m.SessionQuotaSnapshot(model).ForcedBurn.Eligible || !m.SessionQuotaSnapshot("").ForcedBurn.Eligible {
		t.Fatal("valid route was not eligible")
	}
	wrong := m.SessionQuotaSnapshot(wrongModel)
	if wrong.ForcedBurn.Eligible {
		t.Fatal("wrong-model bypass advertised")
	}
	for _, bucket := range wrong.Buckets {
		if bucket.Session == "waiting" && bucket.AdmissionReady {
			t.Fatal(bucket)
		}
	}
	for _, opts := range []cliproxyexecutor.Options{{Metadata: map[string]any{sessionquota.MetadataKey: "waiting"}}, {Metadata: map[string]any{sessionquota.MetadataKey: "waiting", cliproxyexecutor.PinnedAuthMetadataKey: otherID}}} {
		requestModel := wrongModel
		if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
			requestModel = model
		}
		_, _, r, err := m.admitSessionQuota(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: requestModel}, opts)
		if r != nil {
			r.Close(false)
		}
		var exhausted *sessionquota.ExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatal("wrong route/pin bypassed", err)
		}
	}
	if quotaBucket(t, q, "waiting").InFlight != 0 {
		t.Fatal("rejected route leaked slot")
	}
}

func TestForcedBurnExpiryEndStaleExhaustedAndWeeklyRevocation(t *testing.T) {
	for _, reason := range []string{"reset", "end", "stale", "disabled", "five-hour-empty", "weekly-empty", "weekly-cap-50", "five-hour-cap-50", "blocked-model", "unregistered-model", "missing-executor"} {
		t.Run(reason, func(t *testing.T) {
			m, q, b, model, id, now := forcedQuotaFixture(t)
			if !m.SessionQuotaSnapshot(model).ForcedBurn.Eligible {
				t.Fatal("fixture not forced")
			}
			a, _ := m.GetByID(id)
			if reason != "reset" && reason != "end" && reason != "stale" {
				*now = now.Add(time.Second)
			}
			switch reason {
			case "reset":
				*now = now.Add(11 * time.Minute)
			case "end":
				b.mu.Lock()
				b.cfg.Enabled = false
				b.mu.Unlock()
			case "stale":
				*now = now.Add(3 * time.Minute)
			case "disabled":
				a.Disabled = true
			case "five-hour-empty":
				a.Quota = burnTestQuota(*now, 100, 10, now.Add(10*time.Minute), now.Add(24*time.Hour))
			case "weekly-empty":
				a.Quota = burnTestQuota(*now, 10, 100, now.Add(10*time.Minute), now.Add(24*time.Hour))
			case "weekly-cap-50":
				a.Metadata = map[string]any{"weekly_cap_percent": 50.0}
				a.Quota = burnTestQuota(*now, 10, 50, now.Add(10*time.Minute), now.Add(24*time.Hour))
			case "five-hour-cap-50":
				a.Metadata = map[string]any{"five_hour_cap_percent": 50.0}
				a.Quota = burnTestQuota(*now, 50, 10, now.Add(10*time.Minute), now.Add(24*time.Hour))
			case "blocked-model":
				a.ModelStates = map[string]*ModelState{model: {Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}}
			case "unregistered-model":
				registry.GetGlobalRegistry().UnregisterClient(id)
			case "missing-executor":
				m.mu.Lock()
				delete(m.executors, "codex")
				m.mu.Unlock()
			}
			if reason != "reset" && reason != "end" && reason != "stale" && reason != "unregistered-model" && reason != "missing-executor" {
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := m.SessionQuotaSnapshot(model)
			if snapshot.ForcedBurn.Eligible {
				t.Fatal("revoked burn remained public", reason, snapshot)
			}
			for _, bucket := range snapshot.Buckets {
				if bucket.Session == "waiting" && bucket.AdmissionReady {
					t.Fatal("revoked override kept waiter ready", bucket)
				}
			}
			_, _, r, err := m.admitSessionQuota(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "waiting"}})
			if r != nil {
				r.Close(false)
			}
			if reason == "disabled" {
				_, executeErr := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "waiting"}})
				if executeErr == nil {
					t.Fatal("disabled credential used")
				}
			} else if err == nil {
				t.Fatal("revoked burn bypassed budget", q.Snapshot())
			}
			if reason == "weekly-empty" {
				var e *sessionquota.ExhaustedError
				if !errors.As(err, &e) || !e.Weekly {
					t.Fatal("weekly stop not terminal", err)
				}
			}
		})
	}
}

func TestForcedBurnConcurrentZeroAndDebtAdmissionsRevokeWithoutFallback(t *testing.T) {
	m, q, b, model, id, _ := forcedQuotaFixture(t)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "waiting"}}
	req := cliproxyexecutor.Request{Model: model}
	var requests []*sessionquota.Reservation
	var ctx context.Context
	var pinned cliproxyexecutor.Options
	for i := 0; i < 3; i++ {
		c, o, r, err := m.admitSessionQuota(context.Background(), []string{"codex"}, req, opts)
		if err != nil {
			t.Fatal("zero credit burn serialized", err)
		}
		ctx, pinned = c, o
		requests = append(requests, r)
	}
	requests[0].ConsumeRecord("burn", id, 1000, false)
	// Account ID is deliberately explicit and the actual charge belongs to the
	// same zero-credit session regardless of other concurrent builders.
	c, o, r, err := m.admitSessionQuota(context.Background(), []string{"codex"}, req, opts)
	if err != nil {
		t.Fatal("debt burn serialized", err)
	}
	ctx, pinned = c, o
	requests = append(requests, r)
	snapshot := m.SessionQuotaSnapshot(model)
	for _, bucket := range snapshot.Buckets {
		if bucket.Session == "waiting" && (bucket.InFlight != 4 || !bucket.AdmissionReady || bucket.Debt <= 0) {
			t.Fatal("burn did not override both quota gates", bucket)
		}
	}
	lease, _ := ctx.Value(sessionQuotaBurnKey{}).(*sessionQuotaBurnLease)
	a, _ := m.GetByID(id)
	if lease == nil || !lease.permits(a) {
		t.Fatal("lease missing")
	}
	b.mu.Lock()
	b.cfg.Enabled = false
	b.mu.Unlock()
	if lease.permits(a) {
		t.Fatal("ended burn lease usable")
	}
	if _, _, extra, err := m.admitSessionQuota(context.Background(), []string{"codex"}, req, opts); err == nil {
		extra.Close(false)
		t.Fatal("ended override admitted")
	}
	calls := 0
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		return cliproxyexecutor.Response{}, nil
	}})
	_, err = m.executeWithoutSessionQuota(ctx, []string{"codex"}, req, pinned)
	if err == nil || calls != 0 {
		t.Fatal("revoked lease contacted upstream/fell back", err, calls)
	}
	for _, r := range requests {
		r.Close(false)
	}
	if bucket := quotaBucket(t, q, "waiting"); bucket.InFlight != 0 || bucket.AdmissionReady {
		t.Fatal("ended/debted session resumed", bucket)
	}
}
