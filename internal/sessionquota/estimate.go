package sessionquota

import (
	"encoding/json"
	"math"
)

// WeightedTokens counts uncached input, cached input and output (including
// reasoning) without double-counting cache or reasoning subsets.
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

// EstimateTokens deliberately assumes uncached input. Output caps include
// reasoning tokens. Unknown allowances use a conservative 16k output allowance.
func EstimateTokens(body []byte) float64 {
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(body, &payload)
	allowance := float64(16384)
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		var value float64
		if json.Unmarshal(payload[key], &value) == nil && finite(value) && value > 0 {
			allowance = value
			break
		}
	}
	// Count the complete input envelope: history, system instructions, tools,
	// structured input and media metadata, not just the newest user message.
	return math.Ceil(float64(len(body))/3) + 4*allowance + 256
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
