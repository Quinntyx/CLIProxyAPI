package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetRemainingQuotaResponseDoesNotExposeIdentities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.BurnDeadlineSelector{Fallback: coreauth.NewSessionAffinitySelector(&coreauth.RoundRobinSelector{})}, nil)
	a := &coreauth.Auth{ID: "private-account-file", Provider: "codex", Metadata: map[string]any{"account_id": "private-workspace", "access_token": "private-token"}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{authManager: manager}
	for _, tc := range []struct {
		url  string
		want int
	}{{"/?model=gpt-6.1-sol&session_id=unbound", http.StatusOK}, {"/?model=" + strings.Repeat("x", 513), http.StatusBadRequest}, {"/?session_id=" + strings.Repeat("x", 4097), http.StatusBadRequest}} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("GET", tc.url, nil)
		handler.GetRemainingQuota(c)
		if recorder.Code != tc.want {
			t.Fatal(recorder.Code, recorder.Body.String())
		}
		for _, secret := range []string{"private-account-file", "private-workspace", "private-token"} {
			if strings.Contains(recorder.Body.String(), secret) {
				t.Fatal("private identity leaked")
			}
		}
		if tc.want == http.StatusOK {
			var result coreauth.RemainingQuotaStatus
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.CurrentBinding != "unbound" || result.Weekly.CurrentPercent != nil {
				t.Fatal("unknown account guessed", result)
			}
		}
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	(&Handler{}).GetRemainingQuota(c)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatal(recorder.Code)
	}
}
