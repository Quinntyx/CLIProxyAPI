package config

import (
	"encoding/json"
	"testing"
)

const modelRoutesV8YAML = `config-version: 8
model-routes:
  - alias: my-custom-model
    targets:
      - provider: OpenCode-Go
        model: mimo-v2.6-pro
      - provider: openrouter
        model: z-ai/glm-5.3-flash
        params:
          provider:
            only: [wafer, relace]
            allow_fallbacks: false
  - alias: my-custom-model:fast
    targets:
      - provider: inco
        model: deepseek-v4.1-flash:fast
  - alias: my-custom-model
    targets:
      - provider: ignored
        model: duplicate
  - alias: no-targets
    targets: []
`

func TestModelRoutesLoadFromV8AndPassStrictValidation(t *testing.T) {
	if err := ValidateV8Config([]byte(modelRoutesV8YAML)); err != nil {
		t.Fatalf("ValidateV8Config: %v", err)
	}
	cfg, err := ParseConfigBytes([]byte(modelRoutesV8YAML))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if len(cfg.ModelRoutes) != 2 {
		t.Fatalf("routes = %+v, want 2 after normalization", cfg.ModelRoutes)
	}
	route, ok := cfg.LookupModelRoute("my-custom-model")
	if !ok || len(route.Targets) != 2 || route.Targets[0].Provider != "opencode-go" || route.Targets[1].Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("base route = %+v", route)
	}
	params, err := json.Marshal(route.Targets[1].Params)
	if err != nil || string(params) != `{"provider":{"allow_fallbacks":false,"only":["wafer","relace"]}}` {
		t.Fatalf("params = %s (%v)", params, err)
	}
	fast, ok := cfg.LookupModelRoute("my-custom-model:fast")
	if !ok || len(fast.Targets) != 1 || fast.Targets[0].Provider != "inco" {
		t.Fatalf("colon route = %+v", fast)
	}
	if _, ok := cfg.LookupModelRoute("no-targets"); ok {
		t.Fatal("route without targets must be dropped")
	}
}
