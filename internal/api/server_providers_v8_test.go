package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type usageStub struct {
	mu      sync.Mutex
	seen    []string
	handler func(w http.ResponseWriter, r *http.Request)
}

func newUsageStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *usageStub) {
	stub := &usageStub{handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.seen = append(stub.seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		stub.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, stub
}

func compatAuth(id, name, baseURL, key string) *coreauth.Auth {
	providerKey := "openai-compatible-" + name
	return &coreauth.Auth{ID: id, Provider: providerKey, Label: name, Status: coreauth.StatusActive, Attributes: map[string]string{
		"api_key": key, "base_url": baseURL, "compat_name": name, "provider_key": providerKey,
	}}
}

type providersFixture struct {
	server     *Server
	mistral    *usageStub
	openRouter *usageStub
}

func newProvidersFixture(t *testing.T) *providersFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mistralServer, mistral := newUsageStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/billing/budget":
			_, _ = w.Write([]byte(`{"vibe_budget":{"usage_percentage":40,"initial_budget":20,"currency":"EUR","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},"api_budget":{"usage_percentage":25,"initial_budget":15,"currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":true}}`))
		case "/v1/billing/subscription":
			_, _ = w.Write([]byte(`{"plan_name":"Pro","payg_enabled":true,"vibe_payg_enabled":false,"enterprise":false}`))
		default:
			http.NotFound(w, r)
		}
	})
	goServer, _ := newUsageStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":12.5,"resetsAt":1795000000000},"weekly":{"status":"ok","percent":50,"resetsAt":"2026-11-16T00:00:00Z"},"monthly":{"status":"ok","percent":75}}}`))
	})
	openRouterServer, openRouter := newUsageStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer or-bad-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key sk-or-v1-secretvalue"}}`))
			return
		}
		if r.Header.Get("Authorization") == "Bearer or-limited-key" {
			switch r.URL.Path {
			case "/api/v1/key":
				_, _ = w.Write([]byte(`{"data":{"label":"sk-or-v1-lim...ted","usage":57.5,"usage_daily":0.5,"usage_weekly":1.5,"usage_monthly":7.5,"limit":10,"limit_remaining":2.5,"limit_reset":"monthly","is_free_tier":false}}`))
			case "/api/v1/credits":
				_, _ = w.Write([]byte(`{"data":{"total_credits":100,"total_usage":97}}`))
			default:
				http.NotFound(w, r)
			}
			return
		}
		switch r.URL.Path {
		case "/api/v1/key":
			_, _ = w.Write([]byte(`{"data":{"label":"sk-or-v1-abc...xyz","usage":3.5,"usage_daily":0.5,"usage_weekly":1.5,"usage_monthly":3.5,"limit":null,"limit_remaining":null,"limit_reset":null,"is_free_tier":false}}`))
		case "/api/v1/credits":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	})

	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`config-version: 8
management:
  secret-key: test-password
model-routes:
  - alias: my-custom-model
    targets:
      - provider: opencode-go
        model: mimo-v2.6-pro
      - provider: openrouter
        model: z-ai/glm-5.3-flash
  - alias: my-custom-model:fast
    targets:
      - provider: inco
        model: deepseek-v4.1-flash:fast
api-keys:
  openai-compatibility:
    - name: mistral
      base-url: %[1]s/v1
      keys: [{api-key: mistral-test-key}]
    - name: opencode-go
      base-url: %[2]s/zen/go/v1
      keys: [{api-key: go-test-key}]
    - name: openrouter
      base-url: %[3]s/api/v1
      keys: [{api-key: or-good-key}, {api-key: or-bad-key}, {api-key: or-limited-key}]
    - name: inco
      base-url: https://api.inco.ai/v1
      keys: [{api-key: inco-test-key}]
    - name: featherless
      base-url: https://api.featherless.ai/v1
      keys: [{api-key: featherless-test-key}]
`, mistralServer.URL, goServer.URL, openRouterServer.URL)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = t.TempDir()

	manager := coreauth.NewManager(nil, nil, nil)
	now := time.Now()
	codex := &coreauth.Auth{ID: "codex-oauth", Provider: "codex", Label: "person@example.com", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{
		ObservedAt: now,
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":     "30",
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         fmt.Sprint(now.Add(2 * time.Hour).Unix()),
			"X-Codex-Secondary-Used-Percent":   "60",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       fmt.Sprint(now.Add(48 * time.Hour).Unix()),
		},
	}}
	auths := []*coreauth.Auth{
		compatAuth("mistral-1", "mistral", mistralServer.URL+"/v1", "mistral-test-key"),
		compatAuth("go-1", "opencode-go", goServer.URL+"/zen/go/v1", "go-test-key"),
		compatAuth("or-1", "openrouter", openRouterServer.URL+"/api/v1", "or-good-key"),
		compatAuth("or-2", "openrouter", openRouterServer.URL+"/api/v1", "or-bad-key"),
		compatAuth("or-3", "openrouter", openRouterServer.URL+"/api/v1", "or-limited-key"),
		compatAuth("inco-1", "inco", "https://api.inco.ai/v1", "inco-test-key"),
		codex,
		{ID: "gemini-oauth", Provider: "gemini-cli", Status: coreauth.StatusActive},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("go-1", "openai-compatible-opencode-go", []*registry.ModelInfo{{ID: "mimo-v2.6-pro", Object: "model", OwnedBy: "opencode-go"}})
	reg.RegisterClient("inco-1", "openai-compatible-inco", []*registry.ModelInfo{{ID: "deepseek-v4.1-flash:fast", Object: "model", OwnedBy: "inco"}})
	reg.SetModelRoutes([]registry.ModelRoute{
		{Alias: "my-custom-model", Targets: []registry.ModelRouteTarget{{Provider: "opencode-go", Model: "mimo-v2.6-pro"}, {Provider: "openrouter", Model: "z-ai/glm-5.3-flash"}}},
		{Alias: "my-custom-model:fast", Targets: []registry.ModelRouteTarget{{Provider: "inco", Model: "deepseek-v4.1-flash:fast"}}},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("go-1")
		reg.UnregisterClient("inco-1")
		reg.SetModelRoutes(nil)
	})

	h := management.NewHandler(cfg, path, manager)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	return &providersFixture{server: s, mistral: mistral, openRouter: openRouter}
}

func (f *providersFixture) get(t *testing.T, path string, authorized bool) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	if authorized {
		req.Header.Set("Authorization", "Bearer test-password")
	}
	recorder := httptest.NewRecorder()
	f.server.engine.ServeHTTP(recorder, req)
	return recorder.Code, recorder.Body.Bytes()
}

func assertNoSecrets(t *testing.T, body []byte) {
	t.Helper()
	for _, secret := range []string{"mistral-test-key", "go-test-key", "or-good-key", "or-bad-key", "or-limited-key", "inco-test-key", "featherless-test-key", "sk-or-v1"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("response leaks %q: %s", secret, body)
		}
	}
}

func TestManagementV8ProviderDiscoveryRequiresManagementKey(t *testing.T) {
	f := newProvidersFixture(t)
	for _, path := range []string{"/v8/management/providers", "/v8/management/providers/mistral/usage", "/v8/management/models"} {
		if code, body := f.get(t, path, false); code != http.StatusUnauthorized {
			t.Fatalf("%s without key: status=%d body=%s", path, code, body)
		}
	}
	if len(f.mistral.seen) != 0 {
		t.Fatalf("unauthenticated request reached the vendor: %v", f.mistral.seen)
	}
}

func TestManagementV8ProvidersContract(t *testing.T) {
	f := newProvidersFixture(t)
	code, body := f.get(t, "/v8/management/providers", true)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	assertNoSecrets(t, body)
	var resp struct {
		Providers []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Disabled       *bool    `json:"disabled"`
			ModelIDs       []string `json:"model_ids"`
			UsageSupported *bool    `json:"usage_supported"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, provider := range resp.Providers {
		if provider.Disabled == nil || provider.UsageSupported == nil || provider.ModelIDs == nil || provider.Name == "" {
			t.Fatalf("provider missing required fields: %s", body)
		}
		byID[provider.ID] = i
	}
	want := map[string]bool{"mistral": true, "opencode-go": true, "openrouter": true, "codex": true, "inco": false, "featherless": false, "gemini-cli": false}
	for id, supported := range want {
		i, ok := byID[id]
		if !ok {
			t.Fatalf("provider %q missing: %s", id, body)
		}
		if *resp.Providers[i].UsageSupported != supported {
			t.Fatalf("%s usage_supported=%v want %v", id, *resp.Providers[i].UsageSupported, supported)
		}
	}
	goModels := strings.Join(resp.Providers[byID["opencode-go"]].ModelIDs, ",")
	if goModels != "mimo-v2.6-pro,my-custom-model" {
		t.Fatalf("opencode-go model_ids = %q", goModels)
	}
	if got := strings.Join(resp.Providers[byID["inco"]].ModelIDs, ","); got != "deepseek-v4.1-flash:fast,my-custom-model:fast" {
		t.Fatalf("inco model_ids = %q", got)
	}
	if got := strings.Join(resp.Providers[byID["openrouter"]].ModelIDs, ","); got != "my-custom-model" {
		t.Fatalf("openrouter model_ids = %q", got)
	}
}

// usageResponseWindow is one window of a /v8/management usage response.
type usageResponseWindow struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	UsedPercent *float64 `json:"used_percent"`
	Used        *float64 `json:"used"`
	Limit       *float64 `json:"limit"`
	Remaining   *float64 `json:"remaining"`
	Unit        string   `json:"unit"`
	ResetsAt    string   `json:"resets_at"`
}

type usageResponse struct {
	ProviderID string `json:"provider_id"`
	Accounts   []struct {
		ID        string                `json:"id"`
		Label     string                `json:"label"`
		Status    string                `json:"status"`
		Plan      string                `json:"plan"`
		CheckedAt string                `json:"checked_at"`
		Error     string                `json:"error"`
		Windows   []usageResponseWindow `json:"windows"`
	} `json:"accounts"`
}

func (f *providersFixture) usage(t *testing.T, id string) usageResponse {
	t.Helper()
	code, body := f.get(t, "/v8/management/providers/"+id+"/usage", true)
	if code != http.StatusOK {
		t.Fatalf("%s usage status=%d body=%s", id, code, body)
	}
	assertNoSecrets(t, body)
	if !strings.Contains(string(body), `"windows":[`) {
		t.Fatalf("windows must always be an array: %s", body)
	}
	var resp usageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ProviderID != id {
		t.Fatalf("provider_id=%q", resp.ProviderID)
	}
	for _, account := range resp.Accounts {
		if account.ID == "" || account.Status == "" {
			t.Fatalf("account missing id/status: %+v", account)
		}
		if _, err := time.Parse(time.RFC3339, account.CheckedAt); err != nil {
			t.Fatalf("checked_at %q: %v", account.CheckedAt, err)
		}
	}
	return resp
}

func TestManagementV8MistralUsageSeparatesVibeAndAPIBudgets(t *testing.T) {
	f := newProvidersFixture(t)
	resp := f.usage(t, "mistral")
	if len(resp.Accounts) != 1 || resp.Accounts[0].Status != "ok" || resp.Accounts[0].Plan != "Pro" {
		t.Fatalf("accounts = %+v", resp.Accounts)
	}
	windows := resp.Accounts[0].Windows
	if len(windows) != 2 || windows[0].ID != "vibe" || windows[1].ID != "api" {
		t.Fatalf("windows = %+v", windows)
	}
	api := windows[1]
	if api.Used == nil || *api.Used != 3.75 {
		t.Fatalf("api used = %v", api.Used)
	}
	if api.Remaining == nil || *api.Remaining != 11.25 {
		t.Fatalf("api remaining = %v", api.Remaining)
	}
	if api.Unit != "usd" || api.ResetsAt != "2026-12-01T00:00:00Z" {
		t.Fatalf("api window = %+v", api)
	}
	if api.Limit != nil || api.UsedPercent != nil {
		t.Fatalf("api window must be money: %+v", api)
	}
	vibe := windows[0]
	if vibe.UsedPercent == nil || *vibe.UsedPercent != 40 {
		t.Fatalf("vibe used_percent = %v", vibe.UsedPercent)
	}
	if vibe.Used == nil || *vibe.Used != 8 {
		t.Fatalf("vibe used = %v", vibe.Used)
	}
	if vibe.Limit == nil || *vibe.Limit != 20 {
		t.Fatalf("vibe limit = %v", vibe.Limit)
	}
	if vibe.Remaining == nil || *vibe.Remaining != 12 {
		t.Fatalf("vibe remaining = %v", vibe.Remaining)
	}
	if vibe.Unit != "eur" {
		t.Fatalf("vibe window = %+v", vibe)
	}
	code, body := f.get(t, "/v8/management/providers/mistral/usage", true)
	if code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", code, body)
	}
	apiJSON := gjson.GetBytes(body, `accounts.0.windows.#(id=="api")`)
	vibeJSON := gjson.GetBytes(body, `accounts.0.windows.#(id=="vibe")`)
	if !apiJSON.IsObject() || apiJSON.Get("limit").Exists() || apiJSON.Get("used_percent").Exists() {
		t.Fatalf("api window JSON = %s", apiJSON.Raw)
	}
	if !vibeJSON.IsObject() || !vibeJSON.Get("limit").Exists() || !vibeJSON.Get("used_percent").Exists() {
		t.Fatalf("vibe window JSON = %s", vibeJSON.Raw)
	}
	for _, seen := range f.mistral.seen {
		if !strings.HasSuffix(seen, " Bearer mistral-test-key") || !strings.HasPrefix(seen, "/v1/billing/") {
			t.Fatalf("mistral stub saw %q", seen)
		}
	}
}

func TestManagementV8OpenCodeGoUsageWindows(t *testing.T) {
	f := newProvidersFixture(t)
	resp := f.usage(t, "opencode-go")
	windows := resp.Accounts[0].Windows
	if resp.Accounts[0].Status != "ok" || len(windows) != 3 {
		t.Fatalf("account = %+v", resp.Accounts[0])
	}
	if windows[0].ID != "rolling" || *windows[0].UsedPercent != 12.5 || windows[0].ResetsAt != "2026-11-18T11:06:40Z" {
		t.Fatalf("rolling = %+v", windows[0])
	}
	if windows[1].ResetsAt != "2026-11-16T00:00:00Z" || windows[2].ResetsAt != "" || windows[2].Limit != nil {
		t.Fatalf("weekly/monthly = %+v %+v", windows[1], windows[2])
	}
}

func TestManagementV8OpenRouterUsageIsolatesAccountFailures(t *testing.T) {
	f := newProvidersFixture(t)
	resp := f.usage(t, "openrouter")
	if len(resp.Accounts) != 3 {
		t.Fatalf("accounts = %+v", resp.Accounts)
	}
	var ok, unlimited, failed int
	for _, account := range resp.Accounts {
		switch account.Status {
		case "ok":
			ok++
			key := account.Windows[0]
			if key.ID != "key" {
				t.Fatalf("first window must be key spend: %+v", key)
			}
			if key.Limit != nil {
				// A key with a limit reports spend against that limit.
				continue
			}
			unlimited++
			if *key.Used != 3.5 || key.Remaining != nil || key.UsedPercent != nil {
				t.Fatalf("null key limit must stay unknown: %+v", key)
			}
			for _, window := range account.Windows {
				if window.ID == "credits" {
					t.Fatalf("forbidden credits endpoint produced a window: %+v", window)
				}
			}
		case "error":
			failed++
			if len(account.Windows) != 0 || account.Error != "usage endpoint returned HTTP 401" {
				t.Fatalf("failed account = %+v", account)
			}
		}
	}
	if ok != 2 || unlimited != 1 || failed != 1 {
		t.Fatalf("ok=%d unlimited=%d failed=%d accounts=%+v", ok, unlimited, failed, resp.Accounts)
	}
}

func TestManagementV8OpenRouterCreditsAreBalanceNotQuota(t *testing.T) {
	f := newProvidersFixture(t)
	resp := f.usage(t, "openrouter")
	var limited int
	for _, account := range resp.Accounts {
		var key, credits *usageResponseWindow
		for i := range account.Windows {
			window := &account.Windows[i]
			switch window.ID {
			case "key":
				key = window
			case "credits":
				credits = window
			}
		}
		if key == nil || key.Limit == nil {
			continue
		}
		limited++
		if *key.Used != 7.5 || *key.Limit != 10 || key.Remaining == nil || *key.Remaining != 2.5 || key.UsedPercent == nil || *key.UsedPercent != 75 || key.Unit != "usd" {
			t.Fatalf("limited key window = %+v", *key)
		}
		if credits == nil {
			t.Fatalf("readable credits endpoint must produce a window: %+v", account)
		}
		if credits.Used == nil || *credits.Used != 97 || credits.Remaining == nil || *credits.Remaining != 3 || credits.Unit != "usd" {
			t.Fatalf("credits window = %+v", *credits)
		}
		if credits.Limit != nil || credits.UsedPercent != nil {
			t.Fatalf("credits are a balance, not a quota: %+v", *credits)
		}
	}
	if limited != 1 {
		t.Fatalf("limited=%d accounts=%+v", limited, resp.Accounts)
	}

	// The wire form must not carry a limit or percentage on the balance window.
	code, body := f.get(t, "/v8/management/providers/openrouter/usage", true)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	assertNoSecrets(t, body)
	var found bool
	for _, account := range gjson.GetBytes(body, "accounts").Array() {
		for _, window := range account.Get("windows").Array() {
			if window.Get("id").String() != "credits" {
				continue
			}
			found = true
			if window.Get("limit").Exists() || window.Get("used_percent").Exists() {
				t.Fatalf("credits window exposes an allowance: %s", window.Raw)
			}
		}
	}
	if !found {
		t.Fatalf("no credits window in body: %s", body)
	}
}

func TestManagementV8UnsupportedAndNativeUsage(t *testing.T) {
	f := newProvidersFixture(t)
	inco := f.usage(t, "inco")
	if len(inco.Accounts) != 1 || inco.Accounts[0].Status != "unsupported" || len(inco.Accounts[0].Windows) != 0 {
		t.Fatalf("inco = %+v", inco)
	}
	featherless := f.usage(t, "featherless")
	if featherless.Accounts[0].Status != "unsupported" {
		t.Fatalf("featherless = %+v", featherless)
	}
	codex := f.usage(t, "codex")
	windows := codex.Accounts[0].Windows
	if codex.Accounts[0].Status != "ok" || len(windows) != 2 || windows[0].ID != "five-hour" || *windows[0].UsedPercent != 30 || windows[1].ID != "weekly" || *windows[1].UsedPercent != 60 {
		t.Fatalf("codex = %+v", codex)
	}
	gemini := f.usage(t, "gemini-cli")
	if gemini.Accounts[0].Status != "unsupported" {
		t.Fatalf("gemini = %+v", gemini)
	}
	if code, _ := f.get(t, "/v8/management/providers/does-not-exist/usage", true); code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d", code)
	}
}

func TestManagementV8ModelsListsRouteAliases(t *testing.T) {
	f := newProvidersFixture(t)
	code, body := f.get(t, "/v8/management/models", true)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, model := range resp.Data {
		id, _ := model["id"].(string)
		ids[id] = true
		if model["object"] != "model" {
			t.Fatalf("model entry = %v", model)
		}
	}
	if resp.Object != "list" || !ids["my-custom-model"] || !ids["my-custom-model:fast"] || !ids["mimo-v2.6-pro"] {
		t.Fatalf("models = %s", body)
	}
}
