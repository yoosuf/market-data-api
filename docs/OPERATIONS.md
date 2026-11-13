# Operations

## The refresh command is optional

`cmd/refresh` warms the SQLite cache ahead of the first HTTP request. The API
is correct without it: a cold request simply fetches on demand. Nothing in the
serving path depends on the job having run.

```sh
DB_PATH=/srv/market/market.db REFRESH_SYMBOLS=IBM,AAPL,MSFT go run ./cmd/refresh
```

The job reads its providers from the same `providers` table the API uses, so
it cannot drift onto a different upstream than the one being served. It needs
no API key in its environment: credentials are columns. If the table is empty
it exits non-zero rather than fetching nothing quietly.

Build it once for cron:

```sh
go build -o bin/refresh ./cmd/refresh
```

## One fetch per symbol per run

Alpha Vantage returns a symbol's **entire** monthly history and accepts no year
parameter. Fetching per symbol-year would multiply quota use and return the
same payload every time. The job fetches once per symbol, and every year of
that symbol becomes servable at once.

Duplicates in `REFRESH_SYMBOLS` are collapsed before fetching, so a sloppy list
does not spend quota twice.

## Cadence

Pick it from what has to stay fresh:

| If the service serves… | Then… |
|---|---|
| Completed years only | A one-time prewarm is enough. History does not change. A monthly run covers newly completed years. |
| The current year as well | The current-year aggregate is refreshed on demand when its cached import is over 24h old, so a daily run is already redundant for correctness — it only moves latency off the first request. |

A daily run after market close is a reasonable default:

```cron
# 06:10 UTC daily — absolute paths, both for the binary and the database
10 6 * * * DB_PATH=/srv/market/market.db \
  /srv/market/bin/refresh >> /var/log/market-refresh.log 2>&1
```

**Use an absolute `DB_PATH`.** Cron's working directory is not yours. A
relative path would create a second database the API never reads — the failure
is silent, which is what makes it worth stating twice.

## Quota

The free tier is roughly 25 requests per day. Each symbol costs one request per
run against the first provider that answers, so the symbol list and the
schedule multiply directly:

```
requests/day = len(REFRESH_SYMBOLS) × runs/day
```

A symbol that the first provider cannot serve costs an extra request against
each fallback it tries, so a chain of three providers has a worst case of
three times the budget above.

Three symbols daily is 3 of ~25, leaving room for on-demand misses. Exceeding
the quota is not fatal: the provider returns an informational message with HTTP
200, the adapter classifies it as *unavailable*, and cached data keeps being
served.

## Partial failure

The job continues to the next symbol when one fails, so a single bad symbol or
one rate-limited call does not deny the rest their refresh. Each failure is
logged with the symbol, and the command exits **non-zero** if any symbol
failed, which is what lets cron's `MAILTO` or a log alert notice.

A failed symbol keeps whatever it had: the import transaction either commits
whole or not at all, and a failure never advances `last_success_at`.

## Overlapping runs

A run bounded by `REFRESH_TIMEOUT_SECONDS` (default 120s) finishes long before
a daily schedule fires again. If you schedule it frequently enough to overlap,
wrap it in a lock rather than relying on the database:

```cron
*/5 * * * * /usr/bin/flock -n /tmp/market-refresh.lock /srv/market/bin/refresh
```

Overlap is a quota and log-noise problem, not a data-integrity one: concurrent
writers wait on `busy_timeout` (5s), and the `(symbol, month)` key means a
double import cannot corrupt an aggregate.

## Shared database file

The API and the job must point at the same file — it carries both the cached
market data and the provider configuration. The database runs in WAL mode,
so the job's writes do not block the API's reads. Both processes create the
schema on start, so either may run first.

## Shutdown

The API handles `SIGINT` and `SIGTERM`, stops accepting connections, and gives
in-flight requests up to 10 seconds to finish before closing the database.
