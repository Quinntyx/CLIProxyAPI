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

// Proxy-local headers carrying best-effort observations from trusted clients.
const InputTokensHeader = "X-CLIProxyAPI-Quota-Input-Tokens"
const CachedTokensHeader = "X-CLIProxyAPI-Quota-Cached-Tokens"

// EstimateTokens assumes uncached input when no usage observations are provided.
// Output caps include reasoning; missing caps reserve a conservative 16k.
func EstimateTokens(body []byte) float64 {
	return EstimateTokensWithHints(body, 0, 0)
}

// Hints are best-effort observations from authenticated Pi clients, not billing
// records. Output allowance is still reserved in full; completion reconciles all
// token types, including a cache miss or an underestimated input envelope.
func EstimateTokensWithHints(body []byte, inputHint, cachedHint float64) float64 {
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
	input := math.Ceil(float64(len(body)) / 3)
	cached := 0.0
	if finite(inputHint) && inputHint > 0 && inputHint <= 10000000 {
		input = math.Min(input, math.Ceil(inputHint))
		if finite(cachedHint) && cachedHint > 0 {
			cached = math.Min(input, cachedHint)
		}
	}
	return input - 0.9*cached + 4*allowance + 256
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
