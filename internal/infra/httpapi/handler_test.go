package httpapi_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/infra/httpapi"
	"github.com/yoosuf/market-data-api/internal/shared/market"
	"github.com/yoosuf/market-data-api/internal/shared/markettest"
)

// newAPI wires the real store against a temporary database and a fake
// provider, so the test exercises the whole stack below HTTP.
func newAPI(t *testing.T, p *markettest.Provider) http.Handler {
	t.Helper()
	store := markettest.NewStore(t)
	now := markettest.Now()
	svc := annual.New(store, p, annual.Config{Now: now})
	return httpapi.New(svc, nil, now)
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return string(b)
}

func TestColdThenWarmRequest(t *testing.T) {
	p := markettest.NewProvider(markettest.History(t))
	h := newAPI(t, p)

	resp := get(t, h, "/symbols/IBM/annual/2023")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body(t, resp))
	}
	raw := body(t, resp)
	if p.Calls() != 1 {
		t.Fatalf("cold request made %d provider calls, want 1", p.Calls())
	}

	// The three aggregate fields must be JSON strings, not numbers.
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	for _, field := range []string{"high", "low", "volume"} {
		if _, ok := generic[field].(string); !ok {
			t.Errorf("field %q is %T, want string (JSON numbers lose precision)", field, generic[field])
		}
	}

	var got struct {
		Symbol string `json:"symbol"`
		Year   int    `json:"year"`
		High   string `json:"high"`
		Low    string `json:"low"`
		Volume string `json:"volume"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Symbol != "IBM" || got.Year != 2023 {
		t.Errorf("symbol/year = %s/%d", got.Symbol, got.Year)
	}
	if got.High != "153.2100" || got.Low != "129.0000" {
		t.Errorf("high/low = %s/%s, want 153.2100/129.0000", got.High, got.Low)
	}
	if got.Volume != "132687444" {
		t.Errorf("volume = %s, want 132687444", got.Volume)
	}

	// Warm request: served from SQLite, provider untouched.
	resp2 := get(t, h, "/symbols/IBM/annual/2023")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("warm status = %d", resp2.StatusCode)
	}
	if raw2 := body(t, resp2); raw2 != raw {
		t.Errorf("warm body differs:\n%s\n%s", raw, raw2)
	}
	if p.Calls() != 1 {
		t.Fatalf("warm request contacted the provider (calls=%d)", p.Calls())
	}
}

func TestSymbolIsNormalized(t *testing.T) {
	p := markettest.NewProvider(markettest.History(t))
	h := newAPI(t, p)

	if resp := get(t, h, "/symbols/ibm/annual/2023"); resp.StatusCode != http.StatusOK {
		t.Fatalf("lowercase symbol rejected: %d", resp.StatusCode)
	}
	// The uppercase form must hit the same cache entry.
	resp := get(t, h, "/symbols/IBM/annual/2023")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if p.Calls() != 1 {
		t.Fatalf("case variation caused a second fetch (calls=%d)", p.Calls())
	}
}

func TestInvalidInput(t *testing.T) {
	cases := map[string]struct {
		path string
		want int
	}{
		"three digit year": {"/symbols/IBM/annual/202", http.StatusBadRequest},
		"five digit year":  {"/symbols/IBM/annual/20233", http.StatusBadRequest},
		"non numeric year": {"/symbols/IBM/annual/abcd", http.StatusBadRequest},
		"future year":      {"/symbols/IBM/annual/2099", http.StatusBadRequest},
		"year before 1900": {"/symbols/IBM/annual/1800", http.StatusBadRequest},
		"symbol too long":  {"/symbols/TOOLONGSYMBOL/annual/2023", http.StatusBadRequest},
		"symbol injection": {"/symbols/IBM';DROP/annual/2023", http.StatusBadRequest},
		"unknown route":    {"/symbols/IBM/annual", http.StatusNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := markettest.NewProvider(markettest.History(t))
			h := newAPI(t, p)
			resp := get(t, h, c.path)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, c.want, body(t, resp))
			}
			// Invalid input must never reach the provider.
			if p.Calls() != 0 {
				t.Fatalf("invalid input triggered %d provider calls", p.Calls())
			}
		})
	}
}

// Legitimate punctuation must not be rejected.
func TestSymbolPunctuationIsAccepted(t *testing.T) {
	for _, sym := range []string{"BRK.B", "RDS-A"} {
		p := markettest.NewProvider(markettest.History(t))
		h := newAPI(t, p)
		if resp := get(t, h, "/symbols/"+sym+"/annual/2023"); resp.StatusCode != http.StatusOK {
			t.Errorf("symbol %s rejected with %d", sym, resp.StatusCode)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(markettest.History(t)))
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(method, "/symbols/IBM/annual/2023", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

// A successful upstream answer with no history for the year is 404.
func TestNoDataIsNotFound(t *testing.T) {
	p := markettest.NewProvider(markettest.History(t))
	h := newAPI(t, p)

	resp := get(t, h, "/symbols/IBM/annual/2019")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, `"no data"`) {
		t.Errorf("body = %s", got)
	}
}

func TestUnknownSymbolIsNotFound(t *testing.T) {
	p := markettest.FailingProvider(market.ErrNoData)
	h := newAPI(t, p)

	if resp := get(t, h, "/symbols/NOSUCH/annual/2023"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// An outage is never reported as "no data": that would tell the caller the
// data does not exist when the service simply could not look.
func TestProviderOutageIsServiceUnavailable(t *testing.T) {
	p := markettest.FailingProvider(market.ErrUpstreamUnavailable)
	h := newAPI(t, p)

	resp := get(t, h, "/symbols/IBM/annual/2023")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := body(t, resp); strings.Contains(got, "no data") {
		t.Fatalf("outage reported as absence: %s", got)
	}
}

func TestBadUpstreamPayloadIsBadGateway(t *testing.T) {
	p := markettest.FailingProvider(market.ErrUpstreamPayload)
	h := newAPI(t, p)

	if resp := get(t, h, "/symbols/IBM/annual/2023"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// Nothing internal may reach the client: not SQL, not the API key, not the
// upstream error text.
func TestErrorBodiesDoNotLeakInternals(t *testing.T) {
	secrets := []string{"SUPERSECRETKEY", "SELECT", "monthly_prices", "sqlite", "alphavantage.co", "goroutine"}
	leaky := errors.New("pq: SELECT MAX(high) FROM monthly_prices failed for key SUPERSECRETKEY at alphavantage.co")

	p := markettest.FailingProvider(leaky)
	h := newAPI(t, p)

	resp := get(t, h, "/symbols/IBM/annual/2023")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	got := body(t, resp)
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Errorf("response leaks %q: %s", s, got)
		}
	}
}

func TestHealthz(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(nil))
	resp := get(t, h, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "ok") {
		t.Errorf("body = %s", got)
	}
}

func TestContentTypeIsJSON(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(markettest.History(t)))
	resp := get(t, h, "/symbols/IBM/annual/2023")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
}
