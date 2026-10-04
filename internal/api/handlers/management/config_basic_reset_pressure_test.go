package management

import "testing"

func TestNormalizeRoutingStrategyResetPressure(t *testing.T) {
	for _, input := range []string{"reset-pressure", "resetpressure", "rp", " RESET-PRESSURE "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "reset-pressure" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v", input, got, ok)
		}
	}
}
