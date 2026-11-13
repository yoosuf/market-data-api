// Package annual owns the cache-aside workflow and its persistence.
package annual

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

const annualSchema = `
CREATE TABLE IF NOT EXISTS monthly_prices (
    symbol     TEXT    NOT NULL,
    month      TEXT    NOT NULL,
    year       INTEGER NOT NULL,
    high       INTEGER NOT NULL,
    low        INTEGER NOT NULL,
    volume     INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (symbol, month)
);

CREATE INDEX IF NOT EXISTS idx_monthly_prices_symbol_year
    ON monthly_prices (symbol, year);

CREATE TABLE IF NOT EXISTS symbol_imports (
    symbol          TEXT PRIMARY KEY,
    last_success_at INTEGER,
    last_status     TEXT    NOT NULL,
    updated_at      INTEGER NOT NULL
);
`

// Init creates this module's tables. Safe to run on every start.
func Init(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, annualSchema)
	if err != nil {
		return fmt.Errorf("init annual schema: %w", err)
	}
	return nil
}

// Store is the SQLite-backed persistence adapter for annual market data.
type SQLiteStore struct {
	db *sql.DB
}

// New wraps an open database handle.
func NewSQLite(db *sql.DB) *SQLiteStore { return &SQLiteStore{db: db} }

// AnnualSummary aggregates one symbol-year entirely in SQL.
func (s *SQLiteStore) AnnualSummary(ctx context.Context, symbol string, year int) (market.AnnualSummary, bool, error) {
	const q = `
		SELECT MAX(high), MIN(low), SUM(volume), COUNT(*)
		FROM monthly_prices
		WHERE symbol = ? AND year = ?`

	var high, low, volume sql.NullInt64
	var months int
	err := s.db.QueryRowContext(ctx, q, symbol, year).Scan(&high, &low, &volume, &months)
	if err != nil {
		return market.AnnualSummary{}, false, fmt.Errorf("aggregate %s %d: %w", symbol, year, err)
	}
	if months == 0 || !high.Valid || !low.Valid || !volume.Valid {
		return market.AnnualSummary{}, false, nil
	}
	return market.AnnualSummary{
		Symbol: symbol,
		Year:   year,
		High:   market.Price(high.Int64),
		Low:    market.Price(low.Int64),
		Volume: volume.Int64,
		Months: months,
	}, true, nil
}

// ImportMonthly upserts a batch of monthly records and records a successful
// import for the symbol, in one transaction.
func (s *SQLiteStore) ImportMonthly(ctx context.Context, symbol string, recs []market.MonthlyRecord, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const upsert = `
		INSERT INTO monthly_prices (symbol, month, year, high, low, volume, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(symbol, month) DO UPDATE SET
			year       = excluded.year,
			high       = excluded.high,
			low        = excluded.low,
			volume     = excluded.volume,
			updated_at = excluded.updated_at`

	stmt, err := tx.PrepareContext(ctx, upsert)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	ts := at.Unix()
	for _, r := range recs {
		if r.Symbol != symbol {
			return fmt.Errorf("record symbol %q does not match import symbol %q", r.Symbol, symbol)
		}
		if _, err := stmt.ExecContext(ctx, r.Symbol, r.Month, r.Year, int64(r.High), int64(r.Low), r.Volume, ts); err != nil {
			return fmt.Errorf("upsert %s %s: %w", r.Symbol, r.Month, err)
		}
	}

	if err := markImport(ctx, tx, symbol, "success", &ts, ts); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import: %w", err)
	}
	return nil
}

// RecordImportFailure notes a failed attempt without touching last_success_at.
func (s *SQLiteStore) RecordImportFailure(ctx context.Context, symbol, status string, at time.Time) error {
	return markImport(ctx, s.db, symbol, status, nil, at.Unix())
}

// ImportState reports what is known about past fetches for a symbol.
func (s *SQLiteStore) ImportState(ctx context.Context, symbol string) (market.ImportState, bool, error) {
	const q = `SELECT last_success_at, last_status FROM symbol_imports WHERE symbol = ?`

	var successAt sql.NullInt64
	var status string
	err := s.db.QueryRowContext(ctx, q, symbol).Scan(&successAt, &status)
	switch {
	case err == sql.ErrNoRows:
		return market.ImportState{}, false, nil
	case err != nil:
		return market.ImportState{}, false, fmt.Errorf("import state %s: %w", symbol, err)
	}
	st := market.ImportState{Symbol: symbol, LastStatus: status}
	if successAt.Valid {
		st.LastSuccessAt = time.Unix(successAt.Int64, 0).UTC()
	}
	return st, true, nil
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func markImport(ctx context.Context, e execer, symbol, status string, successAt *int64, now int64) error {
	const q = `
		INSERT INTO symbol_imports (symbol, last_success_at, last_status, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(symbol) DO UPDATE SET
			last_success_at = COALESCE(excluded.last_success_at, symbol_imports.last_success_at),
			last_status     = excluded.last_status,
			updated_at      = excluded.updated_at`

	var sa any
	if successAt != nil {
		sa = *successAt
	}
	if _, err := e.ExecContext(ctx, q, symbol, sa, status, now); err != nil {
		return fmt.Errorf("record import state %s: %w", symbol, err)
	}
	return nil
}
