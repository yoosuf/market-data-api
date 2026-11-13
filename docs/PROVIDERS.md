# Provider configuration

Upstream providers live in the database, not the environment. They can be
added, reordered, disabled and removed at runtime through a CRUD API, and
changes take effect on the next request without a restart.

> ## Security notice
>
> **These endpoints are unauthenticated and return API keys in full.**
> That is a deliberate configuration choice, documented here so it is not a
> surprise. The consequences:
>
> - Anyone who can reach the port can read every provider credential with a
>   single `GET /admin/providers`.
> - Anyone who can reach the port can repoint the service at a host they
>   control, and the service will send its stored credentials there.
> - Anyone with a copy of `market.db` — a backup, a stray scp, a laptop — has
>   every key in cleartext.
>
> Run this on a trusted network only: bind to localhost, or keep the port
> behind a firewall or an authenticating proxy. Do not expose it to the
> internet. Set `PROVIDER_ADMIN_API=false` to remove the routes entirely while
> keeping database-backed configuration.

## The model

| Column | Meaning |
|---|---|
| `id` | Assigned on create |
| `name` | Operator label, unique |
| `kind` | Selects the adapter in code. Currently `alphavantage` |
| `base_url` | Absolute http/https endpoint the adapter calls |
| `api_key` | Credential handed to the adapter |
| `priority` | Lower is tried first. Default 100 |
| `enabled` | Disabled providers are never called. Default true |
| `timeout_ms` | Per-request budget, 1 ms to 60 s. Default 10000 |

**`kind` is not free-form.** A row supplies *configuration*; the adapter
supplies *behavior* — how to build the request and how to read the payload.
Supporting a provider with a different response shape means adding an adapter
package and one line in `provider.DefaultRegistry()`. Posting an unregistered
kind is rejected with the list of kinds this build knows.

## Failover

Enabled providers are tried in `priority` order, ties broken by `id`, until
one answers. The classification rule is the part that matters:

| What happened | Chain result |
|---|---|
| A provider returned history | That history, immediately. Later providers are not called |
| A provider said "no such symbol" | Try the next one — another may know it |
| **Every** provider said "no such symbol" | `no data` → 404, and the result may be cached |
| Any provider failed, timed out, or was rate-limited | `upstream unavailable` → 503, **even if others said no data** |
| No providers configured or enabled | `upstream unavailable` → 503 |
| A row names an unknown `kind` | Counts as a failure, not as absence |
| The caller cancelled | Stop; do not try the rest |

The fourth row is the important one. If one provider could not answer, the
service has not established that the symbol does not exist, so it must not
report 404 — and must not let the workflow cache a negative result it has not
actually proven.

## Endpoints

### `GET /admin/providers`

```json
{"providers": [{"id": 1, "name": "alphavantage", "kind": "alphavantage",
  "base_url": "https://www.alphavantage.co/query", "api_key": "...",
  "priority": 100, "enabled": true, "timeout_ms": 10000,
  "created_at": "2026-11-13T12:21:54Z", "updated_at": "2026-11-13T12:22:13Z"}]}
```

Includes disabled providers. Ordered by priority.

### `POST /admin/providers`

```sh
curl -s -X POST localhost:8080/admin/providers \
  -H 'Content-Type: application/json' \
  -d '{"name":"alphavantage","kind":"alphavantage",
       "base_url":"https://www.alphavantage.co/query","api_key":"YOUR_KEY"}'
```

`name`, `kind` and `base_url` are required; `priority` (100), `enabled`
(true) and `timeout_ms` (10000) default. Returns **201** with the created row.

`api_key` is **optional**, because an endpoint may need no credential. The
consequence is worth knowing: a row created without a key is accepted, and an
upstream that does require one will then answer with an error the adapter
classifies as *unavailable*. Check the server log if a provider you just added
never seems to answer.

Unknown JSON fields are **rejected** rather than ignored, so a typo like
`timout_ms` fails loudly instead of silently leaving the default in place.

### `GET /admin/providers/{id}`

Returns one provider, or 404.

### `PUT /admin/providers/{id}`

Full replacement; the same body as create. Returns the updated row.

`updated_at` advances on every write, and that is what a running server
watches: the next request rebuilds the adapter, so a changed key or URL takes
effect without a restart.

### `DELETE /admin/providers/{id}`

**204** on success, 404 if it was already gone. Deleting the last provider
leaves the API able to serve cached data and unable to fetch anything new.

### Status codes

| Status | When |
|---|---|
| 200 / 201 / 204 | Success |
| 400 | Validation failure, malformed JSON, unknown field, bad id |
| 404 | No provider with that id |
| 409 | A provider with that name already exists |
| 500 | Database failure |

Validation messages are returned in full here, unlike on the market data path.
They describe the request the operator just sent, not the server's internals.

## Validation

- `name` required, 64 characters or fewer, unique
- `api_key` optional, trimmed
- `kind` must be registered in this build
- `base_url` must be absolute with an `http` or `https` scheme — `file://`,
  relative paths and bare hostnames are rejected
- `priority` must not be negative
- `timeout_ms` must be one minute or less

## First run

A new database has no providers. The API starts, serves `/healthz`, and
answers 503 on a cache miss until one is added — it logs a warning saying so
at startup. Add one with the `POST` above; no restart is needed.

No upstream credential is read from the environment. The database is the only
source of truth, and both `cmd/api` and `cmd/refresh` read the same table, so
the job cannot drift onto a different upstream than the one being served.

`cmd/refresh` exits non-zero when the table is empty rather than quietly
fetching nothing.
