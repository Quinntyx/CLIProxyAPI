package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func quotaTestServer(t *testing.T) *Server {
	s := newTestServer(t)
	_, err := s.handlers.AuthManager.Register(context.Background(), &coreauth.Auth{ID: "quota-account", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth", "session_quota_capacity": "250"}, Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "0", "X-Codex-Primary-Window-Minutes": "300", "X-Codex-Secondary-Used-Percent": "0", "X-Codex-Secondary-Window-Minutes": "10080"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func quotaRequest(s *Server, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response := httptest.NewRecorder()
	s.engine.ServeHTTP(response, request)
	return response
}
func TestSessionQuotaRoutesRequireInferenceKeyAndConserveBalances(t *testing.T) {
	s := quotaTestServer(t)
	for _, request := range []struct{ method, path, body string }{{"GET", "/v1/session-quota", ""}, {"PUT", "/v1/session-quota/sessions/x", `{"cap":100}`}, {"DELETE", "/v1/session-quota/sessions/x", ""}, {"DELETE", "/v1/session-quota/sessions", ""}, {"POST", "/v1/session-quota/skim", `{}`}} {
		response := quotaRequest(s, request.method, request.path, request.body, "")
		if response.Code != 401 {
			t.Fatalf("unauthenticated %s %s: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	response := quotaRequest(s, "PUT", "/v1/session-quota/sessions/work%2Fflow", `{"cap":100}`, "test-key")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var status sessionquota.Snapshot
	_ = json.Unmarshal(response.Body.Bytes(), &status)
	found := false
	for _, b := range status.Buckets {
		if b.Session == "work/flow" {
			found = true
			if b.Remaining != 100 {
				t.Fatal(b)
			}
		}
	}
	if !found {
		t.Fatal(status)
	}
	response = quotaRequest(s, "POST", "/v1/session-quota/skim", `{}`, "test-key")
	var skim struct {
		Transferred float64               `json:"transferred"`
		Quota       sessionquota.Snapshot `json:"quota"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &skim)
	if response.Code != 200 || skim.Transferred != 10 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, b := range skim.Quota.Buckets {
		if b.Session == "work/flow" && (b.Cap != 100 || b.Remaining != 90) {
			t.Fatal(b)
		}
	}
	response = quotaRequest(s, "DELETE", "/v1/session-quota/sessions/work%2Fflow", "", "test-key")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = quotaRequest(s, "GET", "/v1/session-quota", "", "test-key")
	_ = json.Unmarshal(response.Body.Bytes(), &status)
	if len(status.Buckets) != 1 || status.Buckets[0].Cap != 250 || status.Buckets[0].Remaining != 250 {
		t.Fatal(status)
	}
}
func TestSessionQuotaRoutesValidateInputs(t *testing.T) {
	s := quotaTestServer(t)
	for _, test := range []struct{ method, path, body string }{{"PUT", "/v1/session-quota/sessions/x", `{}`}, {"PUT", "/v1/session-quota/sessions/x", `{"cap":251}`}, {"PUT", "/v1/session-quota/sessions/x", `{"cap":-1}`}, {"POST", "/v1/session-quota/skim", `{"amount":-1}`}} {
		response := quotaRequest(s, test.method, test.path, test.body, "test-key")
		if response.Code != 400 {
			t.Fatal(test, response.Code, response.Body.String())
		}
	}
}
