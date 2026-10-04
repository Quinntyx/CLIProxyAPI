# Burn-deadline routing

The fork's dev branch supports `routing.strategy: reset-pressure` plus a `routing.burn-deadline` controller.
Enable `routing.session-affinity` to retain account affinity by session and model. New sessions use reset pressure; calibrated burn deadlines can override affinity between requests. WebSocket cached deltas are rejected before upstream acceptance when changing accounts, letting Pi resend full context safely.

```yaml
routing:
  strategy: reset-pressure
  session-affinity: true
  session-affinity-ttl: 168h
  burn-deadline:
    enabled: true
    threshold: 0.75
    poll-interval: 1m
    max-observation-age: 2m
    state-file: ~/.local/state/cliproxyapi/burn.json
```

The controller observes usage records and passively reported quota, and polls the Codex usage endpoint independently of prompting. It reports rolling output TPS and aggregate active token-equivalent throughput (overlapping request intervals count once). Input cache reads are discounted; observed quota changes calibrate account/window burn costs. Calibration needs at least two positive quota changes. Until then, normal sticky/reset-pressure routing continues without speculative forced switches.

For both five-hour and weekly windows, an account becomes urgent when `remaining usable quota / projected continuous burn >= threshold`. Weekly projection is bounded by the other five-hour window's capacity/reset. The earliest urgent deadline wins, and the choice is latched until its deadline, cap or availability changes. Predictions are estimates, not fixed Codex token allowances or guarantees of future throughput.

## Weekly accounting

Each completed subscription-week contributes `100 - observed weekly used percent` waste. Inefficiency averages active weeks with nonzero locally recorded token usage; total waste averages all observed completed weeks, including idle weeks. Five-hour resets never enter these tallies. Initial/missing history cannot be reconstructed from tokens; stale closures are marked estimated. Quota consumed by other clients changes the observed remainder. Missing provider observations aren't invented as exact idle periods.

`GET /v8/management/observability/burn` returns current account/window estimates, TPS, forced target and weekly history/tallies. It is protected by the existing management key. State is atomically written mode 0600; no access/refresh tokens or prompt content are persisted.

## Shared-account caps

These fields belong in the private Codex credential JSON, not model configuration:

```json
{"weekly_cap_percent":50,"five_hour_cap_percent":50,"exclude_burn_metrics":true,"websockets":true}
```

Caps apply to total provider-reported utilization, including other users' usage, not an additional personal allowance. Either cap at/above 50 blocks new traffic. Unknown, expired or stale quota fails closed. A selected capped account is checked against the quota endpoint immediately before a new request; failed checks also fail closed. Pending accepted requests are not canceled, and provider-side exact reservations are unavailable, so in-flight/external consumption can overshoot the threshold. Shared accounts are excluded from both metrics when `exclude_burn_metrics` is true.

## Pi WebSockets

Use the `openai-codex-responses` API, base URL `http://127.0.0.1:8317/backend-api/codex`, and `transport: websocket-cached`. The local proxy API key must include the account claim expected by Pi's native Codex client. This opaque local key is only authenticated by the proxy; the selected real OAuth token/account ID replaces it upstream. Do not put OAuth tokens or the local key in public model configuration.

`examples/pi-proxy-websocket-smoke.mjs` runs two tiny actual generations using the installed pi-ai directory passed as its argument. It asserts one connection, reuse, a delta request and no SSE fallback. Cache token hits themselves depend on matching sufficiently long prefixes.

Tests: `go test ./...`. The existing `TestAdvertiserAndBrowser_Integration` requires functioning multicast DNS on the host; use `-skip '^TestAdvertiserAndBrowser_Integration$'` for the otherwise complete suite on hosts that cannot discover their multicast advertiser.
