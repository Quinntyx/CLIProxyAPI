package sessionquota

import "testing"

func TestSkimToRegisteredBucketUsesOtherBucketsWithoutEstimates(t *testing.T) {
	q := ledger(t, 250)
	_ = q.Set("Mysticism", 100)
	_ = q.Set("deus-runtime", 100)
	hold, err := q.Reserve("deus-runtime", 950000)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := q.SkimTo("Mysticism", 10)
	if err != nil {
		t.Fatal(err)
	}
	near(t, moved, 10)
	near(t, bucket(t, q, "Mysticism").Remaining, 110)
	near(t, bucket(t, q, "deus-runtime").Remaining, 100-20.0/3)
	near(t, bucket(t, q, "deus-runtime").Held, 0)
	near(t, bucket(t, q, "").Remaining, 50-10.0/3)
	near(t, bucket(t, q, "Mysticism").Cap, 100)
	invariant(t, q)
	hold.Close(true)
}
func TestSkimUnregisteredDestinationFallsBackToInteractive(t *testing.T) {
	q := ledger(t, 100)
	_ = q.Set("x", 100)
	moved, err := q.SkimTo("unregistered", 10)
	if err != nil {
		t.Fatal(err)
	}
	near(t, moved, 10)
	near(t, bucket(t, q, "").Remaining, 10)
	near(t, bucket(t, q, "x").Remaining, 90)
	invariant(t, q)
}
