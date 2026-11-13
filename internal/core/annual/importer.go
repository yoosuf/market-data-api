package annual

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// ImportStore is the narrow write capability the refresh command needs.
//
// It deliberately excludes AnnualSummary: the refresh job exists to fetch
// fresh history, and must not be able to call a read path that can return
// satisfied without contacting the provider.
type ImportStore interface {
	ImportMonthly(ctx context.Context, symbol string, recs []market.MonthlyRecord, at time.Time) error
	RecordImportFailure(ctx context.Context, symbol, status string, at time.Time) error
}

// Importer fetches and persists history for a symbol, unconditionally.
type Importer struct {
	store    ImportStore
	provider Provider
	now      func() time.Time
}

// NewImporter wires the one-shot import path.
func NewImporter(store ImportStore, provider Provider, now func() time.Time) *Importer {
	if now == nil {
		now = time.Now
	}
	return &Importer{store: store, provider: provider, now: now}
}

// Import always contacts the provider. It reports how many months were
// persisted so a caller can log progress.
func (i *Importer) Import(ctx context.Context, symbol string) (int, error) {
	recs, err := i.provider.MonthlyHistory(ctx, symbol)
	if err != nil {
		// A definitive "no such symbol" is a determination, not a failure, and
		// is recorded the same way the serving path records it: a successful
		// import of zero months. Whichever path discovers it, the negative
		// cache behaves identically.
		if errors.Is(err, market.ErrNoData) {
			if ierr := i.store.ImportMonthly(ctx, symbol, nil, i.now()); ierr != nil {
				return 0, fmt.Errorf("%w (and recording it failed: %v)", err, ierr)
			}
			return 0, err
		}
		if ferr := i.store.RecordImportFailure(ctx, symbol, "failed", i.now()); ferr != nil {
			return 0, fmt.Errorf("%w (and recording it failed: %v)", err, ferr)
		}
		return 0, err
	}
	if err := i.store.ImportMonthly(ctx, symbol, recs, i.now()); err != nil {
		return 0, err
	}
	return len(recs), nil
}
