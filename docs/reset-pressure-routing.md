# Reset-pressure routing

Select `routing.strategy: reset-pressure` (`rp` and `resetpressure` are aliases).
The strategy is available at startup, during configuration reload, and through
the management API in both legacy and v8 configurations.

```yaml
routing:
  strategy: reset-pressure
  session-affinity: false
```

## How requests are distributed

For each account, valid observations of the five-hour and seven-day quotas give:

```
window pressure = (1 - used fraction) * window duration / max(time to reset, 60s)
account weight  = 1 + min(1000, max(5h pressure, weekly pressure))
```

A smooth weighted selection allocates requests proportionally to these weights.
Consequently, unused quota close to **either** reset gets more traffic. Window
length normalization prevents the weekly budget from being dwarfed by the 5h
budget. The maximum, rather than the sum, avoids counting the same request's
consumption twice. Less remaining quota reduces pressure; a known 100%-consumed
window excludes the account until its observed reset. If every candidate is
exhausted, selection returns the normal cooldown error and earliest recovery.

For example, otherwise identical unused 5h accounts resetting in one and four
hours have weights 6 and 2.25: about 73% and 27% of requests. An unused weekly
account resetting in one day has weight 8. As time passes toward reset, its
weight increases without resetting the selection credits.

This is a bounded **heuristic**, not a promise of optimal token allocation:
percentages do not reveal absolute subscription capacity or future demand, and
both budgets constrain actual execution. All quotas use upstream observations;
there is no quota-polling traffic and no synthetic quota metadata persisted in
credential files.

## Observations and safety

- Codex HTTP responses and websocket quota events already feed the passive quota
  snapshot. Primary/secondary `used-percent`, `window-minutes`, `reset-at`, and
  `reset-after-seconds` are consumed. Explicit window durations must be 300 or
  10080 minutes, so primary and secondary can be swapped safely. Without a
  duration, the conventional primary=5h / secondary=7d mapping is used.
- Claude's unified `5h` / `7d` utilization (fractions) and reset headers are also
  supported. Additional model-specific limits are deliberately not scored.
- Absolute Unix-second or RFC3339 resets take precedence. Relative countdowns
  are anchored to **observation time**, not selection time. Once a reset passes,
  that window's old observation is ignored, not assumed to have refilled.
- Missing, malformed, nonfinite, out-of-range, expired, or implausibly distant
  observations contribute no pressure. Unknown accounts and unsupported
  providers retain weight 1, giving exploration traffic and ordinary fair
  balancing. A new/restarted process learns quota information as responses arrive.
- Existing disablement, model capability, retry exclusions, credential priority,
  cooldown, and websocket eligibility remain authoritative. Pressure balances
  accounts within the highest available explicit priority tier. Static `weight`
  fields are specific to weighted-round-robin and are not used here.
- Session affinity and an installed scheduler plugin can override per-request
  balancing. Disable affinity and avoid an overriding scheduler plugin when
  reset-pressure should govern every request. The strategy uses the manager's
  live selection path, not the static round-robin scheduler fast path.
- Selection is concurrency-safe, bounds rotation state, preserves fractional
  credits across changing pressure, and never modifies credential or cooldown
  state. A one-minute denominator floor and maximum boost bound near-reset spikes.

Known exhaustion also excludes accounts that might otherwise consume purchased
overage/credits: this strategy optimizes included quota rather than spending
extra credits. Other clients consuming the same account can make snapshots
inexact; upstream errors and normal retries remain the final authority.
