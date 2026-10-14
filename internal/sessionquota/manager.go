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

// NearEmptyPoints is a fixed concurrency floor, never a request-cost estimate.
const NearEmptyPoints = 1.0

// Remaining is a signed durable balance internally; snapshots expose nonnegative
// credit and separate debt. Held is retained for wire compatibility and is zero.
type Bucket struct {
	Session        string  `json:"session"`
	Cap            float64 `json:"cap"`
	Remaining      float64 `json:"remaining"`
	Held           float64 `json:"held"`
	Available      float64 `json:"available"`
	Debt           float64 `json:"debt"`
	InFlight       int     `json:"in_flight"`
	AdmissionReady bool    `json:"admission_ready"`
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

// ForcedBurn exposes a short-lived routing capability without credential identity.
type ForcedBurn struct {
	Eligible  bool      `json:"eligible"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type Snapshot struct {
	AdmissionMode      string     `json:"admission_mode"`
	NearEmptyPoints    float64    `json:"near_empty_points"`
	ForcedBurn         ForcedBurn `json:"forced_burn"`
	Capacity           float64    `json:"capacity"`
	TotalRemaining     float64    `json:"total_remaining"`
	AllWeeklyExhausted bool       `json:"all_weekly_exhausted"`
	Buckets            []Bucket   `json:"buckets"`
	RetryAfterSeconds  int        `json:"retry_after_seconds"`
	Estimated          bool       `json:"estimated"`
	PersistenceError   string     `json:"persistence_error,omitempty"`
}
type state struct {
	Version  int                 `json:"version"`
	Buckets  map[string]*Bucket  `json:"buckets"`
	Accounts map[string]*Account `json:"accounts"`
	// Per-account window deductions are private durable bookkeeping, never public credential IDs.
	Penalties map[string]map[string]float64 `json:"refill_penalties,omitempty"`
}
type Manager struct {
	mu               sync.Mutex
	state            state
	path             string
	now              func() time.Time
	persistenceError error
	forcedBurn       ForcedBurn
}

type ExhaustedError struct {
	Weekly     bool
	RetryAfter int
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
	payload, _ := json.Marshal(map[string]any{"error": details})
	return string(payload)
}

func New(path string) (*Manager, error) {
	m := &Manager{path: path, now: time.Now, state: state{Version: 2, Buckets: map[string]*Bucket{"": {Session: "", Cap: Capacity}}, Accounts: make(map[string]*Account), Penalties: make(map[string]map[string]float64)}}
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
	if (m.state.Version != 1 && m.state.Version != 2) || m.state.Buckets == nil || m.state.Accounts == nil || m.state.Buckets[""] == nil {
		return nil, errors.New("unsupported session quota state")
	}
	for id, a := range m.state.Accounts {
		if a == nil || !finite(a.Capacity) || !finite(a.Remaining) || !finite(a.ObservedRemaining) || !finite(a.TokensPerPoint) || !finite(a.TokensSinceObservation) || a.Capacity < 0 || a.Capacity > Capacity || (m.state.Version == 1 && a.Remaining < 0) || a.Remaining > a.Capacity+epsilon || a.TokensPerPoint <= 0 || a.TokensSinceObservation < 0 {
			return nil, errors.New("invalid session quota account")
		}
		a.ID = id
	}
	caps := 0.0
	for name, b := range m.state.Buckets {
		if b == nil || !finite(b.Cap) || !finite(b.Remaining) || !finite(b.Held) || b.Cap < 0 || (m.state.Version == 1 && b.Remaining < 0) || b.Held < 0 {
			return nil, errors.New("invalid session quota bucket")
		}
		b.Session = name
		caps += b.Cap
		// Old estimates and crashed slots are not reported usage. Migration and
		// restart release them without fabricating a charge.
		b.Held, b.InFlight, b.Debt, b.AdmissionReady = 0, 0, 0, false
	}
	if math.Abs(caps-Capacity) > epsilon {
		return nil, errors.New("session quota caps must sum to 250")
	}
	if m.state.Penalties == nil {
		m.state.Penalties = make(map[string]map[string]float64)
	}
	for _, deductions := range m.state.Penalties {
		for name, amount := range deductions {
			if m.state.Buckets[name] == nil || !finite(amount) || amount < 0 || amount > Capacity {
				return nil, errors.New("invalid session quota refill penalty")
			}
		}
	}
	m.state.Version = 2
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
			total += a.Remaining
		} else {
			total += math.Min(0, a.Remaining)
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
			m.refillWindowLocked(a.ID)
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
	if delta < 0 {
		// External corrections remain proportional to positive balances. Never
		// redistribute one session's overrun: its signed debit already matches
		// the account debit before reconciliation.
		positive := 0.0
		for _, b := range m.state.Buckets {
			positive += math.Max(0, b.Remaining)
		}
		if positive > 0 {
			for _, b := range m.state.Buckets {
				if b.Remaining > 0 {
					b.Remaining += delta * b.Remaining / positive
				}
			}
		}
		return
	}
	deficits := 0.0
	for _, b := range m.state.Buckets {
		deficits += m.refillDeficitLocked(b)
	}
	if deficits > epsilon {
		grant := math.Min(delta, deficits)
		for _, b := range m.state.Buckets {
			b.Remaining += grant * m.refillDeficitLocked(b) / deficits
		}
		delta -= grant
	}
	// Repayment never enlarges the debtor's deficit share. Unassigned funds
	// belong to interactive treasury, not to another named budget.
	m.state.Buckets[""].Remaining += delta
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
		// A confirmed rollover can arrive before a predicted reset is observed.
		// A post-prediction watermark (ResetAt cleared above) only confirms the
		// new window and must not settle the same penalty twice.
		if !a.ObservedAt.IsZero() && math.Abs(a.Capacity-cap) <= epsilon && ((!a.ResetAt.IsZero() && o.ResetAt.After(a.ResetAt)) || (a.ResetAt.IsZero() && remaining >= cap-epsilon && a.ObservedRemaining < cap-epsilon)) {
			m.refillWindowLocked(a.ID)
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
	burn := m.forcedBurnLocked()
	snapshot := Snapshot{AdmissionMode: "soft", NearEmptyPoints: NearEmptyPoints, ForcedBurn: burn, Capacity: Capacity, TotalRemaining: math.Max(0, m.totalLocked()), AllWeeklyExhausted: m.weeklyLocked(), RetryAfterSeconds: m.retryLocked(), Estimated: true}
	names := make([]string, 0, len(m.state.Buckets))
	for name := range m.state.Buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b := *m.state.Buckets[name]
		if name != "" && b.Cap == 0 && b.Remaining == 0 && b.InFlight == 0 {
			continue
		}
		b.Debt = math.Max(0, -b.Remaining)
		b.Remaining = math.Max(0, b.Remaining)
		b.Available = b.Remaining
		b.AdmissionReady = !snapshot.AllWeeklyExhausted && (burn.Eligible || (b.Remaining > 0 && (b.Remaining > NearEmptyPoints || b.InFlight == 0)))
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
	need := math.Max(0, m.refillCeilingLocked(old)-math.Max(0, old.Remaining))
	if old.Remaining < 0 {
		need = 0
	}
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
		// Keep a tombstone for debt and late request records; clear is not forgiveness.
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
		// Keep a tombstone for debt and late request records; clear is not forgiveness.
	}
	return m.persistLocked()
}

// Skim transfers an absolute number of quota points, allocation-weighted, and
// redistributes a dry donor's shortfall among the other funded donors.
func (m *Manager) Skim(amount float64) (float64, error) {
	return m.SkimTo("", amount)
}

// SkimTo funds the requested registered bucket, or interactive when unregistered.
// Other buckets (including interactive) donate by allocation size, without
// touching held reservations or changing caps. Explicit skims may exceed a cap.
func (m *Manager) SkimTo(session string, amount float64) (float64, error) {
	if !finite(amount) || amount < 0 || amount > Capacity {
		return 0, errors.New("skim amount must be 0..250 quota points")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	destination := m.state.Buckets[session]
	if destination == nil {
		destination = m.state.Buckets[""]
	}
	left := amount
	for left > epsilon {
		weight := 0.0
		for _, b := range m.state.Buckets {
			if b != destination && b.Cap > 0 && b.Remaining-b.Held > epsilon {
				weight += b.Cap
			}
		}
		if weight <= epsilon {
			break
		}
		moved := 0.0
		for _, b := range m.state.Buckets {
			available := b.Remaining - b.Held
			// Use the same donors as the weight calculation. Ineligible dust
			// must not be debited in addition to the requested transfer.
			if b == destination || b.Cap <= 0 || available <= epsilon {
				continue
			}
			// Bound each share by this round's remaining budget, including
			// floating-point overshoot from earlier shares.
			take := math.Min(available, math.Min(left*b.Cap/weight, math.Max(0, left-moved)))
			b.Remaining -= take
			moved += take
		}
		if moved <= 0 {
			break
		}
		left = math.Max(0, left-moved)
		destination.Remaining += moved
		if moved <= epsilon {
			break
		}
	}
	return amount - left, m.persistLocked()
}

// Reservation is one atomic admission covering all attempts in a client request.
// Methods are serialized by the owning Manager lock, including concurrent records.
type Reservation struct {
	manager *Manager
	bucket  *Bucket
	closed  bool
	records map[string]float64
}

// SetForcedBurn publishes only a validated, routable capability from the auth manager.
// Admission passes its own request-specific capability; this snapshot is not authority.
func (m *Manager) SetForcedBurn(burn ForcedBurn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forcedBurn = burn
}
func (m *Manager) forcedBurnLocked() ForcedBurn {
	burn := m.forcedBurn
	if !burn.Eligible || !burn.ExpiresAt.After(m.now()) {
		return ForcedBurn{}
	}
	return burn
}

// Reserve admits by credit, not by payload cost. The ignored argument is kept
// for source compatibility with embedders; no monetary estimate is evaluated.
func (m *Manager) Reserve(session string, _ float64) (*Reservation, error) {
	return m.ReserveRequest(session, false)
}
func (m *Manager) ReserveRequest(session string, forcedBurn bool) (*Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	if m.weeklyLocked() {
		return nil, &ExhaustedError{Weekly: true, RetryAfter: m.retryLocked()}
	}
	b := m.state.Buckets[session]
	if b == nil || (b.Cap <= 0 && b.Remaining >= 0 && b.InFlight == 0) {
		b = m.state.Buckets[""]
	}
	if !forcedBurn && (b.Remaining <= 0 || (b.Remaining <= NearEmptyPoints && b.InFlight > 0)) {
		return nil, &ExhaustedError{RetryAfter: m.retryLocked()}
	}
	b.InFlight++
	r := &Reservation{manager: m, bucket: b, records: make(map[string]float64)}
	if err := m.persistLocked(); err != nil {
		b.InFlight--
		return nil, fmt.Errorf("persist session quota admission: %w", err)
	}
	return r, nil
}
func (m *Manager) debitAccountsLocked(points float64) {
	total := 0.0
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted {
			total += math.Max(0, a.Remaining)
		}
	}
	if total <= 0 {
		return
	}
	for _, a := range m.state.Accounts {
		if !a.WeeklyExhausted && a.Remaining > 0 {
			a.Remaining -= points * a.Remaining / total
		}
	}
}

// Consume records a single execution; repeated cumulative usage is idempotent.
func (r *Reservation) Consume(authID string, weightedTokens float64, knownNoCharge bool) {
	r.ConsumeRecord("", authID, weightedTokens, knownNoCharge)
}

// ConsumeRecord remains valid after Close/cancellation. Each execution ID has a
// cumulative high watermark, so late known usage is retained without double debit.
// A failed/unknown record cannot erase a preceding actual measurement.
func (r *Reservation) ConsumeRecord(recordID, authID string, weightedTokens float64, _ bool) {
	if r == nil || !finite(weightedTokens) || weightedTokens <= 0 {
		return
	}
	m := r.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := r.records[recordID]
	if weightedTokens <= previous {
		return
	}
	r.records[recordID] = weightedTokens
	tokens := weightedTokens - previous
	rate := DefaultTokensPerPoint
	if a := m.state.Accounts[authID]; a != nil {
		if a.TokensPerPoint > 0 {
			rate = a.TokensPerPoint
		}
		a.TokensSinceObservation += tokens
	}
	cost := tokens / rate
	if a := m.state.Accounts[authID]; a != nil {
		a.Remaining -= cost
	} else {
		m.debitAccountsLocked(cost)
	}
	// Signed balance carries the entire overrun against THIS session. It never
	// clamps the debit and reconciles the remainder against other allocations.
	r.bucket.Remaining -= cost
	_ = m.persistLocked()
}

// Close releases the request slot only. Missing usage is not an estimated bill.
func (r *Reservation) Close(_ bool) {
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
	r.bucket.InFlight--
	_ = m.persistLocked()
}

func (m *Manager) refillCeilingLocked(b *Bucket) float64 {
	penalty := 0.0
	for _, deductions := range m.state.Penalties {
		penalty += deductions[b.Session]
	}
	return math.Max(0, b.Cap-penalty)
}

// Each credential's new window pays a proportional part of the debtor's next
// normal allocation, rather than adding debt to its refill deficit. Deductions
// survive restarts/plateau observations and expire only on that credential's
// following rollover. Large overruns take multiple owner refills to repay.
func (m *Manager) refillWindowLocked(accountID string) {
	if m.state.Penalties == nil {
		m.state.Penalties = make(map[string]map[string]float64)
	}
	expired := 0.0
	for _, amount := range m.state.Penalties[accountID] {
		expired += amount
	}
	interactive := m.state.Buckets[""]
	interactive.Remaining -= math.Min(expired, math.Max(0, interactive.Remaining-interactive.Cap))
	capacity := 0.0
	for _, a := range m.state.Accounts {
		capacity += a.Capacity
	}
	share := 0.0
	if a := m.state.Accounts[accountID]; a != nil && capacity > 0 {
		share = a.Capacity / capacity
	}
	deductions := make(map[string]float64)
	for name, b := range m.state.Buckets {
		repayment := math.Min(math.Max(0, -b.Remaining), b.Cap*share)
		if repayment > 0 {
			b.Remaining += repayment
			if name != "" {
				interactive.Remaining -= math.Min(repayment, math.Max(0, interactive.Remaining-interactive.Cap))
			}
			deductions[name] = repayment
		}
	}
	m.state.Penalties[accountID] = deductions
	// A previous partial rollover can leave unassigned repayment treasury
	// backing a still-negative owner. Return only its over-cap surplus when
	// the next proportional owner grant completes repayment.
	deficits := 0.0
	for name, b := range m.state.Buckets {
		if name != "" {
			deficits += m.refillDeficitLocked(b)
		}
	}
	if deficits > epsilon {
		grant := math.Min(deficits, math.Max(0, interactive.Remaining-interactive.Cap))
		for name, b := range m.state.Buckets {
			if name != "" {
				b.Remaining += grant * m.refillDeficitLocked(b) / deficits
			}
		}
		interactive.Remaining -= grant
	}
}
func (m *Manager) refillDeficitLocked(b *Bucket) float64 {
	// Unpaid carry debt cannot enlarge an ordinary refill share or be silently
	// repaid again by the same window. Only the next owner's normal grant (or
	// an explicitly authorized targeted skim) may settle it.
	if b.Remaining < 0 {
		return 0
	}
	return math.Max(0, m.refillCeilingLocked(b)-b.Remaining)
}
