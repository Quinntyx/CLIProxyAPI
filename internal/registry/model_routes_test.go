package registry

import (
	"strings"
	"testing"
)

func availableIDs(r *ModelRegistry, handlerType string) map[string]map[string]any {
	out := make(map[string]map[string]any)
	for _, model := range r.GetAvailableModels(handlerType) {
		id, _ := model["id"].(string)
		if id == "" {
			name, _ := model["name"].(string)
			id = strings.TrimPrefix(name, "models/")
		}
		out[id] = model
	}
	return out
}

func TestModelRoutesProjectAvailableAliasesIntoCatalog(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("go-key", "openai-compatible-opencode-go", []*ModelInfo{{ID: "mimo-v2.6-pro", Object: "model", OwnedBy: "opencode-go", Created: 1}})
	r.RegisterClient("inco-key", "openai-compatible-inco", []*ModelInfo{{ID: "deepseek-v4.1-flash:fast", Object: "model", OwnedBy: "inco"}})
	r.SetModelRoutes([]ModelRoute{
		{Alias: "my-custom-model", Targets: []ModelRouteTarget{{Provider: "opencode-go", Model: "mimo-v2.6-pro"}, {Provider: "openrouter", Model: "z-ai/glm-5.3-flash"}}},
		{Alias: "my-custom-model:fast", Targets: []ModelRouteTarget{{Provider: "inco", Model: "deepseek-v4.1-flash:fast"}}},
		{Alias: "unavailable-route", Targets: []ModelRouteTarget{{Provider: "openrouter", Model: "z-ai/glm-5.3-flash"}}},
		{Alias: "wrong-provider-route", Targets: []ModelRouteTarget{{Provider: "inco", Model: "mimo-v2.6-pro"}}},
	})

	for _, handlerType := range []string{"openai", "claude", "gemini"} {
		ids := availableIDs(r, handlerType)
		for _, want := range []string{"my-custom-model", "my-custom-model:fast", "mimo-v2.6-pro"} {
			if _, ok := ids[want]; !ok {
				t.Fatalf("%s catalog missing %q: %v", handlerType, want, ids)
			}
		}
		for _, hidden := range []string{"unavailable-route", "wrong-provider-route"} {
			if _, ok := ids[hidden]; ok {
				t.Fatalf("%s catalog lists %q without an available target", handlerType, hidden)
			}
		}
	}
	openai := availableIDs(r, "openai")
	if openai["my-custom-model"]["object"] != "model" || openai["my-custom-model"]["owned_by"] != "opencode-go" {
		t.Fatalf("projected entry = %v", openai["my-custom-model"])
	}
	for key := range openai["my-custom-model:fast"] {
		if key == "fast" || key == "speed" || key == "service_tier" {
			t.Fatalf("colon alias gained speed metadata %q", key)
		}
	}

	// Targets becoming unavailable remove the alias; the cache must not keep it.
	r.UnregisterClient("inco-key")
	if _, ok := availableIDs(r, "openai")["my-custom-model:fast"]; ok {
		t.Fatal("alias remained listed after its only target was unregistered")
	}
}

func TestModelRoutesOwnCollidingModelIDs(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("mistral-key", "openai-compatible-mistral", []*ModelInfo{{ID: "deepseek-v4-1-flash-1017", OwnedBy: "mistral"}})
	r.RegisterClient("or-key", "openai-compatible-openrouter", []*ModelInfo{{ID: "shared-id", OwnedBy: "openrouter"}})
	r.SetModelRoutes([]ModelRoute{{Alias: "shared-id", Targets: []ModelRouteTarget{{Provider: "mistral", Model: "deepseek-v4-1-flash-1017"}}}})

	count := 0
	for _, model := range r.GetAvailableModels("openai") {
		if model["id"] == "shared-id" {
			count++
			if model["owned_by"] != "mistral" {
				t.Fatalf("colliding id kept the registered entry: %v", model)
			}
		}
	}
	if count != 1 {
		t.Fatalf("shared-id listed %d times", count)
	}
}
