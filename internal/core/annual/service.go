// Package annual owns the cache-aside workflow: read SQLite, fetch only on a
// miss, import, then re-run the same aggregation. No SQL string, provider JSON
// key, or HTTP status code appears here.
package annual

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// Store is the persistence capability this workflow needs. It is declared
// here, at the point of use, rather than exported by the adapter.
type Store interface {
	AnnualSummary(ctx context.Context, symbol string, year int) (market.AnnualSummary, bool, error)
	ImportState(ctx context.Context, symbol string) (market.ImportState, bool, error)
	ImportMonthly(ctx context.Context, symbol string, recs []market.MonthlyRecord, at time.Time) error
	RecordImportFailure(ctx context.Context, symbol, status string, at time.Time) error
}

// Provider is the upstream capability this workflow needs.
type Provider interface {
	MonthlyHistory(ctx context.Context, symbol string) ([]market.MonthlyRecord, error)
}

// Service resolves annual aggregates, fetching upstream only when the cache
// cannot answer.
type Service struct {
	store    Store
	provider Provider
	log      *slog.Logger

	// negativeTTL is how long a successful fetch that produced no rows for a
	// year is trusted, so a bad symbol costs one upstream call per window
	// rather than one per request.
	negativeTTL time.Duration
	// currentYearTTL is how stale cached current-year data may be before a
	// refresh is attempted. The current year is partial by definition.
	currentYearTTL time.Duration

	now func() time.Time
}

// Config carries the knobs; zero values fall back to the documented defaults.
type Config struct {
	NegativeTTL    time.Duration
	CurrentYearTTL time.Duration
	Now            func() time.Time
	Logger         *slog.Logger
}

// New wires a service. Dependencies are explicit; there is no container and no
// package-level state.
func New(store Store, provider Provider, cfg Config) *Service {
	s := &Service{
		store:          store,
		provider:       provider,
		log:            cfg.Logger,
		negativeTTL:    cfg.NegativeTTL,
		currentYearTTL: cfg.CurrentYearTTL,
		now:            cfg.Now,
	}
	if s.negativeTTL == 0 {
		s.negativeTTL = 24 * time.Hour
	}
	if s.currentYearTTL == 0 {
		s.currentYearTTL = 24 * time.Hour
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// AnnualSummary returns the aggregate for a symbol and year.
//
// Order of operations, and why:
//  1. Read SQLite. A hit returns without touching the network, except for a
//     stale current year, which is partial by definition.
//  2. On a miss, consult the import state. A recent successful fetch that
//     produced nothing for this year is remembered, so repeated requests for a
//     nonexistent symbol do not burn quota. A past *failure* is never treated
//     as absence.
//  3. Fetch, import, and re-run the same aggregation query rather than
//     aggregating the fetched slice in Go, so one code path defines the answer.
func (s *Service) AnnualSummary(ctx context.Context, symbol string, year int) (market.AnnualSummary, error) {
	summary, found, err := s.store.AnnualSummary(ctx, symbol, year)
	if err != nil {
		return market.AnnualSummary{}, err
	}

	// A completed year that is already cached needs nothing else: skip the
	// import-state lookup rather than paying for a query on the hot path.
	if found && year != s.now().Year() {
		return summary, nil
	}

	state, hasState, err := s.store.ImportState(ctx, symbol)
	if err != nil {
		return market.AnnualSummary{}, err
	}

	if found {
		if !s.currentYearIsStale(year, state) {
			return summary, nil
		}
		// Partial year, gone stale: try to refresh, but a provider outage must
		// not take away data we already hold.
		if err := s.refresh(ctx, symbol); err != nil {
			s.log.WarnContext(ctx, "serving stale current-year data",
				"symbol", symbol, "year", year, "reason", err)
			return summary, nil
		}
		refreshed, ok, err := s.store.AnnualSummary(ctx, symbol, year)
		if err != nil {
			return market.AnnualSummary{}, err
		}
		if !ok {
			return summary, nil
		}
		return refreshed, nil
	}

	// Cache miss. Distinguish "never successfully fetched" from "fetched, and
	// this symbol-year is genuinely empty".
	if hasState && !state.LastSuccessAt.IsZero() &&
		s.now().Sub(state.LastSuccessAt) < s.negativeTTL {
		return market.AnnualSummary{}, market.ErrNoData
	}

	if err := s.refresh(ctx, symbol); err != nil {
		return market.AnnualSummary{}, err
	}

	summary, found, err = s.store.AnnualSummary(ctx, symbol, year)
	if err != nil {
		return market.AnnualSummary{}, err
	}
	if !found {
		// The fetch succeeded and still produced nothing for this year: the
		// year predates the listing, follows a delisting, or has no history.
		return market.AnnualSummary{}, market.ErrNoData
	}
	return summary, nil
}

// refresh fetches and persists full history for a symbol.
//
// The provider call deliberately happens outside any write transaction: a
// SQLite write lock held across a network round trip would block every other
// writer for the duration of the request.
func (s *Service) refresh(ctx context.Context, symbol string) error {
	recs, err := s.provider.MonthlyHistory(ctx, symbol)
	if err != nil {
		if errors.Is(err, market.ErrNoData) {
			// The provider answered and told us this symbol does not exist.
			// That is a determination, not a failure: record it as a
			// successful import of zero months so repeated requests for a
			// bogus symbol cost one upstream call per TTL rather than one
			// call each.
			if ierr := s.store.ImportMonthly(ctx, symbol, nil, s.now()); ierr != nil {
				s.log.ErrorContext(ctx, "recording empty import", "symbol", symbol, "error", ierr)
			}
			return err
		}
		// Anything else is a failure, and must not create a negative cache
		// entry: the store only advances last_success_at on success.
		if ferr := s.store.RecordImportFailure(ctx, symbol, "failed", s.now()); ferr != nil {
			s.log.ErrorContext(ctx, "recording import failure", "symbol", symbol, "error", ferr)
		}
		return err
	}
	if err := s.store.ImportMonthly(ctx, symbol, recs, s.now()); err != nil {
		return fmt.Errorf("import %s: %w", symbol, err)
	}
	return nil
}

// currentYearIsStale reports whether a cached current-year aggregate is old
// enough to be worth refreshing. Completed years never expire: their history
// cannot change.
func (s *Service) currentYearIsStale(year int, state market.ImportState) bool {
	now := s.now()
	if year != now.Year() {
		return false
	}
	if state.LastSuccessAt.IsZero() {
		return true
	}
	return now.Sub(state.LastSuccessAt) >= s.currentYearTTL
}
