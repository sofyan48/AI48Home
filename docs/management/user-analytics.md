# User portal analytics

`GET /user/analytics` uses the existing User API authentication contract (normally
`Authorization: Bearer <user-session-token>`). It is not a Management API route;
management credentials and CPA API keys do not grant access. The existing
`GET /user/capabilities` manifest advertises `capabilities.analytics: true`.

## Filters

| Query | Type | Meaning |
| --- | --- | --- |
| `from` | optional YYYY-MM-DD or RFC3339 timestamp | Inclusive start, normalized to UTC. Defaults to `to` minus 30 days. |
| `to` | optional YYYY-MM-DD or RFC3339 timestamp | **Exclusive** end, normalized to UTC. Defaults to tomorrow's UTC midnight (includes today so far). A date-only end is midnight at the start of that date, not the following day. |
| `model` | optional non-empty string, at most 512 bytes | Exact, case-sensitive match against the immutable charge `model`, not alias or actual response model. Leading/trailing whitespace is trimmed. |
| `api_key_id` | optional positive integer | Must belong to the authenticated user. Soft-deleted owned keys are accepted for historical exports; foreign, nonexistent, or reassigned-away keys return the same 404. |

The range must be positive and no longer than 366 elapsed days. Each recognized
parameter must have exactly one non-empty value if supplied. Unknown parameters
are ignored, including user/ownership overrides. All four aggregates share the
same filters. There is no pagination, truncation, or separate CSV endpoint.

Examples:

```http
GET /user/analytics
GET /user/analytics?from=2026-06-01&to=2026-07-01
GET /user/analytics?from=2026-06-01&to=2026-07-01&model=gpt-4.1-mini&api_key_id=42
```

## Exact JSON response types

```typescript
interface AnalyticsMetrics {
  requests: number;          // integer count of ledger rows
  input_tokens: number;      // integer sum of charge input_tokens
  output_tokens: number;     // integer sum of charge output_tokens
  cache_tokens: number;      // integer sum of charge cache_tokens
  billed_amount: number;     // actual amount charged, after user multiplier
  base_amount: number;       // stored list amount before user multiplier
  unpriced_requests: number; // integer count with diagnostic unpriced_model
}
interface UserAnalytics {
  from: string;              // normalized UTC RFC3339 timestamp, inclusive
  to: string;                // normalized UTC RFC3339 timestamp, exclusive
  timezone: "UTC";
  source: "billing_charge";
  model: string;             // applied model filter, "" when absent
  api_key_id: number | null; // applied key filter, null when absent
  summary: AnalyticsMetrics;
  daily: (AnalyticsMetrics & { date: string })[]; // YYYY-MM-DD ascending
  per_model: (AnalyticsMetrics & { model: string })[]; // model ascending
  per_key: (AnalyticsMetrics & { api_key_id: number | null })[]; // ID ascending, null first
}
```

`daily` includes every UTC date intersecting the half-open range, filling empty
days with zero metrics. The first/last day can be partial for timestamp filters;
a 366-day non-midnight range can intersect 367 calendar dates. Empty results have
a zero summary, zero-filled daily rows, and empty `per_model`/`per_key` arrays
(never null). Historical charges without a key ID use a null `per_key.api_key_id`.
No key value, masked secret, key label, user identity, request payload, upstream
credential, price rule, or price snapshot is returned.

## Accounting and scope

The **immutable billing charge ledger is the sole source** for every metric.
One charge is uniquely linked to usage by `billing_charge.usage_id`; aggregating
the ledger directly avoids multiplying costs through joins and remains valid if
usage rows are pruned. Charge time is captured from the original usage timestamp
in UTC, not the later insertion time. Ownership is always the charge's snapshotted
`user_id`, never current key ownership or a payload-supplied user ID. Current key
ownership is checked only to authorize the optional key filter, not to broaden
the charge scope. After reassignment, old usage remains visible to the old user
in unfiltered aggregates and cannot leak to the new user.

`requests` means **charge-associated requests**, not all attempts: uncharged
failed/non-billable requests, unknown owners, and legacy activity without an
ownership snapshot are excluded. Failed requests with consumed billable tokens
can have charges and are included. Unpriced usage that produced a ledger row is
included at its stored amount and counted in `unpriced_requests`; a deliberately
zero-price rule is not unpriced. Zero billed cost alone does not imply missing
pricing.

Token sums are the charge-time token counters, preserving the same accounting as
billing history. Cache and input counters can overlap under legacy/provider
accounting: do not add all token fields to invent a total. Cost is broken down by
day, model, and key, not recomputed from present-day prices. Amounts use the same
billing units and float64 representation as existing billing APIs; no currency
conversion or claim of USD is introduced. `base_amount` is the persisted value
and can be zero for old records without a captured base amount. Sums are not
re-priced or re-rounded; clients should format monetary display appropriately.
This endpoint does not synthesize per-token-bucket monetary allocation from
historical expression prices or mixed multipliers.

## CSV export

Clients can export `daily`, `per_model`, or `per_key` directly, using their dimension
as the first column and the seven metrics as subsequent columns. Repeat the
returned `from`/`to` metadata when needed. No additional requests are required.
Apply normal CSV quoting and spreadsheet-formula injection protection to model
names (untrusted strings); do not treat a model beginning with `=`, `+`, `-`, or
`@` as a spreadsheet formula. No UI implementation is part of this backend API.

## Errors

Errors use the existing User API `{ error, message }` envelope:

- `401`: unauthenticated/invalid session; existing account approval checks apply.
- `400 invalid_analytics_query`: malformed, empty, repeated recognized filters,
  nonpositive/oversized ranges, invalid key ID syntax, or oversized model.
- `404 analytics_key_not_found`: nonexistent, foreign, or reassigned-away key ID.
- `500 analytics_load_failed`: database/query failure.
