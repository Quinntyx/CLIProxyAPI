package sessionquota

import (
	"math"
	"strings"
	"testing"
)

func TestCachedUsageHintStillReservesFullOutputAndReconciles(t *testing.T) {
	body := []byte(`{"messages":[{"content":"` + strings.Repeat("a", 300000) + `"}],"max_tokens":16384}`)
	cold := EstimateTokens(body)
	warm := EstimateTokensWithHints(body, 91386, 78912)
	near(t, warm, 91386-0.9*78912+4*16384+256)
	if warm >= cold {
		t.Fatal("cached prefix did not reduce admission estimate")
	}
	q := ledger(t, 2.7)
	q.state.Accounts["a"].TokensPerPoint = 39599.804
	if _, err := q.Reserve("", cold); err == nil {
		t.Fatal("cold estimate unexpectedly fit")
	}
	r, err := q.Reserve("", warm)
	if err != nil {
		t.Fatalf("warm request stranded despite enough quota: %v", err)
	}
	actual := WeightedTokens(87890, 1496, 87680, 0, 0, 0)
	r.Consume("a", actual, false)
	r.Close(false)
	near(t, bucket(t, q, "").Held, 0)
	near(t, bucket(t, q, "").Remaining, 2.7-actual/39599.804)
	invariant(t, q)
}

func TestMalformedUsageHintsDoNotCreateCredit(t *testing.T) {
	body := []byte(`{"input":"hi","max_output_tokens":10}`)
	original := EstimateTokens(body)
	for _, hint := range []float64{0, -1, math.NaN(), math.Inf(1), 10000001} {
		near(t, EstimateTokensWithHints(body, hint, 1000000), original)
	}
	hinted := EstimateTokensWithHints(body, 5, 1000000)
	near(t, hinted, 0.1*5+40+256)
	near(t, EstimateTokensWithHints(body, 5, math.NaN()), 5+40+256)
}

func TestSkimToRegisteredBucketUsesOtherBucketsAndProtectsHeld(t *testing.T) {
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
	near(t, bucket(t, q, "deus-runtime").Remaining, 95)
	near(t, bucket(t, q, "deus-runtime").Held, 95)
	near(t, bucket(t, q, "").Remaining, 45)
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
