// Package markettest holds fixtures shared by the package tests.
//
// The fake provider lives here rather than being rewritten in each test
// package so that one definition carries the adapter contract: it either
// returns history, or returns a classified error. A fake that reported an
// empty success where the real adapter errors would quietly disable the
// tests that separate absence from unavailability, which is the distinction
// most of this codebase is built around.
package markettest

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/core/provider"
	"github.com/yoosuf/market-data-api/internal/infra/sqlite"
	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// Provider is a fake upstream whose behavior tests can change mid-run.
// It is safe for concurrent use, so it can back race-detector tests.
type Provider struct {
	mu      sync.Mutex
	calls   int
	records []market.MonthlyRecord
	err     error
	delay   time.Duration
}

// NewProvider returns a provider that serves recs.
func NewProvider(recs []market.MonthlyRecord) *Provider {
	return &Provider{records: recs}
}

// FailingProvider returns a provider that always fails with err.
func FailingProvider(err error) *Provider {
	return &Provider{err: err}
}

// WithDelay makes each call take d, for exercising concurrent misses.
func (p *Provider) WithDelay(d time.Duration) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delay = d
	return p
}

// MonthlyHistory satisfies the provider contract.
func (p *Provider) MonthlyHistory(ctx context.Context, symbol string) ([]market.MonthlyRecord, error) {
	p.mu.Lock()
	p.calls++
	delay, err := p.delay, p.err
	records := append([]market.MonthlyRecord(nil), p.records...)
	p.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	for i := range records {
		records[i].Symbol = symbol
	}
	return records, nil
}

// Calls reports how many times the provider was contacted. Cache behavior is
// asserted through this rather than through internal state.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// SetError makes subsequent calls fail, or succeed again when passed nil.
func (p *Provider) SetError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// SetRecords replaces the history served by subsequent calls.
func (p *Provider) SetRecords(recs []market.MonthlyRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = recs
}

// Records returns the history currently served.
func (p *Provider) Records() []market.MonthlyRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]market.MonthlyRecord(nil), p.records...)
}

// MustPrice parses a decimal price or fails the test.
func MustPrice(t *testing.T, s string) market.Price {
	t.Helper()
	p, err := market.ParsePrice(s)
	if err != nil {
		t.Fatalf("ParsePrice(%q): %v", s, err)
	}
	return p
}

// History is two months of 2023 plus one of 2022, so tests can show that one
// fetch serves several years.
func History(t *testing.T) []market.MonthlyRecord {
	t.Helper()
	return []market.MonthlyRecord{
		{Month: "2023-01", Year: 2023, High: MustPrice(t, "147.1800"), Low: MustPrice(t, "140.2400"), Volume: 70524849},
		{Month: "2023-02", Year: 2023, High: MustPrice(t, "153.2100"), Low: MustPrice(t, "129.0000"), Volume: 62162595},
		{Month: "2022-06", Year: 2022, High: MustPrice(t, "99.0000"), Low: MustPrice(t, "1.0000"), Volume: 50},
	}
}

// NewStore opens an initialized store on a real database file in the test's
// temporary directory, so WAL and locking behave as they do in production.
func NewStore(t *testing.T) *annual.SQLiteStore {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := annual.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := provider.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	return annual.NewSQLite(db)
}

// Now returns a fixed clock, so year-relative behavior is deterministic.
func Now() func() time.Time {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}
