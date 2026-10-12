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
	for _, hint := range []float64{0, -1, 5, math.NaN(), math.Inf(1), 10000001} {
		near(t, EstimateTokensWithHints(body, hint, 1000000), original)
	}
}

func TestRawHistoryFallbackHintCannotInflateOutboundPayload(t *testing.T) {
	body := []byte(`{"input":"` + strings.Repeat("x", 810000) + `","max_output_tokens":100}`)
	cold := EstimateTokens(body)
	near(t, EstimateTokensWithHints(body, 3000000, 2900000), cold)
}

func TestEncryptedReplayRejectsRaw3MFallbackAndAcceptsObserved272K(t *testing.T) {
	body := []byte(`{"input":"` + strings.Repeat("x", 810000) + `","reasoning":{"encrypted_content":"` + strings.Repeat("z", 8190000) + `"},"max_output_tokens":100}`)
	cold := EstimateTokens(body)
	if cold < 500000 || cold > 600000 {
		t.Fatalf("guarded opaque fallback = %.0f, want 500k-600k", cold)
	}
	// A raw-history projection is not an observation of the serialized request
	// and must not recreate the old ~3M-token impossible admission.
	near(t, EstimateTokensWithHints(body, 3000000, 2900000), cold)

	warm := EstimateTokensWithHints(body, 272000, 270000)
	near(t, warm, 272000-0.9*270000+400+256)
	if warm >= cold {
		t.Fatal("cache-aware 272k observation did not replace the guarded fallback")
	}
}

func TestCacheHintMustMatchVisibleOutboundPayload(t *testing.T) {
	body := []byte(`{"input":"` + strings.Repeat("a", 300000) + `","max_output_tokens":10}`)
	cold := EstimateTokens(body)
	if got := EstimateTokensWithHints(body, 10, 10); got != cold {
		t.Fatalf("untrusted tiny hint reduced estimate: got %v want %v", got, cold)
	}
	warm := EstimateTokensWithHints(body, 90000, 80000)
	near(t, warm, 90000-0.9*80000+40+256)
	if warm >= cold {
		t.Fatal("plausible cache-aware hint was ignored")
	}
}

func TestOpaqueEncryptedContentDoesNotAuthorizeTinyHint(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"` + strings.Repeat("z", 900000) + `"}],"max_output_tokens":10}`)
	cold := EstimateTokens(body)
	near(t, EstimateTokensWithHints(body, 1000, 1000), cold)
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

func TestHintsNeverIncreasePayloadDerivedAdmission(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"input":"hello","max_output_tokens":10}`),
		[]byte(`{"input":[{"type":"reasoning","encrypted_content":"` + strings.Repeat("z", 900000) + `"}],"max_output_tokens":10}`),
	} {
		cold := EstimateTokens(body)
		for _, input := range []float64{1, 10, 30000, 60000, 100000, math.Ceil(float64(len(body))/3) + 0.5, 3000000} {
			for _, cached := range []float64{0, input / 2, input, input + 1, math.NaN(), math.Inf(1)} {
				if got := EstimateTokensWithHints(body, input, cached); got > cold {
					t.Fatalf("hint inflated estimate: input=%v cached=%v got=%v cold=%v", input, cached, got, cold)
				}
			}
		}
	}
}

func TestOpaqueEstimateRejectsMalformedJSONAndNonStringState(t *testing.T) {
	for _, body := range []string{
		`{"input":"hi","encrypted_content":"` + strings.Repeat("z", 10000) + `"`,
		`{"input":"hi","encrypted_content":{"text":"` + strings.Repeat("z", 10000) + `"},"max_tokens":10}`,
		`{"input":"hi","encrypted_content":["` + strings.Repeat("z", 10000) + `"],"max_tokens":10}`,
	} {
		allowance := 10.0
		if !strings.HasSuffix(body, `,"max_tokens":10}`) {
			allowance = 16384
		}
		near(t, EstimateTokens([]byte(body)), math.Ceil(float64(len(body))/3)+4*allowance+256)
	}
}
