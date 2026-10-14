package sessionquota

import "testing"

func TestReportedUsageUsesLearnedAccountRateOnlyAfterAdmission(t *testing.T) {
	q := ledger(t, .25)
	q.mu.Lock()
	q.state.Accounts["a"].TokensPerPoint = 50000
	q.mu.Unlock()
	r, err := q.Reserve("", 49453200000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, bucket(t, q, "").Remaining, .25)
	r.Consume("a", 50000, false)
	r.Close(false)
	near(t, bucket(t, q, "").Debt, .75)
	invariant(t, q)
}
