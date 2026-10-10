package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestLocalQuotaHeadersIgnorePassthroughSetting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, weekly := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/weekly=%t", passthrough, weekly), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
				handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: passthrough}, nil)
				quotaErr := &sessionquota.ExhaustedError{Weekly: weekly, RetryAfter: 30, Needed: 0.12}
				handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
					StatusCode: http.StatusTooManyRequests,
					Error:      fmt.Errorf("wrapped: %w", quotaErr),
					Addon:      http.Header{"X-Cliproxyapi-Quota-Code": {"spoofed"}},
				})
				for key, values := range quotaErr.Headers() {
					if got := recorder.Header().Get(key); got != values[0] {
						t.Fatalf("%s = %q, want %q", key, got, values[0])
					}
				}
			})
		}
	}
}

func TestUpstreamCannotSpoofQuotaHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("upstream rate limit"),
		Addon:      http.Header{"X-Cliproxyapi-Quota-Code": {"provisioned_quota_exhausted"}},
	})
	if got := recorder.Header().Get("X-CLIProxyAPI-Quota-Code"); got != "" {
		t.Fatalf("untrusted quota header forwarded: %q", got)
	}
	if got := FilterUpstreamHeaders(http.Header{"X-Cliproxyapi-Quota-Code": {"spoofed"}}); len(got) != 0 {
		t.Fatalf("untrusted quota metadata survived filter: %v", got)
	}
}
