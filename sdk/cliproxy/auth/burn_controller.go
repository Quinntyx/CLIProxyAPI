package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// BurnController keeps private telemetry; it never stores prompts or OAuth tokens.
// A window's PercentPerToken is empirically calibrated, not a fixed plan allowance.
type BurnController struct {
	mu                          sync.Mutex
	cfg                         config.BurnDeadlineConfig
	aliases                     map[string]string
	accounts                    map[string]*BurnAccount
	history                     []BurnWeek
	samples                     []burnSample
	forced                      string
	forcedReset                 time.Time
	list                        func() []*Auth
	now                         func() time.Time
	lastTokenTPS, lastOutputTPS float64
	capChecks                   map[string]chan struct{}
	capFailures                 map[string]time.Time
}
type BurnWindow struct {
	Used                float64   `json:"used_percent"`
	Reset               time.Time `json:"reset_at"`
	Observed            time.Time `json:"observed_at"`
	TokensAtObservation float64   `json:"tokens_at_observation"`
	PercentPerToken     float64   `json:"percent_per_token"`
	CalibrationSamples  int       `json:"calibration_samples"`
	Closed              bool      `json:"closed,omitempty"`
	RemainingTokens     *float64  `json:"remaining_token_equivalents,omitempty"`
	ProjectedTokens     *float64  `json:"projected_full_burn_token_equivalents,omitempty"`
	Urgency             *float64  `json:"urgency_ratio,omitempty"`
}
type BurnAccount struct {
	Windows          map[string]*BurnWindow `json:"windows"`
	EquivalentTokens float64                `json:"equivalent_tokens"`
	ActiveWeek       bool                   `json:"active_week"`
	Exclude          bool                   `json:"exclude_metrics"`
	Caps             map[string]float64     `json:"caps_percent,omitempty"`
}
type BurnWeek struct {
	AuthID           string    `json:"auth_id"`
	Reset            time.Time `json:"reset_at"`
	RemainingPercent float64   `json:"remaining_percent"`
	Active           bool      `json:"active"`
	Excluded         bool      `json:"excluded"`
	Estimated        bool      `json:"estimated"`
}
type burnSample struct {
	Start, End     time.Time
	Tokens, Output float64
	AuthID, Model  string
}
type burnDisk struct {
	Version   int                     `json:"version"`
	Accounts  map[string]*BurnAccount `json:"accounts"`
	Weeks     []BurnWeek              `json:"weeks"`
	Aliases   map[string]string       `json:"aliases"`
	TokenTPS  float64                 `json:"token_tps"`
	OutputTPS float64                 `json:"output_tps"`
}
type BurnStatus struct {
	Enabled        bool                    `json:"enabled"`
	ForcedAuth     string                  `json:"forced_auth,omitempty"`
	Threshold      float64                 `json:"threshold"`
	ActiveTokenTPS float64                 `json:"active_token_tps"`
	OutputTPS      float64                 `json:"output_tps"`
	Inefficiency   *float64                `json:"inefficiency_percent"`
	TotalWaste     *float64                `json:"total_waste_percent"`
	ActiveWeeks    int                     `json:"active_weeks"`
	CompletedWeeks int                     `json:"completed_weeks"`
	EstimatedWeeks int                     `json:"estimated_weeks"`
	Accounts       map[string]*BurnAccount `json:"accounts"`
}

var DefaultBurnController = NewBurnController()

func NewBurnController() *BurnController {
	return &BurnController{accounts: map[string]*BurnAccount{}, aliases: map[string]string{}, now: time.Now}
}
func (b *BurnController) configure(cfg config.BurnDeadlineConfig) {
	if strings.HasPrefix(cfg.StateFile, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.StateFile = home + cfg.StateFile[1:]
		}
	}
	if cfg.Threshold <= 0 || cfg.Threshold > 1 || math.IsNaN(cfg.Threshold) {
		cfg.Threshold = .75
	}
	b.mu.Lock()
	b.cfg = cfg
	b.mu.Unlock()
}
func (b *BurnController) enabled() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.cfg.Enabled }
func (b *BurnController) maxAge() time.Duration {
	age, err := time.ParseDuration(b.cfg.MaxObservationAge)
	if err != nil || age <= 0 {
		return 2 * time.Minute
	}
	return age
}
func (b *BurnController) account(id string) *BurnAccount {
	id = b.key(id)
	a := b.accounts[id]
	if a == nil {
		a = &BurnAccount{Windows: map[string]*BurnWindow{}}
		b.accounts[id] = a
	}
	if a.Windows == nil {
		a.Windows = map[string]*BurnWindow{}
	}
	return a
}
func metadataBool(a *Auth, key string) bool {
	if a == nil {
		return false
	}
	v, _ := a.Metadata[key].(bool)
	return v
}
func capValue(a *Auth, key string) (float64, bool) {
	if a == nil {
		return 0, false
	}
	v, ok := a.Metadata[key]
	if !ok {
		return 100, false
	}
	n, err := strconv.ParseFloat(fmt.Sprint(v), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
		return 0, true
	}
	return n, true
}
func hasBurnCaps(a *Auth) bool {
	_, w := capValue(a, "weekly_cap_percent")
	_, h := capValue(a, "five_hour_cap_percent")
	return w || h
}
func burnEquivalentTokens(d usage.Detail) float64 {
	cached := d.CacheReadTokens
	if cached == 0 {
		cached = d.CachedTokens
	}
	cached = max(0, min(cached, d.InputTokens))
	// Cached input is discounted; calibration absorbs provider/model-specific costs.
	return float64(max(0, d.OutputTokens)+max(0, d.InputTokens-cached)) + .1*float64(cached)
}
func (b *BurnController) HandleUsage(ctx context.Context, r usage.Record) {
	if r.Provider != "codex" || r.AuthID == "" || (r.Generate != nil && !*r.Generate) {
		return
	}
	tokens := burnEquivalentTokens(r.Detail)
	if tokens <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.cfg.Enabled {
		return
	}
	now := b.now()
	a := b.account(r.AuthID)
	// Close a crossed weekly boundary before attributing the new request to its week.
	b.closeCrossedWeek(r.AuthID, a, now)
	a.EquivalentTokens += tokens
	// A long response may finish after its weekly reset; attribute it to its accepted turn.
	historical := false
	for i := range b.history {
		w := &b.history[i]
		if w.AuthID == b.key(r.AuthID) && !r.RequestedAt.IsZero() && r.RequestedAt.Before(w.Reset) && !r.RequestedAt.Before(w.Reset.Add(-7*24*time.Hour)) {
			w.Active = true
			historical = true
		}
	}
	if !historical {
		a.ActiveWeek = true
	}
	latency := r.Latency
	if latency <= 0 {
		latency = time.Millisecond
	}
	end := r.RequestedAt.Add(latency)
	if r.RequestedAt.IsZero() {
		end = now
	}
	b.samples = append(b.samples, burnSample{Start: end.Add(-latency), End: end, Tokens: tokens, Output: float64(max(0, r.Detail.OutputTokens)), AuthID: r.AuthID, Model: r.Model})
	b.prune(now)
	b.lastTokenTPS, b.lastOutputTPS = b.rates(now)
	var q QuotaState
	if q.ObserveResponseHeadersForProvider("codex", r.ResponseHeaders, end) {
		b.observeLocked(r.AuthID, a, q, now)
	}
}
func (b *BurnController) prune(now time.Time) {
	keep := b.samples[:0]
	for _, s := range b.samples {
		if s.End.After(now.Add(-30 * time.Minute)) {
			keep = append(keep, s)
		}
	}
	b.samples = keep
	if len(b.samples) > 4096 {
		b.samples = append([]burnSample(nil), b.samples[len(b.samples)-4096:]...)
	}
}

// rates uses the UNION of active intervals, so overlapping requests are not counted twice.
func (b *BurnController) rates(now time.Time) (float64, float64) {
	b.prune(now)
	if len(b.samples) == 0 {
		return b.lastTokenTPS, b.lastOutputTPS
	}
	ss := append([]burnSample(nil), b.samples...)
	sort.Slice(ss, func(i, j int) bool { return ss[i].Start.Before(ss[j].Start) })
	start, end := ss[0].Start, ss[0].End
	seconds, tokens, output := 0.0, 0.0, 0.0
	for _, s := range ss {
		tokens += s.Tokens
		output += s.Output
		if s.Start.After(end) {
			seconds += end.Sub(start).Seconds()
			start, end = s.Start, s.End
		} else if s.End.After(end) {
			end = s.End
		}
	}
	seconds += end.Sub(start).Seconds()
	if seconds <= 0 {
		return 0, 0
	}
	return tokens / seconds, output / seconds
}
func (b *BurnController) Observe(a *Auth, q QuotaState, now time.Time) {
	if a == nil || a.Provider != "codex" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureIdentity(a)
	state := b.account(a.ID)
	b.setPolicy(a, state)
	b.observeLocked(a.ID, state, q, now)
}
func (b *BurnController) closeCrossedWeek(id string, a *BurnAccount, now time.Time) {
	w := a.Windows["weekly"]
	if w == nil || w.Closed || w.Reset.IsZero() || now.Before(w.Reset) {
		return
	}
	b.closeWeek(id, a, w)
	w.Closed = true
	a.ActiveWeek = false
}
func (b *BurnController) closeWeek(id string, a *BurnAccount, w *BurnWindow) {
	id = b.key(id)
	// Observation timestamps are retained, so estimates cannot masquerade as exact data.
	for _, old := range b.history {
		if old.AuthID == id && old.Reset.Equal(w.Reset) {
			return
		}
	}
	b.history = append(b.history, BurnWeek{AuthID: id, Reset: w.Reset, RemainingPercent: math.Max(0, math.Min(100, 100-w.Used)), Active: a.ActiveWeek, Excluded: a.Exclude, Estimated: w.Observed.Before(w.Reset)})
}
func (b *BurnController) observeLocked(id string, a *BurnAccount, q QuotaState, now time.Time) {
	if q.ObservedAt.IsZero() || q.ObservedAt.After(now.Add(time.Minute)) {
		return
	}
	h := map[string]string{}
	for k, v := range q.Signals {
		h[strings.ToLower(k)] = v
	}
	for _, prefix := range []string{"x-codex-primary-", "x-codex-secondary-"} {
		mins, err := strconv.Atoi(h[prefix+"window-minutes"])
		if err != nil || (mins != 300 && mins != 10080) {
			continue
		}
		kind := "five_hour"
		if mins == 10080 {
			kind = "weekly"
		}
		used, err := strconv.ParseFloat(h[prefix+"used-percent"], 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
			continue
		}
		reset, ok := resetPressureReset(h, prefix, q.ObservedAt, now)
		if !ok || reset.Sub(now) > time.Duration(mins)*time.Minute+time.Minute {
			continue
		}
		old := a.Windows[kind]
		if old != nil && !q.ObservedAt.After(old.Observed) {
			continue
		}
		if old != nil && reset.Before(old.Reset.Add(-time.Minute)) {
			continue
		}
		next := &BurnWindow{Used: used, Reset: reset, Observed: q.ObservedAt, TokensAtObservation: a.EquivalentTokens}
		if old != nil {
			next.PercentPerToken = old.PercentPerToken
			next.CalibrationSamples = old.CalibrationSamples
			if reset.Sub(old.Reset).Abs() < time.Minute {
				delta, tokens := used-old.Used, a.EquivalentTokens-old.TokensAtObservation
				// Rounded percentages are accumulated until a positive movement is observed.
				if delta > 0 && tokens > 0 {
					sample := delta / tokens
					if next.CalibrationSamples == 0 {
						next.PercentPerToken = sample
					} else {
						next.PercentPerToken = .8*next.PercentPerToken + .2*sample
					}
					next.CalibrationSamples++
				} else if delta == 0 {
					next.TokensAtObservation = old.TokensAtObservation
				}
			} else if kind == "weekly" && !old.Closed {
				b.closeWeek(id, a, old)
				a.ActiveWeek = false
			}
		}
		a.Windows[kind] = next
	}
}
func (b *BurnController) effectiveLocked(a *Auth, now time.Time) *Auth {
	copy := a.Clone()
	state := b.accounts[b.key(a.ID)]
	if state == nil {
		return copy
	}
	headers := http.Header{}
	var latest time.Time
	for kind, w := range state.Windows {
		if w == nil || !w.Reset.After(now) {
			continue
		}
		prefix := "x-codex-primary-"
		mins := 300
		if kind == "weekly" {
			prefix = "x-codex-secondary-"
			mins = 10080
		}
		headers.Set(prefix+"used-percent", strconv.FormatFloat(w.Used, 'f', -1, 64))
		headers.Set(prefix+"reset-at", strconv.FormatInt(w.Reset.Unix(), 10))
		headers.Set(prefix+"window-minutes", strconv.Itoa(mins))
		if w.Observed.After(latest) {
			latest = w.Observed
		}
	}
	if latest.After(copy.Quota.ObservedAt) {
		copy.Quota.ObserveResponseHeadersForProvider("codex", headers, latest)
	}
	return copy
}
func (b *BurnController) capPermitsLocked(a *Auth, now time.Time) bool {
	if !hasBurnCaps(a) {
		return true
	}
	state := b.accounts[b.key(a.ID)]
	if state == nil {
		return false
	}
	for _, limit := range []struct{ key, kind string }{{"weekly_cap_percent", "weekly"}, {"five_hour_cap_percent", "five_hour"}} {
		cap, present := capValue(a, limit.key)
		if !present {
			continue
		}
		w := state.Windows[limit.kind]
		if cap <= 0 || w == nil || (!b.capFailures[b.key(a.ID)].IsZero() && !w.Observed.After(b.capFailures[b.key(a.ID)])) || w.Observed.After(now.Add(time.Minute)) || now.Sub(w.Observed) > b.maxAge() || !w.Reset.After(now) || w.Used >= cap {
			return false
		}
	}
	return true
}
func (b *BurnController) CapPermits(a *Auth, now time.Time) bool {
	if !hasBurnCaps(a) {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureIdentity(a)
	state := b.account(a.ID)
	b.setPolicy(a, state)
	b.observeLocked(a.ID, state, a.Quota, now)
	return b.capPermitsLocked(a, now)
}
func (b *BurnController) chooseForcedLocked(auths []*Auth, now time.Time) string {
	if !b.cfg.Enabled {
		b.forced = ""
		return ""
	}
	tps, _ := b.rates(now)
	if tps <= 0 {
		return b.forcedEligibleLocked(auths, now)
	}
	// Keep a chosen burn target through transient rate fluctuations, but never past its deadline/cap.
	if id := b.forcedEligibleLocked(auths, now); id != "" {
		return id
	}
	var deadline time.Time
	for _, a := range auths {
		if a == nil || a.Provider != "codex" || a.Disabled || !b.capPermitsLocked(a, now) {
			continue
		}
		state := b.accounts[b.key(a.ID)]
		if state == nil {
			continue
		}
		effective := b.effectiveLocked(a, now)
		weight, _ := resetPressureWeight(effective, "codex", now)
		if weight <= 0 {
			continue
		}
		for kind, w := range state.Windows {
			if w == nil || !w.Reset.After(now) || now.Sub(w.Observed) > b.maxAge() || w.CalibrationSamples < 2 || w.PercentPerToken <= 0 {
				continue
			}
			ceiling := 100.0
			key := "five_hour_cap_percent"
			if kind == "weekly" {
				key = "weekly_cap_percent"
			}
			if n, yes := capValue(a, key); yes {
				ceiling = n
			}
			remainder := ceiling - w.Used
			projected := b.projectedTokens(state, kind, tps, now) * w.PercentPerToken
			if remainder <= 0 || projected <= 0 || remainder/projected < b.cfg.Threshold {
				continue
			}
			if deadline.IsZero() || w.Reset.Before(deadline) {
				b.forced = a.ID
				b.forcedReset = w.Reset
				deadline = w.Reset
			}
		}
	}
	return b.forced
}
func (b *BurnController) forcedEligibleLocked(auths []*Auth, now time.Time) string {
	if b.forced == "" {
		return ""
	}
	if b.forcedReset.After(now) {
		for _, a := range auths {
			if a != nil && a.ID == b.forced && !a.Disabled && b.capPermitsLocked(a, now) {
				effective := b.effectiveLocked(a, now)
				weight, _ := resetPressureWeight(effective, "codex", now)
				if weight > 0 {
					return b.forced
				}
			}
		}
	}
	b.forced = ""
	b.forcedReset = time.Time{}
	return ""
}

// BurnDeadlineSelector wraps affinity, rather than being hidden under a sticky selector.
type BurnDeadlineSelector struct {
	Fallback   Selector
	Controller *BurnController
}

func (s *BurnDeadlineSelector) Pick(ctx context.Context, provider, model string, opts exec.Options, auths []*Auth) (*Auth, error) {
	b := s.Controller
	if b == nil {
		b = DefaultBurnController
	}
	now := b.now()
	b.mu.Lock()
	candidates := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if a != nil {
			b.ensureIdentity(a)
			state := b.account(a.ID)
			b.setPolicy(a, state)
			b.observeLocked(a.ID, state, a.Quota, now)
			if b.capPermitsLocked(a, now) {
				candidates = append(candidates, b.effectiveLocked(a, now))
			}
		}
	}
	id := b.chooseForcedLocked(candidates, now)
	b.mu.Unlock()
	available, err := getSelectorAvailableAuths(ctx, candidates, provider, model, now)
	if err != nil {
		return nil, err
	}
	if id != "" {
		for _, a := range available {
			if a.ID == id {
				available = []*Auth{a}
				break
			}
		}
	}
	fallback := s.Fallback
	if fallback == nil {
		fallback = &ResetPressureSelector{}
	}
	return fallback.Pick(ctx, provider, model, opts, available)
}
func (b *BurnController) WantsSwitch(current *Auth, auths []*Auth, now time.Time, models ...string) bool {
	if current == nil || current.Provider != "codex" {
		return false
	}
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	if blocked, _, _ := isAuthBlockedForModel(current, model, now); blocked || !b.CapPermits(current, now) {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.chooseForcedLocked(auths, now)
	return id != "" && id != current.ID
}
func (b *BurnController) Snapshot() BurnStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	tps, out := b.rates(now)
	result := BurnStatus{Enabled: b.cfg.Enabled, ForcedAuth: b.forced, Threshold: b.cfg.Threshold, ActiveTokenTPS: tps, OutputTPS: out, Accounts: map[string]*BurnAccount{}}
	sum, activeSum := 0.0, 0.0
	for _, w := range b.history {
		a := b.accounts[w.AuthID]
		if w.Excluded || (a != nil && a.Exclude) {
			continue
		}
		result.CompletedWeeks++
		sum += w.RemainingPercent
		if w.Active {
			result.ActiveWeeks++
			activeSum += w.RemainingPercent
		}
		if w.Estimated {
			result.EstimatedWeeks++
		}
	}
	if result.CompletedWeeks > 0 {
		v := sum / float64(result.CompletedWeeks)
		result.TotalWaste = &v
	}
	if result.ActiveWeeks > 0 {
		v := activeSum / float64(result.ActiveWeeks)
		result.Inefficiency = &v
	}
	// JSON clone ensures callers cannot mutate shared maps or race with observations.
	raw, _ := json.Marshal(b.accounts)
	_ = json.Unmarshal(raw, &result.Accounts)
	for _, a := range result.Accounts {
		for kind, w := range a.Windows {
			if w == nil || w.Closed || w.CalibrationSamples < 2 || w.PercentPerToken <= 0 || !w.Reset.After(now) || now.Sub(w.Observed) > b.maxAge() {
				continue
			}
			cap := 100.0
			if value, yes := a.Caps[kind]; yes {
				cap = value
			}
			remaining := math.Max(0, cap-w.Used) / w.PercentPerToken
			projected := b.projectedTokens(a, kind, tps, now)
			w.RemainingTokens = &remaining
			w.ProjectedTokens = &projected
			if projected > 0 {
				v := remaining / projected
				w.Urgency = &v
			}
		}
	}
	return result
}
func (b *BurnController) save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	path := b.cfg.StateFile
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(burnDisk{Version: 1, Accounts: b.accounts, Weeks: b.history, Aliases: b.aliases, TokenTPS: b.lastTokenTPS, OutputTPS: b.lastOutputTPS}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".burn-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func (b *BurnController) load() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.StateFile == "" {
		return nil
	}
	raw, err := os.ReadFile(b.cfg.StateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state burnDisk
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.Version != 1 {
		return fmt.Errorf("unsupported burn state version %d", state.Version)
	}
	if state.Accounts != nil {
		b.accounts = state.Accounts
	}
	b.history = state.Weeks
	if state.Aliases != nil {
		b.aliases = state.Aliases
	}
	b.lastTokenTPS = state.TokenTPS
	b.lastOutputTPS = state.OutputTPS
	return nil
}

func ConfigureBurnDeadline(cfg config.BurnDeadlineConfig) { DefaultBurnController.configure(cfg) }
func BurnRequiresTurnRouting(auths []*Auth) bool {
	if DefaultBurnController.enabled() {
		return true
	}
	for _, a := range auths {
		if hasBurnCaps(a) {
			return true
		}
	}
	return false
}

func (b *BurnController) setPolicy(auth *Auth, a *BurnAccount) {
	a.Exclude = metadataBool(auth, "exclude_burn_metrics")
	a.Caps = map[string]float64{}
	for _, p := range []struct{ key, kind string }{{"weekly_cap_percent", "weekly"}, {"five_hour_cap_percent", "five_hour"}} {
		if v, yes := capValue(auth, p.key); yes {
			a.Caps[p.kind] = v
		}
	}
}

// Subscription identity survives token refreshes, file renames and device re-enrolment.
func (b *BurnController) key(id string) string {
	if key := b.aliases[id]; key != "" {
		return key
	}
	return id
}
func (b *BurnController) ensureIdentity(a *Auth) {
	account, _ := a.Metadata["account_id"].(string)
	if account == "" {
		return
	}
	digest := sha256.Sum256([]byte(account))
	key := fmt.Sprintf("codex:%x", digest)
	old := b.key(a.ID)
	b.aliases[a.ID] = key
	if old != key && b.accounts[key] == nil && b.accounts[old] != nil {
		b.accounts[key] = b.accounts[old]
		delete(b.accounts, old)
		for i := range b.history {
			if b.history[i].AuthID == old {
				b.history[i].AuthID = key
			}
		}
	}
}

// A weekly forecast cannot ignore pauses caused by the OTHER quota window.
// Do not clamp to the target's own remainder, which would make every account urgent.
func (b *BurnController) projectedTokens(a *BurnAccount, kind string, tps float64, now time.Time) float64 {
	target := a.Windows[kind]
	if target == nil || !target.Reset.After(now) || tps <= 0 {
		return 0
	}
	otherKind := "weekly"
	cycle := 7 * 24 * time.Hour
	if kind == "weekly" {
		otherKind = "five_hour"
		cycle = 5 * time.Hour
	}
	other := a.Windows[otherKind]
	if other == nil || !other.Reset.After(now) || other.CalibrationSamples < 2 || other.PercentPerToken <= 0 || now.Sub(other.Observed) > b.maxAge() {
		return tps * target.Reset.Sub(now).Seconds()
	}
	cap := 100.0
	if value, yes := a.Caps[otherKind]; yes {
		cap = value
	}
	available := math.Max(0, cap-other.Used) / other.PercentPerToken
	end, cursor, total := other.Reset, now, 0.0
	for cursor.Before(target.Reset) {
		limit := end
		if target.Reset.Before(limit) {
			limit = target.Reset
		}
		total += math.Min(tps*limit.Sub(cursor).Seconds(), available)
		cursor = limit
		end = end.Add(cycle)
		available = cap / other.PercentPerToken
	}
	return total
}
