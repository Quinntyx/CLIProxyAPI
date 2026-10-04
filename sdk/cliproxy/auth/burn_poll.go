package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// StartBurnController polls quota WITHOUT generating model tokens, including idle weeks.
// There is a single poll loop, bounded to three concurrent requests, with per-request deadlines.
func StartBurnController(ctx context.Context, cfg config.BurnDeadlineConfig, list func() []*Auth) func() {
	b := DefaultBurnController
	if strings.HasPrefix(cfg.StateFile, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.StateFile = home + cfg.StateFile[1:]
		}
	}
	b.configure(cfg)
	b.mu.Lock()
	b.list = list
	b.mu.Unlock()
	if !cfg.Enabled {
		return func() {}
	}
	if err := b.load(); err != nil {
		log.WithError(err).Warn("burn telemetry state could not be loaded")
	}
	usage.RegisterNamedPlugin("burn-deadline", b)
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.pollAll(pollCtx)
		interval, err := time.ParseDuration(cfg.PollInterval)
		if err != nil || interval < 15*time.Second {
			interval = time.Minute
		}
		timer := time.NewTicker(interval)
		defer timer.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-timer.C:
				b.pollAll(pollCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		if err := b.save(); err != nil {
			log.WithError(err).Warn("burn telemetry state save failed")
		}
	}
}
func (b *BurnController) pollAll(ctx context.Context) {
	b.mu.Lock()
	list := b.list
	b.mu.Unlock()
	if list == nil {
		return
	}
	semaphore := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, a := range list() {
		if a != nil && a.Provider == "codex" {
			b.mu.Lock()
			b.ensureIdentity(a)
			b.setPolicy(a, b.account(a.ID))
			b.mu.Unlock()
		}
		if a == nil || a.Provider != "codex" || a.Disabled {
			continue
		}
		a := a
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			q, err := pollCodexQuota(ctx, a)
			if err != nil {
				log.WithField("auth_id", a.ID).Debug("burn quota poll unavailable")
				return
			}
			b.Observe(a, q, b.now())
		}()
	}
	wg.Wait()
	b.mu.Lock()
	now := b.now()
	for id, a := range b.accounts {
		b.closeCrossedWeek(id, a, now)
	}
	b.mu.Unlock()
	if err := b.save(); err != nil {
		log.WithError(err).Warn("burn telemetry state save failed")
	}
}

// pollCodexQuota uses the selected credential's current refreshed token and account ID.
// OAuth tokens and response bodies are never persisted in burn history or logs.
func pollCodexQuota(ctx context.Context, a *Auth) (QuotaState, error) {
	var empty QuotaState
	token, _ := a.Metadata["access_token"].(string)
	account, _ := a.Metadata["account_id"].(string)
	if token == "" || account == "" {
		return empty, fmt.Errorf("credential has no Codex access token/account ID")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if a.ProxyURL != "" {
		proxy, err := url.Parse(a.ProxyURL)
		if err != nil {
			return empty, err
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return empty, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-ID", account)
	req.Header.Set("User-Agent", "codex_cli_rs/0.126.0")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return empty, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("quota endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		RateLimit struct {
			Primary   *burnQuotaWindow `json:"primary_window"`
			Secondary *burnQuotaWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return empty, err
	}
	headers := http.Header{}
	for i, w := range []*burnQuotaWindow{payload.RateLimit.Primary, payload.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		prefix := "x-codex-primary-"
		if i == 1 {
			prefix = "x-codex-secondary-"
		}
		if w.Reset <= 0 && w.ResetAfter > 0 {
			w.Reset = time.Now().Unix() + w.ResetAfter
		}
		headers.Set(prefix+"used-percent", fmt.Sprint(w.Used))
		headers.Set(prefix+"reset-at", fmt.Sprint(w.Reset))
		headers.Set(prefix+"window-minutes", fmt.Sprint(w.Seconds/60))
	}
	if !empty.ObserveResponseHeadersForProvider("codex", headers, time.Now()) {
		return empty, fmt.Errorf("quota response had no recognized windows")
	}
	return empty, nil
}

type burnQuotaWindow struct {
	Used       float64 `json:"used_percent"`
	Reset      int64   `json:"reset_at"`
	ResetAfter int64   `json:"reset_after_seconds"`
	Seconds    int64   `json:"limit_window_seconds"`
}

// RefreshCaps checks the shared account immediately before accepting a new request.
// A failed check denies selection even when an earlier snapshot was still fresh.
// Already accepted requests are never canceled; provider-side exact reservations are unavailable.
func (b *BurnController) RefreshCaps(ctx context.Context, a *Auth) bool {
	if a == nil {
		return false
	}
	if !hasBurnCaps(a) {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	b.ensureIdentity(a)
	key := b.key(a.ID)
	if b.capChecks == nil {
		b.capChecks = make(map[string]chan struct{})
	}
	gate := b.capChecks[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		b.capChecks[key] = gate
	}
	b.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-gate }()
	q, err := pollCodexQuota(ctx, a)
	if err != nil {
		b.mu.Lock()
		if b.capFailures == nil {
			b.capFailures = make(map[string]time.Time)
		}
		b.capFailures[key] = b.now()
		b.mu.Unlock()
		return false
	}
	b.Observe(a, q, b.now())
	return b.CapPermits(a, b.now())
}
