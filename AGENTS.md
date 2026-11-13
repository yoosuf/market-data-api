# AGENTS.md

Operating guide for AI agents working in this repository.
Read `SPEC.md` (contract) and `TASKS.md` (phase plan) before changing anything.

## What this project is

A Go + SQLite service with one read endpoint:
`GET /symbols/{symbol}/annual/{year}` returns the annual high, low, and traded
volume for a symbol, aggregated in SQL from monthly history cached in SQLite and
backfilled from Alpha Vantage only on a cache miss.

Standard library only for HTTP. `database/sql` with parameterized queries.
No ORM, no web framework, no router dependency, no DI container, no global
mutable state.

## Non-negotiable invariants

Violating any of these is a blocking defect, not a style note.

1. **SQLite is read first.** A cache hit must not contact the provider. The only
   exception is the current-year freshness rule in `SPEC.md` §5, and it lives in
   the service, never in the store.
2. **404 is a claim about the world.** It may be returned only after a
   *successful* provider response. An upstream failure is 502/503 and never
   writes a negative-cache marker.
3. **Aggregation happens in SQL.** `MAX(high)`, `MIN(low)`, `SUM(volume)` run in
   SQLite. Do not pull rows into Go and fold them.
4. **Volume is idempotent.** The unique constraint on `(symbol, month)` plus
   upsert is what guarantees a retry or repeated import cannot double-count.
   Any change that touches the write path must preserve this.
5. **Prices never pass through `float64`.** Decimal string -> scaled `int64`
   (10^-4) by string parsing. More than four fractional digits is a loud import
   error naming symbol and month, never a silent rounding.
6. **No secrets or internals in output.** Responses and logs carry fixed strings
   only. Never `err.Error()` on an HTTP error — a `*url.Error` contains the full
   URL including the API key.
7. **No write transaction is held across an HTTP call** to the provider.
8. **A partial outage is never absence.** The provider chain returns
   `market.ErrNoData` only when every enabled provider answered and agreed the
   symbol is unknown. Any failure makes the result unavailability, because
   `ErrNoData` is what authorizes a cached 404.
9. **`base_url` is validated as an absolute http/https URL** before storage.
   The admin API is unauthenticated by configuration choice, so this is the
   only thing standing between a stored row and an arbitrary fetch target.

## Boundaries

| Package | Owns | Must not contain |
|---|---|---|
| `internal/shared/market` | domain types, price parsing, outcome sentinels | I/O, third-party imports |
| `internal/shared/markettest` | shared test fixtures: fake provider, store, clock | production use; it is imported only by tests |
| `internal/infra/sqlite` | opening the database and its pragmas | schema or business rules |
| `internal/core/annual` | cache-aside sequence + its store, schema (`Init`), aggregation | provider JSON keys, HTTP status codes |
| `internal/core/provider` | stored config, kind registry, failover and its classification + store/schema | HTTP status codes, caching decisions |
| `internal/core/provider/alphavantage` | Alpha Vantage HTTP, payload validation, mapping to domain | SQL, caching decisions |
| `internal/infra/httpapi` | REST route parsing, validation, status and JSON mapping | business rules, SQL, any provider package import |
| `internal/infra/graphqlapi` | GraphQL schema, resolvers, `extensions.code` mapping | business rules, SQL, any provider package import |
| `cmd/*` | config from env, wiring concrete dependencies | logic of any kind |

Interfaces are declared by the **consumer** (`internal/core/annual`), not exported by
the adapters. `cmd/refresh` gets a single-method importer interface and must not
be able to reach the annual aggregation, which can return without fetching.

Outcomes cross boundaries as `market.ErrNoData`,
`market.ErrUpstreamUnavailable` and `market.ErrUpstreamPayload`. No package
outside `internal/core/provider/alphavantage` imports the adapter to classify an error.

A provider payload change should be confined to `internal/core/provider/alphavantage`. If a
change forces edits in `internal/core/annual`, the internal contract was wrong —
say so rather than threading provider details through the workflow.

Both transports are adapters over the same `annual.Service`. A caching,
validation or freshness rule added to one of them is in the wrong place: it
belongs in the workflow, where both inherit it. The parity test in
`internal/infra/graphqlapi` exists to catch that drift.

## Working agreement

- **Work one phase at a time**, in `TASKS.md` order. Do not start the next phase
  before the current gate passes.
- **By default the human writes the Go, SQL, tests, cron config, and README**,
  and makes every commit. Agents plan, review, and verify. Flip this per task if
  the human explicitly asks for code.
- **Never commit, push, open a PR, or send mail.** The human does that.
- **Do not add dependencies** beyond the SQLite driver without asking. Each new
  module must name the concrete problem it solves.
- **Report test results honestly.** Never claim a check passed without pasting
  the command output. A skipped step is reported as skipped.
- When reviewing, give the affected file, the concrete consequence, and how to
  verify it. Blocking correctness issues before optional design improvements.

## Design review criteria

Apply as review questions, not as a checklist to satisfy with extra layers.

- **SRP** — one clear responsibility per component; see the boundary table.
- **OCP** — a replacement provider implements the existing interface without
  edits to `internal/core/annual`. Do not build a plugin system for a hypothetical
  second provider.
- **LSP** — fakes honor the same success / no-data / failure semantics as the
  real adapters. A fake that returns an empty successful result where the real
  one errors is a broken test, and it will hide bug class #2 above.
- **ISP** — small interfaces at the point of use, only where they are a real
  test or replacement seam.
- **DIP** — workflows depend on capabilities; concrete adapters are selected at
  startup in `cmd/*`.

Prefer concrete types where no seam is needed. Domain-specific operations, not
generic CRUD repositories. No CQRS, event buses, speculative factories, or extra
layers. If you propose a pattern, name the current problem it solves and why
simpler code is insufficient.

## Testing

- SQL behavior: a temporary SQLite database per test. Cover year boundaries,
  symbol isolation, prices that mis-sort as text, re-import leaving volume
  unchanged, and rollback on a failed import.
- Provider behavior: `httptest` fixtures. Cover non-2xx, timeout, malformed
  JSON, and the HTTP-200 error shapes (`Error Message`, `Note`, `Information`).
  A 200 is not evidence of history.
- Workflow behavior: the shared fake in `internal/shared/markettest`, asserting
  **provider call counts** so cache hits and misses are verified as behavior
  rather than implementation. Add fixtures there rather than re-declaring a
  fake per package; one definition is what keeps the fakes honest.
- Never consume live API quota in an automated test. Live calls are manual
  smoke tests only.

## Commands

```sh
gofmt -l .        # must print nothing
go vet ./...
go build ./...
go test ./...
go run ./cmd/api
```

Environment: `DB_PATH` (default `./market.db`), `ADDR` (default `:8080`),
`PROVIDER_ADMIN_API` (default true). No upstream endpoint or credential comes
from the environment: those are columns in the `providers` table, managed over
`/admin/providers`.

Use an **absolute** `DB_PATH` when both the API and the refresh command run:
a relative path resolves against each process's working directory, so they can
silently use two different databases.

## Committing (step by step)

The human owns the git history. Agents may prepare and stage changes, but never
run `git commit`, `git push`, `git tag`, or open a PR. When a change spans
multiple concerns, split it into small, reviewable commits rather than one
large dump — reviewers (and recruiters reading the history) should be able to
follow the evolution phase by phase.

Recommended commit order for this repository (each commit must leave
`gofmt`, `go vet`, `go build`, `go test` green):

1. `chore: initialize Go project` — `go.mod`, `.gitignore`, `.env.example`, `cmd/api`.
2. `feat: market domain and scaled-integer price parsing` — `internal/shared/market`.
3. `feat: SQLite persistence and aggregation` — `internal/core/annual/store.go`, `internal/infra/sqlite`.
4. `feat: Alpha Vantage adapter` — `internal/core/provider/alphavantage`.
5. `feat: provider config, registry, and failover chain` — `internal/core/provider`.
6. `feat: cache-aside annual service` — `internal/core/annual/service.go`, `importer.go`.
7. `feat: REST endpoint` — `internal/infra/httpapi`.
8. `feat: GraphQL transport` — `internal/infra/graphqlapi`.
9. `feat: optional scheduled refresh` — `cmd/refresh`.
10. `test: shared fixtures` — `internal/shared/markettest`.
11. `docs: architecture, setup, and operations` — `README.md`, `AGENTS.md`, `docs/*`, `LICENSE`.

Rules of thumb:

- Stage with `git add -p` (or explicit paths) so a commit never accidentally
  includes `*.db`, `.env`, or a stray binary. Check `git status --porcelain`
  and `git diff --cached` before committing.
- Each commit should compile and pass tests on its own; do not commit a
  half-wired change that only works after the next commit.
- Never `git add -A` / `git add .` blindly — that is how a `market.db`
  containing a live API key ends up in history.
- Prefer many small commits over one "WIP" commit; squash only at the human's
  request.

## Never commit

`*.db`, `*.db-wal`, `*.db-shm`, `.env`, `.env.*`, compiled binaries, `.DS_Store`.
A key committed once is in history forever.
