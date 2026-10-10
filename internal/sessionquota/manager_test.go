package sessionquota

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func ledger(t *testing.T, remaining float64) *Manager {
	t.Helper()
	q, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	q.Sync([]Observation{{ID: "a", Capacity: 250, Known: true, Remaining: remaining, ObservedAt: time.Now(), ResetAt: time.Now().Add(time.Hour)}})
	return q
}
func bucket(t *testing.T, q *Manager, name string) Bucket {
	t.Helper()
	for _, b := range q.Snapshot().Buckets {
		if b.Session == name {
			return b
		}
	}
	t.Fatalf("missing bucket %q", name)
	return Bucket{}
}
func near(t *testing.T, a, b float64) {
	t.Helper()
	if math.Abs(a-b) > 1e-6 {
		t.Fatalf("got %.9f want %.9f", a, b)
	}
}
func invariant(t *testing.T, q *Manager) {
	t.Helper()
	s := q.Snapshot()
	caps, remaining := 0.0, 0.0
	for _, b := range s.Buckets {
		caps += b.Cap
		remaining += b.Remaining
		if b.Cap < 0 || b.Remaining < 0 || b.Held < 0 || b.Available < 0 {
			t.Fatal(b)
		}
	}
	near(t, caps, 250)
	near(t, remaining, s.TotalRemaining)
	q.mu.Lock()
	pool := q.poolLocked()
	q.mu.Unlock()
	near(t, remaining, pool)
}
func TestSetTransfersAllAvailableInteractiveQuota(t *testing.T) {
	q := ledger(t, 100)
	if err := q.Set("workflow", 100); err != nil {
		t.Fatal(err)
	}
	near(t, bucket(t, q, "workflow").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 0)
	invariant(t, q)
	if _, err := q.Reserve("unregistered", 100); err == nil {
		t.Fatal("interactive borrowed protected quota")
	}
}
func TestReplenishmentFollowsDeficitsAndFullBucketsGetNothing(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("x", 100)
	q.mu.Lock()
	q.state.Buckets["x"].Remaining = 20
	q.state.Buckets[""].Remaining = 130
	q.state.Accounts["a"].Remaining = 150
	q.reconcileLocked(250)
	q.state.Accounts["a"].Remaining = 250
	q.mu.Unlock()
	near(t, bucket(t, q, "x").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 150)
	invariant(t, q)
	q.mu.Lock()
	q.state.Buckets[""].Remaining = 50
	q.state.Accounts["a"].Remaining = 150
	q.reconcileLocked(250)
	q.state.Accounts["a"].Remaining = 250
	q.mu.Unlock()
	near(t, bucket(t, q, "x").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 150)
}
func TestDownwardReconciliationFollowsRemaining(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("x", 100)
	q.mu.Lock()
	q.state.Accounts["a"].Remaining = 125
	q.reconcileLocked(125)
	q.mu.Unlock()
	near(t, bucket(t, q, "x").Remaining, 50)
	near(t, bucket(t, q, "").Remaining, 75)
	invariant(t, q)
}
func TestSkimUsesAbsolutePointsAndCapWeights(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("x", 100)
	_ = q.Set("y", 50)
	transferred, err := q.Skim(10)
	if err != nil {
		t.Fatal(err)
	}
	near(t, transferred, 10)
	near(t, bucket(t, q, "x").Remaining, 100-20.0/3)
	near(t, bucket(t, q, "y").Remaining, 50-10.0/3)
	near(t, bucket(t, q, "").Remaining, 110)
	near(t, bucket(t, q, "x").Cap, 100)
	invariant(t, q)
}
func TestSkimRedistributesDryDonorAndReportsShortfall(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("x", 200)
	_ = q.Set("y", 50)
	q.mu.Lock()
	q.state.Buckets["x"].Remaining = 2
	q.state.Accounts["a"].Remaining = 52
	q.mu.Unlock()
	moved, _ := q.Skim(10)
	near(t, moved, 10)
	near(t, bucket(t, q, "x").Remaining, 0)
	near(t, bucket(t, q, "y").Remaining, 42)
	moved, _ = q.Skim(100)
	near(t, moved, 42)
	invariant(t, q)
}
func TestSkimAndCapCommandsCannotTakeHeldQuota(t *testing.T) {
	q := ledger(t, 100)
	_ = q.Set("x", 100)
	r, err := q.Reserve("x", 900000)
	if err != nil {
		t.Fatal(err)
	}
	moved, _ := q.Skim(100)
	near(t, moved, 10)
	near(t, bucket(t, q, "x").Held, 90)
	_ = q.Clear("x")
	near(t, bucket(t, q, "").Remaining, 10)
	r.Close(true)
	near(t, bucket(t, q, "").Remaining, 100)
	invariant(t, q)
}
func TestAtomicAdmissionConcurrent(t *testing.T) {
	q := ledger(t, 5)
	_ = q.Set("x", 5)
	reservations := make(chan *Reservation, 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := q.Reserve("x", 10000); err == nil {
				reservations <- r
			}
		}()
	}
	wg.Wait()
	close(reservations)
	if len(reservations) != 5 {
		t.Fatalf("admitted %d", len(reservations))
	}
	near(t, bucket(t, q, "x").Held, 5)
	for r := range reservations {
		r.Close(true)
	}
	invariant(t, q)
}
func TestCompletionUsesActualUsageAndNoChargeReleasesReservation(t *testing.T) {
	q := ledger(t, 100)
	_ = q.Set("x", 100)
	r, _ := q.Reserve("x", 50000)
	r.Consume("a", 20000, false)
	r.Close(false)
	r.Close(false)
	near(t, bucket(t, q, "x").Remaining, 98)
	near(t, bucket(t, q, "x").Held, 0)
	r, _ = q.Reserve("x", 50000)
	r.Close(true)
	near(t, bucket(t, q, "x").Remaining, 98)
	invariant(t, q)
}
func TestUnknownConsumptionKeepsConservativeCharge(t *testing.T) {
	q := ledger(t, 100)
	r, _ := q.Reserve("", 50000)
	r.Close(false)
	near(t, bucket(t, q, "").Remaining, 95)
	near(t, bucket(t, q, "").Held, 0)
	invariant(t, q)
}
func TestWeeklyExhaustionTerminatesRatherThanLocalWait(t *testing.T) {
	q := ledger(t, 100)
	q.Sync([]Observation{{ID: "a", Capacity: 250, Known: true, Remaining: 100, ObservedAt: time.Now().Add(time.Minute), WeeklyExhausted: true, WeeklyResetAt: time.Now().Add(7 * 24 * time.Hour)}})
	_, err := q.Reserve("", 10000)
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) || !exhausted.Weekly {
		t.Fatal(err)
	}
	near(t, q.Snapshot().TotalRemaining, 0)
	invariant(t, q)
}
func TestStaleWatermarksDoNotUndoPredictedReset(t *testing.T) {
	q, _ := New("")
	now := time.Now()
	q.now = func() time.Time { return now }
	observation := Observation{ID: "a", Capacity: 250, Known: true, Remaining: 0, ObservedAt: now, ResetAt: now.Add(time.Minute)}
	_ = q.Set("x", 100)
	q.Sync([]Observation{observation})
	near(t, bucket(t, q, "x").Remaining, 0)
	now = now.Add(2 * time.Minute)
	near(t, bucket(t, q, "x").Remaining, 100)
	q.Sync([]Observation{observation})
	near(t, bucket(t, q, "x").Remaining, 100)
	invariant(t, q)
}
func TestCoarseUnchangedWatermarksKeepTokenEstimatesForCalibration(t *testing.T) {
	q, _ := New("")
	now := time.Now()
	o := Observation{ID: "a", Capacity: 250, Known: true, Remaining: 250, ObservedAt: now, ResetAt: now.Add(time.Hour)}
	q.Sync([]Observation{o})
	r, _ := q.Reserve("", 10000)
	r.Consume("a", 1000, false)
	r.Close(false)
	o.ObservedAt = now.Add(time.Minute)
	q.Sync([]Observation{o})
	near(t, q.Snapshot().TotalRemaining, 249.9)
	q.mu.Lock()
	tokens := q.state.Accounts["a"].TokensSinceObservation
	q.mu.Unlock()
	near(t, tokens, 1000)
	o.Remaining = 249
	o.ObservedAt = now.Add(2 * time.Minute)
	q.Sync([]Observation{o})
	q.mu.Lock()
	rate := q.state.Accounts["a"].TokensPerPoint
	q.mu.Unlock()
	near(t, rate, 8200)
	invariant(t, q)
}
func TestPersistedCapsAndCrashReservationsFailConservatively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	q, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	q.Sync([]Observation{{ID: "a", Capacity: 250, Known: true, Remaining: 100, ObservedAt: time.Now()}})
	_ = q.Set("x", 100)
	_, err = q.Reserve("x", 30000)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	near(t, bucket(t, restored, "x").Cap, 100)
	near(t, bucket(t, restored, "x").Remaining, 97)
	near(t, bucket(t, restored, "x").Held, 0)
	invariant(t, restored)
}
func TestCapacityValidationClearAllAndUnknownSessions(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("x", 200)
	if err := q.Set("y", 51); err == nil {
		t.Fatal("caps exceeded pool")
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), -1, 251} {
		if err := q.Set("x", bad); err == nil {
			t.Fatal("accepted invalid cap")
		}
	}
	if err := q.ClearAll(); err != nil {
		t.Fatal(err)
	}
	near(t, bucket(t, q, "").Cap, 250)
	near(t, bucket(t, q, "").Remaining, 250)
	r, err := q.Reserve("unregistered", 10000)
	if err != nil {
		t.Fatal(err)
	}
	r.Close(true)
	invariant(t, q)
}
func TestWeeklyBlockedAccountExcludedWithoutRemovingOtherAccounts(t *testing.T) {
	q, _ := New("")
	now := time.Now()
	q.Sync([]Observation{{ID: "a", Capacity: 100, Known: true, Remaining: 100, ObservedAt: now, WeeklyExhausted: true}, {ID: "b", Capacity: 100, Known: true, Remaining: 50, ObservedAt: now}})
	near(t, q.Snapshot().TotalRemaining, 50)
	if q.Snapshot().AllWeeklyExhausted {
		t.Fatal("weekly status included usable account")
	}
	invariant(t, q)
}
func TestAccountCapNormalizationDoesNotHideConsumption(t *testing.T) {
	q, _ := New("")
	now := time.Now()
	q.Sync([]Observation{{ID: "a", Capacity: 100, Known: true, Remaining: 100, ObservedAt: now}, {ID: "b", Capacity: 100, Known: true, Remaining: 100, ObservedAt: now}, {ID: "c", Capacity: 100, Known: true, Remaining: 100, ObservedAt: now}})
	near(t, q.Snapshot().TotalRemaining, 250)
	r, _ := q.Reserve("", 10000)
	r.Consume("a", 10000, false)
	r.Close(false)
	near(t, q.Snapshot().TotalRemaining, 249)
	invariant(t, q)
}
func TestTokenSubsetsAreNotDoubleCounted(t *testing.T) {
	near(t, WeightedTokens(100, 50, 80, 80, 0, 40), 228)
	near(t, WeightedTokens(0, 0, 0, 0, 0, 25), 100)
	if EstimateTokens([]byte(`{"input":"hi","max_output_tokens":100}`)) >= EstimateTokens([]byte(`{"input":"hi","max_output_tokens":200}`)) {
		t.Fatal("output allowance ignored")
	}
}

func TestAdmissionFailureReportsRequiredPoints(t *testing.T) {
	q := ledger(t, 0.25)
	_, err := q.Reserve("unregistered", DefaultTokensPerPoint)
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected provisioned quota error, got %v", err)
	}
	near(t, exhausted.Needed, 1)
	var body struct {
		Error struct {
			Code     string  `json:"code"`
			Required float64 `json:"required_points"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(exhausted.Error()), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "provisioned_quota_exhausted" {
		t.Fatal(body)
	}
	near(t, body.Error.Required, 1)
	invariant(t, q)
}

func TestQuotaResponseHeadersRetainAdmissionIdentity(t *testing.T) {
	for _, weekly := range []bool{false, true} {
		err := &ExhaustedError{Weekly: weekly, RetryAfter: 60, Needed: 0.125}
		code := "provisioned_quota_exhausted"
		if weekly {
			code = "weekly_quota_exhausted"
		}
		if got := err.Headers().Get("X-CLIProxyAPI-Quota-Code"); got != code {
			t.Fatalf("code %q, want %q", got, code)
		}
		if got := err.Headers().Get("X-CLIProxyAPI-Quota-Required-Points"); got != "0.125" {
			t.Fatalf("required points %q", got)
		}
	}
}
