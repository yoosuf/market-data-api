# Testing

```sh
go test ./...        # full suite, no network access required
go test -race ./...  # the concurrency test is only meaningful under -race
go vet ./...
gofmt -l .           # prints nothing when clean
```

No test contacts Alpha Vantage. Provider behavior is driven by local
`httptest` servers, so the suite consumes no API quota, runs offline, and is
deterministic.

## What each package proves

### `internal/shared/market` — exact arithmetic

- Four-decimal strings parse to the expected scaled integers, and render back
  identically.
- `0.1 + 0.2 == 0.3` holds, which is the float failure the scaled-integer
  choice exists to avoid.
- Five significant decimals produce `ErrPrecision` rather than a rounded value.
- Malformed input (`1,000.00`, `1e5`, `1.2.3`, `.`) is rejected.
- Symbol normalization accepts `BRK.B` and `RDS-A`, rejects over-long and
  punctuated-wrong input.
- Year validation rejects 3- and 5-digit years, non-numerics, future years.

### `internal/core/annual` + `internal/core/provider` — SQL behavior

Each test opens a real database file in `t.TempDir()` rather than `:memory:`,
so WAL and locking behave as they do in production.

- Aggregation returns `MAX`/`MIN`/`SUM` across months, using `"9.9000"` and
  `"10.0000"` — values that sort incorrectly as text.
- Year boundaries: December of the prior year and January of the next are
  present in the table and must not leak into the requested year.
- Symbol isolation: another symbol's rows must not affect the aggregate.
- **Idempotency**: importing the same months three times leaves `SUM(volume)`
  unchanged, and a revised month replaces rather than accumulates.
- Zero rows yield `found == false`, not a zero-valued summary, because
  `MAX`/`MIN`/`SUM` return `NULL` over an empty set.
- A one-month year is valid data.
- **Rollback**: a batch that fails partway leaves the previously cached data
  byte-for-byte unchanged.
- Import state: a failure never sets or erases `last_success_at`.
- A symbol containing `'; DROP TABLE monthly_prices; --` is treated as data,
  and the table survives.

### `internal/core/provider/alphavantage` — provider behavior

- A valid payload maps to domain records, including history from other years,
  and the request carries `function`, `symbol`, and `apikey`.
- `Error Message` → `ErrUnknownSymbol` (absence).
- `Note` and `Information` → `ErrUnavailable` (quota/rate limit). Both arrive
  with **HTTP 200**, and the test asserts neither is ever read as absence.
- Non-2xx statuses → `ErrUnavailable`.
- Malformed payloads → `ErrBadPayload`: not JSON, missing series key, bad date
  key, unparseable price, excess precision, `low > high`, fractional volume,
  entry that is not an object.
- An empty-but-valid series is a success with zero records.
- A context deadline → `ErrUnavailable`.
- A transport error against a dead port does not contain the API key.

### `internal/core/annual` — cache-aside workflow

Behavior is asserted through **provider call counts**, so these are tests of
caching behavior rather than of implementation details.

- Cold request fetches once; the warm request adds no calls.
- A second year is served from the first fetch — zero additional calls.
- A no-data year is remembered: three requests, one upstream call.
- After the TTL expires, it refetches.
- A provider failure is **not** cached: each request retries, and the error is
  never reported as no-data.
- A warm request still succeeds while the provider is down.
- The current year refreshes once stale, and a completed year never refetches.
- A failed current-year refresh still serves the cached aggregate.
- An unknown symbol costs one upstream call across three requests, and is
  re-checked once the window closes, so a newly listed symbol self-heals.
- A warm hit on a completed year performs no import-state query.
- Eight concurrent cold requests under `-race` all see the correct aggregate:
  duplicate fetches cannot duplicate rows.
- The importer always fetches, and running it twice does not inflate volume.
- The refresh job and the serving path agree about a definitive "no such
  symbol": whichever discovers it, the other reuses the determination instead
  of spending a second upstream call.

### `internal/infra/httpapi` — the endpoint end to end

Wired against a real store on a temporary database plus a fake provider.

- Cold then warm: identical bodies, one provider call total.
- `high`, `low`, `volume` decode as JSON **strings**, checked by unmarshalling
  into `map[string]any` and asserting the dynamic type.
- Lowercase symbols hit the same cache entry — no second fetch.
- `BRK.B` and `RDS-A` are accepted.
- Invalid input returns 400 and makes **zero** provider calls.
- `POST`/`PUT`/`DELETE` → 405.
- No data → 404; unknown symbol → 404; outage → 503; bad payload → 502.
- An error containing SQL text, a table name, and an API key returns 500 with
  none of those strings in the body.

### `internal/infra/graphqlapi` — the GraphQL transport

Wired against a real store on a temporary database plus a fake provider, the
same way the REST tests are.

- A query returns the aggregate, and `high`, `low`, `volume` decode as JSON
  **strings**, checked by asserting the dynamic type.
- A warm query makes no provider call, including when the symbol arrives in a
  different case.
- `extensions.code` is asserted for all five outcomes: `BAD_USER_INPUT`,
  `NOT_FOUND`, `UPSTREAM_UNAVAILABLE`, `UPSTREAM_ERROR`, `INTERNAL_ERROR` —
  and each returns `"annual": null` rather than failing the query.
- Invalid input makes **zero** provider calls.
- An error carrying SQL text, a table name and an API key leaks none of them.
- `GET` with query parameters works, not just `POST`.
- Malformed requests (bad JSON, missing `query`, empty `GET`, `DELETE`) return
  **400**; an invalid query *document* returns 200 with an errors array.
- Introspection resolves.

### `internal/core/provider` — selection and failover

- The first provider to answer wins, and later providers are not called.
- A failing provider falls through to the next.
- A single provider's "no such symbol" falls through too — another may know it.
- All providers agreeing on no-data yields `ErrNoData`.
- **One no-data plus one outage yields unavailability**, not absence: this is
  the test that protects the negative cache from an unproven 404.
- No providers configured, and a row naming an unknown `kind`, are both
  unavailability rather than absence.
- A cancelled context stops the chain instead of trying the rest.
- Changing `updated_at` rebuilds the cached adapter, so an edit through the
  CRUD API takes effect without a restart.
- Validation accepts and normalizes good configs, and rejects missing names,
  unknown kinds, relative URLs, `file://` schemes, negative priorities and
  hour-long timeouts.

### Provider CRUD (`internal/core/provider`, `internal/infra/httpapi`)

- Create, read, update and delete round-trip every field; `created_at` is
  preserved across updates while `updated_at` advances.
- Enabled providers come back ordered by priority, with disabled rows excluded
  from the fetch list but present in the admin listing.
- Duplicate names are a 409; unknown ids are 404 on get, update and delete.
- Validation failures are 400, including an unknown `kind` with the list of
  kinds this build supports.
- Unknown JSON fields are rejected, so `timout_ms` fails instead of silently
  leaving the default.
- Minimal create bodies get the documented defaults.
- Seeding from the environment happens only on an empty table, and never
  twice.
- The admin routes are absent unless mounted.
- One test documents the configured behavior that `api_key` is returned in
  full.

### Parity

`TestRESTAndGraphQLAgree` drives both adapters against one service and one
database: the REST response and the GraphQL response must carry identical
values, and the two requests together must cause exactly **one** upstream
call. A rule that creeps into one adapter instead of the workflow fails here.

## On fakes

`internal/shared/markettest` holds the shared fixtures: the fake provider, the price
helper, the history fixture, a store on a temporary database, and a fixed
clock.

The fake provider lives in one place rather than being rewritten per test
package so that a single definition carries the adapter contract: it either
returns history, or returns a classified error. It never reports an empty
success in place of a failure. A fake that swallowed errors would make the
404-versus-5xx split untestable — exactly the bug those tests exist to catch —
and three near-identical copies could drift into doing so one at a time.

Cache behavior is asserted through `Calls()`, so the tests describe observable
behavior rather than internal state.
