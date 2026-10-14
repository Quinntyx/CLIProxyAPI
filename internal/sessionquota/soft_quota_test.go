package sessionquota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOverrunPenaltyReducesNextWindowNotOtherAllocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	q, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	q.now = func() time.Time { return now }
	q.Sync([]Observation{{ID: "a", Capacity: 250, Known: true, Remaining: 250, ObservedAt: now, ResetAt: now.Add(time.Hour)}})
	_ = q.Set("work", 10)
	_ = q.Set("other", 100)
	r, _ := q.ReserveRequest("work", false)
	r.ConsumeRecord("actual", "a", 110000, false)
	r.Close(false)
	near(t, bucket(t, q, "work").Debt, 1)
	near(t, bucket(t, q, "other").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 140)
	// Snapshot and unchanged coarse observations cannot forgive the debt.
	o := Observation{ID: "a", Capacity: 250, Known: true, Remaining: 250, ObservedAt: now.Add(time.Minute), ResetAt: now.Add(time.Hour)}
	q.Sync([]Observation{o})
	near(t, bucket(t, q, "work").Debt, 1)
	invariant(t, q)
	restored, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	q = restored
	q.now = func() time.Time { return now }
	near(t, bucket(t, q, "work").Debt, 1)
	now = now.Add(2 * time.Hour)
	near(t, bucket(t, q, "work").Remaining, 9)
	near(t, bucket(t, q, "work").Debt, 0)
	near(t, bucket(t, q, "other").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 141)
	o.ObservedAt = now
	o.ResetAt = now.Add(time.Hour)
	q.Sync([]Observation{o})
	for i := 0; i < 5; i++ {
		near(t, bucket(t, q, "work").Remaining, 9)
		o.ObservedAt = o.ObservedAt.Add(time.Second)
		q.Sync([]Observation{o})
	}
	// Setting the same cap is not a way to steal treasury to erase repayment.
	_ = q.Set("work", 10)
	near(t, bucket(t, q, "work").Remaining, 9)
	restored, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	q = restored
	q.now = func() time.Time { return now }
	near(t, bucket(t, q, "work").Remaining, 9)
	invariant(t, q)
	now = now.Add(2 * time.Hour)
	near(t, bucket(t, q, "work").Remaining, 10)
	near(t, bucket(t, q, "other").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 140)
	invariant(t, q)
}

func TestHugeOverrunTakesMultipleOwnerRefills(t *testing.T) {
	q := ledger(t, 250)
	now := time.Now()
	q.now = func() time.Time { return now }
	_ = q.Set("work", 10)
	_ = q.Set("other", 100)
	r, _ := q.ReserveRequest("work", false)
	r.ConsumeRecord("huge", "a", 3010000, false)
	r.Close(false)
	near(t, bucket(t, q, "work").Debt, 291)
	near(t, bucket(t, q, "other").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 140)
	for i := 0; i < 3; i++ {
		q.mu.Lock()
		q.state.Accounts["a"].ResetAt = now.Add(time.Minute)
		q.mu.Unlock()
		now = now.Add(2 * time.Minute)
		b := bucket(t, q, "work")
		near(t, b.Debt, 291-float64(i+1)*10)
		near(t, b.Remaining, 0)
		near(t, bucket(t, q, "other").Remaining, 100)
		invariant(t, q)
	}
}

func TestConfirmedRolloverCarriesPenaltyAndMultiAccountRefillIsProportional(t *testing.T) {
	q, _ := New("")
	now := time.Now().Truncate(time.Second)
	q.now = func() time.Time { return now }
	observations := []Observation{{ID: "a", Capacity: 125, Known: true, Remaining: 125, ObservedAt: now, ResetAt: now.Add(time.Hour)}, {ID: "b", Capacity: 125, Known: true, Remaining: 125, ObservedAt: now, ResetAt: now.Add(2 * time.Hour)}}
	q.Sync(observations)
	_ = q.Set("work", 10)
	_ = q.Set("other", 100)
	r, _ := q.ReserveRequest("work", false)
	r.ConsumeRecord("actual", "a", 160000, false)
	r.Close(false)
	// Confirmed rollover delivered before the local clock crosses the old reset.
	observations[0].Remaining = 125
	observations[0].ObservedAt = now.Add(time.Minute)
	observations[0].ResetAt = now.Add(6 * time.Hour)
	q.Sync(observations)
	near(t, bucket(t, q, "work").Debt, 1)
	near(t, bucket(t, q, "work").Remaining, 0)
	near(t, bucket(t, q, "other").Remaining, 100)
	now = now.Add(3 * time.Hour)
	near(t, bucket(t, q, "work").Debt, 0)
	near(t, bucket(t, q, "work").Remaining, 4)
	near(t, bucket(t, q, "other").Remaining, 100)
	invariant(t, q)
}

func TestClearSetTargetedSkimAndLateRecordsKeepDebtOwner(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("work", 10)
	_ = q.Set("other", 100)
	r, _ := q.ReserveRequest("work", false)
	r.Close(false)
	if err := q.Clear("work"); err != nil {
		t.Fatal(err)
	}
	r.ConsumeRecord("late", "a", 20000, false)
	r.ConsumeRecord("late", "a", 20000, false)
	near(t, bucket(t, q, "work").Debt, 2)
	near(t, bucket(t, q, "other").Remaining, 100)
	near(t, bucket(t, q, "").Remaining, 150)
	if _, err := q.ReserveRequest("work", false); err == nil {
		t.Fatal("cleared debtor borrowed interactive")
	}
	if err := q.Set("work", 10); err != nil {
		t.Fatal(err)
	}
	near(t, bucket(t, q, "work").Debt, 2)
	near(t, bucket(t, q, "").Remaining, 150)
	moved, err := q.SkimTo("work", 3)
	if err != nil {
		t.Fatal(err)
	}
	near(t, moved, 3)
	near(t, bucket(t, q, "work").Debt, 0)
	near(t, bucket(t, q, "work").Remaining, 1)
	donor := bucket(t, q, "other").Remaining
	if err := q.ClearAll(); err != nil {
		t.Fatal(err)
	}
	r.ConsumeRecord("late", "a", 40000, false)
	near(t, bucket(t, q, "work").Debt, 2)
	if q.state.Buckets["other"].Cap != 0 || donor >= 100 {
		t.Fatal("targeted skim did not explicitly transfer donor credit")
	}
	invariant(t, q)
}

func TestUnknownAbortKnownActualAndConcurrentLateRecordsAreIdempotent(t *testing.T) {
	q := ledger(t, .25)
	r, _ := q.ReserveRequest("", false)
	r.ConsumeRecord("execution", "a", 1000, true)
	r.Close(false)
	near(t, bucket(t, q, "").Remaining, .15)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Close(false)
			r.ConsumeRecord("execution", "a", 5000, false)
			r.ConsumeRecord("execution", "a", 0, true)
		}()
	}
	wg.Wait()
	b := bucket(t, q, "")
	near(t, b.Debt, .25)
	if b.InFlight != 0 {
		t.Fatal(b)
	}
	invariant(t, q)
}

func TestMigrationDropsOldEstimatedHoldsWithoutBilling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1")
	old := state{Version: 1, Buckets: map[string]*Bucket{"": {Cap: 250, Remaining: 100, Held: 20}}, Accounts: map[string]*Account{"a": {ID: "a", Capacity: 250, Remaining: 100, ObservedRemaining: 100, TokensPerPoint: DefaultTokensPerPoint}}}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	q, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	b := bucket(t, q, "")
	near(t, b.Remaining, 100)
	near(t, b.Held, 0)
	if b.InFlight != 0 {
		t.Fatal(b)
	}
	_ = q.Set("work", 10)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var migrated state
	if err = json.Unmarshal(data, &migrated); err != nil || migrated.Version != 2 {
		t.Fatal(err, migrated.Version)
	}
}

func TestForcedBurnSnapshotExpiryAndWeeklyGate(t *testing.T) {
	q := ledger(t, 0)
	now := time.Now()
	q.now = func() time.Time { return now }
	q.SetForcedBurn(ForcedBurn{Eligible: true, ExpiresAt: now.Add(time.Minute)})
	if !bucket(t, q, "").AdmissionReady {
		t.Fatal("eligible burn failed to unpause")
	}
	r, err := q.ReserveRequest("", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Close(false)
	now = now.Add(2 * time.Minute)
	if q.Snapshot().ForcedBurn.Eligible || bucket(t, q, "").AdmissionReady {
		t.Fatal("expired capability remained ready")
	}
	q.Sync([]Observation{{ID: "a", Capacity: 250, Known: true, Remaining: 0, WeeklyExhausted: true, ObservedAt: now}})
	if _, err = q.ReserveRequest("", true); err == nil {
		t.Fatal("burn bypassed weekly exhaustion")
	}
}
