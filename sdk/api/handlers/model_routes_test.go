package handlers

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type routeCall struct {
	provider       string
	model          string
	requestedModel any
	params         any
}

type routeTestExecutor struct {
	id      string
	mu      *sync.Mutex
	calls   *[]routeCall
	execute func(req coreexecutor.Request) (coreexecutor.Response, error)
	stream  func(req coreexecutor.Request) []coreexecutor.StreamChunk
}

func (e *routeTestExecutor) Identifier() string { return e.id }

func (e *routeTestExecutor) record(req coreexecutor.Request, opts coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.calls = append(*e.calls, routeCall{
		provider:       e.id,
		model:          req.Model,
		requestedModel: opts.Metadata[coreexecutor.RequestedModelMetadataKey],
		params:         opts.Metadata[coreexecutor.ModelRouteParamsMetadataKey],
	})
}

func (e *routeTestExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req, opts)
	return e.execute(req)
}

func (e *routeTestExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(req, opts)
	chunks := e.stream(req)
	ch := make(chan coreexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *routeTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *routeTestExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req, opts)
	return e.execute(req)
}

func (e *routeTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", HTTPStatus: http.StatusNotImplemented}
}

type routeFixture struct {
	handler *BaseAPIHandler
	calls   *[]routeCall
	mu      *sync.Mutex
}

func (f *routeFixture) snapshot() []routeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]routeCall(nil), (*f.calls)...)
}

func upstreamStatusError(status int, message string) error {
	return &coreauth.Error{Code: "upstream", Message: message, HTTPStatus: status}
}

// newRouteFixture registers an OpenAI-compatible provider "alpha" and a built-in
// style provider "beta" serving unrelated models.
func newRouteFixture(t *testing.T, alpha, beta *routeTestExecutor, routes []sdkconfig.ModelRoute) *routeFixture {
	t.Helper()
	mu := &sync.Mutex{}
	calls := &[]routeCall{}
	alpha.id, alpha.mu, alpha.calls = "openai-compatible-alpha", mu, calls
	beta.id, beta.mu, beta.calls = "beta", mu, calls
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(alpha)
	manager.RegisterExecutor(beta)
	alphaAuth := &coreauth.Auth{ID: "route-alpha-auth", Provider: alpha.id, Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "alpha-test", "compat_name": "alpha", "provider_key": alpha.id}}
	betaAuth := &coreauth.Auth{ID: "route-beta-auth", Provider: beta.id, Status: coreauth.StatusActive}
	for _, auth := range []*coreauth.Auth{alphaAuth, betaAuth} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	registry.GetGlobalRegistry().RegisterClient(alphaAuth.ID, alpha.id, []*registry.ModelInfo{{ID: "mimo-v2.6-pro"}})
	registry.GetGlobalRegistry().RegisterClient(betaAuth.ID, beta.id, []*registry.ModelInfo{{ID: "glm-5.3-flash"}, {ID: "deepseek-v4.1-flash:fast"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(alphaAuth.ID)
		registry.GetGlobalRegistry().UnregisterClient(betaAuth.ID)
	})
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelRoutes: routes}, manager)
	return &routeFixture{handler: handler, calls: calls, mu: mu}
}

var testRoutes = []sdkconfig.ModelRoute{
	{Alias: "my-custom-model", Targets: []sdkconfig.ModelRouteTarget{
		{Provider: "alpha", Model: "mimo-v2.6-pro"},
		{Provider: "beta", Model: "glm-5.3-flash", Params: map[string]any{"provider": map[string]any{"only": []any{"wafer"}, "allow_fallbacks": false}}},
	}},
	{Alias: "my-custom-model:fast", Targets: []sdkconfig.ModelRouteTarget{
		{Provider: "beta", Model: "deepseek-v4.1-flash:fast"},
	}},
}

func okBody(req coreexecutor.Request) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"id":"x","object":"chat.completion","model":"` + req.Model + `","choices":[]}`)}, nil
}

func TestModelRouteFallsBackAcrossUnrelatedModelsInOrder(t *testing.T) {
	alpha := &routeTestExecutor{execute: func(coreexecutor.Request) (coreexecutor.Response, error) {
		return coreexecutor.Response{}, upstreamStatusError(http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`)
	}}
	beta := &routeTestExecutor{execute: okBody}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	body, _, errMsg := f.handler.ExecuteWithAuthManager(context.Background(), "openai", "my-custom-model", []byte(`{"model":"my-custom-model","messages":[]}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "my-custom-model" {
		t.Fatalf("response model = %q, want alias", got)
	}
	calls := f.snapshot()
	if len(calls) < 2 {
		t.Fatalf("calls = %+v, want alpha then beta", calls)
	}
	first, last := calls[0], calls[len(calls)-1]
	if first.provider != "openai-compatible-alpha" || first.model != "mimo-v2.6-pro" {
		t.Fatalf("first call = %+v", first)
	}
	if last.provider != "beta" || last.model != "glm-5.3-flash" {
		t.Fatalf("last call = %+v", last)
	}
	for _, call := range calls {
		if call.requestedModel != "my-custom-model" {
			t.Fatalf("requested model metadata = %v, want alias", call.requestedModel)
		}
	}
	if first.params != nil {
		t.Fatalf("alpha params = %v, want none", first.params)
	}
	params, ok := last.params.(map[string]any)
	if !ok || params["provider"] == nil {
		t.Fatalf("beta params = %#v", last.params)
	}
}

func TestModelRouteColonAliasIsAnOrdinaryIndependentRoute(t *testing.T) {
	alpha := &routeTestExecutor{execute: okBody}
	beta := &routeTestExecutor{execute: okBody}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	body, _, errMsg := f.handler.ExecuteWithAuthManager(context.Background(), "openai", "my-custom-model:fast", []byte(`{"model":"my-custom-model:fast"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "my-custom-model:fast" {
		t.Fatalf("response model = %q", got)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].provider != "beta" || calls[0].model != "deepseek-v4.1-flash:fast" {
		t.Fatalf("calls = %+v", calls)
	}

	body, _, errMsg = f.handler.ExecuteWithAuthManager(context.Background(), "openai", "my-custom-model", []byte(`{"model":"my-custom-model"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected base error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "my-custom-model" {
		t.Fatalf("base response model = %q", got)
	}
	calls = f.snapshot()
	if calls[len(calls)-1].provider != "openai-compatible-alpha" || calls[len(calls)-1].model != "mimo-v2.6-pro" {
		t.Fatalf("base route did not use its own first target: %+v", calls)
	}
}

func TestModelRouteStopsOnCallerRequestFault(t *testing.T) {
	alpha := &routeTestExecutor{execute: func(coreexecutor.Request) (coreexecutor.Response, error) {
		return coreexecutor.Response{}, upstreamStatusError(http.StatusBadRequest, `{"error":{"message":"messages is required","type":"invalid_request_error"}}`)
	}}
	beta := &routeTestExecutor{execute: okBody}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	_, _, errMsg := f.handler.ExecuteWithAuthManager(context.Background(), "openai", "my-custom-model", []byte(`{"model":"my-custom-model"}`), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("errMsg = %+v, want 400", errMsg)
	}
	for _, call := range f.snapshot() {
		if call.provider == "beta" {
			t.Fatalf("request fault failed over to beta: %+v", f.snapshot())
		}
	}
}

func TestModelRouteDoesNotFailOverAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	alpha := &routeTestExecutor{execute: func(coreexecutor.Request) (coreexecutor.Response, error) {
		cancel()
		return coreexecutor.Response{}, context.Canceled
	}}
	beta := &routeTestExecutor{execute: okBody}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	_, _, errMsg := f.handler.ExecuteWithAuthManager(ctx, "openai", "my-custom-model", []byte(`{"model":"my-custom-model"}`), "")
	if errMsg == nil {
		t.Fatal("expected cancellation error")
	}
	for _, call := range f.snapshot() {
		if call.provider == "beta" {
			t.Fatalf("canceled request failed over: %+v", f.snapshot())
		}
	}
}

func collectRouteStream(t *testing.T, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage) (string, *interfaces.ErrorMessage) {
	t.Helper()
	var out strings.Builder
	var lastErr *interfaces.ErrorMessage
	for dataChan != nil || errChan != nil {
		select {
		case chunk, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			out.Write(chunk)
			out.WriteString("\n")
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if errMsg != nil {
				lastErr = errMsg
			}
		}
	}
	return out.String(), lastErr
}

func TestModelRouteStreamFailsOverBeforeFirstOutput(t *testing.T) {
	alpha := &routeTestExecutor{stream: func(coreexecutor.Request) []coreexecutor.StreamChunk {
		return []coreexecutor.StreamChunk{{Err: upstreamStatusError(http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)}}
	}}
	beta := &routeTestExecutor{stream: func(req coreexecutor.Request) []coreexecutor.StreamChunk {
		return []coreexecutor.StreamChunk{
			{Payload: []byte(`{"model":"` + req.Model + `","choices":[{"delta":{"content":"hi"}}]}`)},
			{Payload: []byte(`{"model":"` + req.Model + `","choices":[{"delta":{"content":"!"}}]}`)},
		}
	}}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	dataChan, _, errChan := f.handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "my-custom-model", []byte(`{"model":"my-custom-model","stream":true}`), "")
	out, errMsg := collectRouteStream(t, dataChan, errChan)
	if errMsg != nil {
		t.Fatalf("unexpected stream error: %v", errMsg.Error)
	}
	if strings.Count(out, `"model":"my-custom-model"`) != 2 || strings.Contains(out, "glm-5.3-flash") {
		t.Fatalf("stream output = %q, want alias model on both chunks", out)
	}
	calls := f.snapshot()
	if calls[0].provider != "openai-compatible-alpha" || calls[len(calls)-1].provider != "beta" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestModelRouteStreamNeverFailsOverAfterOutputBegins(t *testing.T) {
	alpha := &routeTestExecutor{stream: func(req coreexecutor.Request) []coreexecutor.StreamChunk {
		return []coreexecutor.StreamChunk{
			{Payload: []byte(`{"model":"` + req.Model + `","choices":[{"delta":{"content":"partial"}}]}`)},
			{Err: upstreamStatusError(http.StatusBadGateway, `{"error":{"message":"upstream reset"}}`)},
		}
	}}
	beta := &routeTestExecutor{stream: func(coreexecutor.Request) []coreexecutor.StreamChunk {
		return []coreexecutor.StreamChunk{{Payload: []byte(`{"choices":[]}`)}}
	}}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	dataChan, _, errChan := f.handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "my-custom-model", []byte(`{"model":"my-custom-model","stream":true}`), "")
	out, errMsg := collectRouteStream(t, dataChan, errChan)
	if !strings.Contains(out, "partial") || !strings.Contains(out, `"model":"my-custom-model"`) {
		t.Fatalf("stream output = %q", out)
	}
	if errMsg == nil {
		t.Fatal("expected the mid-stream error to reach the client")
	}
	for _, call := range f.snapshot() {
		if call.provider == "beta" {
			t.Fatalf("stream failed over after output began: %+v", f.snapshot())
		}
	}
}

func TestModelRouteDoesNotInterceptUnroutedModels(t *testing.T) {
	alpha := &routeTestExecutor{execute: okBody}
	beta := &routeTestExecutor{execute: okBody}
	f := newRouteFixture(t, alpha, beta, testRoutes)

	body, _, errMsg := f.handler.ExecuteWithAuthManager(context.Background(), "openai", "glm-5.3-flash", []byte(`{"model":"glm-5.3-flash"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "glm-5.3-flash" {
		t.Fatalf("direct model response = %q", got)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].requestedModel != "glm-5.3-flash" || calls[0].params != nil {
		t.Fatalf("calls = %+v", calls)
	}
}
