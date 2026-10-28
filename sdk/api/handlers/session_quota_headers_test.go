package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestQuotaFailuresReturnWithoutLocalPauseProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, message := range []string{"5-hour usage_limit_reached", "weekly usage_limit_reached"} {
			t.Run(fmt.Sprintf("passthrough=%t/%s", passthrough, message), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
				handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: passthrough}, nil)
				handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
					StatusCode: http.StatusTooManyRequests,
					Error:      errors.New(message),
					Addon: http.Header{
						"Retry-After":                     {"30"},
						"X-Cliproxyapi-Quota-Code":        {"provisioned_quota_exhausted"},
						"X-Cliproxyapi-Quota-Retry-After": {"30"},
					},
				})
				if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), message) {
					t.Fatalf("quota failure was rewritten: %d %s", recorder.Code, recorder.Body.String())
				}
				for key := range recorder.Header() {
					if strings.HasPrefix(strings.ToLower(key), "x-cliproxyapi-quota-") {
						t.Errorf("obsolete pause header emitted: %s", key)
					}
				}
				wantRetryAfter := ""
				if passthrough {
					wantRetryAfter = "30"
				}
				if got := recorder.Header().Get("Retry-After"); got != wantRetryAfter {
					t.Fatalf("Retry-After = %q, want %q", got, wantRetryAfter)
				}
			})
		}
	}
}

func TestUpstreamCannotSpoofQuotaHeaders(t *testing.T) {
	if got := FilterUpstreamHeaders(http.Header{"X-Cliproxyapi-Quota-Code": {"spoofed"}}); len(got) != 0 {
		t.Fatalf("untrusted quota metadata survived filter: %v", got)
	}
}
