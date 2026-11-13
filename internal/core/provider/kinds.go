package provider

import (
	"net/http"

	"github.com/yoosuf/market-data-api/internal/core/provider/alphavantage"
)

// KindAlphaVantage is the `kind` value selecting the Alpha Vantage adapter.
const KindAlphaVantage = "alphavantage"

// DefaultRegistry returns the kinds this build can talk to.
//
// Adding a provider with a different payload shape means writing an adapter
// and registering it here — one file and one line. What a stored row cannot
// do is describe an unknown JSON shape; mapping a payload to domain records
// is code, and keeping it as code is what lets it be tested.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(KindAlphaVantage, func(cfg Config) (Fetcher, error) {
		return alphavantage.New(cfg.APIKey,
			alphavantage.WithBaseURL(cfg.BaseURL),
			alphavantage.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
		), nil
	})
	return r
}
