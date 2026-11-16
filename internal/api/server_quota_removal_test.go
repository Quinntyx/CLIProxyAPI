package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type quotaFailureExecutor struct {
	codexSearchCaptureExecutor
	err   error
	calls int
}

func (e *quotaFailureExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	return coreexecutor.Response{}, e.err
}

func (e *quotaFailureExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls++
	return nil, e.err
}

func TestCodexQuotaFailureReturnsNormally(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
		for _, stream := range []bool{false, true} {
			for _, window := range []string{"5-hour", "weekly"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, window), func(t *testing.T) {
					server := newTestServer(t)
					manager := server.handlers.AuthManager
					manager.SetRetryConfig(0, 0, 0)
					executor := &quotaFailureExecutor{err: &auth.Error{
						Code: "usage_limit_reached", Message: window + " quota exhausted", HTTPStatus: http.StatusTooManyRequests,
					}}
					manager.RegisterExecutor(executor)
					credential := &auth.Auth{ID: t.Name(), Provider: "codex", Status: auth.StatusActive}
					if _, err := manager.Register(context.Background(), credential); err != nil {
						t.Fatal(err)
					}
					const model = "quota-removal-model"
					registry.GetGlobalRegistry().RegisterClient(credential.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })

					httpServer := httptest.NewServer(server.engine)
					defer httpServer.Close()
					payload := fmt.Sprintf(`{"model":%q,"input":"hi","stream":%t}`, model, stream)
					req, err := http.NewRequest(http.MethodPost, httpServer.URL+path, strings.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer test-key")
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-CLIProxyAPI-Session", "old-tmux-window")
					response, err := httpServer.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = response.Body.Close() }()
					body, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), window+" quota exhausted") || executor.calls != 1 {
						t.Fatalf("quota failure was intercepted or replayed: status=%d calls=%d body=%s", response.StatusCode, executor.calls, body)
					}
					for key := range response.Header {
						if strings.HasPrefix(strings.ToLower(key), "x-cliproxyapi-quota-") {
							t.Errorf("obsolete pause header emitted: %s", key)
						}
					}
				})
			}
		}
	}
}

func TestSessionQuotaEndpointsRemoved(t *testing.T) {
	server := newTestServer(t)
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/session-quota", ""},
		{http.MethodPut, "/v1/session-quota/sessions/window", `{"cap":100}`},
		{http.MethodDelete, "/v1/session-quota/sessions/window", ""},
		{http.MethodDelete, "/v1/session-quota/sessions", ""},
		{http.MethodPost, "/v1/session-quota/skim", `{}`},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			req := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, req)
			if response.Code != http.StatusNotFound {
				t.Fatalf("obsolete endpoint returned %d, want 404: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestLegacyQuotaStateDoesNotBlockRequests(t *testing.T) {
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	const invalidState = "obsolete unreadable session quota ledger"
	statePath := ""
	server := newTestServerWithConfig(t, cfg, WithEngineConfigurator(func(_ *gin.Engine) {
		statePath = filepath.Join(cfg.AuthDir, ".session-quota.state")
		if err := os.WriteFile(statePath, []byte(invalidState), 0o600); err != nil {
			t.Fatal(err)
		}
	}))
	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	for _, request := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/v1/models", "", http.StatusOK},
		{http.MethodPost, "/v1/responses", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/backend-api/codex/responses", `{}`, http.StatusBadRequest},
		{http.MethodGet, "/v1/quota/remaining", "", http.StatusOK},
	} {
		t.Run(request.path, func(t *testing.T) {
			req, err := http.NewRequest(request.method, httpServer.URL+request.path, strings.NewReader(request.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CLIProxyAPI-Session", "old-tmux-window")
			response, err := httpServer.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != request.want {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, request.want, body)
			}
			for key := range response.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-cliproxyapi-quota-") {
					t.Errorf("obsolete pause header emitted: %s", key)
				}
			}
		})
	}
	state, err := os.ReadFile(statePath)
	if err != nil || string(state) != invalidState {
		t.Fatalf("legacy ledger should remain untouched: %q, %v", state, err)
	}
}
