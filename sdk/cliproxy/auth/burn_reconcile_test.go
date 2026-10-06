package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestReconcilePolledQuotaConfirmedRefillRestoresRouting(t *testing.T) {
	now := time.Now()
	oldReset := now.Add(48 * time.Hour)
	model := "refill-model"
	manager := NewManager(nil, nil, nil)
	id := "confirmed-refill-auth"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(id) })
	e := &Error{HTTPStatus: 429, Code: "usage_limit_reached", Message: "usage_limit_reached"}
	oldQuota := burnTestQuota(now.Add(-time.Minute), 0, 100, now.Add(time.Hour), oldReset)
	oldQuota.Exceeded = true
	oldQuota.Reason = "credential_quota"
	oldQuota.NextRecoverAt = oldReset
	auth := &Auth{ID: id, Provider: "codex", Status: StatusError, Unavailable: true, LastError: e, Quota: oldQuota, NextRetryAfter: oldReset, ModelStates: map[string]*ModelState{model: {Status: StatusError, Unavailable: true, LastError: e, NextRetryAfter: oldReset, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: oldReset}}}}
	a, err := manager.Register(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetModelQuotaExceeded(id, model)
	reg.SuspendClientModel(id, model, "quota")
	fresh := burnTestQuota(now, 3, 0, now.Add(5*time.Hour), now.Add(7*24*time.Hour))
	manager.ReconcilePolledQuota(context.Background(), a, fresh)
	got, _ := manager.GetByID(id)
	if got.Unavailable || got.Quota.Exceeded || got.Status != StatusActive || !got.NextRetryAfter.IsZero() {
		t.Fatalf("refill did not restore credential: %+v", got)
	}
	if s := got.ModelStates[model]; s.Unavailable || s.Quota.Exceeded || s.LastError != nil {
		t.Fatalf("refill did not restore model: %+v", s)
	}
	if reg.GetModelCount(model) != 1 {
		t.Fatalf("registry remains blocked")
	}
	if got.Quota.Signals["X-Codex-Secondary-Used-Percent"] != "0" {
		t.Fatalf("fresh observations not applied")
	}
}

func TestReconcilePolledQuotaDoesNotClearOtherFailuresOrBypassCaps(t *testing.T) {
	for _, tc := range []string{"same_window", "weekly_exhausted", "five_hour_exhausted", "cap", "unauthorized", "generic_429", "newer_generation", "disabled", "denied", "missing_window", "stale", "model_auth_error"} {
		t.Run(tc, func(t *testing.T) {
			now := time.Now()
			reset := now.Add(48 * time.Hour)
			manager := NewManager(nil, nil, nil)
			q := burnTestQuota(now.Add(-time.Minute), 0, 100, now.Add(time.Hour), reset)
			q.Exceeded = true
			q.NextRecoverAt = reset
			e := &Error{HTTPStatus: 429, Code: "usage_limit_reached", Message: "usage_limit_reached"}
			auth := &Auth{ID: fmt.Sprint("guard-", tc), Provider: "codex", Status: StatusError, Unavailable: true, LastError: e, Quota: q, NextRetryAfter: reset, Metadata: map[string]any{}}
			if tc == "unauthorized" {
				auth.LastError = &Error{HTTPStatus: 401, Message: "bad token"}
			}
			if tc == "generic_429" {
				auth.LastError = &Error{HTTPStatus: 429, Message: "requests per minute"}
			}
			if tc == "disabled" {
				auth.Disabled = true
			}
			if tc == "cap" {
				auth.Metadata["weekly_cap_percent"] = 50
			}
			if tc == "model_auth_error" {
				auth.ModelStates = map[string]*ModelState{"other": {Status: StatusError, Unavailable: true, LastError: &Error{HTTPStatus: 403, Message: "forbidden"}}}
			}
			a, err := manager.Register(context.Background(), auth)
			if err != nil {
				t.Fatal(err)
			}
			fresh := burnTestQuota(now, 3, 0, now.Add(5*time.Hour), now.Add(7*24*time.Hour))
			switch tc {
			case "same_window":
				fresh = burnTestQuota(now, 3, 20, now.Add(5*time.Hour), reset)
			case "weekly_exhausted":
				fresh = burnTestQuota(now, 3, 100, now.Add(5*time.Hour), now.Add(7*24*time.Hour))
			case "five_hour_exhausted":
				fresh = burnTestQuota(now, 100, 0, now.Add(5*time.Hour), now.Add(7*24*time.Hour))
			case "cap":
				fresh = burnTestQuota(now, 3, 50, now.Add(5*time.Hour), now.Add(7*24*time.Hour))
			case "newer_generation":
				a.Generation--
			case "denied":
				fresh.Signals["X-Codex-Allowed"] = "false"
			case "missing_window":
				delete(fresh.Signals, "X-Codex-Primary-Used-Percent")
			case "stale":
				fresh.ObservedAt = now.Add(-time.Hour)
			}
			before, _ := manager.GetByID(a.ID)
			manager.ReconcilePolledQuota(context.Background(), a, fresh)
			got, _ := manager.GetByID(a.ID)
			if got.Generation != before.Generation || got.Unavailable != before.Unavailable || got.Quota.Exceeded != before.Quota.Exceeded {
				t.Fatalf("unsafe reset for %s", tc)
			}
		})
	}
}
