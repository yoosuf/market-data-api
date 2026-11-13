package graphqlapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/infra/graphqlapi"
	"github.com/yoosuf/market-data-api/internal/infra/httpapi"
	"github.com/yoosuf/market-data-api/internal/shared/market"
	"github.com/yoosuf/market-data-api/internal/shared/markettest"
)

func newAPI(t *testing.T, p *markettest.Provider) http.Handler {
	t.Helper()
	store := markettest.NewStore(t)
	now := markettest.Now()
	svc := annual.New(store, p, annual.Config{Now: now})
	h, err := graphqlapi.New(svc, nil, now)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return h
}

// gqlResponse is the standard GraphQL envelope.
type gqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func post(t *testing.T, h http.Handler, query string, vars map[string]any) (*http.Response, gqlResponse) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	resp := rec.Result()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = resp.Body.Close()

	var out gqlResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp, out
}

const annualQuery = `query($s: String!, $y: Int!) {
  annual(symbol: $s, year: $y) { symbol year high low volume months }
}`

func TestQueryReturnsAggregate(t *testing.T) {
	p := markettest.NewProvider(markettest.History(t))
	h := newAPI(t, p)

	resp, out := post(t, h, annualQuery, map[string]any{"s": "IBM", "y": 2023})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(out.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", out.Errors)
	}
	if p.Calls() != 1 {
		t.Fatalf("cold query made %d provider calls, want 1", p.Calls())
	}

	// The three aggregate fields must be strings here too: GraphQL Float is a
	// double, and volume can exceed a 32-bit Int.
	var generic map[string]any
	if err := json.Unmarshal(out.Data["annual"], &generic); err != nil {
		t.Fatalf("decode annual: %v", err)
	}
	for _, field := range []string{"high", "low", "volume"} {
		if _, ok := generic[field].(string); !ok {
			t.Errorf("field %q is %T, want string", field, generic[field])
		}
	}
	if generic["high"] != "153.2100" || generic["low"] != "129.0000" {
		t.Errorf("high/low = %v/%v", generic["high"], generic["low"])
	}
	if generic["volume"] != "132687444" {
		t.Errorf("volume = %v", generic["volume"])
	}
	if generic["symbol"] != "IBM" {
		t.Errorf("symbol = %v", generic["symbol"])
	}
}

// The GraphQL transport must use the same cache as REST: a warm query does
// not contact the provider.
func TestWarmQueryDoesNotFetch(t *testing.T) {
	p := markettest.NewProvider(markettest.History(t))
	h := newAPI(t, p)

	if _, out := post(t, h, annualQuery, map[string]any{"s": "IBM", "y": 2023}); len(out.Errors) != 0 {
		t.Fatalf("cold: %+v", out.Errors)
	}
	if _, out := post(t, h, annualQuery, map[string]any{"s": "ibm", "y": 2023}); len(out.Errors) != 0 {
		t.Fatalf("warm: %+v", out.Errors)
	}
	if p.Calls() != 1 {
		t.Fatalf("warm query refetched: %d calls", p.Calls())
	}
}

// GraphQL has no status line, so absence and unavailability are distinguished
// by extensions.code. Conflating them is the same bug the REST 404/503 split
// exists to prevent.
func TestErrorCodes(t *testing.T) {
	cases := map[string]struct {
		provider *markettest.Provider
		symbol   string
		year     int
		wantCode string
	}{
		"no data for year": {markettest.NewProvider(markettest.History(t)), "IBM", 2019, "NOT_FOUND"},
		"unknown symbol":   {markettest.FailingProvider(market.ErrNoData), "NOSUCH", 2023, "NOT_FOUND"},
		"provider outage":  {markettest.FailingProvider(market.ErrUpstreamUnavailable), "IBM", 2023, "UPSTREAM_UNAVAILABLE"},
		"bad payload":      {markettest.FailingProvider(market.ErrUpstreamPayload), "IBM", 2023, "UPSTREAM_ERROR"},
		"database failure": {markettest.FailingProvider(errors.New("disk I/O error")), "IBM", 2023, "INTERNAL_ERROR"},
		"invalid symbol":   {markettest.NewProvider(markettest.History(t)), "TOOLONGSYMBOL", 2023, "BAD_USER_INPUT"},
		"future year":      {markettest.NewProvider(markettest.History(t)), "IBM", 2099, "BAD_USER_INPUT"},
		"year before 1900": {markettest.NewProvider(markettest.History(t)), "IBM", 1800, "BAD_USER_INPUT"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newAPI(t, c.provider)
			resp, out := post(t, h, annualQuery, map[string]any{"s": c.symbol, "y": c.year})

			// Execution errors use the GraphQL envelope, not an HTTP status.
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if len(out.Errors) != 1 {
				t.Fatalf("got %d errors, want 1: %+v", len(out.Errors), out.Errors)
			}
			if got := out.Errors[0].Extensions["code"]; got != c.wantCode {
				t.Errorf("code = %v, want %s", got, c.wantCode)
			}
			// A nullable field means absence is data, not a crash.
			if raw, ok := out.Data["annual"]; ok && string(raw) != "null" {
				t.Errorf("annual = %s, want null", raw)
			}
		})
	}
}

// Invalid input must never reach the provider.
func TestInvalidInputDoesNotFetch(t *testing.T) {
	for _, c := range []struct {
		symbol string
		year   int
	}{{"TOOLONGSYMBOL", 2023}, {"IBM", 2099}, {"IB M", 2023}} {
		p := markettest.NewProvider(markettest.History(t))
		h := newAPI(t, p)
		if _, out := post(t, h, annualQuery, map[string]any{"s": c.symbol, "y": c.year}); len(out.Errors) == 0 {
			t.Errorf("%v: expected an error", c)
		}
		if p.Calls() != 0 {
			t.Errorf("%v: invalid input made %d provider calls", c, p.Calls())
		}
	}
}

func TestNoInternalsLeakInErrors(t *testing.T) {
	secrets := []string{"SUPERSECRETKEY", "SELECT", "monthly_prices", "sqlite", "alphavantage.co"}
	leaky := errors.New("SELECT MAX(high) FROM monthly_prices failed with key SUPERSECRETKEY at alphavantage.co")

	h := newAPI(t, markettest.FailingProvider(leaky))
	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ annual(symbol:\"IBM\", year:2023) { high } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Errorf("response leaks %q: %s", s, body)
		}
	}
}

func TestGetRequestsAreSupported(t *testing.T) {
	p := markettest.NewProvider(markettest.History(t))
	h := newAPI(t, p)

	q := url.Values{"query": {`{ annual(symbol: "IBM", year: 2023) { high volume } }`}}
	req := httptest.NewRequest(http.MethodGet, "/graphql?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "153.2100") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// A request that never reached execution is a transport-level error, so it
// gets a 400 rather than a 200 with an errors array.
func TestMalformedRequestsAreBadRequest(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(markettest.History(t)))

	cases := map[string]*http.Request{
		"not json":         httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{nope`)),
		"missing query":    httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"variables":{}}`)),
		"empty get":        httptest.NewRequest(http.MethodGet, "/graphql", nil),
		"unsupported verb": httptest.NewRequest(http.MethodDelete, "/graphql", nil),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if req.Method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// A malformed query document is a validation error: 200 with an errors array.
func TestInvalidQueryDocument(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(markettest.History(t)))

	resp, out := post(t, h, `{ annual(symbol: "IBM", year: 2023) { nosuchfield } }`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(out.Errors) == 0 {
		t.Fatal("expected a validation error")
	}
}

func TestIntrospectionWorks(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(nil))
	_, out := post(t, h, `{ __schema { queryType { name } } }`, nil)
	if len(out.Errors) != 0 {
		t.Fatalf("introspection failed: %+v", out.Errors)
	}
	if !strings.Contains(string(out.Data["__schema"]), "Query") {
		t.Fatalf("schema = %s", out.Data["__schema"])
	}
}

// Parity: both transports resolve through one service, so the same request
// must produce the same values and the same cache behavior. If this ever
// fails, one adapter has grown a rule that belongs in the workflow.
func TestRESTAndGraphQLAgree(t *testing.T) {
	store := markettest.NewStore(t)
	now := markettest.Now()
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(store, p, annual.Config{Now: now})

	gql, err := graphqlapi.New(svc, nil, now)
	if err != nil {
		t.Fatalf("graphql: %v", err)
	}
	rest := httpapi.New(svc, nil, now)

	// REST first: one fetch.
	restRec := httptest.NewRecorder()
	rest.ServeHTTP(restRec, httptest.NewRequest(http.MethodGet, "/symbols/IBM/annual/2023", nil))
	if restRec.Code != http.StatusOK {
		t.Fatalf("REST status = %d", restRec.Code)
	}
	var restBody map[string]any
	if err := json.Unmarshal(restRec.Body.Bytes(), &restBody); err != nil {
		t.Fatalf("decode REST: %v", err)
	}

	// GraphQL second: must be served from the same cache, no extra fetch.
	_, out := post(t, gql, annualQuery, map[string]any{"s": "IBM", "y": 2023})
	if len(out.Errors) != 0 {
		t.Fatalf("graphql errors: %+v", out.Errors)
	}
	var gqlBody map[string]any
	if err := json.Unmarshal(out.Data["annual"], &gqlBody); err != nil {
		t.Fatalf("decode GraphQL: %v", err)
	}

	if p.Calls() != 1 {
		t.Fatalf("two transports caused %d provider calls, want 1", p.Calls())
	}
	for _, field := range []string{"symbol", "high", "low", "volume"} {
		if restBody[field] != gqlBody[field] {
			t.Errorf("%s: REST %v, GraphQL %v", field, restBody[field], gqlBody[field])
		}
	}
	// year is a JSON number in both; compare through the decoded float.
	if restBody["year"] != gqlBody["year"] {
		t.Errorf("year: REST %v, GraphQL %v", restBody["year"], gqlBody["year"])
	}
}
