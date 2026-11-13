// Package alphavantage translates Alpha Vantage responses into domain
// records. Provider JSON keys and quirks stop here; nothing downstream knows
// this provider exists.
//
// Outcomes are reported with the shared sentinels from the market package:
// market.ErrNoData for a symbol the provider does not know,
// market.ErrUpstreamUnavailable for anything that stopped it from answering,
// and market.ErrUpstreamPayload for an answer it could not parse. Nothing
// downstream imports this package to interpret an error.
package alphavantage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// defaultBaseURL is the public endpoint, used only when a caller supplies no
// base URL. In this service every client is built from a stored provider row,
// which always carries its own.
const defaultBaseURL = "https://www.alphavantage.co/query"

const (
	// maxBody bounds memory use; full monthly history for a symbol is well
	// under a megabyte.
	maxBody = 8 << 20
)

// Client fetches monthly history. The zero value is not usable; call New.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient injects an HTTP client, chiefly so tests can point at a local
// server and production can bound the request with a timeout.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithBaseURL overrides the endpoint. Stored provider configuration supplies
// it in production; tests point it at a local server.
func WithBaseURL(u string) Option {
	return func(cl *Client) { cl.baseURL = u }
}

// New builds a client for the given API key.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    defaultBaseURL,
		apiKey:     apiKey,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// payload mirrors the shapes Alpha Vantage returns on HTTP 200. Note that
// three of the four are errors: a 200 is not evidence of history.
type payload struct {
	ErrorMessage string                     `json:"Error Message"`
	Note         string                     `json:"Note"`
	Information  string                     `json:"Information"`
	Series       map[string]json.RawMessage `json:"Monthly Time Series"`
}

type month struct {
	High   string `json:"2. high"`
	Low    string `json:"3. low"`
	Volume string `json:"5. volume"`
}

// MonthlyHistory fetches the full monthly series for a symbol. The provider
// returns history by symbol and accepts no year parameter, so callers filter
// by year after persisting.
//
// Every returned error is sanitized: the request URL carries the API key, so
// transport errors are classified rather than wrapped.
func (c *Client) MonthlyHistory(ctx context.Context, symbol string) ([]market.MonthlyRecord, error) {
	q := url.Values{
		"function": {"TIME_SERIES_MONTHLY"},
		"symbol":   {symbol},
		"apikey":   {c.apiKey},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building request", market.ErrUpstreamPayload)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Never wrap err: a *url.Error carries the full URL, API key included.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: request cancelled or timed out", market.ErrUpstreamUnavailable)
		}
		return nil, fmt.Errorf("%w: request failed", market.ErrUpstreamUnavailable)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: provider returned status %d", market.ErrUpstreamUnavailable, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response body", market.ErrUpstreamUnavailable)
	}

	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%w: response is not valid JSON", market.ErrUpstreamPayload)
	}

	switch {
	case p.ErrorMessage != "":
		// The provider's way of saying the symbol does not exist.
		return nil, market.ErrNoData
	case p.Note != "", p.Information != "":
		// Rate limit or quota exhaustion, served with HTTP 200.
		return nil, fmt.Errorf("%w: provider declined to serve the request", market.ErrUpstreamUnavailable)
	case p.Series == nil:
		return nil, fmt.Errorf("%w: no monthly time series in response", market.ErrUpstreamPayload)
	}

	recs := make([]market.MonthlyRecord, 0, len(p.Series))
	for date, raw := range p.Series {
		var m month
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%w: malformed entry for %s", market.ErrUpstreamPayload, date)
		}
		rec, err := toRecord(symbol, date, m)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", market.ErrUpstreamPayload, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

func toRecord(symbol, date string, m month) (market.MonthlyRecord, error) {
	monthKey, year, err := market.MonthKey(date)
	if err != nil {
		return market.MonthlyRecord{}, err
	}
	high, err := market.ParsePrice(m.High)
	if err != nil {
		return market.MonthlyRecord{}, fmt.Errorf("%s high: %w", date, err)
	}
	low, err := market.ParsePrice(m.Low)
	if err != nil {
		return market.MonthlyRecord{}, fmt.Errorf("%s low: %w", date, err)
	}
	volume, err := parseVolume(m.Volume)
	if err != nil {
		return market.MonthlyRecord{}, fmt.Errorf("%s volume: %w", date, err)
	}
	if low > high {
		return market.MonthlyRecord{}, fmt.Errorf("%s low %s exceeds high %s", date, low, high)
	}
	return market.MonthlyRecord{
		Symbol: symbol,
		Month:  monthKey,
		Year:   year,
		High:   high,
		Low:    low,
		Volume: volume,
	}, nil
}
