package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func quotaManager(t *testing.T) (*Manager, *sessionquota.Manager, string) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	model := "quota-model-" + t.Name()
	id := "quota-auth-" + t.Name()
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, "session_quota_capacity": "250"}})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := sessionquota.New("")
	m.SetSessionQuota(q)
	m.SyncSessionQuota()
	return m, q, model
}
func quotaBucket(t *testing.T, q *sessionquota.Manager, name string) sessionquota.Bucket {
	t.Helper()
	for _, b := range q.Snapshot().Buckets {
		if b.Session == name {
			return b
		}
	}
	t.Fatal("missing bucket", name)
	return sessionquota.Bucket{}
}
func TestSessionQuotaExecuteAccountsUsageAndRejectsInteractiveBeforeUpstream(t *testing.T) {
	m, q, model := quotaManager(t)
	if err := q.Set("workflow", 250); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		if opts.Headers.Get(sessionquota.Header) != "" {
			t.Error("session header leaked upstream")
		}
		usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 100}})
		return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}})
	request := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}
	_, err := m.Execute(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	b := quotaBucket(t, q, "workflow")
	if math.Abs(b.Remaining-249.86) > 1e-6 || b.Held != 0 {
		t.Fatal(b)
	}
	_, err = m.Execute(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{})
	var exhausted *sessionquota.ExhaustedError
	if !errors.As(err, &exhausted) || exhausted.Weekly || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestSessionQuotaStreamReconcilesBeforeEOF(t *testing.T) {
	m, q, model := quotaManager(t)
	_ = q.Set("workflow", 250)
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		chunks := make(chan cliproxyexecutor.StreamChunk)
		go func() {
			defer close(chunks)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
			usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 100}})
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}})
	result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}, cliproxyexecutor.Options{Stream: true, Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	b := quotaBucket(t, q, "workflow")
	if b.Held != 0 || math.Abs(b.Remaining-249.86) > 1e-6 {
		t.Fatal(b)
	}
}
func TestSessionQuotaWeeklySnapshotSkipsKnownBlockedCredential(t *testing.T) {
	m, q, _ := quotaManager(t)
	_, err := m.Register(context.Background(), &Auth{ID: "weekly-blocked", Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, "session_quota_capacity": "100"}, Quota: QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "1", "X-Codex-Primary-Window-Minutes": "300", "X-Codex-Secondary-Used-Percent": "100", "X-Codex-Secondary-Window-Minutes": "10080"}}})
	if err != nil {
		t.Fatal(err)
	}
	m.SyncSessionQuota()
	eligibility := authSelectionEligibility{sessionQuota: q}
	blocked, _ := m.GetByID("weekly-blocked")
	if eligibility.allows(blocked) {
		t.Fatal("weekly-exhausted account eligible")
	}
	if q.Snapshot().AllWeeklyExhausted {
		t.Fatal("usable account omitted")
	}
}
func TestSessionQuotaNoSubscriptionPoolDoesNotGateAPIKeyCodex(t *testing.T) {
	m := NewManager(nil, nil, nil)
	q, _ := sessionquota.New("")
	m.SetSessionQuota(q)
	_, _ = m.Register(context.Background(), &Auth{ID: "apikey", Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAPIKey: "test"}})
	_, _, reservation, err := m.admitSessionQuota(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil || reservation != nil || q.HasAccounts() {
		t.Fatal(err, reservation, q.Snapshot())
	}
}
func TestSessionQuotaOtherProvidersUnaffected(t *testing.T) {
	m := NewManager(nil, nil, nil)
	q, _ := sessionquota.New("")
	m.SetSessionQuota(q)
	_, _, reservation, err := m.admitSessionQuota(context.Background(), []string{"claude"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil || reservation != nil {
		t.Fatal(err)
	}
}
func TestSessionQuotaCredentialQuotaCooldownInfersZeroWithoutCalibration(t *testing.T) {
	m, q, model := quotaManager(t)
	id := "quota-auth-" + t.Name()
	auth, _ := m.GetByID(id)
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "credential_quota"
	auth.Quota.NextRecoverAt = time.Now().Add(time.Hour)
	auth.UpdatedAt = time.Now()
	auth.LastError = &Error{Message: `{"error":{"type":"usage_limit_reached"}}`}
	if _, err := m.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	m.SyncSessionQuota()
	if q.Snapshot().TotalRemaining != 0 || q.Snapshot().AllWeeklyExhausted {
		t.Fatal(q.Snapshot())
	}
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	var exhausted *sessionquota.ExhaustedError
	if !errors.As(err, &exhausted) || exhausted.Weekly {
		t.Fatal(err)
	}
}
func TestSessionQuotaWeeklyCooldownProducesTerminalWeeklyError(t *testing.T) {
	m, q, model := quotaManager(t)
	id := "quota-auth-" + t.Name()
	a, _ := m.GetByID(id)
	a.Quota.Exceeded = true
	a.Quota.Reason = "credential_quota"
	a.Quota.NextRecoverAt = time.Now().Add(24 * time.Hour)
	a.UpdatedAt = time.Now()
	a.LastError = &Error{Message: "weekly quota exhausted"}
	_, _ = m.Update(context.Background(), a)
	m.SyncSessionQuota()
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	var exhausted *sessionquota.ExhaustedError
	if !errors.As(err, &exhausted) || !exhausted.Weekly || !q.Snapshot().AllWeeklyExhausted {
		t.Fatal(err, q.Snapshot())
	}
}

func TestSessionQuotaAbsoluteCredentialCapsAndSharedIdentity(t *testing.T) {
	m := NewManager(nil, nil, nil)
	q, _ := sessionquota.New("")
	m.SetSessionQuota(q)
	observed := time.Now()
	for _, fixture := range []struct {
		id, account string
		cap, used   float64
	}{
		{"session-cap-alias-a", "same-subscription", 50, 40},
		{"session-cap-alias-b", "same-subscription", 100, 40},
		{"session-cap-other", "another-subscription", 100, 20},
	} {
		_, err := m.Register(context.Background(), &Auth{ID: fixture.id, Provider: "codex", Status: StatusActive,
			Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
			Metadata:   map[string]any{"account_id": fixture.account, "five_hour_cap_percent": fixture.cap},
			Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": fmt.Sprint(fixture.used), "X-Codex-Primary-Window-Minutes": "300",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	m.SyncSessionQuota()
	// 50-point cap minus 40 used = 10; the duplicate credential is not a second
	// subscription. Other account contributes 100 - 20 = 80.
	if got := q.Snapshot().TotalRemaining; math.Abs(got-90) > 1e-6 {
		t.Fatalf("pool=%v want 90", got)
	}
	a, _ := m.GetByID("session-cap-alias-a")
	if !q.EligibleAccount(sessionQuotaAccountID(a)) {
		t.Fatal("shared subscription should be eligible")
	}
	// A policy edit applies immediately even when the usage watermark is unchanged.
	a.Metadata["five_hour_cap_percent"] = float64(30)
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m.SyncSessionQuota()
	if got := q.Snapshot().TotalRemaining; math.Abs(got-80) > 1e-6 {
		t.Fatalf("pool after cap change=%v want 80", got)
	}
	if q.EligibleAccount(sessionQuotaAccountID(a)) {
		t.Fatal("exhausted shared subscription was eligible")
	}
}

func TestSessionQuotaExplicitZeroCredentialCapDoesNotInventCapacity(t *testing.T) {
	m := NewManager(nil, nil, nil)
	q, _ := sessionquota.New("")
	m.SetSessionQuota(q)
	_, err := m.Register(context.Background(), &Auth{ID: "session-zero-cap", Provider: "codex", Status: StatusActive,
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}, Metadata: map[string]any{"five_hour_cap_percent": float64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	m.SyncSessionQuota()
	if !q.HasAccounts() || q.Snapshot().TotalRemaining != 0 {
		t.Fatal("zero cap became a missing or full account", q.Snapshot())
	}
	_, err = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "any", Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{})
	var exhausted *sessionquota.ExhaustedError
	if !errors.As(err, &exhausted) || exhausted.Weekly {
		t.Fatalf("expected provisioned-quota error, got %v", err)
	}
}

func TestSessionQuotaUsesBackgroundPollerWhilePaused(t *testing.T) {
	m := NewManager(nil, nil, nil)
	q, _ := sessionquota.New("")
	m.SetSessionQuota(q)
	auth := &Auth{ID: "session-poller-watermark", Provider: "codex", Status: StatusActive,
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
		Quota: QuotaState{ObservedAt: time.Now().Add(-time.Minute), Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "10", "X-Codex-Primary-Window-Minutes": "300",
		}},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	b := DefaultBurnController
	b.mu.Lock()
	key := b.key(auth.ID)
	previous := b.accounts[key]
	b.accounts[key] = &BurnAccount{Windows: map[string]*BurnWindow{
		"five_hour": {Used: 88, Observed: time.Now(), Reset: time.Now().Add(time.Hour)},
	}}
	b.mu.Unlock()
	t.Cleanup(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if previous == nil {
			delete(b.accounts, key)
		} else {
			b.accounts[key] = previous
		}
	})
	m.SyncSessionQuota()
	if got := q.Snapshot().TotalRemaining; math.Abs(got-12) > 1e-6 {
		t.Fatalf("pool=%v want freshest polled 12", got)
	}
}
