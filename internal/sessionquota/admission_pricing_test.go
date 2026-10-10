package sessionquota

import (
	"testing"
)

func TestAdmissionUsesLearnedRateFromUsableAccounts(t *testing.T) {
	q, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	q.Sync([]Observation{
		{ID: "usable", Capacity: 100, Remaining: 49, Known: true},
		{ID: "empty", Capacity: 100, Remaining: 0, Known: true},
		{ID: "weekly", Capacity: 50, Remaining: 50, Known: true, WeeklyExhausted: true},
	})
	q.mu.Lock()
	q.state.Accounts["usable"].TokensPerPoint = 50000
	q.state.Accounts["empty"].TokensPerPoint = 1000
	q.state.Accounts["weekly"].TokensPerPoint = 1000
	q.mu.Unlock()
	r, err := q.Reserve("", 494532)
	if err != nil {
		t.Fatalf("live account can afford this calibrated request: %v", err)
	}
	defer r.Close(true)
	near(t, r.held, 494532.0/50000)
	invariant(t, q)
}

func TestAdmissionKeepsWorstUsableRateAndDefaultFallback(t *testing.T) {
	q, _ := New("")
	q.Sync([]Observation{{ID: "efficient", Capacity: 100, Remaining: 100, Known: true}, {ID: "expensive", Capacity: 100, Remaining: 100, Known: true}})
	q.mu.Lock()
	q.state.Accounts["efficient"].TokensPerPoint = 50000
	q.state.Accounts["expensive"].TokensPerPoint = 20000
	q.mu.Unlock()
	r, err := q.Reserve("", 100000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, r.held, 5)
	r.Close(true)
	q.mu.Lock()
	for _, a := range q.state.Accounts {
		a.TokensPerPoint = 0
	}
	q.mu.Unlock()
	r, err = q.Reserve("", DefaultTokensPerPoint)
	if err != nil {
		t.Fatal(err)
	}
	near(t, r.held, 1)
	r.Close(true)
	invariant(t, q)
}
