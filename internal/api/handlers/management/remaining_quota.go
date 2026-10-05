package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetRemainingQuota returns cap-adjusted totals and the actual affinity-bound
// account for the caller's session/model. It never guesses or routes a request.
// No credential, workspace/seat identifier, or account ID is returned.
func (h *Handler) GetRemainingQuota(c *gin.Context) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	session := strings.TrimSpace(c.Query("session_id"))
	if len(model) > 512 || len(session) > 4096 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model or session_id too long"})
		return
	}
	var current *coreauth.Auth
	binding := "unbound"
	if model != "" && session != "" {
		current, binding = manager.LookupSessionAffinity("codex", model, session)
	}
	result := coreauth.DefaultBurnController.RemainingQuota(manager.List(), current)
	result.CurrentBinding = binding
	c.JSON(http.StatusOK, result)
}
