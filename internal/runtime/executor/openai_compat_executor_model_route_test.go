package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Route target params pin OpenRouter hosts, but user payload rules remain the
// final barrier in both execution paths.
func TestOpenAICompatExecutorModelRouteParamsPrecedeUserPayloadRules(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatible-openrouter", &config.Config{
		Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "my-custom-model", Protocol: "openai"}},
			Params: map[string]any{"provider.allow_fallbacks": true},
		}}},
	})
	auth := &cliproxyauth.Auth{Provider: "openai-compatible-openrouter", Attributes: map[string]string{
		"base_url": server.URL + "/api/v1", "api_key": "test", "compat_name": "openrouter", "provider_key": "openai-compatible-openrouter",
	}}
	params := map[string]any{"provider": map[string]any{
		"only":            []any{"wafer", "relace"},
		"order":           []any{"wafer", "relace"},
		"allow_fallbacks": false,
	}}
	meta := map[string]any{
		cliproxyexecutor.RequestedModelMetadataKey:   "my-custom-model",
		cliproxyexecutor.ModelRouteParamsMetadataKey: params,
	}
	req := cliproxyexecutor.Request{Model: "deepseek/deepseek-v4.1-flash-1017", Payload: []byte(`{"model":"my-custom-model","messages":[{"role":"user","content":"hi"}]}`)}
	if _, err := executor.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Metadata: meta}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stream, err := executor.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Stream: true, Metadata: meta})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for range stream.Chunks {
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream bodies = %d, want 2", len(bodies))
	}
	for _, body := range bodies {
		if got := gjson.GetBytes(body, "model").String(); got != "deepseek/deepseek-v4.1-flash-1017" {
			t.Fatalf("upstream model = %q; body=%s", got, body)
		}
		if got := gjson.GetBytes(body, "provider.only").Raw; got != `["wafer","relace"]` {
			t.Fatalf("provider.only = %s; body=%s", got, body)
		}
		if got := gjson.GetBytes(body, "provider.order").Raw; got != `["wafer","relace"]` {
			t.Fatalf("provider.order = %s; body=%s", got, body)
		}
		// The user override targeting the alias is applied last and wins.
		if got := gjson.GetBytes(body, "provider.allow_fallbacks"); got.Type != gjson.True {
			t.Fatalf("provider.allow_fallbacks = %s, want user override true; body=%s", got.Raw, body)
		}
	}
}
