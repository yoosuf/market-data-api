# SPEC — Annual Market Data API

Go + SQLite service exposing one read endpoint: the annual high, low, and
traded volume for a stock symbol, aggregated from monthly history cached
locally and backfilled from Alpha Vantage on a cache miss.

## 1. Stack and constraints

| Item | Value |
|---|---|
| Language | Go 1.26+ (floor set by the SQLite driver's own requirement) |
| Storage | SQLite, `database/sql`, parameterized SQL, no ORM |
| HTTP | `net/http` standard library, no framework, no router dependency |
| GraphQL | `graphql-go/graphql`, second transport over the same service (extension) |
| Driver | `modernc.org/sqlite` (pure Go, no CGO) |
| Providers | Configured in the database; Alpha Vantage `TIME_SERIES_MONTHLY` adapter shipped |
| Repository | API and schema live in one repository |

Hard constraints: SQLite is queried first and a cache hit never contacts the
provider; aggregation happens in SQL, not in Go; a retry or repeated import
must not double-count volume; the scheduled refresh is optional and the API
must work without it.

## 2. Public contract

**`GET /symbols/{symbol}/annual/{year}`**

- `symbol` — case-insensitive, normalized to uppercase. Charset `A-Z 0-9 . -`,
  length 1–10. Dots and hyphens are legitimate (`BRK.B`, `RDS-A`) and are not
  rejected.
- `year` — exactly four digits, range 1900 … current year.
- Method must be `GET`; anything else is 405.

Success body returns `high`, `low`, `volume` **as JSON strings**, preserving
exact precision. Strings, not numbers: JSON numbers are IEEE-754 doubles in
most clients, which is the same precision loss that rules out REAL storage.

Implemented response shape:

```json
{"symbol": "IBM", "year": 2023, "high": "153.2100", "low": "129.0000", "volume": "132687444"}
```

Prices render with exactly four decimals, matching the precision the provider
publishes and the scale used in storage. Volume renders as a plain integer
string. See `docs/API.md` for the full reference.

Semantics: `high` = MAX of monthly highs in the year, `low` = MIN of monthly
lows, `volume` = SUM of monthly volumes. All three computed in SQLite, not in Go.

### GraphQL (extension)

`POST /graphql` and `GET /graphql` expose `annual(symbol, year)` returning the
same aggregate. Prices and volume are `String` for the same precision reason;
`volume` additionally exceeds GraphQL's 32-bit `Int`. Outcomes are carried in
`extensions.code`, mirroring the REST status mapping below. Full schema in
`docs/GRAPHQL.md`.

The REST endpoint is the contract; GraphQL is additive and may be removed
without affecting it.

## 3. Error mapping

| Condition | Status | Body |
|---|---|---|
| Malformed year or symbol | 400 | `{"error":"invalid year"}` / `{"error":"invalid symbol"}` |
| Non-GET method | 405 | — |
| Provider succeeded; no months in that year | 404 | `{"error":"no data"}` |
| Provider reports unknown symbol | 404 | `{"error":"no data"}` |
| Provider unreachable, timeout, rate-limited | 503 | `{"error":"upstream unavailable"}` |
| Provider HTTP 200 with unparseable history | 502 | `{"error":"upstream error"}` |
| SQLite failure | 500 | `{"error":"internal error"}` |

**Invariant:** 404 is a claim about the world and may be returned *only* after a
successful provider response. No upstream failure ever produces 404, and no
upstream failure ever writes a negative-cache marker.

**Invariant:** response bodies and logs contain fixed strings only. Never
`err.Error()` — a `*url.Error` carries the full URL including the API key.

## 4. Data model

### `monthly_prices`

| Column | Type | Notes |
|---|---|---|
| symbol | TEXT NOT NULL | normalized uppercase |
| month | TEXT NOT NULL | `YYYY-MM`, the provider's month, not its trading-day date |
| year | INTEGER NOT NULL | denormalized for the aggregation filter + index |
| high, low | INTEGER NOT NULL | price × 10^4 |
| volume | INTEGER NOT NULL | raw share count |
| updated_at | INTEGER NOT NULL | unix seconds |

Primary key / unique constraint on `(symbol, month)` — this is what makes
re-import idempotent and stops volume double-counting. Index on `(symbol, year)`
for the aggregation.

### `providers`

| Column | Notes |
|---|---|
| id | Assigned on create |
| name | Operator label, UNIQUE |
| kind | Selects the adapter in code (`alphavantage`) |
| base_url | Absolute http/https endpoint |
| api_key | Credential, stored and returned in plaintext (see security note) |
| priority | Lower is tried first |
| enabled | Disabled providers are never called |
| timeout_ms | Per-request budget |
| created_at, updated_at | unix seconds; `updated_at` drives adapter rebuilds |

Indexed on `(enabled, priority, id)` so the fetch order is stable rather than
planner-dependent. Managed over `/admin/providers` — see `docs/PROVIDERS.md`. No upstream
endpoint or credential is read from the environment.

**Security:** those routes are unauthenticated and return keys in full, by
configuration choice. The port must stay private.

### `symbol_imports`

| Column | Notes |
|---|---|
| symbol | PK, normalized |
| last_success_at | unix seconds; written only on a successful, validated fetch |
| last_status | last outcome, for diagnostics |

This table distinguishes *unattempted miss* from *fetched, genuinely empty*.
Without it, a valid symbol whose requested year predates its IPO refetches full
history on every request, forever.

### Price representation

Scaled `int64`, scale 10^-4, parsed from the decimal string **without** passing
through `float64`. More than four fractional digits is an import error naming
symbol and month — never silent rounding.

- REAL excluded: cannot represent `0.01` exactly.
- TEXT excluded: `"9.9000" > "10.0000"` lexically.

### Provider selection

Enabled providers are tried in priority order until one answers.
`market.ErrNoData` is returned only when **every** provider answered and agreed
the symbol is unknown; if any provider failed, the chain reports
`market.ErrUpstreamUnavailable`, because absence is unproven and must not be
cached.

## 5. Cache rules

| Rows for (symbol, year) | Import record | Action |
|---|---|---|
| yes | any | hit — serve, no provider call |
| no | none | miss — fetch |
| no | success, < 24h | negative hit — 404, no provider call |
| no | success, >= 24h | refetch |
| no | failure only | miss — fetch |

Current year is served as year-to-date, and refetched when `last_success_at` is
older than 24h even if rows exist. This is the only case where a hit still
fetches, and that rule lives in the service, never in the store.

A year with fewer than 12 months is valid data (IPO year, delisting year,
current year), not an incomplete cache. **Row count is never a completeness test.**

## 6. Architecture

```
cmd/api                wiring: config -> db -> store, client, service, handler -> server
cmd/refresh            wiring: config -> db -> store, client -> importer (Phase 6)
internal/shared/market  domain types; no I/O, no third-party imports
internal/infra/sqlite   database open and pragmas
internal/core/annual    cache-aside workflow (owns read -> fetch -> import -> reread) + store/schema
internal/core/provider  stored config, kind registry, failover chain + store/schema
internal/core/provider/alphavantage  HTTP client, provider JSON -> domain records
internal/infra/httpapi  route parsing, validation, status/JSON mapping
internal/infra/graphqlapi  GraphQL schema, resolvers, error codes
```

Interfaces are declared in `internal/core/annual` (consumer side), not exported by the
adapters. `cmd/refresh` depends on a single-method importer interface and must
not be able to reach the annual aggregation, because that path can return
without fetching.

**Driver:** `modernc.org/sqlite` — pure Go, builds on a reviewer's machine with
no C toolchain and no `CGO_ENABLED` trap. DSN sets `journal_mode(WAL)` and
`busy_timeout(5000)`; without the latter, a concurrent API request and cron run
yield `SQLITE_BUSY` instead of waiting.

No ORM. No DI container. No global mutable state.

## 7. Open assumptions to document in the README

1. Prices stored at scale 10^-4; higher precision is an import error.
2. Negative results cached 24h; upstream failures never cached.
3. Current year served year-to-date with a 24h freshness window.
4. Fewer than 12 months in a year is valid data.
5. Response envelope: fields are returned flat, with prices at four decimals.
   If a reference payload requires a different field set or a trimmed format,
   only `internal/infra/httpapi` changes — storage is already exact.
