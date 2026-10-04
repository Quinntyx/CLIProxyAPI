package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestResetPressureRoutingSelector(t *testing.T) {
	for _, input := range []string{"reset-pressure", "resetpressure", "rp", " RESET-PRESSURE "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: input}})
		if state.strategy != "reset-pressure" {
			t.Fatalf("strategy(%q) = %q", input, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.ResetPressureSelector); !ok {
			t.Fatalf("selector = %T, want *auth.ResetPressureSelector", newRoutingSelector(state))
		}
		state.sessionAffinity = true
		if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
			t.Fatalf("affinity wrapper = %T", newRoutingSelector(state))
		}
	}
}
