package config

import "strings"

// ModelRoute exposes one client-visible model slug backed by an ordered target list.
// Targets are tried in order; the proxy has no other understanding of the alias.
type ModelRoute struct {
	// Alias is the exposed model id clients request and see in model catalogs.
	Alias string `yaml:"alias" json:"alias"`
	// Targets are tried in order until one starts producing output.
	Targets []ModelRouteTarget `yaml:"targets" json:"targets"`
}

// ModelRouteTarget selects a configured provider and the model id it serves.
type ModelRouteTarget struct {
	// Provider is a configured provider id: an openai-compatibility name
	// (for example "openrouter") or a built-in provider key (for example "claude").
	Provider string `yaml:"provider" json:"provider"`
	// Model is the model id served by that provider.
	Model string `yaml:"model" json:"model"`
	// Params sets payload JSON paths for this target on OpenAI-compatible providers.
	// User payload rules are still applied afterwards and win on conflicts.
	Params map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
}

// NormalizeModelRoutes trims entries, drops incomplete ones and keeps the first
// definition of a duplicated alias.
func NormalizeModelRoutes(routes []ModelRoute) []ModelRoute {
	if len(routes) == 0 {
		return nil
	}
	out := make([]ModelRoute, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		alias := strings.TrimSpace(route.Alias)
		if alias == "" {
			continue
		}
		if _, dup := seen[alias]; dup {
			continue
		}
		targets := make([]ModelRouteTarget, 0, len(route.Targets))
		for _, target := range route.Targets {
			target.Provider = strings.ToLower(strings.TrimSpace(target.Provider))
			target.Model = strings.TrimSpace(target.Model)
			if target.Provider == "" || target.Model == "" {
				continue
			}
			targets = append(targets, target)
		}
		if len(targets) == 0 {
			continue
		}
		seen[alias] = struct{}{}
		out = append(out, ModelRoute{Alias: alias, Targets: targets})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LookupModelRoute returns the route configured for an exposed model id.
func (cfg *SDKConfig) LookupModelRoute(model string) (ModelRoute, bool) {
	if cfg == nil {
		return ModelRoute{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelRoute{}, false
	}
	for _, route := range cfg.ModelRoutes {
		if strings.TrimSpace(route.Alias) == model && len(route.Targets) > 0 {
			return route, true
		}
	}
	return ModelRoute{}, false
}
