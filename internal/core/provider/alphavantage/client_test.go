package alphavantage_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/provider/alphavantage"
	"github.com/yoosuf/market-data-api/internal/shared/market"
)

const validBody = `{
  "Meta Data": {"2. Symbol": "IBM"},
  "Monthly Time Series": {
    "2023-01-31": {"1. open": "141.1", "2. high": "147.1800", "3. low": "140.2400", "4. close": "144.1", "5. volume": "70524849"},
    "2023-02-28": {"1. open": "144.5", "2. high": "153.2100", "3. low": "129.0000", "4. close": "130.5", "5. volume": "62162595"},
    "2022-12-30": {"1. open": "140.0", "2. high": "999.0000", "3. low": "100.0000", "4. close": "141.0", "5. volume": "11111111"}
  }
}`

// serve starts a local provider stub and returns a client pointed at it,
// along with a counter of requests received.
func serve(t *testing.T, h http.HandlerFunc) (*alphavantage.Client, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return alphavantage.New("test-key",
		alphavantage.WithBaseURL(srv.URL),
		alphavantage.WithHTTPClient(srv.Client())), &calls
}

func TestMonthlyHistoryParsesValidPayload(t *testing.T) {
	var gotQuery string
	c, calls := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(validBody))
	})

	recs, err := c.MonthlyHistory(context.Background(), "IBM")
	if err != nil {
		t.Fatalf("MonthlyHistory: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("calls = %d, want 1", *calls)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}

	// The request must carry the documented parameters.
	for _, want := range []string{"function=TIME_SERIES_MONTHLY", "symbol=IBM", "apikey=test-key"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}

	byMonth := map[string]market.MonthlyRecord{}
	for _, r := range recs {
		byMonth[r.Month] = r
	}
	jan, ok := byMonth["2023-01"]
	if !ok {
		t.Fatal("missing 2023-01")
	}
	if jan.High.String() != "147.1800" || jan.Low.String() != "140.2400" {
		t.Errorf("jan high/low = %s/%s", jan.High, jan.Low)
	}
	if jan.Volume != 70524849 {
		t.Errorf("jan volume = %d", jan.Volume)
	}
	if jan.Year != 2023 || jan.Symbol != "IBM" {
		t.Errorf("jan year/symbol = %d/%s", jan.Year, jan.Symbol)
	}
	// The provider returns full history, including other years.
	if _, ok := byMonth["2022-12"]; !ok {
		t.Error("history from other years must be preserved for later aggregation")
	}
}

// Alpha Vantage reports an unknown symbol with HTTP 200 and an error body.
// That is evidence of absence, and is distinct from a provider outage.
func TestUnknownSymbolIsNotAnOutage(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Error Message": "Invalid API call."}`))
	})
	_, err := c.MonthlyHistory(context.Background(), "NOSUCH")
	if !errors.Is(err, market.ErrNoData) {
		t.Fatalf("got %v, want ErrUnknownSymbol", err)
	}
	if errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatal("unknown symbol must not be classified as unavailable")
	}
}

// Rate limiting also arrives as HTTP 200. It is NOT evidence of absence.
func TestRateLimitNoteIsUnavailable(t *testing.T) {
	for _, body := range []string{
		`{"Note": "Thank you for using Alpha Vantage! Our standard API call frequency is 25 requests per day."}`,
		`{"Information": "The demo API key is for demo purposes only."}`,
	} {
		c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		_, err := c.MonthlyHistory(context.Background(), "IBM")
		if !errors.Is(err, market.ErrUpstreamUnavailable) {
			t.Errorf("body %s: got %v, want ErrUnavailable", body, err)
		}
		if errors.Is(err, market.ErrNoData) {
			t.Errorf("body %s: quota message must never mean 'no such symbol'", body)
		}
	}
}

func TestNon2xxIsUnavailable(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusForbidden} {
		c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		})
		_, err := c.MonthlyHistory(context.Background(), "IBM")
		if !errors.Is(err, market.ErrUpstreamUnavailable) {
			t.Errorf("status %d: got %v, want ErrUnavailable", code, err)
		}
	}
}

func TestMalformedPayloads(t *testing.T) {
	cases := map[string]string{
		"not json":          `<html>maintenance</html>`,
		"no series key":     `{"Meta Data": {"2. Symbol": "IBM"}}`,
		"bad date key":      `{"Monthly Time Series": {"not-a-date": {"2. high": "1.0", "3. low": "1.0", "5. volume": "1"}}}`,
		"bad price":         `{"Monthly Time Series": {"2023-01-31": {"2. high": "abc", "3. low": "1.0", "5. volume": "1"}}}`,
		"excess precision":  `{"Monthly Time Series": {"2023-01-31": {"2. high": "1.234567", "3. low": "1.0", "5. volume": "1"}}}`,
		"low exceeds high":  `{"Monthly Time Series": {"2023-01-31": {"2. high": "1.0", "3. low": "9.0", "5. volume": "1"}}}`,
		"fractional volume": `{"Monthly Time Series": {"2023-01-31": {"2. high": "1.0", "3. low": "1.0", "5. volume": "1.5"}}}`,
		"entry not object":  `{"Monthly Time Series": {"2023-01-31": "nope"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			_, err := c.MonthlyHistory(context.Background(), "IBM")
			if !errors.Is(err, market.ErrUpstreamPayload) {
				t.Fatalf("got %v, want ErrBadPayload", err)
			}
		})
	}
}

// An empty but well-formed series is a successful response with no history.
func TestEmptySeriesIsSuccessWithNoRecords(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Monthly Time Series": {}}`))
	})
	recs, err := c.MonthlyHistory(context.Background(), "IBM")
	if err != nil {
		t.Fatalf("got %v, want success", err)
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0", len(recs))
	}
}

func TestTimeoutIsUnavailable(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(validBody))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.MonthlyHistory(ctx, "IBM")
	if !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

// The request URL carries the API key, so no error may ever expose it.
func TestErrorsDoNotLeakAPIKey(t *testing.T) {
	c := alphavantage.New("SUPERSECRETKEY",
		alphavantage.WithBaseURL("http://127.0.0.1:1"), // nothing listening
		alphavantage.WithHTTPClient(&http.Client{Timeout: time.Second}))

	_, err := c.MonthlyHistory(context.Background(), "IBM")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SUPERSECRETKEY") {
		t.Fatalf("error leaks the API key: %v", err)
	}
	if !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}
