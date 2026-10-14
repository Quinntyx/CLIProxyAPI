# Session soft quotas

Session quotas govern admission at request granularity, not by predicting the cost of a prompt.
An ordinary request can start while its bucket has positive credit. At one quota point or less,
only one request may be in flight in that bucket. Requests already admitted are allowed to finish.
This is a soft cap: overlapping requests can overrun the remaining credit.

## Accounting and cancellation

No prompt-size estimate is reserved or used to reject monetary admission. Provider-reported input,
cached-input, and output usage is converted to points using the learned account calibration after
execution. That conversion is still approximate; points are not an authoritative OpenAI billing meter.
Genuine context/payload limits remain separate request errors and are not refillable quota waits.

A request slot ends on terminal usage, EOF, or cancellation. Cancellation releases it even when the
upstream producer never closes. Late measured usage remains attached to the original session and
is deduplicated per execution. Missing final usage is not replaced with a full estimated charge;
subsequent subscription observations reconcile the shared pool when authoritative usage is unavailable.
Legacy estimated holds are cleared on migration without billing them.

An overrun belongs to the spending session. It does not debit another named allocation. Debt reduces
the owner's next normal refill and may take several windows to repay; rollover penalties persist
across restarts and unchanged observations. Explicit targeted skims can provide additional credit.

## Forced burn

A currently routable, genuinely forced burn target overrides both the positive-credit gate and the
near-empty concurrency gate, so waiting agents can work in parallel before the quota expires. Actual
usage is still accounted to their session. The request is pinned to the eligible target; stale,
expired, disabled, weekly-exhausted, wrong-model, or conflicting pinned routes cannot bypass admission
or fall back to an unrelated account under the same override. Normal preference rules apply otherwise.

## Public status

`GET /v1/session-quota?model=<active-model>` reports `admission_mode: "soft"`,
`near_empty_points: 1`, and `forced_burn: {eligible, expires_at}`. Each bucket exposes `available`,
`debt`, `in_flight`, and `admission_ready`; legacy `held` remains zero. Polling clients should use
model-scoped `admission_ready`, not an obsolete required-points estimate. A paused Pi run remains
pending and cancelable until readiness changes; weekly exhaustion is a real terminal outcome.
