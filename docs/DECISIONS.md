# Design decisions

Each entry states the problem, the choice, and what was rejected. These are the
questions a reviewer is most likely to ask.

## 1. Prices are integers scaled by 10⁴

**Problem.** The response must preserve exact price precision.

**Choice.** Store `price × 10000` in an `INTEGER` column. Parse the provider's
decimal string by string manipulation; never construct a `float64`.

**Rejected — `REAL`.** Binary floating point cannot represent `0.01` exactly.
Accumulated error in a `MAX`/`MIN` is small but the stored value is already
wrong on arrival.

**Rejected — `TEXT`.** Exact, but `MAX` compares lexically: `"9.9000"` sorts
above `"10.0000"`, so a year's high would be wrong whenever the price crosses
a digit-count boundary. Casting to `REAL` inside the query reintroduces the
float. This is covered by a test using exactly those two values.

**Range.** `int64` at this scale overflows past roughly 9×10¹⁴ currency units.

**Unexpected precision.** A price with more than four significant decimals
fails the import, naming the symbol and month. Silently rounding would corrupt
the cache permanently, and a cached wrong price is unrecoverable in a way a
loud failure is not. Trailing zeros beyond the scale (`1.23000`) carry no
value and are accepted.

## 2. `symbol_imports` exists separately from the data

**Problem.** Monthly rows can express "we have data for 2023". They cannot
express "we asked, and this symbol has no 2023". Conflating the two means a
valid symbol whose requested year predates its IPO refetches full history on
every request, forever — on a free tier of roughly 25 calls a day.

**Choice.** Record `last_success_at` per symbol, advanced only by a successful,
validated fetch. A cache miss with a recent success is a *negative hit*: 404,
no upstream call, remembered for 24h.

**Rejected — a row count threshold.** "Fewer than 12 months means incomplete"
breaks IPO years, delisting years, and the current year. Row count is never
used as a completeness test anywhere in this codebase.

**Unknown symbols are remembered too.** When the provider answers that it has
no such symbol, that is a determination, not a failure: it is recorded as a
successful import of zero months, so a bogus symbol costs one upstream call per
24h rather than one per request. A regression test asserts three requests
produce one call. The refresh command records it identically, so the same fact
produces the same cache state whichever path discovered it.

**The invariant.** `RecordImportFailure` writes `last_status` but never touches
`last_success_at`, so a failure cannot create or extend a negative entry. An
outage is not evidence that data does not exist.

## 3. 404 is a claim about the world

**Problem.** Four conditions must be distinguishable: bad input, no data,
upstream failure, database failure.

**Choice.** 400 / 404 / 502–503 / 500 respectively. 404 is returned only after
a successful provider response, or a remembered one.

**Why it matters.** Alpha Vantage returns rate-limit and quota messages with
**HTTP 200**. Treating a 200 as success would turn an exhausted quota into
"this symbol has no data" — a wrong answer the client has no way to detect,
which would then be cached. The adapter classifies `Error Message` as absence
and `Note`/`Information` as unavailability, and both paths are tested.

## 4. Volume idempotency comes from the schema

**Problem.** A retry or repeated import must not double-count volume.

**Choice.** `PRIMARY KEY (symbol, month)` plus `INSERT … ON CONFLICT DO
UPDATE`. Re-importing the same months updates rows in place. Correctness is a
property of the schema, not of caller discipline.

**Rejected — delete-then-insert per symbol.** A wider lock, and a failure
between the two statements loses cached data.

## 5. The provider call sits outside the write transaction

**Problem.** SQLite serializes writers. A write transaction held open across a
network round trip blocks every other writer for the length of that request.

**Choice.** Fetch, then open a transaction to persist. The workflow is read →
fetch → import → re-read, and the re-read runs the *same* aggregation query as
the first read, so one piece of SQL defines the answer and the fetched slice is
never aggregated in Go.

## 6. Concurrent misses are not coordinated

**Problem.** Several simultaneous first requests for one symbol can each fetch.

**Choice.** Allow it. Duplicate *upstream calls* and duplicate *rows* are
separate problems, and the unique constraint already solves the second: a test
fires eight concurrent cold requests under `-race` and asserts the aggregate is
exactly right.

**Rejected — single-flight.** It would save at most a handful of calls, once,
on a cold start, in exchange for a shared map, lifecycle management, and a new
failure mode where one caller's cancellation affects another's result. If quota
pressure made this matter, the refresh command is the cheaper lever.

## 7. The current year is refreshed, completed years are not

**Problem.** The current year is partial by definition; a completed year's
history cannot change.

**Choice.** Serve the current year year-to-date, refreshing when the cached
import is over 24h old. Completed years never expire.

**Failure behavior.** If that refresh fails, the stale aggregate is served
anyway with a warning logged. An outage must not take away data already held.

## 8. The refresh command cannot read the cache path

**Problem.** The refresh job exists to fetch *fresh* history. If it could call
the annual workflow, it would be satisfied by a cache hit and refresh nothing.

**Choice.** `annual.Importer` takes `ImportStore`, a two-method write
interface. The read path is not reachable from `cmd/refresh` — enforced by the
type, not by a comment.

## 9. The SQLite driver

**Choice.** `modernc.org/sqlite`, pure Go. It was the only dependency until
GraphQL was added; see §12 for that one.

**Rejected — `mattn/go-sqlite3`.** Faster and more mature, but requires CGO and
a C toolchain, breaks cross-compilation, and silently produces a binary with no
registered driver under `CGO_ENABLED=0`. For a service reviewed by cloning and
running `go test ./...`, building everywhere matters more than write
throughput at this scale.

Routing uses `net/http` patterns (`GET /symbols/{symbol}/annual/{year}`), which
have been standard library since Go 1.22. One route does not justify a router
dependency. No ORM: every statement is hand-written and parameterized.

## 10. Outcome sentinels live in the domain package

**Problem.** The workflow and the HTTP layer both need to tell absence from
unavailability. If those sentinels belong to the Alpha Vantage adapter, then
`internal/infra/httpapi` has to import a specific provider in order to pick a status
code, and swapping providers means editing the layers above.

**Choice.** `market.ErrNoData`, `market.ErrUpstreamUnavailable` and
`market.ErrUpstreamPayload` are declared in the domain package. The adapter
*reports* in those terms; the workflow and handler *classify* in those terms.
A replacement provider implements the same semantics and nothing above it
changes.

## 11. Errors are classified, never wrapped, at the provider boundary

**Problem.** The request URL contains the API key. `http.Client.Do` returns a
`*url.Error` carrying that full URL, so wrapping it would leak the key into
logs and, if ever rendered, into responses.

**Choice.** The adapter returns sentinel errors with fixed messages and never
wraps the transport error. A test points a client with a known key at a dead
port and asserts the key does not appear in the error text.

## 12. GraphQL is a second transport, not a second implementation

**Problem.** Exposing GraphQL alongside REST invites two copies of the same
rules — validation here, a cache check there — which drift silently until the
two endpoints disagree about the same symbol and year.

**Choice.** `internal/infra/graphqlapi` is an adapter at the same level as
`internal/infra/httpapi`. Its resolver calls the identical `annual.Service`. Neither
adapter holds a caching rule, a freshness rule, or knowledge of the provider.
A parity test drives both with the same request and asserts identical values
and **one** upstream call in total.

**Rejected — a GraphQL-specific service or loader.** There is one field and no
nesting, so there is no N+1 problem to solve and nothing for a dataloader to
batch. Adding one would be speculative.

**Library — `graphql-go/graphql`.** Schema built programmatically in Go, no
codegen step, no generated files in the repository. `gqlgen` is the better
choice for a large schema-first API with many types, but for a single query it
would add a build step and thousands of generated lines. The whole schema here
is about ninety readable lines.

## 13. GraphQL scalars: `String`, not `Float` or `Int`

**Problem.** GraphQL's `Float` is an IEEE-754 double and its `Int` is
specified as 32-bit signed.

**Choice.** `high`, `low` and `volume` are `String!`.

`Float` would reintroduce exactly the precision loss that scaled-integer
storage exists to prevent — the storage decision would be undone at the last
hop. `Int` would be worse for volume: a liquid symbol trades well over two
billion shares a year, overflowing the specified range. `months` stays an
`Int` because it is a small count, not a quantity to compute with.

## 14. GraphQL outcomes travel in `extensions.code`

**Problem.** GraphQL answers with HTTP 200 whether or not the field resolved,
so the four-way split between bad input, absence, upstream failure and
database failure has nowhere to go in the status line.

**Choice.** Every error carries `extensions.code`:
`BAD_USER_INPUT`, `NOT_FOUND`, `UPSTREAM_UNAVAILABLE`, `UPSTREAM_ERROR`,
`INTERNAL_ERROR` — a one-to-one mirror of 400/404/503/502/500.

**The invariant holds across both transports.** `NOT_FOUND` is returned only
after the provider actually answered; an outage is `UPSTREAM_UNAVAILABLE`.
Collapsing them would tell a client the data does not exist when the service
merely could not look, and the GraphQL tests assert the split explicitly.

**`annual` is nullable.** Absence is a legitimate result, so the field returns
`null` with an error entry rather than failing the whole query. Making it
non-null would propagate the error up and discard sibling fields in a future
multi-field query.

**Messages are fixed strings.** `graphql-go` renders a resolver error's text
into the response, so resolvers return a sanitized error type carrying only a
safe message and a code. A test drives a leaky error containing SQL, a table
name and an API key through the resolver and asserts none of it appears.

## 15. Transport-level failures still use HTTP status

**Choice.** A request that never became a valid GraphQL request — malformed
JSON, missing `query`, a verb other than GET or POST — returns **400**.
Anything that reached execution returns **200** with an `errors` array.

The distinction is "did the server get a GraphQL request at all", which is a
transport question, versus "did the query succeed", which is a GraphQL
question. Clients that only inspect the status line still notice a broken
request, and GraphQL clients get the envelope they expect.

## 16. Provider configuration lives in the database

**Problem.** One provider hard-wired through environment variables means a
new upstream, a rotated key or a changed endpoint all require a redeploy, and
only one provider can ever exist.

**Choice.** A `providers` table holding endpoint, credential, priority,
timeout and enabled state, managed through CRUD endpoints. `updated_at`
advances on every write, and the running chain uses it to rebuild the adapter
on the next request — a rotated key takes effect without a restart.

**What stays in code.** The `kind` column selects an adapter. A row supplies
configuration; the adapter supplies behavior — how to build the request, and
how to map the payload to domain records. A stored row cannot describe an
unknown JSON shape.

**Rejected — a generic field-mapping config** (JSONPath expressions in the
database describing where to find `high`, `low` and `volume`). It sounds like
it removes the need for adapters, but it moves parsing into untested
configuration, cannot express per-provider quirks such as Alpha Vantage
returning rate-limit messages with HTTP 200, and fails at runtime rather than
at compile time. Adding a provider is one small package and one registry line.

**One source of truth.** No upstream endpoint or credential is read from the
environment at all. An env-seeded first row was implemented and then removed:
it meant the same credential could be specified in two places, with a
precedence rule to explain, and it let a provider deleted through the API
reappear on the next restart. A new database starts with no providers and says
so in the startup log; adding one is a single `POST`.

## 17. A partial outage must never look like absence

**Problem.** With several providers, "nobody gave me data" has two very
different causes: every provider genuinely does not know the symbol, or some
of them could not answer.

**Choice.** The chain returns `ErrNoData` only when **every** enabled provider
answered and agreed the symbol is unknown. If any provider failed, timed out,
was rate-limited, or named a `kind` this build does not know, the result is
`ErrUpstreamUnavailable`.

**Why it matters.** `ErrNoData` is what lets the workflow write a negative
cache entry and answer 404 for the next 24 hours. Returning it after a partial
outage would cache an unproven absence, and a symbol that exists would read as
nonexistent long after the outage ended. A test drives one no-data provider
plus one failing provider and asserts the result is unavailability.

A no-data answer from a single provider does *not* stop the chain: another
provider may cover a market the first does not.

## 18. The admin API is unauthenticated and returns keys in full

**Choice, made explicitly by the operator.** The CRUD endpoints require no
credential, and `GET` returns `api_key` in cleartext. Keys are stored
unencrypted in SQLite.

**The exposure, stated plainly.** Anyone who can reach the port can read every
provider credential, and can repoint the service at a host they control, which
causes it to send those credentials there. Anyone holding a copy of
`market.db` — including a backup — has the keys.

**Rejected alternatives, and what they would have cost.** A bearer token from
the environment with constant-time comparison is roughly thirty lines.
AES-256-GCM encryption at rest with a master key from the environment, plus
masked reads, is roughly a hundred and fifty. Either could be added later
without touching the schema's shape.

**Mitigations that are present.** `base_url` must be an absolute `http` or
`https` URL, so a row cannot aim the fetcher at `file://`. The routes can be
removed entirely with `PROVIDER_ADMIN_API=false` while database-backed
configuration keeps working. The deployment requirement — keep the port
private — is documented in the README and in `docs/PROVIDERS.md`.
