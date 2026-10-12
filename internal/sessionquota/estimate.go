package sessionquota

import (
	"bytes"
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

const (
	opaqueFallbackBytesPerToken  = 32.0
	opaqueHintFloorBytesPerToken = 48.0
)

// Hints are best-effort observations from Pi clients, not billing records.
// They are accepted only when consistent with the serialized provider payload:
// arbitrary tiny values cannot erase visible input, while raw-history fallback
// values cannot inflate a much smaller outbound request. Output allowance is
// always reserved in full and completion reconciles authoritative usage.
func EstimateTokensWithHints(body []byte, inputHint, cachedHint float64) float64 {
	var payload map[string]json.RawMessage
	validPayload := json.Unmarshal(body, &payload) == nil
	allowance := float64(16384)
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		var value float64
		if json.Unmarshal(payload[key], &value) == nil && finite(value) && value > 0 {
			allowance = value
			break
		}
	}

	// The wire envelope is a conservative upper bound. The structural estimate
	// excludes opaque encrypted replay state: its byte length is not a token
	// count and can dwarf the actual context represented by the request.
	envelope := math.Ceil(float64(len(body)) / 3)
	structural, opaque := envelope, 0.0
	if validPayload {
		structural, opaque = estimateInputShape(body, envelope)
	}
	input := envelope
	if opaque > 0 {
		// Ciphertext is replay state, not provider-tokenized text. When a client
		// observation is absent or rejected, reserve a conservative opaque-state
		// allowance rather than treating every serialized base64 byte as a token.
		input = math.Min(envelope, structural+math.Ceil(opaque/opaqueFallbackBytesPerToken))
	}
	cached := 0.0
	if plausibleInputHint(inputHint, structural, input, envelope, opaque) {
		// A hint may reduce admission, never inflate the payload-derived bound.
		input = math.Min(input, math.Ceil(inputHint))
		if finite(cachedHint) && cachedHint > 0 && cachedHint <= inputHint {
			cached = math.Min(input, math.Ceil(cachedHint))
		}
	}
	return input - 0.9*cached + 4*allowance + 256
}

func estimateInputShape(body []byte, fallback float64) (structural, opaque float64) {
	opaqueBytes := opaqueJSONBytes(body)
	if opaqueBytes <= 0 || opaqueBytes >= len(body) {
		return fallback, 0
	}
	visibleBytes := len(body) - opaqueBytes
	return math.Min(fallback, math.Max(1, math.Ceil(float64(visibleBytes)/3))), float64(opaqueBytes)
}

// opaqueJSONBytes scans serialized JSON without decoding large ciphertext
// strings. Malformed input returns zero so admission falls back conservatively.
func opaqueJSONBytes(body []byte) int {
	total := 0
	for i := 0; i < len(body); {
		if body[i] != '"' {
			i++
			continue
		}
		end, ok := jsonStringEnd(body, i)
		if !ok {
			return 0
		}
		key := body[i+1 : end-1]
		cursor := skipJSONSpace(body, end)
		if !bytes.Equal(key, []byte("encrypted_content")) || cursor >= len(body) || body[cursor] != ':' {
			i = end
			continue
		}
		valueStart := skipJSONSpace(body, cursor+1)
		// Only ciphertext strings are opaque. Objects, arrays and numeric values
		// under this name must not hide arbitrary tokenized input.
		valueEnd, ok := jsonStringEnd(body, valueStart)
		if !ok {
			return 0
		}
		total += valueEnd - valueStart
		i = valueEnd
	}
	return total
}

func jsonStringEnd(body []byte, start int) (int, bool) {
	if start >= len(body) || body[start] != '"' {
		return 0, false
	}
	escaped := false
	for i := start + 1; i < len(body); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch body[i] {
		case '\\':
			escaped = true
		case '"':
			return i + 1, true
		}
	}
	return 0, false
}

func skipJSONSpace(body []byte, start int) int {
	for start < len(body) {
		switch body[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			return start
		}
	}
	return start
}

func plausibleInputHint(hint, structural, fallback, envelope, opaqueBytes float64) bool {
	if !finite(hint) || hint <= 0 || hint > 10000000 || hint > envelope {
		return false
	}
	lower := math.Max(1, structural*0.75)
	upper := math.Max(structural+16384, structural*4)
	if opaqueBytes > 0 {
		// Encrypted replay state can expand far beyond represented context. Keep
		// an independent floor so an untrusted tiny hint cannot erase it, while
		// allowing observed context substantially below the raw body/3 envelope.
		lower = math.Max(lower, math.Ceil(opaqueBytes/opaqueHintFloorBytesPerToken))
		upper = math.Max(upper, fallback*2)
	} else {
		lower = math.Max(lower, envelope*0.25)
	}
	return hint+1 >= lower && hint <= upper
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
