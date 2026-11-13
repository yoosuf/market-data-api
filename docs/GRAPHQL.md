# GraphQL API

A second transport over the same service. REST and GraphQL resolve through the
identical cache-aside workflow, so cache behavior, validation and upstream
semantics cannot drift between them. This is asserted by a parity test.

> GraphQL is an extension beyond the original REST contract, not a replacement
> for it. `GET /symbols/{symbol}/annual/{year}` remains the primary endpoint.

## Endpoint

```
POST /graphql     Content-Type: application/json
GET  /graphql?query=...&variables=...&operationName=...
```

`GET` is supported so a query can be shared as a URL. Both forms are read-only;
the schema defines no mutations.

## Schema

```graphql
type Query {
  """Annual aggregate for a symbol. Null with a NOT_FOUND error when the
  symbol has no history for that year."""
  annual(symbol: String!, year: Int!): AnnualSummary
}

type AnnualSummary {
  symbol: String!
  year: Int!
  high: String!    # four decimals, exact
  low: String!     # four decimals, exact
  volume: String!
  months: Int!     # monthly records behind the aggregate
}
```

Introspection is enabled, so GraphiQL, Altair or `graphql-schema` tooling can
discover the schema without a copy of this file.

### Why `high`, `low` and `volume` are `String`

GraphQL's `Float` is an IEEE-754 double, which cannot represent every
four-decimal price exactly — the same reason the database does not use `REAL`.
Rendering them as strings hands the client the exact stored value.

`volume` has a second reason: GraphQL's `Int` is specified as 32-bit signed,
which overflows above 2,147,483,647. A single liquid symbol's annual volume is
routinely a hundred times that.

`months` is an honest `Int`: it is small, and it is a count rather than a
quantity the caller will do arithmetic on.

## Examples

```sh
# POST with variables
curl -s localhost:8080/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"query($s:String!,$y:Int!){ annual(symbol:$s, year:$y){ symbol year high low volume months } }",
       "variables":{"s":"IBM","y":2023}}'
```

```json
{"data":{"annual":{"symbol":"IBM","year":2023,"high":"166.3400","low":"120.5500","volume":"1112911966","months":12}}}
```

```sh
# GET, shareable as a URL
curl -s -G localhost:8080/graphql \
  --data-urlencode 'query={ annual(symbol: "IBM", year: 2015) { high low volume } }'

# Introspect the type
curl -s localhost:8080/graphql -H 'Content-Type: application/json' \
  -d '{"query":"{ __type(name:\"AnnualSummary\"){ fields { name } } }"}'
```

## Errors

`annual` is **nullable**: absence is data, not a crash. A request that finds
nothing returns `"annual": null` alongside an error entry.

GraphQL has no status line, so the four-way outcome split that REST expresses
with status codes is carried in `extensions.code`:

| `extensions.code` | REST equivalent | Meaning |
|---|---|---|
| `BAD_USER_INPUT` | 400 | Symbol or year failed validation |
| `NOT_FOUND` | 404 | Upstream answered; this symbol-year has no history |
| `UPSTREAM_UNAVAILABLE` | 503 | Provider unreachable, timed out, or rate-limited |
| `UPSTREAM_ERROR` | 502 | Provider answered with something unusable |
| `INTERNAL_ERROR` | 500 | Database or unclassified failure |

```json
{
  "data": {"annual": null},
  "errors": [{
    "message": "no data",
    "path": ["annual"],
    "extensions": {"code": "NOT_FOUND"}
  }]
}
```

**`NOT_FOUND` is a claim about the world.** It is returned only after the
provider actually answered. An outage produces `UPSTREAM_UNAVAILABLE`, because
the data may well exist and the service simply could not look — the same
invariant the REST 404/503 split protects.

Messages are fixed strings. No SQL, no driver text, no upstream error body and
no API key reaches a client; detail goes to the server log.

## HTTP status codes

| Situation | Status |
|---|---|
| Query executed, with or without field errors | 200 |
| Query document failed to parse or validate | 200 with `errors` |
| Request never reached execution — bad JSON, missing `query`, wrong method | 400 |

Returning 200 for execution errors is the GraphQL-over-HTTP convention, and
it is why `extensions.code` has to carry the real outcome. A request that was
never a valid GraphQL request at all is a transport-level failure, so it gets
a 400.

## Limits

Request bodies are capped at 64 KiB. The schema exposes one field with no
nesting and no lists, so query depth and complexity limits would guard nothing
today; they become necessary the moment a resolver can recurse.
