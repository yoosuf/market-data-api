package annual_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/infra/sqlite"
	"github.com/yoosuf/market-data-api/internal/shared/market"
)

func newStore(t *testing.T) *annual.SQLiteStore {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := annual.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	return annual.NewSQLite(db)
}

func price(t *testing.T, s string) market.Price {
	t.Helper()
	p, err := market.ParsePrice(s)
	if err != nil {
		t.Fatalf("ParsePrice(%q): %v", s, err)
	}
	return p
}

func rec(t *testing.T, symbol, month, high, low string, volume int64) market.MonthlyRecord {
	t.Helper()
	m, year, err := market.MonthKey(month + "-01")
	if err != nil {
		t.Fatalf("MonthKey: %v", err)
	}
	return market.MonthlyRecord{
		Symbol: symbol, Month: m, Year: year,
		High: price(t, high), Low: price(t, low), Volume: volume,
	}
}

func TestInitIsIdempotent(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := annual.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := annual.Init(context.Background(), db); err != nil {
		t.Fatalf("second Init: %v", err)
	}
}

func TestAggregationAcrossMonths(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// "9.9000" sorts after "10.0000" as text; as scaled integers it must not.
	recs := []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "9.9000", 100),
		rec(t, "IBM", "2023-02", "9.9000", "2.5000", 200),
		rec(t, "IBM", "2023-03", "120.5000", "8.0000", 300),
	}
	if err := s.ImportMonthly(ctx, "IBM", recs, time.Now()); err != nil {
		t.Fatalf("import: %v", err)
	}

	got, found, err := s.AnnualSummary(ctx, "IBM", 2023)
	if err != nil || !found {
		t.Fatalf("summary: found=%v err=%v", found, err)
	}
	if got.High.String() != "120.5000" {
		t.Errorf("high = %s, want 120.5000", got.High)
	}
	if got.Low.String() != "2.5000" {
		t.Errorf("low = %s, want 2.5000", got.Low)
	}
	if got.Volume != 600 {
		t.Errorf("volume = %d, want 600", got.Volume)
	}
	if got.Months != 3 {
		t.Errorf("months = %d, want 3", got.Months)
	}
}

func TestYearIsolationAtBoundaries(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	recs := []market.MonthlyRecord{
		rec(t, "IBM", "2022-12", "999.0000", "1.0000", 1_000_000), // must not leak in
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 10),
		rec(t, "IBM", "2023-12", "20.0000", "4.0000", 20),
		rec(t, "IBM", "2024-01", "888.0000", "2.0000", 2_000_000), // nor this
	}
	if err := s.ImportMonthly(ctx, "IBM", recs, time.Now()); err != nil {
		t.Fatalf("import: %v", err)
	}

	got, found, err := s.AnnualSummary(ctx, "IBM", 2023)
	if err != nil || !found {
		t.Fatalf("summary: found=%v err=%v", found, err)
	}
	if got.High.String() != "20.0000" || got.Low.String() != "4.0000" || got.Volume != 30 {
		t.Fatalf("year boundary leak: high=%s low=%s volume=%d", got.High, got.Low, got.Volume)
	}
}

func TestSymbolIsolation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.ImportMonthly(ctx, "IBM", []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 10),
	}, time.Now()); err != nil {
		t.Fatalf("import IBM: %v", err)
	}
	if err := s.ImportMonthly(ctx, "AAPL", []market.MonthlyRecord{
		rec(t, "AAPL", "2023-01", "500.0000", "400.0000", 99),
	}, time.Now()); err != nil {
		t.Fatalf("import AAPL: %v", err)
	}

	got, _, err := s.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.High.String() != "10.0000" || got.Volume != 10 {
		t.Fatalf("symbol leak: high=%s volume=%d", got.High, got.Volume)
	}
}

// The requirement that a retry must not double-count volume.
func TestReimportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	recs := []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 100),
		rec(t, "IBM", "2023-02", "11.0000", "6.0000", 200),
	}
	for i := 0; i < 3; i++ {
		if err := s.ImportMonthly(ctx, "IBM", recs, time.Now()); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}

	got, _, err := s.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.Volume != 300 {
		t.Fatalf("volume = %d after 3 imports, want 300", got.Volume)
	}
	if got.Months != 2 {
		t.Fatalf("months = %d after 3 imports, want 2", got.Months)
	}
}

func TestReimportUpdatesRevisedValues(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.ImportMonthly(ctx, "IBM", []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 100),
	}, time.Now()); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// The provider revised the month: the upsert must replace, not accumulate.
	if err := s.ImportMonthly(ctx, "IBM", []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "12.0000", "4.0000", 150),
	}, time.Now()); err != nil {
		t.Fatalf("second import: %v", err)
	}

	got, _, err := s.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.High.String() != "12.0000" || got.Low.String() != "4.0000" || got.Volume != 150 {
		t.Fatalf("revision not applied: high=%s low=%s volume=%d", got.High, got.Low, got.Volume)
	}
}

func TestAggregateWithNoRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// MAX/MIN/SUM over zero rows return NULL, which must surface as not-found
	// rather than a zero-valued summary.
	got, found, err := s.AnnualSummary(ctx, "NOSUCH", 2023)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if found {
		t.Fatalf("found = true for empty table, summary = %+v", got)
	}
}

func TestSingleMonthYearIsValid(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// An IPO or delisting year legitimately has fewer than 12 months.
	if err := s.ImportMonthly(ctx, "NEW", []market.MonthlyRecord{
		rec(t, "NEW", "2023-11", "10.0000", "5.0000", 42),
	}, time.Now()); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, found, err := s.AnnualSummary(ctx, "NEW", 2023)
	if err != nil || !found {
		t.Fatalf("one-month year rejected: found=%v err=%v", found, err)
	}
	if got.Months != 1 || got.Volume != 42 {
		t.Fatalf("got months=%d volume=%d", got.Months, got.Volume)
	}
}

// A failed import must leave previously cached data exactly as it was.
func TestFailedImportRollsBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.ImportMonthly(ctx, "IBM", []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 100),
	}, time.Now()); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	// A batch whose second record belongs to another symbol: the whole
	// transaction must roll back.
	bad := []market.MonthlyRecord{
		rec(t, "IBM", "2023-02", "99.0000", "1.0000", 999),
		rec(t, "AAPL", "2023-03", "50.0000", "2.0000", 500),
	}
	if err := s.ImportMonthly(ctx, "IBM", bad, time.Now()); err == nil {
		t.Fatal("expected mixed-symbol import to fail")
	}

	got, found, err := s.AnnualSummary(ctx, "IBM", 2023)
	if err != nil || !found {
		t.Fatalf("summary: found=%v err=%v", found, err)
	}
	if got.Months != 1 || got.Volume != 100 || got.High.String() != "10.0000" {
		t.Fatalf("partial write survived rollback: months=%d volume=%d high=%s",
			got.Months, got.Volume, got.High)
	}
}

func TestImportStateLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, found, err := s.ImportState(ctx, "IBM"); err != nil || found {
		t.Fatalf("expected no state: found=%v err=%v", found, err)
	}

	// A failure must not create a success marker.
	if err := s.RecordImportFailure(ctx, "IBM", "failed", time.Now()); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	st, found, err := s.ImportState(ctx, "IBM")
	if err != nil || !found {
		t.Fatalf("state after failure: found=%v err=%v", found, err)
	}
	if !st.LastSuccessAt.IsZero() {
		t.Fatalf("failure set last_success_at to %v", st.LastSuccessAt)
	}

	at := time.Unix(1_700_000_000, 0).UTC()
	if err := s.ImportMonthly(ctx, "IBM", []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 1),
	}, at); err != nil {
		t.Fatalf("import: %v", err)
	}
	st, _, err = s.ImportState(ctx, "IBM")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.LastSuccessAt.Equal(at) {
		t.Fatalf("last_success_at = %v, want %v", st.LastSuccessAt, at)
	}

	// A later failure must not erase the earlier success.
	if err := s.RecordImportFailure(ctx, "IBM", "failed", time.Now()); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	st, _, err = s.ImportState(ctx, "IBM")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.LastSuccessAt.Equal(at) {
		t.Fatalf("failure erased success: last_success_at = %v", st.LastSuccessAt)
	}
	if st.LastStatus != "failed" {
		t.Fatalf("last_status = %q, want failed", st.LastStatus)
	}
}

// Parameterized SQL must treat a hostile symbol as data, not syntax.
func TestSymbolIsParameterized(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.ImportMonthly(ctx, "IBM", []market.MonthlyRecord{
		rec(t, "IBM", "2023-01", "10.0000", "5.0000", 100),
	}, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, found, err := s.AnnualSummary(ctx, "'; DROP TABLE monthly_prices; --", 2023); err != nil || found {
		t.Fatalf("injection attempt: found=%v err=%v", found, err)
	}
	if _, found, err := s.AnnualSummary(ctx, "IBM", 2023); err != nil || !found {
		t.Fatalf("table damaged by injection attempt: found=%v err=%v", found, err)
	}
}
