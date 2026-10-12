package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestRequestSizeErrorStopsBeforeCredentialRotation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 0)
	model := "request-size-model"
	ids := []string{"request-size-a", "request-size-b"}
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			registry.GetGlobalRegistry().UnregisterClient(id)
		}
	})

	calls := 0
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		return cliproxyexecutor.Response{}, customStatusError{code: http.StatusBadGateway, msg: "exceeded request buffer limit while retrying upstream"}
	}})

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	var sizeErr *requestSizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("Execute() error = %T/%v, want requestSizeError", err, err)
	}
	if sizeErr.StatusCode() != http.StatusRequestEntityTooLarge || !strings.Contains(sizeErr.Error(), `"code":"context_too_large"`) {
		t.Fatalf("unexpected typed error: %v", sizeErr)
	}
	if calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
	for _, id := range ids {
		auth, ok := m.GetByID(id)
		if !ok || auth.Unavailable || !auth.NextRetryAfter.IsZero() {
			t.Fatalf("credential %s poisoned by request fault: %+v", id, auth)
		}
	}
}

func TestRequestSizeErrorStopsCountBeforeCredentialRotation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 0)
	model := "request-size-count-model"
	ids := []string{"request-size-count-a", "request-size-count-b"}
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			registry.GetGlobalRegistry().UnregisterClient(id)
		}
	})

	calls := 0
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", countFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		return cliproxyexecutor.Response{}, customStatusError{code: http.StatusBadGateway, msg: "request exceeds the buffer limit"}
	}})

	_, err := m.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	var sizeErr *requestSizeError
	if !errors.As(err, &sizeErr) || calls != 1 {
		t.Fatalf("ExecuteCount() error/calls = %T/%v/%d, want requestSizeError/1", err, err, calls)
	}
	for _, id := range ids {
		auth, ok := m.GetByID(id)
		if !ok || auth.Unavailable || !auth.NextRetryAfter.IsZero() {
			t.Fatalf("credential %s poisoned by count request fault: %+v", id, auth)
		}
	}
}

func TestStreamingRequestSizeErrorsStopBeforeCredentialRotation(t *testing.T) {
	for _, test := range []struct {
		name     string
		streamFn func(*int) func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	}{
		{
			name: "setup",
			streamFn: func(calls *int) func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				return func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					(*calls)++
					return nil, customStatusError{code: http.StatusBadGateway, msg: "request buffer limit exceeded"}
				}
			},
		},
		{
			name: "bootstrap",
			streamFn: func(calls *int) func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				return func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					(*calls)++
					chunks := make(chan cliproxyexecutor.StreamChunk, 1)
					chunks <- cliproxyexecutor.StreamChunk{Err: customStatusError{code: http.StatusBadGateway, msg: "exceeded request buffer limit while retrying upstream"}}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(3, 0, 0)
			model := "request-size-stream-model-" + test.name
			ids := []string{"request-size-stream-a-" + test.name, "request-size-stream-b-" + test.name}
			for _, id := range ids {
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
				if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, id := range ids {
					registry.GetGlobalRegistry().UnregisterClient(id)
				}
			})

			calls := 0
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: test.streamFn(&calls)})
			_, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
			var sizeErr *requestSizeError
			if !errors.As(err, &sizeErr) || calls != 1 {
				t.Fatalf("ExecuteStream() error/calls = %T/%v/%d, want requestSizeError/1", err, err, calls)
			}
			for _, id := range ids {
				auth, ok := m.GetByID(id)
				if !ok || auth.Unavailable || !auth.NextRetryAfter.IsZero() {
					t.Fatalf("credential %s poisoned by stream request fault: %+v", id, auth)
				}
			}
		})
	}
}

func TestStatusBearingErrorIsNotReclassifiedByMessage(t *testing.T) {
	original := customStatusError{code: http.StatusBadGateway, msg: "upstream request body too large"}
	if got := normalizeRequestSizeError(original); got != original {
		t.Fatalf("status-bearing error was reclassified: %T/%v", got, got)
	}
}

func TestRequestBufferErrorContractAndClassificationBoundaries(t *testing.T) {
	for _, status := range []int{0, 400, 413, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			cause := customStatusError{code: status, msg: "exceeded request buffer limit while retrying upstream"}
			err := normalizeRequestSizeError(fmt.Errorf("wrapped: %w", cause))
			var sizeErr *requestSizeError
			if !errors.As(err, &sizeErr) || !errors.Is(err, cause) || !isRequestInvalidError(err) || !noQuotaCharge(err) {
				t.Fatalf("lost request fault/cause: %T/%v", err, err)
			}
			var body struct {
				Error struct {
					Code      string `json:"code"`
					Type      string `json:"type"`
					Retryable *bool  `json:"retryable"`
				} `json:"error"`
			}
			if errJSON := json.Unmarshal([]byte(err.Error()), &body); errJSON != nil || body.Error.Code != "context_too_large" || body.Error.Type != "invalid_request_error" || body.Error.Retryable == nil || *body.Error.Retryable {
				t.Fatalf("unstable client contract: %v", err)
			}
			if sizeErr.StatusCode() != 413 || sizeErr.Headers().Get("Retry-After") != "" || normalizeRequestSizeError(err) != err {
				t.Fatalf("unstable status/headers/idempotence: %v", err)
			}
		})
	}
	for _, original := range []error{
		customStatusError{code: 401, msg: "request buffer limit exceeded"},
		customStatusError{code: 403, msg: "request buffer limit exceeded"},
		customStatusError{code: 429, msg: "request buffer limit exceeded"},
		customStatusError{code: 502, msg: "upstream overloaded"},
		errors.New("response buffer limit exceeded"),
		fmt.Errorf("request buffer limit exceeded: %w", context.Canceled),
		fmt.Errorf("request buffer limit exceeded: %w", context.DeadlineExceeded),
	} {
		if got := normalizeRequestSizeError(original); got != original {
			t.Fatalf("boundary reclassified: %T/%v", got, got)
		}
	}
}

func TestLateStreamingRequestBufferErrorDoesNotPoisonCredentials(t *testing.T) {
	m, q, model := quotaManager(t)
	m.SetRetryConfig(3, 0, 0)
	calls := 0
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		calls++
		chunks := make(chan cliproxyexecutor.StreamChunk, 2)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: started\n\n")}
		chunks <- cliproxyexecutor.StreamChunk{Err: customStatusError{code: 502, msg: "exceeded request buffer limit while retrying upstream"}}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}})
	result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hi","max_tokens":10}`)}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var terminal error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			terminal = chunk.Err
		}
	}
	var sizeErr *requestSizeError
	if !errors.As(terminal, &sizeErr) || calls != 1 {
		t.Fatalf("terminal error/calls = %T/%v/%d", terminal, terminal, calls)
	}
	a, _ := m.GetByID("quota-auth-" + t.Name())
	if a.Unavailable || !a.NextRetryAfter.IsZero() || a.Quota.Exceeded {
		t.Fatalf("credential poisoned: %+v", a)
	}
	b := quotaBucket(t, q, "")
	if b.Held != 0 || b.Remaining != 250 {
		t.Fatalf("late terminal fault leaked/charged reservation: %+v", b)
	}
	// A subsequent reduced request must remain routable with the same credential.
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		return cliproxyexecutor.Response{}, nil
	}})
	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil || calls != 2 {
		t.Fatalf("recovery request unavailable: %v calls=%d", err, calls)
	}
}
