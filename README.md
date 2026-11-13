# Annual Market Data API

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![Tests](https://img.shields.io/badge/tests-passing-brightgreen)]()
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A Go + SQLite service exposing one endpoint: the annual high, low, and traded
volume for a stock symbol. SQLite is the cache of record; Alpha Vantage is
consulted only when the cache cannot answer.

**Highlights for reviewers**

- Standard library only for HTTP (`net/http`); no router, no framework, no ORM.
- Modular monolith: bounded contexts under `internal/core/*`, shared kernel
  under `internal/shared/*`, infrastructure under `internal/infra/*`.
- Prices never touch `float64` — parsed from decimal strings into scaled
  `int64` (10^-4); more than 4 decimals is a loud import error.
- Aggregation runs in SQL (`MAX`/`MIN`/`SUM`), not in Go.
- Idempotent volume via a unique constraint + upsert; retries cannot double-count.
- Two transports (REST + GraphQL) over one `annual.Service`; a parity test
  guards against drift.
- Provider failover with classification: `ErrNoData` only when every provider
  agreed; any failure is unavailability, never cached as absence.
- Database-backed provider configuration with CRUD admin API; no secrets in env.

```
GET /symbols/{symbol}/annual/{year}
```

```json
{
  "symbol": "IBM",
  "year": 2023,
  "high": "153.2100",
  "low": "129.0000",
  "volume": "132687444"
}
```

`high`, `low`, and `volume` are JSON **strings**. JSON numbers are IEEE-754
doubles in most clients, which would discard the exact precision the service
stores.

The same data is also available over GraphQL at `POST /graphql`, resolving
through the identical cache-aside service:

```graphql
{ annual(symbol: "IBM", year: 2023) { symbol year high low volume months } }
```

See [docs/GRAPHQL.md](docs/GRAPHQL.md). REST remains the primary contract;
GraphQL is an addition, not a replacement.

## Requirements

- Go 1.26 or newer (the `go` directive in `go.mod`; a newer toolchain is fetched automatically unless `GOTOOLCHAIN=local` is set)
- At least one upstream provider key, supplied through the provider API rather
  than the environment. Alpha Vantage is free:
  <https://www.alphavantage.co/support/#api-key>

No C toolchain is required: the SQLite driver is pure Go.

## Run

```sh
go run ./cmd/api
```

The server creates and migrates `./market.db` on start, then listens on
`:8080`. No separate migration step exists.

A fresh database has no providers, so cache misses answer 503 until you add
one. Providers live in the database, not the environment:

```sh
curl -s -X POST localhost:8080/admin/providers \
  -H 'Content-Type: application/json' \
  -d '{"name":"alphavantage","kind":"alphavantage",
       "base_url":"https://www.alphavantage.co/query","api_key":"YOUR_KEY"}'
```

That is the only setup step. Several providers can be configured; they are
tried in `priority` order until one answers. See
[docs/PROVIDERS.md](docs/PROVIDERS.md).

> **Security:** the `/admin/providers` routes are **unauthenticated** and
> return API keys **in full**, by configuration choice. Anyone who can reach
> the port can read every credential and repoint the service at a host they
> control, and anyone with a copy of `market.db` has the keys in cleartext.
> Keep the port private, or set `PROVIDER_ADMIN_API=false` to remove the
> routes while keeping database-backed configuration.

```sh
curl -s localhost:8080/symbols/IBM/annual/2023
curl -s localhost:8080/healthz

curl -s localhost:8080/graphql -H 'Content-Type: application/json' \
  -d '{"query":"{ annual(symbol:\"IBM\", year:2023){ high low volume } }"}'
```

## Configuration

| Variable | Default | Used by | Notes |
|---|---|---|---|
| `ADDR` | `:8080` | api | Listen address. |
| `DB_PATH` | `./market.db` | both | Use an **absolute** path if you also run the refresh job. |
| `PROVIDER_ADMIN_API` | `true` | api | `false` removes the `/admin/providers` routes. |
| `REFRESH_SYMBOLS` | `IBM,AAPL,MSFT` | refresh | Comma-separated. |
| `REFRESH_TIMEOUT_SECONDS` | `120` | refresh | Whole-run budget. |

**No upstream credential is read from the environment.** Endpoint, API key and
timeout are per-provider columns in the `providers` table, so a second
provider or a rotated key needs no change to the deployment's configuration.

A relative `DB_PATH` resolves against each process's working directory, so the
API and a cron job started elsewhere would silently use two different
databases. Cron especially: give it an absolute path.

## Test

```sh
go test ./...          # full suite
go test -race ./...    # includes the concurrent cache-miss test
go vet ./...
gofmt -l .             # prints nothing when formatting is clean
```

No test contacts Alpha Vantage. Provider behavior is covered with local
`httptest` servers, so the suite consumes no API quota and runs offline.

## How a request is served

1. Aggregate the symbol-year in SQLite.
2. **Hit** → return. The provider is not contacted. (One exception: the
   current year is partial by definition, so a cached current-year aggregate
   older than 24h triggers a refresh attempt first.)
3. **Miss** → check whether a fetch for this symbol has ever succeeded. A
   recent success that produced no rows for this year means the data genuinely
   does not exist; return 404 without calling the provider.
4. Otherwise fetch the symbol's full monthly history, validate it, upsert it,
   and re-run the **same** aggregation query.

`MAX(high)`, `MIN(low)` and `SUM(volume)` are computed in SQL. The provider
returns history by symbol and accepts no year parameter, so one fetch serves
every year of that symbol's history.

## Design decisions

**Prices are scaled integers, not floats or text.** Stored as `price × 10⁴`
in an `INTEGER` column. `REAL` cannot represent `0.01` exactly; `TEXT` sorts
`"9.9000"` above `"10.0000"`, which would corrupt `MAX` and `MIN`. Parsing is
pure string manipulation — no value passes through `float64` anywhere. A price
with more than four decimals is a loud import error, never a silent rounding.

**Volume cannot be double-counted.** `monthly_prices` is keyed on
`(symbol, month)` and writes are upserts, so re-importing the same history
updates rows in place. Three imports of the same months give the same
`SUM(volume)` as one.

**A definitive "no such symbol" is remembered; a failure never is.** `symbol_imports` records that a
fetch succeeded, separately from the data itself. Only a *successful* fetch
advances `last_success_at`, so an outage can never create a negative cache
entry, and 404 is only ever returned after the provider actually answered.
Upstream problems are 502/503.

**The provider call happens outside the write transaction.** Holding a SQLite
write lock across a network round trip would block every other writer for the
duration of the request.

**Concurrent misses are not serialized.** Simultaneous first requests for the
same symbol may each fetch, but the unique constraint means they cannot
duplicate rows or inflate volume. Single-flight coordination would add
lifecycle complexity to save at most a few duplicate calls on a cold start;
the quota saving did not justify it. This is tested, not assumed.

**Providers are data, not configuration-at-build-time.** Endpoint, credential,
priority, timeout and enabled state are rows. What stays in code is each
upstream's *shape* — how to build the request and read the payload — selected
by a `kind` column. A row cannot describe an unknown JSON format, and keeping
that mapping as code is what makes it testable.

**A partial outage never looks like absence.** The chain reports "no data"
only when every enabled provider answered and agreed the symbol is unknown.
If any provider failed, the result is 503, because absence has not been
established and must not be cached.

**Two transports, one workflow.** The GraphQL resolver and the REST handler
both call the same `annual.Service`. Neither owns a caching rule, so they
cannot disagree; a parity test asserts identical values and a single upstream
call across both.

**Two dependencies.** `modernc.org/sqlite`, a pure-Go driver, so the project
builds anywhere without a C toolchain or `CGO_ENABLED` surprises; and
`graphql-go/graphql` for schema execution, chosen over `gqlgen` to avoid a
codegen step and thousands of generated lines for one query. Everything else —
routing, JSON, HTTP, SQL — is the standard library. No ORM: every statement is
hand-written and parameterized.

## Edge cases

| Case | Behavior |
|---|---|
| Year with fewer than 12 months (IPO, delisting) | Valid. Row count is never a completeness test. |
| Current year | Served year-to-date, refreshed if the cached import is over 24h old. |
| Valid symbol, year predates its listing | 404 after one upstream check, remembered for 24h. |
| Unknown symbol | 404, remembered for 24h. Alpha Vantage reports this with HTTP 200 and an error body. |
| Rate limit / quota exhausted | 503. Also arrives as HTTP 200, and must never be read as "no data". |
| Provider down during a warm request | Cached data is still served. |
| Symbols with punctuation (`BRK.B`, `RDS-A`) | Accepted. |
| Future year, 3- or 5-digit year | 400, with no upstream call. |

## Optional: cache warming

The API works correctly whether or not this ever runs.

```sh
DB_PATH=/absolute/path/market.db REFRESH_SYMBOLS=IBM,AAPL,MSFT go run ./cmd/refresh
```

One fetch per symbol per run — not per symbol and year. A symbol that fails is
logged and the run continues with the rest; the command exits non-zero if any
symbol failed, so cron can alert. A failed refresh leaves existing cached data
untouched and servable.

See [docs/OPERATIONS.md](docs/OPERATIONS.md) for cadence, crontab examples, and
quota budgeting.

## Documentation

| Document | Contents |
|---|---|
| [docs/SPEC.md](docs/SPEC.md) | The contract: endpoint, error mapping, schema, cache rules |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Why each design choice was made, and what was rejected |
| [docs/API.md](docs/API.md) | REST request and response reference |
| [docs/GRAPHQL.md](docs/GRAPHQL.md) | GraphQL schema, examples, and error codes |
| [docs/PROVIDERS.md](docs/PROVIDERS.md) | Provider CRUD, failover rules, security notice |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Running the refresh job, cron, quota |
| [docs/TESTING.md](docs/TESTING.md) | What is tested and why |
| [docs/TASKS.md](docs/TASKS.md) | Phase plan and status |
| [AGENTS.md](AGENTS.md) | Conventions and invariants for contributors |

## Routes

| Route | Purpose |
|---|---|
| `GET /symbols/{symbol}/annual/{year}` | The annual aggregate |
| `POST /graphql`, `GET /graphql` | The same data over GraphQL |
| `GET /healthz` | Liveness |
| `GET /admin/providers` | List providers |
| `POST /admin/providers` | Add a provider |
| `GET /admin/providers/{id}` | Read one |
| `PUT /admin/providers/{id}` | Replace one |
| `DELETE /admin/providers/{id}` | Remove one |

The `/admin` routes are omitted entirely when `PROVIDER_ADMIN_API=false`.

## Layout

```
cmd/
  api/                  HTTP server; wires every dependency explicitly
  refresh/              one-shot cache warmer for cron
internal/
  core/                 business modules (bounded contexts)
    annual/             cache-aside workflow + store + schema (Init)
    provider/           config, kind registry, failover chain + store + schema
      alphavantage/     Alpha Vantage HTTP adapter (owned by provider module)
  shared/               shared kernel and test fixtures
    market/             domain types, price parsing, outcome sentinels (no I/O)
    markettest/         fake provider, store, clock; tests only
  infra/                infrastructure adapters
    sqlite/             database open + pragmas
    httpapi/            REST routing, validation, status/JSON mapping
    graphqlapi/         GraphQL schema, resolvers, error codes
docs/                   SPEC, TASKS, API, GRAPHQL, PROVIDERS, OPERATIONS, TESTING, DECISIONS
```
