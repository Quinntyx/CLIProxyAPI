package management

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ProviderSummary is one configured upstream provider in GET /v8/management/providers.
type ProviderSummary struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Disabled       bool     `json:"disabled"`
	ModelIDs       []string `json:"model_ids"`
	UsageSupported bool     `json:"usage_supported"`
}

// ProviderUsage is the normalized subscription usage of one provider.
type ProviderUsage struct {
	ProviderID string                 `json:"provider_id"`
	Accounts   []ProviderUsageAccount `json:"accounts"`
}

// Account statuses for ProviderUsageAccount.Status.
const (
	usageStatusOK          = "ok"
	usageStatusUnsupported = "unsupported"
	usageStatusUnavailable = "unavailable"
	usageStatusError       = "error"
)

// ProviderUsageAccount reports one credential. Unknown values are omitted.
type ProviderUsageAccount struct {
	ID        string                `json:"id"`
	Label     string                `json:"label,omitempty"`
	Status    string                `json:"status"`
	Plan      string                `json:"plan,omitempty"`
	CheckedAt string                `json:"checked_at"`
	Windows   []ProviderUsageWindow `json:"windows"`
	Error     string                `json:"error,omitempty"`
}

// ProviderUsageWindow is one distinct vendor allowance window.
type ProviderUsageWindow struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	UsedPercent *float64 `json:"used_percent,omitempty"`
	Used        *float64 `json:"used,omitempty"`
	Limit       *float64 `json:"limit,omitempty"`
	Remaining   *float64 `json:"remaining,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	ResetsAt    string   `json:"resets_at,omitempty"`
}

// configuredProvider groups the configuration and credentials behind one provider id.
type configuredProvider struct {
	id          string
	name        string
	providerKey string
	compat      *config.OpenAICompatibility
	auths       []*coreauth.Auth
}

var nativeProviderNames = map[string]string{
	"aistudio":    "AI Studio",
	"antigravity": "Antigravity",
	"claude":      "Claude",
	"codex":       "Codex",
	"devin":       "Devin",
	"gemini":      "Gemini",
	"gemini-cli":  "Gemini CLI",
	"kimi":        "Kimi",
	"meta":        "Meta",
	"vertex":      "Vertex",
	"xai":         "xAI",
}

// configuredProviders lists OpenAI-compatible providers from configuration and
// every other provider that has registered credentials, keyed by stable id.
func (h *Handler) configuredProviders() []*configuredProvider {
	h.mu.Lock()
	cfg := h.cfg
	manager := h.authManager
	h.mu.Unlock()

	byKey := make(map[string]*configuredProvider)
	var ordered []*configuredProvider
	if cfg != nil {
		for i := range cfg.OpenAICompatibility {
			compat := &cfg.OpenAICompatibility[i]
			id := strings.ToLower(strings.TrimSpace(compat.Name))
			if id == "" {
				continue
			}
			key := util.OpenAICompatibleProviderKey(id)
			if _, exists := byKey[key]; exists {
				continue
			}
			name := strings.TrimSpace(compat.Name)
			provider := &configuredProvider{id: id, name: name, providerKey: key, compat: compat}
			byKey[key] = provider
			ordered = append(ordered, provider)
		}
	}
	if manager != nil {
		for _, auth := range manager.List() {
			if auth == nil {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(auth.Provider))
			if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" && strings.TrimSpace(auth.Attributes["compat_name"]) != "" {
				key = util.OpenAICompatibleProviderKey(providerKey)
			}
			if key == "" || key == "home" {
				continue
			}
			provider := byKey[key]
			if provider == nil {
				if strings.HasPrefix(key, "openai-compatible-") || key == "openai-compatibility" {
					// Credentials for a provider no longer in configuration.
					continue
				}
				name := nativeProviderNames[key]
				if name == "" {
					name = key
				}
				provider = &configuredProvider{id: key, name: name, providerKey: key}
				byKey[key] = provider
				ordered = append(ordered, provider)
			}
			provider.auths = append(provider.auths, auth)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].id < ordered[j].id })
	for _, provider := range ordered {
		sort.SliceStable(provider.auths, func(i, j int) bool { return provider.auths[i].ID < provider.auths[j].ID })
	}
	return ordered
}

func (p *configuredProvider) disabled() bool {
	if p.compat != nil {
		return p.compat.Disabled
	}
	for _, auth := range p.auths {
		if !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			return false
		}
	}
	return true
}

// modelIDs returns exposed proxy model ids served by the provider, including
// configured route aliases with a target on it.
func (p *configuredProvider) modelIDs(routes []config.ModelRoute) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, model := range registry.GetGlobalRegistry().GetAvailableModelsByProvider(p.providerKey) {
		if model != nil {
			add(model.ID)
		}
	}
	for _, route := range routes {
		for _, target := range route.Targets {
			if containsString(registry.ModelRouteProviderKeys(target.Provider), p.providerKey) {
				add(route.Alias)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (h *Handler) modelRoutes() []config.ModelRoute {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return nil
	}
	return h.cfg.ModelRoutes
}

// GetProvidersV8 lists configured upstream providers without credential material.
func (h *Handler) GetProvidersV8(c *gin.Context) {
	routes := h.modelRoutes()
	providers := h.configuredProviders()
	out := make([]ProviderSummary, 0, len(providers))
	for _, provider := range providers {
		out = append(out, ProviderSummary{
			ID:             provider.id,
			Name:           provider.name,
			Disabled:       provider.disabled(),
			ModelIDs:       provider.modelIDs(routes),
			UsageSupported: provider.usageAdapter() != nil,
		})
	}
	c.JSON(http.StatusOK, gin.H{"providers": out})
}

// GetProviderUsageV8 returns normalized subscription usage for one provider.
// Account failures are reported per account and never fail the whole response.
func (h *Handler) GetProviderUsageV8(c *gin.Context) {
	id := strings.ToLower(strings.TrimSpace(c.Param("id")))
	var provider *configuredProvider
	for _, candidate := range h.configuredProviders() {
		if candidate.id == id {
			provider = candidate
			break
		}
	}
	if provider == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}
	now := time.Now().UTC()
	adapter := provider.usageAdapter()
	checkedAt := now.Format(time.RFC3339)
	if len(provider.auths) == 0 {
		account := ProviderUsageAccount{ID: "unconfigured", Status: usageStatusUnavailable, CheckedAt: checkedAt,
			Windows: []ProviderUsageWindow{}, Error: "no active credentials configured"}
		if adapter == nil {
			account.Status, account.Error = usageStatusUnsupported, "usage API not supported for this provider"
		}
		c.JSON(http.StatusOK, ProviderUsage{ProviderID: provider.id, Accounts: []ProviderUsageAccount{account}})
		return
	}
	accounts := make([]ProviderUsageAccount, len(provider.auths))
	var wg sync.WaitGroup
	for i, auth := range provider.auths {
		account := ProviderUsageAccount{ID: auth.EnsureIndex(), Label: accountLabel(provider, auth), CheckedAt: checkedAt, Windows: []ProviderUsageWindow{}}
		switch {
		case adapter == nil:
			account.Status = usageStatusUnsupported
			account.Error = "usage API not supported for this provider"
		case auth.Disabled || auth.Status == coreauth.StatusDisabled:
			account.Status = usageStatusUnavailable
			account.Error = "credential disabled"
		}
		accounts[i] = account
		if account.Status != "" {
			continue
		}
		wg.Add(1)
		go func(i int, auth *coreauth.Auth) {
			defer wg.Done()
			result := adapter(c.Request.Context(), h, provider, auth, now)
			result.ID, result.Label, result.CheckedAt = accounts[i].ID, accounts[i].Label, accounts[i].CheckedAt
			if result.Windows == nil {
				result.Windows = []ProviderUsageWindow{}
			}
			accounts[i] = result
		}(i, auth)
	}
	wg.Wait()
	c.JSON(http.StatusOK, ProviderUsage{ProviderID: provider.id, Accounts: accounts})
}

func accountLabel(provider *configuredProvider, auth *coreauth.Auth) string {
	label := strings.TrimSpace(auth.Label)
	if label == "" || (provider.compat != nil && strings.EqualFold(label, provider.compat.Name)) {
		return ""
	}
	return label
}

// GetModelsV8 returns the exposed proxy model catalog in OpenAI list form,
// using the same registry visibility as /v1/models.
func (h *Handler) GetModelsV8(c *gin.Context) {
	models := registry.GetGlobalRegistry().GetAvailableModels("openai")
	sort.SliceStable(models, func(i, j int) bool {
		left, _ := models[i]["id"].(string)
		right, _ := models[j]["id"].(string)
		return left < right
	})
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
}

// providerBaseURL returns the configured https/http base URL for API-key providers.
func providerBaseURL(provider *configuredProvider, auth *coreauth.Auth) *url.URL {
	base := ""
	if auth != nil {
		base = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if base == "" && provider.compat != nil {
		base = strings.TrimSpace(provider.compat.BaseURL)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil
	}
	return parsed
}
