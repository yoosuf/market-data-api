# TASKS — phase plan

Go + SQLite annual market data API. One phase at a time; each phase ends with a
review, then a commit. See SPEC.md for the contract these tasks implement.

---

## Phase 0 — Settle the contract — DONE (no commit)

Decided: scaled-integer prices, separate `symbol_imports` table, 24h negative
cache, current year served year-to-date, `modernc.org/sqlite`.

Response format assumed: flat envelope, prices at four decimals, volume as an
integer string. Storage is exact, so a different wire format is a change to
`internal/infra/httpapi` alone.

---

## Phase 1 — Runnable Go baseline — DONE

Goal: builds, starts, health endpoint answers, nothing secret can reach git.

- [x] `go mod init github.com/<you>/market-data-api`
- [x] Declare the lowest `go` directive the dependency graph allows (1.26;
      `modernc.org/libc` sets the floor). `ServeMux` method+wildcard routing
      needs only 1.22.
- [x] `cmd/api` only — no `internal/` yet, empty packages are noise
- [x] Config struct read from env once, passed down as arguments:
      `ADDR` (`:8080`), `DB_PATH` (`./market.db`),
      `ALPHAVANTAGE_API_KEY` (**fail fast at startup if empty**)
      — *superseded in Phase 9: upstream credentials moved into the
      `providers` table and this variable was removed.*
- [x] `http.Server` value with `ReadHeaderTimeout`, `ReadTimeout`,
      `WriteTimeout`, `IdleTimeout` — not `http.ListenAndServe`
- [x] `GET /healthz` only
- [x] Graceful shutdown: `signal.NotifyContext` + `srv.Shutdown`;
      `http.ErrServerClosed` is **not** a fatal error
- [x] `.gitignore`: `*.db`, `*.db-wal`, `*.db-shm`, `.env`, `.env.*`,
      binaries, `.DS_Store`

Edge cases: a `.gitignore` written after `git add` untracks nothing; a key
committed once is in history forever; a relative `DB_PATH` resolves against each
process's working directory, so the API and cron job can silently use two
different databases.

Verify:

```sh
gofmt -l .            # no output
go vet ./...          # no output
go build ./...        # no output
go run ./cmd/api &   # Phase 1 form; the key argument is gone as of Phase 9
curl -i localhost:8080/healthz    # 200
curl -i localhost:8080/nope       # 404
go run ./cmd/api                  # exits non-zero, message about missing key
touch market.db && git check-ignore -v market.db .env   # both matched
git status --porcelain            # no .db/.env/.DS_Store entries
```

Commit: `chore: initialize Go API project`

---

## Phase 2 — SQLite persistence and aggregation — DONE

- [x] `internal/shared/market` types; decimal-string -> scaled int parser + unit tests
- [x] Idempotent schema init
- [x] Transactional upsert of a month slice; one transaction per import
- [x] Aggregation returns `(summary, found, error)` — `MAX/MIN/SUM` over zero
      rows yield NULL, which maps to `found=false`, **not** zero
- [x] Tests on temp SQLite: year isolation, symbol isolation, year boundaries,
      prices that mis-sort as text, re-import leaves volume unchanged, failed
      import rolls back

Commit: `feat: persist and aggregate monthly market data`

---

## Phase 3 — Alpha Vantage adapter — DONE

- [x] Client with injected `*http.Client`, base URL, key; context propagated
- [x] Map provider JSON -> `[]market.MonthlyRecord`
- [x] Failure modes: non-2xx, timeout, malformed JSON, and the three HTTP-200
      error shapes (`Error Message`, `Note`, `Information`) — a 200 is not
      evidence of history
- [x] `httptest` fixtures; live calls manual only, no quota burned in CI

Commit: `feat: add Alpha Vantage monthly data adapter`

---

## Phase 4 — Cache-aside service — DONE

- [x] Read -> on miss fetch -> import -> re-run the *same* aggregation query
- [x] No write transaction held across the HTTP call
- [x] Decide on concurrent-miss coordination; duplicate upstream calls and
      duplicate rows are separate problems, and the unique constraint already
      solves the second
- [x] Tests with fakes: cold fetches, warm does not, second year reuses the
      import, warm request survives a provider outage

Commit: `feat: add cache-aside annual market data service`

---

## Phase 5 — REST endpoint — DONE

- [x] Route, validation, normalization, JSON encoding, error mapping per SPEC §3
- [x] End-to-end tests: temp SQLite + fake provider, asserting provider
      **call counts**
- [x] Assert `high`, `low`, `volume` are JSON strings
- [x] No API keys, SQL errors, or raw upstream errors in responses or logs

Commit: `feat: expose annual market data endpoint`

---

## Phase 6 — Optional scheduled refresh — DONE

- [x] One-shot command, configurable symbol list (IBM, AAPL, MSFT), one fetch
      per symbol per run — not per symbol and year
- [x] Same provider + upsert path; same DB file; absolute `DB_PATH`
- [x] Must not call the annual cache-hit workflow
- [x] Define failure reporting, continue-on-error policy, overlap handling,
      quota budget (free tier is ~25 requests/day)

Commit: `feat: add optional scheduled cache refresh`

---

## Phase 7 — Verify and prepare submission — DONE

- [x] `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test ./...`
- [x] Clean-checkout smoke test: SQLite initializes, API starts without cron
- [x] README: build/run/test, DB path, dependency rationale, documented edge
      cases, refresh instructions. *The "API key env var" item of the original
      plan no longer applies: Phase 9 moved credentials into the database.*
- [x] Check every acceptance criterion: route shape, string-typed JSON fields,
      SQLite-first reads, SQL aggregation, parameterized queries, no ORM,
      idempotent volume, optional cron
- [x] Confirm the commit history shows incremental progress

Commit: `docs: explain setup, verification, and cache refresh`

---

## Phase 8 — GraphQL transport (extension) — DONE

Beyond the original REST contract; added on request.

- [x] `internal/infra/graphqlapi`: schema, resolvers, sanitized error type
- [x] `annual(symbol, year)` resolving through the same `annual.Service`
- [x] `String` scalars for `high`, `low`, `volume`; nullable `annual`
- [x] `extensions.code` mirroring the REST status mapping
- [x] `POST` and `GET` support, 64 KiB body cap, introspection enabled
- [x] Mounted at `/graphql` alongside REST in `cmd/api`
- [x] `market.ValidateYear` extracted so both transports share one range rule
- [x] Tests: outcomes, leak check, transport errors, introspection, parity

Commit: `feat: expose annual market data over GraphQL`

---

## Phase 9 — Database-backed providers (extension) — DONE

- [x] `providers` table: name, kind, base_url, api_key, priority, enabled,
      timeout_ms, timestamps; indexed on (enabled, priority, id)
- [x] `internal/core/provider`: Config, Registry of kinds, Validate, failover Chain
- [x] Chain classification: ErrNoData only when every provider agrees
- [x] Adapter cache keyed on updated_at, so edits apply without a restart
- [x] CRUD at `/admin/providers`, opt-in via `httpapi.WithProviderAdmin`
- [x] No upstream credential in the environment; the database is the only
      source of truth
- [x] Both `cmd/api` and `cmd/refresh` read the same provider table
- [x] Tests: failover, classification, CRUD, validation, ordering, seeding

Known risk, accepted by the operator: the admin API is unauthenticated and
returns keys in full. See DECISIONS.md §18.

Commit: `feat: configure providers in the database with CRUD endpoints`

---

## Phase 10 — Cleanup — DONE

- [x] Removed the `annual.ErrNoData` alias; one name per concept
- [x] Unexported `httpapi.Handler`, which never left its package
- [x] Folded an orphaned comment into the alphavantage package doc and fixed
      a comment that the provider work had made false
- [x] Deduplicated the Alpha Vantage endpoint constant
- [x] Aligned the refresh job with the serving path on a definitive
      "no such symbol", with a test asserting they agree
- [x] `PROVIDER_ADMIN_API` parsed with `strconv.ParseBool`; a typo is now a
      startup failure rather than a silently mounted admin surface
- [x] `http.MaxBytesReader` given the ResponseWriter at both call sites
- [x] Extracted `internal/shared/markettest`; three copies of the fake provider
      became one
- [x] Refreshed OPERATIONS.md and AGENTS.md, which still documented the
      removed environment configuration

Commit: `refactor: consolidate test fixtures and tidy provider changes`

---

## Phase 11 — Remove env credentials, audit docs — DONE

- [x] Deleted `provider.SeedFromEnv`, its `SeedStore` interface and its test;
      no upstream credential is read from the environment
- [x] Deleted `sqlitestore.CountProviders`, which existed only for seeding
- [x] Unexported `alphavantage.defaultBaseURL`, which stopped being used
      outside its package when the seeder went
- [x] Documented `market.ImportState.LastStatus` as diagnostic-only, so a
      write-mostly field does not read as an oversight
- [x] Enumerated all 83 functions and methods: none unreferenced
- [x] Audited every doc claim against the code. Fixed: `api_key` documented
      as required when validation treats it as optional; DECISIONS §9 titled
      "One dependency" after GraphQL made it two; README and SPEC layout
      blocks missing `internal/shared/markettest`; Phase 1 and Phase 7 checklists
      naming the removed environment variable, now marked superseded
- [x] Added the route table to README and cross-linked the admin surface from
      API.md

Commit: `refactor: drop env credentials and reconcile documentation`

---

## Verification record

Run on a clean copy of the tree (no `.git`, no database files):

```
gofmt -l .        clean
go vet ./...      clean
go build ./...    clean
go test ./...     ok (5 packages)
go test -race ./...  ok (5 packages)
```

Smoke tests performed:

- Startup with no providers configured logs a warning and still serves
  `/healthz`; cache misses answer 503 until one is added.
- SQLite and its WAL sidecars are created on first start; no migration step.
- `GET /healthz` → 200. `GET /symbols/IBM/annual/202` → 400. `POST` → 405.
  Unknown route → 404.
- `SIGTERM` logs "shutting down" and exits 0.
- `.gitignore` matches `market.db`, `market.db-wal`, `.env`, `.DS_Store`.
- Live end-to-end against Alpha Vantage (one call, IBM): 2023 returned
  `high 166.3400 / low 120.5500 / volume 1112911966`; 324 months persisted
  spanning 1999-12 to 2026-11.
- With that cache populated and the provider made unreachable: 2015 served
  from the same single import, 1950 → 404, an uncached symbol → 503.

GraphQL smoke tests (same running server, live provider):

- `POST /graphql` returned the same values as REST for IBM 2023.
- `GET /graphql` served year 2015 from the same single import.
- Year 1950 returned `"annual": null` with `extensions.code = NOT_FOUND`.
- Introspection of `AnnualSummary` listed all six fields.

Provider smoke tests (live, same running server):

- Cold request with no providers configured → 503, not 404, with a startup
  warning naming the endpoint to POST to.
- `POST` created a dead primary (priority 10) and a working secondary (20).
- Failover served IBM 2023 from the secondary after the primary refused.
- `PUT` rotated the secondary's key; the next request used it without a
  restart.
- `PUT` disabled and `DELETE` removed the primary; 204 and an updated listing.
- `POST` with `kind: bloomberg` → 400 naming the kinds this build supports.
- GraphQL served year 2015 through the same chain and the same cache.

Outstanding: compare the response envelope against a reference payload if one
is supplied.
