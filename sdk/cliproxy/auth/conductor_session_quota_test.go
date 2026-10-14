package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
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

func TestSessionQuotaHintsStayLocalAndActualUsageIsAuthoritative(t *testing.T) {
	m, q, model := quotaManager(t)
	_ = q.Set("workflow", 250)
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		for _, key := range []string{sessionquota.Header, sessionquota.InputTokensHeader, sessionquota.CachedTokensHeader} {
			if opts.Headers.Get(key) != "" {
				t.Fatalf("proxy-local header leaked: %s", key)
			}
		}
		usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 100}})
		return cliproxyexecutor.Response{}, nil
	}})
	headers := http.Header{}
	headers.Set(sessionquota.Header, "workflow")
	headers.Set(sessionquota.InputTokensHeader, "10")
	headers.Set(sessionquota.CachedTokensHeader, "10")
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello","max_output_tokens":10}`)}, cliproxyexecutor.Options{Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get(sessionquota.InputTokensHeader) != "10" {
		t.Fatal("mutated caller headers")
	}
	b := quotaBucket(t, q, "workflow")
	if math.Abs(b.Remaining-249.86) > 1e-6 || b.Held != 0 {
		t.Fatalf("hint became billing record: %+v", b)
	}
}

func TestSessionQuotaFinalUsageReleasesHeldBeforeStreamEOF(t *testing.T) {
	m, q, model := quotaManager(t)
	_ = q.Set("workflow", 250)
	terminalUsage := make(chan struct{})
	eof := make(chan struct{})
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		chunks := make(chan cliproxyexecutor.StreamChunk)
		go func() {
			defer close(chunks)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: tool call\n\n")}
			usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 100}})
			close(terminalUsage)
			<-eof
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}})
	stream, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Chunks
	select {
	case <-terminalUsage:
	case <-time.After(time.Second):
		t.Fatal("terminal usage missing")
	}
	held := quotaBucket(t, q, "workflow").Held
	close(eof)
	for range stream.Chunks {
	}
	if held != 0 {
		t.Fatalf("completed model response held %.6f points while client executes tools", held)
	}
	if math.Abs(quotaBucket(t, q, "workflow").Remaining-249.86) > 1e-6 {
		t.Fatal("completion double billed")
	}
}

func TestSessionQuotaRequestSizeErrorsReleaseReservation(t *testing.T) {
	for _, test := range []struct {
		name     string
		register func(*Manager)
		invoke   func(*Manager, string) error
	}{
		{
			name: "execute",
			register: func(m *Manager) {
				m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					return cliproxyexecutor.Response{}, errors.New("exceeded request buffer limit")
				}})
			},
			invoke: func(m *Manager, model string) error {
				_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
				return err
			},
		},
		{
			name: "count tokens",
			register: func(m *Manager) {
				m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", countFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					return cliproxyexecutor.Response{}, errors.New("request exceeds the buffer limit")
				}})
			},
			invoke: func(m *Manager, model string) error {
				_, err := m.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
				return err
			},
		},
		{
			name: "stream setup",
			register: func(m *Manager) {
				m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					return nil, errors.New("request buffer limit exceeded")
				}})
			},
			invoke: func(m *Manager, model string) error {
				_, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
				return err
			},
		},
		{
			name: "stream bootstrap",
			register: func(m *Manager) {
				m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					chunks := make(chan cliproxyexecutor.StreamChunk, 1)
					chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("request body too large")}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}})
			},
			invoke: func(m *Manager, model string) error {
				_, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_output_tokens":1000}`)}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, q, model := quotaManager(t)
			if err := q.Set("workflow", 250); err != nil {
				t.Fatal(err)
			}
			test.register(m)
			err := test.invoke(m, model)
			var sizeErr *requestSizeError
			if !errors.As(err, &sizeErr) || sizeErr.StatusCode() != http.StatusRequestEntityTooLarge {
				t.Fatalf("error = %T/%v, want requestSizeError", err, err)
			}
			b := quotaBucket(t, q, "workflow")
			if b.Held != 0 || math.Abs(b.Remaining-250) > 1e-6 {
				t.Fatalf("reservation leaked or charged: %+v", b)
			}
		})
	}
}

func TestSessionQuotaTinyCreditHugePayloadContactsUpstream(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			m, q, model := quotaManager(t)
			if err := q.Set("workflow", .000001); err != nil {
				t.Fatal(err)
			}
			calls := 0
			check := func(ctx context.Context, a *Auth, req cliproxyexecutor.Request) {
				calls++
				if len(req.Payload) < 1000000 {
					t.Fatal("payload was compacted")
				}
				usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 100}})
			}
			m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(ctx context.Context, a *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				check(ctx, a, req)
				return cliproxyexecutor.Response{}, nil
			}})
			if streaming {
				m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, a *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					check(ctx, a, req)
					chunks := make(chan cliproxyexecutor.StreamChunk)
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}})
			}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"` + strings.Repeat("x", 1000000) + `","max_output_tokens":100000000}`)}
			opts := cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}}
			if streaming {
				result, err := m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for range result.Chunks {
				}
			} else if _, err := m.Execute(context.Background(), []string{"codex"}, req, opts); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			b := quotaBucket(t, q, "workflow")
			if math.Abs(b.Debt-(.14-.000001)) > 1e-6 || b.Remaining != 0 || b.InFlight != 0 {
				t.Fatal(b)
			}
			if b := quotaBucket(t, q, ""); math.Abs(b.Remaining-(250-.000001)) > 1e-6 {
				t.Fatal("stole interactive credit", b)
			}
		})
	}
}

func TestSessionQuotaPayloadFaultIsNotConvertedToWeeklyWait(t *testing.T) {
	m, q, _ := quotaManager(t)
	q.Sync([]sessionquota.Observation{{ID: "quota-auth-" + t.Name(), Capacity: 250, Known: true, WeeklyExhausted: true, ObservedAt: time.Now().Add(time.Second)}})
	original := normalizeRequestSizeError(errors.New("exceeded request buffer limit"))
	if err := m.sessionQuotaError(original); err != original {
		t.Fatalf("non-refillable payload error converted to quota wait: %T/%v", err, err)
	}
}

func TestSessionQuotaCancellationReleasesSlotWithoutProducerEOFAndBillsLateUsage(t *testing.T) {
	for _, measured := range []bool{false, true} {
		t.Run(fmt.Sprint(measured), func(t *testing.T) {
			m, q, model := quotaManager(t)
			_ = q.Set("workflow", .25)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var execution context.Context
			var authID string
			// This producer deliberately never closes. No drain/EOF is needed to release.
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				execution = ctx
				authID = a.ID
				chunks := make(chan cliproxyexecutor.StreamChunk, 1)
				chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: started\n\n")}
				return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
			}})
			stream, err := m.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi"}`)}, cliproxyexecutor.Options{Metadata: map[string]any{sessionquota.MetadataKey: "workflow"}})
			if err != nil {
				t.Fatal(err)
			}
			<-stream.Chunks
			if b := quotaBucket(t, q, "workflow"); b.InFlight != 1 || b.AdmissionReady {
				t.Fatal(b)
			}
			cancel()
			select {
			case _, ok := <-stream.Chunks:
				if ok {
					t.Fatal("unexpected chunk")
				}
			case <-time.After(time.Second):
				t.Fatal("cancel waited for upstream EOF")
			}
			b := quotaBucket(t, q, "workflow")
			if b.InFlight != 0 || b.Remaining != .25 || !b.AdmissionReady {
				t.Fatal(b)
			}
			if measured {
				record := usage.Record{RequestID: "late", Provider: "codex", AuthID: authID, Failed: true, Detail: usage.Detail{InputTokens: 3000, OutputTokens: 100}}
				usage.PublishRecord(execution, record)
				usage.PublishRecord(execution, record)
				b = quotaBucket(t, q, "workflow")
				if b.InFlight != 0 || math.Abs(b.Debt-.09) > 1e-6 {
					t.Fatal("late debit missing/doubled", b)
				}
				record.Detail.InputTokens = 4000
				usage.PublishRecord(execution, record)
				b = quotaBucket(t, q, "workflow")
				if math.Abs(b.Debt-.19) > 1e-6 {
					t.Fatal("late cumulative delta missing", b)
				}
			}
			if b := quotaBucket(t, q, ""); b.Remaining != 249.75 {
				t.Fatal("late usage stole other budget", b)
			}
		})
	}
}

func TestSessionQuotaNoChargeUsageRecordPreventsFallbackCharge(t *testing.T) {
	for _, status := range []int{413, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, q, model := quotaManager(t)
			m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				usage.PublishRecord(ctx, usage.Record{Provider: "codex", AuthID: a.ID, Failed: true, Fail: usage.Failure{StatusCode: status}})
				// The usage record, not the transport's terminal error, owns known
				// no-charge accounting when the connection breaks during cleanup.
				return cliproxyexecutor.Response{}, errors.New("transport closed after rejection")
			}})
			_, _ = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if b := quotaBucket(t, q, ""); b.Held != 0 || b.Remaining != 250 {
				t.Fatalf("known no-charge record billed fallback: %+v", b)
			}
		})
	}
}
