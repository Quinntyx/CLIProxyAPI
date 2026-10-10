package api

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionquota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// Session budget commands use the same authenticated principal as inference;
// no management password needs to be exposed to a Pi plugin.
func (s *Server) setupSessionQuota() {
	if s.handlers == nil || s.handlers.AuthManager == nil {
		return
	}
	path := ""
	// AuthDir is persistent in container deployments; a non-.json filename
	// keeps the credential watcher from treating the ledger as an auth record.
	if s.cfg != nil && s.cfg.AuthDir != "" {
		directory, resolveErr := util.ResolveAuthDir(s.cfg.AuthDir)
		if resolveErr != nil {
			s.sessionQuotaError = resolveErr
			return
		}
		path = filepath.Join(directory, ".session-quota.state")
	} else if s.configFilePath != "" {
		path = filepath.Join(filepath.Dir(s.configFilePath), "session-quota.json")
	}
	q, err := sessionquota.New(path)
	if err != nil {
		// Do not discard allocations on corrupt/unreadable state: budgets must fail closed.
		log.Errorf("session quota state unavailable: %v", err)
		s.sessionQuotaError = err
		return
	}
	s.handlers.AuthManager.SetSessionQuota(q)
}
func (s *Server) sessionQuotaReady(c *gin.Context) *sessionquota.Manager {
	if s.sessionQuotaError != nil || s.handlers == nil || s.handlers.AuthManager == nil || s.handlers.AuthManager.SessionQuota() == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "session_quota_unavailable", "message": "Session quota state is unavailable"}})
		return nil
	}
	s.handlers.AuthManager.SyncSessionQuota()
	return s.handlers.AuthManager.SessionQuota()
}
func (s *Server) sessionQuotaStatus(c *gin.Context) {
	if q := s.sessionQuotaReady(c); q != nil {
		c.JSON(http.StatusOK, q.Snapshot())
	}
}
func (s *Server) sessionQuotaSet(c *gin.Context) {
	q := s.sessionQuotaReady(c)
	if q == nil {
		return
	}
	var body struct {
		Cap *float64 `json:"cap"`
	}
	if c.ShouldBindJSON(&body) != nil || body.Cap == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cap required in quota points"})
		return
	}
	if err := q.Set(strings.TrimPrefix(c.Param("session"), "/"), *body.Cap); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, q.Snapshot())
}
func (s *Server) sessionQuotaClear(c *gin.Context) {
	q := s.sessionQuotaReady(c)
	if q == nil {
		return
	}
	if err := q.Clear(strings.TrimPrefix(c.Param("session"), "/")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, q.Snapshot())
}
func (s *Server) sessionQuotaClearAll(c *gin.Context) {
	q := s.sessionQuotaReady(c)
	if q == nil {
		return
	}
	if err := q.ClearAll(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, q.Snapshot())
}
func (s *Server) sessionQuotaSkim(c *gin.Context) {
	q := s.sessionQuotaReady(c)
	if q == nil {
		return
	}
	var body struct {
		Amount *float64 `json:"amount"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if c.ShouldBindJSON(&body) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skim amount"})
			return
		}
	}
	amount := 10.0
	if body.Amount != nil {
		amount = *body.Amount
	}
	moved, err := q.Skim(amount)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"transferred": moved, "quota": q.Snapshot()})
}
func (s *Server) sessionQuotaFailClosed() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.sessionQuotaError != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "session_quota_unavailable", "message": "Session quota state is unavailable"}})
			return
		}
		c.Next()
	}
}
