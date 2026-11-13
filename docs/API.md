# API reference

## `GET /symbols/{symbol}/annual/{year}`

Returns the annual aggregate for one symbol.

### Parameters

| Name | In | Rules |
|---|---|---|
| `symbol` | path | Case-insensitive, normalized to uppercase. 1–10 characters of `A-Z`, `0-9`, `.`, `-`. Punctuation is legitimate (`BRK.B`, `RDS-A`). |
| `year` | path | Exactly four digits, between 1900 and the current year inclusive. |

### 200 — success

```json
{
  "symbol": "IBM",
  "year": 2023,
  "high": "153.2100",
  "low": "129.0000",
  "volume": "132687444"
}
```

| Field | Type | Meaning |
|---|---|---|
| `symbol` | string | Normalized symbol |
| `year` | number | The requested calendar year |
| `high` | string | `MAX` of the year's monthly highs, four decimals |
| `low` | string | `MIN` of the year's monthly lows, four decimals |
| `volume` | string | `SUM` of the year's monthly volumes |

`high`, `low`, and `volume` are strings. Most JSON parsers decode numbers as
IEEE-754 doubles, which cannot represent every four-decimal price exactly;
rendering them as strings hands the caller the exact stored value.

### Errors

All error bodies have the same shape:

```json
{"error": "no data"}
```

| Status | `error` | Meaning |
|---|---|---|
| 400 | `invalid symbol` | Symbol failed the charset or length rules |
| 400 | `invalid year` | Not four digits, or outside 1900…current year |
| 404 | `no data` | The provider answered successfully and this symbol-year has no history |
| 405 | — | Method other than `GET` |
| 502 | `upstream error` | The provider answered with something unusable |
| 503 | `upstream unavailable` | The provider was unreachable, timed out, or rate-limited |
| 500 | `internal error` | Database or unclassified failure |

**404 vs 5xx.** A 404 asserts that the data does not exist, and is returned
only after the provider actually answered. Anything that prevented the service
from looking — an outage, a timeout, an exhausted quota — is a 5xx, because
the data may well exist. Error bodies carry fixed strings only: no SQL, no
upstream text, no API key.

## Provider administration

`/admin/providers` exposes CRUD over the configured upstreams. It is
documented separately in [PROVIDERS.md](PROVIDERS.md), including its security
posture: the routes are unauthenticated and return API keys in full.

## `GET /healthz`

```json
{"status": "ok"}
```

Liveness only. It does not probe SQLite or the provider.

## Examples

```sh
# Completed year
curl -s localhost:8080/symbols/IBM/annual/2023

# Lowercase is normalized to the same cache entry
curl -s localhost:8080/symbols/ibm/annual/2023

# Punctuated symbol
curl -s localhost:8080/symbols/BRK.B/annual/2023

# 400: three-digit year, no upstream call is made
curl -i -s localhost:8080/symbols/IBM/annual/202

# 404: valid symbol, year predates its listing
curl -i -s localhost:8080/symbols/IBM/annual/1901
```
