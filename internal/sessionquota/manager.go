// Package sessionquota partitions the Codex five-hour pool into protected tmux-session budgets.
// All amounts are normalized quota points, not percentages of another quantity.
package sessionquota

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const Capacity = 250.0
const Header = "X-CLIProxyAPI-Session"
const MetadataKey = "tmux_quota_session"
const DefaultTokensPerPoint = 10000.0
const epsilon = 1e-8

// Bucket.Remaining includes Held. Only Available may be reassigned or admitted.
type Bucket struct {
	Session   string  `json:"session"`
	Cap       float64 `json:"cap"`
	Remaining float64 `json:"remaining"`
	Held      float64 `json:"held"`
	Available float64 `json:"available"`
}
type Account struct {
	ID                     string    `json:"id"`
	Capacity               float64   `json:"capacity"`
	Remaining              float64   `json:"remaining"`
	ObservedRemaining      float64   `json:"observed_remaining"`
	ObservedAt             time.Time `json:"observed_at"`
	ResetAt                time.Time `json:"reset_at"`
	WeeklyResetAt          time.Time `json:"weekly_reset_at"`
	WeeklyExhausted        bool      `json:"weekly_exhausted"`
	TokensPerPoint         float64   `json:"tokens_per_point"`
	TokensSinceObservation float64   `json:"tokens_since_observation"`
}

// Observation is an upstream credential snapshot. Unknown watermarks preserve persisted estimates.
type Observation struct {
	ID                 string
	Capacity           float64
	CapacityConfigured bool // Allows an explicitly zero credential cap.
	Known              bool
	Estimated          bool // Fallback cooldown state is not a calibration watermark.
	Remaining          float64
	ObservedAt         time.Time
	ResetAt            time.Time
	WeeklyResetAt      time.Time
	WeeklyExhausted    bool
}
type Snapshot struct {
	Capacity           float64  `json:"capacity"`
	TotalRemaining     float64  `json:"total_remaining"`
	AllWeeklyExhausted bool     `json:"all_weekly_exhausted"`
	Buckets            []Bucket `json:"buckets"`
	RetryAfterSeconds  int      `json:"retry_after_seconds"`
	Estimated          bool     `json:"estimated"`
	PersistenceError   string   `json:"persistence_error,omitempty"`
}
type state struct {
	Version  int                 `json:"version"`
	Buckets  map[string]*Bucket  `json:"buckets"`
	Accounts map[string]*Account `json:"accounts"`
}
type Manager struct {
	mu               sync.Mutex
	state            state
	path             string
	now              func() time.Time
	persistenceError error
}

type ExhaustedError struct {
	Weekly     bool
	RetryAfter int
	Needed     float64
}

func (e *ExhaustedError) StatusCode() int { return 429 }
func (e *ExhaustedError) Headers() http.Header {
	code := "provisioned_quota_exhausted"
	if e.Weekly {
		code = "weekly_quota_exhausted"
	}
	headers := http.Header{
		"Retry-After":                     []string{strconv.Itoa(e.RetryAfter)},
		"X-Cliproxyapi-Quota-Code":        []string{code},
		"X-Cliproxyapi-Quota-Retry-After": []string{strconv.Itoa(e.RetryAfter)},
	}
	if e.Needed > 0 && finite(e.Needed) {
		headers.Set("X-CLIProxyAPI-Quota-Required-Points", strconv.FormatFloat(e.Needed, 'g', -1, 64))
	}
	return headers
}
func (e *ExhaustedError) Error() string {
	code, message := "provisioned_quota_exhausted", "Out of provisioned quota"
	if e.Weekly {
		code, message = "weekly_quota_exhausted", "Weekly quota exhausted"
	}
	// "quota exceeded" prevents native Codex HTTP retries; the Pi extension
	// owns the asynchronous admission wait instead.
	details := map[string]any{"code": code, "type": code, "message": message, "reason": "quota exceeded", "retry_after_seconds": e.RetryAfter}
	if e.Needed > 0 && finite(e.Needed) {
		details["required_points"] = e.Needed
	}
	payload, _ := json.Marshal(map[string]any{"error": details})
	return string(payload)
}

func New(path string) (*Manager, error) {
	m := &Manager{path: path, now: time.Now, state: state{Version: 1, Buckets: map[string]*Bucket{"": {Session: "", Cap: Capacity}}, Accounts: make(map[string]*Account)}}
	if path == "" {
		return m, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &m.state); err != nil {
		return nil, fmt.Errorf("invalid session quota state: %w", err)
	}
	if m.state.Version != 1 || m.state.Buckets == nil || m.state.Accounts == nil || m.state.Buckets[""] == nil {
		return nil, errors.New("unsupported session quota state")
	}
	for id, a := range m.state.Accounts {
		if a == nil || !finite(a.Capacity) || !finite(a.Remaining) || !finite(a.ObservedRemaining) || !finite(a.TokensPerPoint) || !finite(a.TokensSinceObservation) || a.Capacity <= 0 || a.Capacity > Capacity || a.Remaining < 0 || a.Remaining > a.Capacity+epsilon || a.TokensPerPoint <= 0 || a.TokensSinceObservation < 0 {
			return nil, errors.New("invalid session quota account")
		}
		a.ID = id
	}
	caps := 0.0
	for name, b := range m.state.Buckets {
		if b == nil || !finite(b.Cap) || !finite(b.Remaining) || !finite(b.Held) || b.Cap < 0 || b.Remaining < 0 || b.Held < 0 {
			return nil, errors.New("invalid session quota bucket")
		}
		b.Session = name
		caps += b.Cap
		// A crashed request has unknown consumption. Keep its reservation charged until
		// a fresh upstream observation reconciles the discrepancy; never refund blindly.
		stranded := math.Min(b.Remaining, b.Held)
		b.Remaining -= stranded
		b.Held = 0
		m.debitAccountsLocked(stranded)
	}
	if math.Abs(caps-Capacity) > epsilon {
		return nil, errors.New("session quota caps must sum to 250")
	}
	m.reconcileLocked(m.poolLocked())
	return m, nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func (m *Manager) persistLocked() error {
	if m.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(m.path), 0700)
	}
	var tmp *os.File
	if err == nil {
		tmp, err = os.CreateTemp(filepath.Dir(m.path), ".session-quota-*")
	}
	if err == nil {
		err = tmp.Chmod(0600)
	}
	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if tmp != nil {
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = os.Rename(tmp.Name(), m.path)
	}
	// Keep a failed temporary file for diagnostics/recovery rather than deleting it.
	m.persistenceError = err
	return err
}
func (m *Manager) poolLocked() float64 {
	total := 0.0
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted {
			total += math.Max(0, a.Remaining)
		}
	}
	return math.Min(Capacity, total)
}
func (m *Manager) totalLocked() float64 {
	total := 0.0
	for _, b := range m.state.Buckets {
		total += b.Remaining
	}
	return total
}
func (m *Manager) advanceLocked() {
	now := m.now()
	for _, a := range m.state.Accounts {
		if a.WeeklyExhausted && !a.WeeklyResetAt.IsZero() && !now.Before(a.WeeklyResetAt) {
			a.WeeklyExhausted = false
			a.WeeklyResetAt = time.Time{}
		}
		if !a.ResetAt.IsZero() && !now.Before(a.ResetAt) {
			a.Remaining = a.Capacity
			a.ObservedRemaining = a.Capacity
			a.TokensSinceObservation = 0
			a.ResetAt = time.Time{} // Do not invent a new window until upstream reports its start.
			a.ObservedAt = now      // An old passive watermark must not undo the predicted reset.
		}
	}
	m.reconcileLocked(m.poolLocked())
}

// reconcileLocked conserves the pool. Positive deltas follow deficits; negative
// deltas follow current balances (including interactive), never cap weights.
func (m *Manager) reconcileLocked(target float64) {
	total := m.totalLocked()
	delta := target - total
	if math.Abs(delta) <= epsilon {
		return
	}
	if delta < 0 && total > 0 {
		for _, b := range m.state.Buckets {
			b.Remaining = math.Max(0, b.Remaining*(target/total))
		}
		return
	}
	deficits := 0.0
	for _, b := range m.state.Buckets {
		deficits += math.Max(0, b.Cap-b.Remaining)
	}
	if deficits <= epsilon {
		return
	}
	for _, b := range m.state.Buckets {
		b.Remaining += delta * math.Max(0, b.Cap-b.Remaining) / deficits
	}
}
func (m *Manager) Sync(observations []Observation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	observations = append([]Observation(nil), observations...)
	capacitySum := 0.0
	for _, o := range observations {
		if o.Capacity > 0 {
			capacitySum += o.Capacity
		} else if !o.CapacityConfigured {
			capacitySum += 100
		}
	}
	if capacitySum > Capacity {
		scale := Capacity / capacitySum
		for i := range observations {
			cap := observations[i].Capacity
			if cap <= 0 && !observations[i].CapacityConfigured {
				cap = 100
			}
			observations[i].Capacity = cap * scale
			observations[i].Remaining *= scale
		}
	}
	present := make(map[string]bool, len(observations))
	for _, o := range observations {
		present[o.ID] = true
		a := m.state.Accounts[o.ID]
		cap := o.Capacity
		if cap <= 0 && !o.CapacityConfigured {
			cap = 100
		}
		if a == nil {
			a = &Account{ID: o.ID, Capacity: cap, Remaining: cap, ObservedRemaining: cap, TokensPerPoint: DefaultTokensPerPoint}
			m.state.Accounts[o.ID] = a
		}
		if !o.Known {
			continue
		}
		if !a.ObservedAt.IsZero() && !o.ObservedAt.After(a.ObservedAt) && math.Abs(a.Capacity-cap) <= epsilon {
			continue
		}
		remaining := math.Max(0, math.Min(cap, o.Remaining))
		consumed := a.ObservedRemaining - remaining
		// Coarse watermarks calibrate aggregates, never pretend to identify one concurrent request.
		if !o.Estimated && math.Abs(a.Capacity-cap) <= epsilon && !a.ObservedAt.IsZero() && consumed > 0.01 && a.TokensSinceObservation > 0 && (a.ResetAt.IsZero() || a.ResetAt.Equal(o.ResetAt)) && !o.WeeklyExhausted {
			observedRate := a.TokensSinceObservation / consumed
			if finite(observedRate) && observedRate > 0 {
				a.TokensPerPoint = math.Max(100, math.Min(10000000, 0.8*a.TokensPerPoint+0.2*observedRate))
			}
		}
		plateau := math.Abs(a.ObservedRemaining-remaining) <= epsilon && math.Abs(a.Capacity-cap) <= epsilon && a.WeeklyExhausted == o.WeeklyExhausted && (a.ResetAt.IsZero() || a.ResetAt.Equal(o.ResetAt))
		a.Capacity = cap
		if !plateau {
			a.Remaining = remaining
			a.TokensSinceObservation = 0
		}
		a.ObservedRemaining = remaining
		a.ObservedAt = o.ObservedAt
		a.ResetAt = o.ResetAt
		a.WeeklyResetAt = o.WeeklyResetAt
		a.WeeklyExhausted = o.WeeklyExhausted
	}
	for id := range m.state.Accounts {
		if !present[id] {
			delete(m.state.Accounts, id)
		}
	}
	m.advanceLocked()
	_ = m.persistLocked()
}
func (m *Manager) weeklyLocked() bool {
	if len(m.state.Accounts) == 0 {
		return false
	}
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted {
			return false
		}
	}
	return true
}
func (m *Manager) retryLocked() int {
	now := m.now()
	next := time.Time{}
	for _, a := range m.state.Accounts {
		t := a.ResetAt
		if a.WeeklyExhausted {
			t = a.WeeklyResetAt
		}
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if next.IsZero() {
		return 30
	}
	return int(math.Max(1, math.Ceil(next.Sub(now).Seconds())))
}
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	snapshot := Snapshot{Capacity: Capacity, TotalRemaining: m.totalLocked(), AllWeeklyExhausted: m.weeklyLocked(), RetryAfterSeconds: m.retryLocked(), Estimated: true}
	names := make([]string, 0, len(m.state.Buckets))
	for name := range m.state.Buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b := *m.state.Buckets[name]
		b.Available = math.Max(0, b.Remaining-b.Held)
		snapshot.Buckets = append(snapshot.Buckets, b)
	}
	if m.persistenceError != nil {
		snapshot.PersistenceError = m.persistenceError.Error()
	}
	return snapshot
}
func validName(name string) bool { return name != "" && len(name) <= 256 }
func (m *Manager) Set(name string, cap float64) error {
	if !validName(name) || !finite(cap) || cap < 0 || cap > Capacity {
		return errors.New("session name and cap (0..250 quota points) required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	old := m.state.Buckets[name]
	oldCap := 0.0
	if old != nil {
		oldCap = old.Cap
	}
	interactive := m.state.Buckets[""]
	if cap-oldCap > interactive.Cap+epsilon {
		return errors.New("sum of session caps exceeds 250 quota points")
	}
	if old == nil {
		old = &Bucket{Session: name}
		m.state.Buckets[name] = old
	}
	interactive.Cap -= cap - old.Cap
	old.Cap = cap
	if old.Remaining > math.Max(cap, old.Held) {
		release := old.Remaining - math.Max(cap, old.Held)
		old.Remaining -= release
		interactive.Remaining += release
	}
	need := math.Max(0, cap-old.Remaining)
	grant := math.Min(need, math.Max(0, interactive.Remaining-interactive.Held))
	old.Remaining += grant
	interactive.Remaining -= grant
	return m.persistLocked()
}
func (m *Manager) Clear(name string) error {
	if !validName(name) {
		return errors.New("session name required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.state.Buckets[name]; b != nil {
		interactive := m.state.Buckets[""]
		interactive.Cap += b.Cap
		b.Cap = 0
		release := math.Max(0, b.Remaining-b.Held)
		b.Remaining -= release
		interactive.Remaining += release
		if b.Held <= epsilon {
			delete(m.state.Buckets, name)
		}
	}
	return m.persistLocked()
}
func (m *Manager) ClearAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	interactive := m.state.Buckets[""]
	for name, b := range m.state.Buckets {
		if name == "" {
			continue
		}
		interactive.Cap += b.Cap
		b.Cap = 0
		release := math.Max(0, b.Remaining-b.Held)
		interactive.Remaining += release
		b.Remaining -= release
		if b.Held <= epsilon {
			delete(m.state.Buckets, name)
		}
	}
	return m.persistLocked()
}

// Skim transfers an absolute number of quota points, allocation-weighted, and
// redistributes a dry donor's shortfall among the other funded donors.
func (m *Manager) Skim(amount float64) (float64, error) {
	if !finite(amount) || amount < 0 || amount > Capacity {
		return 0, errors.New("skim amount must be 0..250 quota points")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	left := amount
	for left > epsilon {
		weight := 0.0
		for name, b := range m.state.Buckets {
			if name != "" && b.Cap > 0 && b.Remaining-b.Held > epsilon {
				weight += b.Cap
			}
		}
		if weight <= epsilon {
			break
		}
		moved := 0.0
		for name, b := range m.state.Buckets {
			if name == "" || b.Cap <= 0 {
				continue
			}
			take := math.Min(math.Max(0, b.Remaining-b.Held), left*b.Cap/weight)
			b.Remaining -= take
			moved += take
		}
		if moved <= epsilon {
			break
		}
		left -= moved
		m.state.Buckets[""].Remaining += moved
	}
	return amount - left, m.persistLocked()
}

// Reservation is one atomic admission covering all attempts in a client request.
// Methods are serialized by the owning Manager lock, including concurrent records.
type Reservation struct {
	manager     *Manager
	bucket      *Bucket
	held        float64
	accounted   bool
	closed      bool
	lastAccount string
}

func (m *Manager) Reserve(session string, weightedTokens float64) (*Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	if m.weeklyLocked() {
		return nil, &ExhaustedError{Weekly: true, RetryAfter: m.retryLocked()}
	}
	b := m.state.Buckets[session]
	if b == nil || b.Cap <= 0 {
		b = m.state.Buckets[""]
	}
	// Reserve against the least favorable calibration among accounts that can
	// actually serve this request. A default-rate floor (or an exhausted account)
	// must not erase learned efficiency from the usable subscription pool.
	rate := math.Inf(1)
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted && a.Remaining > epsilon && finite(a.TokensPerPoint) && a.TokensPerPoint > 0 {
			rate = math.Min(rate, a.TokensPerPoint)
		}
	}
	if !finite(rate) {
		rate = DefaultTokensPerPoint
	}
	points := math.Max(0.000001, weightedTokens/rate)
	if !finite(points) || points > b.Remaining-b.Held+epsilon {
		return nil, &ExhaustedError{RetryAfter: m.retryLocked(), Needed: points}
	}
	b.Held += points
	r := &Reservation{manager: m, bucket: b, held: points}
	if err := m.persistLocked(); err != nil {
		b.Held -= points
		return nil, fmt.Errorf("persist session quota admission: %w", err)
	}
	return r, nil
}
func (m *Manager) debitAccountsLocked(points float64) {
	total := 0.0
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted {
			total += a.Remaining
		}
	}
	if total <= epsilon {
		return
	}
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted {
			a.Remaining = math.Max(0, a.Remaining-points*a.Remaining/total)
		}
	}
}

// Consume replaces reservation estimates with observed token usage. Reasoning
// is already contained in output tokens; cached input is contained in input.
func (r *Reservation) Consume(authID string, weightedTokens float64, knownNoCharge bool) {
	if r == nil {
		return
	}
	m := r.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.closed {
		return
	}
	r.lastAccount = authID
	if knownNoCharge {
		r.accounted = true
	}
	if weightedTokens <= 0 {
		return
	}
	r.accounted = true
	rate := DefaultTokensPerPoint
	if a := m.state.Accounts[authID]; a != nil {
		if a.TokensPerPoint > 0 {
			rate = a.TokensPerPoint
		}
		a.TokensSinceObservation += weightedTokens
	}
	cost := weightedTokens / rate
	if a := m.state.Accounts[authID]; a != nil {
		a.Remaining = math.Max(0, a.Remaining-cost)
	} else {
		m.debitAccountsLocked(cost)
	}
	r.bucket.Remaining = math.Max(0, r.bucket.Remaining-cost)
	released := math.Min(r.held, cost)
	r.held -= released
	r.bucket.Held = math.Max(0, r.bucket.Held-released)
	// An underestimated charge cannot create money. Authoritative pool corrections
	// account for overruns across the pool while future admissions remain guarded.
	m.reconcileLocked(m.poolLocked())
	_ = m.persistLocked()
}
func (r *Reservation) Close(noCharge bool) {
	if r == nil {
		return
	}
	m := r.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if !r.accounted && !noCharge {
		r.bucket.Remaining = math.Max(0, r.bucket.Remaining-r.held)
		if a := m.state.Accounts[r.lastAccount]; a != nil {
			a.Remaining = math.Max(0, a.Remaining-r.held)
		} else {
			m.debitAccountsLocked(r.held)
		}
	}
	r.bucket.Held = math.Max(0, r.bucket.Held-r.held)
	r.held = 0
	if r.bucket.Cap <= 0 && r.bucket.Session != "" && r.bucket.Held <= epsilon {
		m.state.Buckets[""].Remaining += r.bucket.Remaining
		delete(m.state.Buckets, r.bucket.Session)
	}
	m.reconcileLocked(m.poolLocked())
	_ = m.persistLocked()
}
