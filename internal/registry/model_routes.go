package registry

import (
	"strings"
	"time"
)

// openAICompatibleProviderPrefix mirrors util.OpenAICompatibleProviderKey; util
// imports this package, so the prefix is repeated here instead of imported.
const openAICompatibleProviderPrefix = "openai-compatible-"

// ModelRouteTarget is the catalog view of one configured route target.
type ModelRouteTarget struct {
	Provider string
	Model    string
}

// ModelRoute is the catalog view of an exposed alias and its ordered targets.
type ModelRoute struct {
	Alias   string
	Targets []ModelRouteTarget
}

// SetModelRoutes replaces the configured route aliases projected into catalogs.
// An alias is listed while at least one of its targets is available.
func (r *ModelRegistry) SetModelRoutes(routes []ModelRoute) {
	cloned := make([]ModelRoute, 0, len(routes))
	for _, route := range routes {
		alias := strings.TrimSpace(route.Alias)
		if alias == "" || len(route.Targets) == 0 {
			continue
		}
		cloned = append(cloned, ModelRoute{Alias: alias, Targets: append([]ModelRouteTarget(nil), route.Targets...)})
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.modelRoutes = cloned
	r.invalidateAvailableModelsCacheLocked()
}

// ModelRouteProviderKeys returns the registry provider keys a configured
// provider id may refer to, preferring an OpenAI-compatible provider name.
func ModelRouteProviderKeys(provider string) []string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil
	}
	if strings.HasPrefix(provider, openAICompatibleProviderPrefix) {
		return []string{provider}
	}
	return []string{openAICompatibleProviderPrefix + provider, provider}
}

// routeAliasesLocked returns the configured aliases for collision checks.
func (r *ModelRegistry) routeAliasesLocked() map[string]struct{} {
	if len(r.modelRoutes) == 0 {
		return nil
	}
	aliases := make(map[string]struct{}, len(r.modelRoutes))
	for _, route := range r.modelRoutes {
		aliases[route.Alias] = struct{}{}
	}
	return aliases
}

// routeModelInfoLocked projects the first available target as the alias entry.
func (r *ModelRegistry) routeModelInfoLocked(route ModelRoute, now time.Time) *ModelInfo {
	for _, target := range route.Targets {
		registration := r.models[strings.TrimSpace(target.Model)]
		if available, _ := modelRegistrationAvailability(registration, now); !available {
			continue
		}
		for _, key := range ModelRouteProviderKeys(target.Provider) {
			if registration.Providers[key] <= 0 {
				continue
			}
			info := registration.Info
			if providerInfo := registration.InfoByProvider[key]; providerInfo != nil {
				info = providerInfo
			}
			if info == nil {
				continue
			}
			projected := cloneModelInfo(info)
			projected.ID = route.Alias
			projected.DisplayName = route.Alias
			if projected.Name != "" {
				projected.Name = route.Alias
			}
			return projected
		}
	}
	return nil
}
