package sessionquota

// Legacy client hints are stripped, never used for admission or billing.
const InputTokensHeader = "X-CLIProxyAPI-Quota-Input-Tokens"
const CachedTokensHeader = "X-CLIProxyAPI-Quota-Cached-Tokens"

func WeightedTokens(input, output, cached, cacheRead, cacheWrite, reasoning int64) float64 {
	input = max(0, input)
	output = max(0, output)
	cache := min(input, max(cached, cacheRead))
	// Providers reporting reasoning separately from output still incur output cost.
	output = max(output, max(0, reasoning))
	// Cache creation is normally part of input; only count a separately reported excess.
	input = max(input, max(0, cacheWrite))
	return float64(input-cache) + 0.1*float64(cache) + 4*float64(output)
}

func (m *Manager) EligibleAccount(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	a := m.state.Accounts[id]
	return a == nil || (!a.WeeklyExhausted && a.Remaining > epsilon)
}
func (m *Manager) Error() *ExhaustedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceLocked()
	return &ExhaustedError{Weekly: m.weeklyLocked(), RetryAfter: m.retryLocked()}
}
