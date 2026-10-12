package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// requestSizeError converts local request-buffer failures into a stable client
// fault. These failures are payload-scoped: rotating credentials or retrying the
// same request cannot help and must not poison credential availability.
type requestSizeError struct {
	cause error
}

func (e *requestSizeError) Error() string {
	message := "Request exceeds the proxy or upstream request buffer; reduce or compact the context"
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code":      "context_too_large",
		"type":      "invalid_request_error",
		"message":   message,
		"reason":    "request buffer limit exceeded",
		"retryable": false,
	}})
	return string(payload)
}

func (e *requestSizeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *requestSizeError) StatusCode() int       { return http.StatusRequestEntityTooLarge }
func (e *requestSizeError) IsRequestScoped() bool { return true }

// Do not expose an upstream Retry-After for a non-refillable payload fault.
func (e *requestSizeError) Headers() http.Header { return nil }

func normalizeRequestSizeError(err error) error {
	if err == nil {
		return nil
	}
	var existing *requestSizeError
	if errors.As(err, &existing) {
		return err
	}
	status := statusCodeFromError(err)
	// Preserve real authentication and quota boundaries. Proxy buffer failures
	// can arrive as 5xx HTTP responses, not just statusless construction errors.
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"exceeded request buffer limit",
		"request buffer limit exceeded",
		"request exceeds the buffer limit",
		"request exceeds buffer limit",
	} {
		if strings.Contains(message, marker) {
			return &requestSizeError{cause: err}
		}
	}
	// The broader body-size phrase alone is insufficient to reinterpret an
	// arbitrary gateway error; explicit buffer-limit messages above are narrow.
	if (status == 0 || status == http.StatusRequestEntityTooLarge) && strings.Contains(message, "request body too large") {
		return &requestSizeError{cause: err}
	}
	return err
}
