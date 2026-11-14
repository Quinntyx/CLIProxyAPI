package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/context"
)

// modelRouteTarget is one resolved step of a configured model route.
type modelRouteTarget struct {
	// Alias is the client-visible model id; responses and usage keep it.
	Alias string
	// Provider is the resolved execution provider key.
	Provider string
	// Model is the provider model id, carrying any requested thinking suffix.
	Model  string
	Params map[string]any
}

// resolveModelRoute returns the ordered targets for a configured alias. Nested
// target executions, forced-provider executions and Home mode never re-route.
func (h *BaseAPIHandler) resolveModelRoute(modelName string, execOptions modelExecutionOptions) ([]modelRouteTarget, bool) {
	if h == nil || h.Cfg == nil || len(h.Cfg.ModelRoutes) == 0 || execOptions.modelRoute != nil {
		return nil, false
	}
	if strings.TrimSpace(execOptions.ForcedProvider) != "" {
		return nil, false
	}
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return nil, false
	}
	alias := strings.TrimSpace(modelName)
	suffix := ""
	route, ok := h.Cfg.LookupModelRoute(alias)
	if !ok {
		parsed := thinking.ParseSuffix(alias)
		if !parsed.HasSuffix {
			return nil, false
		}
		route, ok = h.Cfg.LookupModelRoute(parsed.ModelName)
		if !ok {
			return nil, false
		}
		alias = strings.TrimSpace(parsed.ModelName)
		suffix = parsed.RawSuffix
	}
	targets := make([]modelRouteTarget, 0, len(route.Targets))
	for _, target := range route.Targets {
		model := target.Model
		if suffix != "" {
			model = fmt.Sprintf("%s(%s)", model, suffix)
		}
		targets = append(targets, modelRouteTarget{
			Alias:    alias,
			Provider: h.modelRouteProviderKey(target.Provider),
			Model:    model,
			Params:   target.Params,
		})
	}
	return targets, len(targets) > 0
}

// modelRouteProviderKey prefers an OpenAI-compatible provider with the
// configured name, then a built-in provider key.
func (h *BaseAPIHandler) modelRouteProviderKey(provider string) string {
	keys := registry.ModelRouteProviderKeys(provider)
	if len(keys) == 0 {
		return ""
	}
	if h != nil && h.AuthManager != nil {
		for _, key := range keys {
			if _, ok := h.AuthManager.Executor(key); ok {
				return key
			}
		}
	}
	return keys[len(keys)-1]
}

func (t *modelRouteTarget) options(execOptions modelExecutionOptions) modelExecutionOptions {
	execOptions.ForcedProvider = t.Provider
	execOptions.modelRoute = t
	return execOptions
}

// applyModelRouteMetadata keeps the alias as the requested model and hands the
// target params to executors, which apply them before user payload rules.
func applyModelRouteMetadata(meta map[string]any, route *modelRouteTarget) {
	if route == nil || meta == nil {
		return
	}
	meta[coreexecutor.RequestedModelMetadataKey] = route.Alias
	if len(route.Params) > 0 {
		meta[coreexecutor.ModelRouteParamsMetadataKey] = route.Params
	}
}

// modelRouteFailoverAllowed reports whether the next target may be tried.
// Cancellation, interceptor terminations and caller request faults stop the
// route; credential, quota, availability and upstream failures continue it.
func modelRouteFailoverAllowed(ctx context.Context, errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errMsg.DirectResponse {
		return false
	}
	if errMsg.Error != nil && (errors.Is(errMsg.Error, context.Canceled) || errors.Is(errMsg.Error, context.DeadlineExceeded)) {
		return false
	}
	return !clienterror.IsRequestFault(errMsg.StatusCode, errMsg.Error)
}

func (h *BaseAPIHandler) executeModelRoute(ctx context.Context, targets []modelRouteTarget, run func(modelRouteTarget) ([]byte, http.Header, *interfaces.ErrorMessage)) ([]byte, http.Header, *interfaces.ErrorMessage) {
	var lastErr *interfaces.ErrorMessage
	for i := range targets {
		body, headers, errMsg := run(targets[i])
		if errMsg == nil {
			return rewriteModelRouteResponse(body, targets[i].Alias), headers, nil
		}
		lastErr = errMsg
		if !modelRouteFailoverAllowed(ctx, errMsg) {
			break
		}
	}
	return nil, nil, lastErr
}

// streamModelRoute tries targets until one yields its first chunk. Once any
// business output has been forwarded, later failures are never retried elsewhere.
func (h *BaseAPIHandler) streamModelRoute(ctx context.Context, targets []modelRouteTarget, run func(modelRouteTarget) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage)) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	var lastErr *interfaces.ErrorMessage
	for i := range targets {
		dataChan, headers, errChan := run(targets[i])
		first, firstOK, errMsg, canceled := firstModelRouteStreamItem(ctx, dataChan, errChan)
		if canceled {
			return closedModelRouteStream(nil)
		}
		if errMsg != nil {
			lastErr = errMsg
			if !modelRouteFailoverAllowed(ctx, errMsg) {
				break
			}
			continue
		}
		return forwardModelRouteStream(ctx, targets[i].Alias, first, firstOK, dataChan, errChan), headers, errChanOrClosed(errChan)
	}
	if lastErr == nil {
		lastErr = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("model route has no executable target")}
	}
	_, _, errOut := closedModelRouteStream(lastErr)
	return nil, nil, errOut
}

// firstModelRouteStreamItem waits for the first data chunk or terminal error.
// Execution has already bootstrapped the upstream, so this does not add latency.
func firstModelRouteStreamItem(ctx context.Context, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage) ([]byte, bool, *interfaces.ErrorMessage, bool) {
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	for dataChan != nil || errChan != nil {
		select {
		case <-done:
			return nil, false, nil, true
		case chunk, ok := <-dataChan:
			if ok {
				return chunk, true, nil, false
			}
			dataChan = nil
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if errMsg != nil {
				return nil, false, errMsg, false
			}
		}
	}
	return nil, false, nil, false
}

func forwardModelRouteStream(ctx context.Context, alias string, first []byte, firstOK bool, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		var done <-chan struct{}
		if ctx != nil {
			done = ctx.Done()
		}
		send := func(chunk []byte) bool {
			select {
			case out <- rewriteModelRouteResponse(chunk, alias):
				return true
			case <-done:
				return false
			}
		}
		if firstOK && !send(first) {
			return
		}
		if dataChan == nil {
			return
		}
		for chunk := range dataChan {
			if !send(chunk) {
				return
			}
		}
	}()
	return out
}

func errChanOrClosed(errChan <-chan *interfaces.ErrorMessage) <-chan *interfaces.ErrorMessage {
	if errChan != nil {
		return errChan
	}
	closed := make(chan *interfaces.ErrorMessage)
	close(closed)
	return closed
}

func closedModelRouteStream(errMsg *interfaces.ErrorMessage) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	errChan := make(chan *interfaces.ErrorMessage, 1)
	if errMsg != nil {
		errChan <- errMsg
	}
	close(errChan)
	return nil, nil, errChan
}

var modelRouteResponseModelPaths = []string{"model", "modelVersion", "response.model", "response.modelVersion", "message.model"}

// rewriteModelRouteResponse restores the alias in JSON bodies and SSE data lines.
func rewriteModelRouteResponse(payload []byte, alias string) []byte {
	if alias == "" || len(payload) == 0 {
		return payload
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) > 0 && trimmed[0] == '{' && gjson.ValidBytes(trimmed) {
		return rewriteModelRouteJSON(payload, alias)
	}
	if !bytes.Contains(payload, []byte("data:")) {
		return payload
	}
	lines := bytes.Split(payload, []byte("\n"))
	for i, line := range lines {
		rest, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data := bytes.TrimSpace(rest)
		if len(data) == 0 || data[0] != '{' || !gjson.ValidBytes(data) {
			continue
		}
		lines[i] = append([]byte("data: "), rewriteModelRouteJSON(data, alias)...)
	}
	return bytes.Join(lines, []byte("\n"))
}

func rewriteModelRouteJSON(data []byte, alias string) []byte {
	for _, path := range modelRouteResponseModelPaths {
		if value := gjson.GetBytes(data, path); value.Exists() && value.Type == gjson.String {
			if updated, errSet := sjson.SetBytes(data, path, alias); errSet == nil {
				data = updated
			}
		}
	}
	return data
}
