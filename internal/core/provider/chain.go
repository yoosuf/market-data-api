package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// ConfigStore supplies the providers to try, already ordered by priority.
// Declared here, at the point of use.
type ConfigStore interface {
	EnabledProviders(ctx context.Context) ([]Config, error)
}

// Chain tries each enabled provider in priority order until one answers.
//
// It satisfies the same contract as a single adapter, so the annual workflow
// is unchanged: it still depends on "something that can fetch monthly
// history", not on how many upstreams exist or which one replied.
type Chain struct {
	store    ConfigStore
	registry *Registry
	log      *slog.Logger

	mu    sync.Mutex
	built map[int64]builtFetcher
}

// builtFetcher caches an adapter so repeated requests reuse its HTTP
// connection pool. The stamp is the config's UpdatedAt: an edit through the
// CRUD API changes it, and the adapter is rebuilt on the next request without
// anyone restarting the process.
type builtFetcher struct {
	stamp   time.Time
	fetcher Fetcher
}

// NewChain wires a chain over stored configuration.
func NewChain(store ConfigStore, registry *Registry, log *slog.Logger) *Chain {
	if log == nil {
		log = slog.Default()
	}
	return &Chain{
		store:    store,
		registry: registry,
		log:      log,
		built:    make(map[int64]builtFetcher),
	}
}

// MonthlyHistory asks each enabled provider in turn.
//
// The classification rule is the important part. market.ErrNoData is only
// returned when **every** enabled provider answered and said the symbol does
// not exist. If any provider was merely unavailable, the chain reports
// unavailability instead — because an upstream that could not answer is not
// evidence of absence, and returning ErrNoData here would let the workflow
// cache a negative result it has not actually established.
func (c *Chain) MonthlyHistory(ctx context.Context, symbol string) ([]market.MonthlyRecord, error) {
	configs, err := c.store.EnabledProviders(ctx)
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("%w: no providers are configured", market.ErrUpstreamUnavailable)
	}

	var sawNoData, sawFailure bool
	for _, cfg := range configs {
		fetcher, err := c.fetcherFor(cfg)
		if err != nil {
			// A row naming a kind this build does not know, or a config the
			// adapter rejects. Operator error, not an outage, but it still
			// means this provider cannot answer.
			c.log.ErrorContext(ctx, "provider unusable",
				"provider", cfg.Name, "kind", cfg.Kind, "error", err)
			sawFailure = true
			continue
		}

		recs, err := fetcher.MonthlyHistory(ctx, symbol)
		switch {
		case err == nil:
			return recs, nil

		case errors.Is(err, market.ErrNoData):
			// This provider answered: it has no such symbol. Another might.
			sawNoData = true
			c.log.DebugContext(ctx, "provider reports no data",
				"provider", cfg.Name, "symbol", symbol)

		case ctx.Err() != nil:
			// The caller gave up; trying the next provider would be pointless
			// and would mask the cancellation.
			return nil, err

		default:
			sawFailure = true
			c.log.WarnContext(ctx, "provider failed, trying next",
				"provider", cfg.Name, "symbol", symbol, "error", err)
		}
	}

	switch {
	case sawFailure:
		// At least one provider could not answer, so absence is unproven.
		return nil, fmt.Errorf("%w: all providers failed or reported no data",
			market.ErrUpstreamUnavailable)
	case sawNoData:
		// Every provider answered, and all agreed the symbol is unknown.
		return nil, market.ErrNoData
	default:
		return nil, fmt.Errorf("%w: no provider produced a result", market.ErrUpstreamUnavailable)
	}
}

// fetcherFor returns a cached adapter, rebuilding when the config changed.
func (c *Chain) fetcherFor(cfg Config) (Fetcher, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cached, ok := c.built[cfg.ID]; ok && cached.stamp.Equal(cfg.UpdatedAt) {
		return cached.fetcher, nil
	}
	f, err := c.registry.Build(cfg)
	if err != nil {
		delete(c.built, cfg.ID)
		return nil, err
	}
	c.built[cfg.ID] = builtFetcher{stamp: cfg.UpdatedAt, fetcher: f}
	return f, nil
}
