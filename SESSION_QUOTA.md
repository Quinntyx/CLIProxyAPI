# Protected tmux-session quota buckets

CLIProxyAPI partitions the usable Codex five-hour allowance into session buckets
and an interactive remainder. Identity is the exact `X-CLIProxyAPI-Session` header;
an unregistered or absent session uses the interactive bucket. Session names are
shared across clients and machines; this is a trusted-client policy, not an
anti-spoofing security boundary.

## Units and allocation

All percentages are absolute normalized quota points (maximum 250). Session caps
plus the interactive cap total 250. Setting a cap funds it immediately from
spendable interactive balance, up to the new cap. Clearing releases uncommitted
balance without invalidating admitted requests.

Positive changes in usable account allowance are allocated proportionally to
`max(0, cap - remaining)`. Negative corrections are proportional to current
remaining balances, including interactive. In-flight holds are included in
remaining and excluded from spendable balance. Skimming transfers an absolute
amount (default 10 points), weighted by assigned session caps; dry donors' shares
are redistributed. It does not change caps or consume in-flight holds.

Admission is atomic under the ledger lock. Requests reserve weighted input,
cache, reasoning/output token allowances. Usage records replace estimates after
completion, using rates calibrated against aggregate upstream quota observations.
Token counts are not an authoritative per-request subscription allowance meter;
the status payload therefore labels balances `estimated`. Account capacity uses
the existing Team-seat/shared-account burn-cap routing policy.

Weekly-exhausted accounts contribute no usable five-hour balance. If all accounts
are weekly-exhausted, admission returns terminal `weekly_quota_exhausted`. A
five-hour exhaustion is reconciled into the provisioned quota protocol instead
of exposing the upstream five-hour error as a terminal failure.

## API

Endpoints use the same client-key authentication as model requests:

- `GET /v1/session-quota`: current buckets, available/held balances and retry time.
- `PUT /v1/session-quota/sessions/:name`: JSON `{ "cap": 100 }`.
- `DELETE /v1/session-quota/sessions/:name`: clear one cap.
- `DELETE /v1/session-quota/sessions`: clear all caps.
- `POST /v1/session-quota/skim`: JSON `{ "amount": 10 }`; returns actual
  `transferred` points and the updated `quota` snapshot.

Names are URL encoded. Exhaustion returns HTTP 429 with a structured JSON error
code and trusted local headers:

- `X-CLIProxyAPI-Quota-Code`: `provisioned_quota_exhausted` or
  `weekly_quota_exhausted`.
- `X-CLIProxyAPI-Quota-Retry-After`: seconds until the next relevant reset.
- `X-CLIProxyAPI-Quota-Required-Points`: admission amount when known.
- `Retry-After`: the same delay.

These local headers are always returned, even when upstream response-header
passthrough is disabled. Upstream headers cannot spoof this metadata. Native
Codex clients may replace every 429 JSON body with friendly upstream-limit text;
clients must retain the trusted header identity to recognize provisioned waits.

## Pi client and persistence

`git:git.quinntyx.dev/quinntyx/pi-session-quota@dev` supplies attribution and
`/quota list`, `set`, `clear`, `clear-all`, and `skim`. It waits asynchronously
inside the pending run, then rebuilds context with queued steering; it does not
replay HTTP bytes or emit `agent_settled` while waiting. Weekly errors remain
terminal. The extension requires Pi's provider-header/response hooks and
pre-settlement continuation support (validated against the installed Pi 1.1.0).

The durable ledger is `.session-quota.state` inside the configured auth directory
(or `session-quota.json` alongside the config when no auth directory is set).
Balances and calibration state are durable; holds are released on restart.
