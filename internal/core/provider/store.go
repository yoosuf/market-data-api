package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const providerColumns = `id, name, kind, base_url, api_key, priority, enabled, timeout_ms, created_at, updated_at`

const providerSchema = `
CREATE TABLE IF NOT EXISTS providers (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    kind       TEXT    NOT NULL,
    base_url   TEXT    NOT NULL,
    api_key    TEXT    NOT NULL DEFAULT '',
    priority   INTEGER NOT NULL DEFAULT 100,
    enabled    INTEGER NOT NULL DEFAULT 1,
    timeout_ms INTEGER NOT NULL DEFAULT 10000,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_providers_enabled_priority
    ON providers (enabled, priority, id);
`

// Init creates this module's tables. Safe to run on every start.
func Init(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, providerSchema)
	if err != nil {
		return fmt.Errorf("init provider schema: %w", err)
	}
	return nil
}

// Store is the SQLite-backed persistence adapter for providers.
type Store struct {
	db *sql.DB
}

// New wraps an open database handle.
func New(db *sql.DB) *Store { return &Store{db: db} }

// EnabledProviders returns the providers to try, in the order to try them.
// Ties on priority break by id, so the order is stable rather than whatever
// the query planner returns today.
func (s *Store) EnabledProviders(ctx context.Context) ([]Config, error) {
	q := `SELECT ` + providerColumns + ` FROM providers WHERE enabled = 1 ORDER BY priority ASC, id ASC`
	return s.queryProviders(ctx, q)
}

// ListProviders returns every configured provider, enabled or not.
func (s *Store) ListProviders(ctx context.Context) ([]Config, error) {
	q := `SELECT ` + providerColumns + ` FROM providers ORDER BY priority ASC, id ASC`
	return s.queryProviders(ctx, q)
}

// GetProvider returns one provider by id.
func (s *Store) GetProvider(ctx context.Context, id int64) (Config, error) {
	q := `SELECT ` + providerColumns + ` FROM providers WHERE id = ?`
	cfg, err := scanProvider(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Config{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	return cfg, err
}

// CreateProvider inserts a provider and returns it with its assigned id.
func (s *Store) CreateProvider(ctx context.Context, cfg Config, at time.Time) (Config, error) {
	const q = `
		INSERT INTO providers (name, kind, base_url, api_key, priority, enabled, timeout_ms, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	ts := at.Unix()
	res, err := s.db.ExecContext(ctx, q,
		cfg.Name, cfg.Kind, cfg.BaseURL, cfg.APIKey, cfg.Priority,
		boolToInt(cfg.Enabled), cfg.Timeout.Milliseconds(), ts, ts)
	if err != nil {
		if isUniqueViolation(err) {
			return Config{}, fmt.Errorf("%w: %q", ErrDuplicateName, cfg.Name)
		}
		return Config{}, fmt.Errorf("create provider: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Config{}, fmt.Errorf("create provider: %w", err)
	}

	cfg.ID = id
	cfg.CreatedAt = time.Unix(ts, 0).UTC()
	cfg.UpdatedAt = cfg.CreatedAt
	return cfg, nil
}

// UpdateProvider replaces every mutable field of an existing provider.
//
// updated_at advances on every write, which is also how a running Chain
// notices that it must rebuild the adapter.
func (s *Store) UpdateProvider(ctx context.Context, cfg Config, at time.Time) (Config, error) {
	const q = `
		UPDATE providers
		SET name = ?, kind = ?, base_url = ?, api_key = ?, priority = ?,
		    enabled = ?, timeout_ms = ?, updated_at = ?
		WHERE id = ?`

	ts := at.Unix()
	res, err := s.db.ExecContext(ctx, q,
		cfg.Name, cfg.Kind, cfg.BaseURL, cfg.APIKey, cfg.Priority,
		boolToInt(cfg.Enabled), cfg.Timeout.Milliseconds(), ts, cfg.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return Config{}, fmt.Errorf("%w: %q", ErrDuplicateName, cfg.Name)
		}
		return Config{}, fmt.Errorf("update provider %d: %w", cfg.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Config{}, fmt.Errorf("update provider %d: %w", cfg.ID, err)
	}
	if n == 0 {
		return Config{}, fmt.Errorf("%w: id %d", ErrNotFound, cfg.ID)
	}
	return s.GetProvider(ctx, cfg.ID)
}

// DeleteProvider removes a provider. Deleting the last one leaves the API
// able to serve cached data and unable to fetch anything new.
func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete provider %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete provider %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	return nil
}

func (s *Store) queryProviders(ctx context.Context, q string, args ...any) ([]Config, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Config
	for rows.Next() {
		cfg, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	return out, nil
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProvider(sc scanner) (Config, error) {
	var (
		cfg       Config
		enabled   int
		timeoutMs int64
		created   int64
		updated   int64
	)
	err := sc.Scan(&cfg.ID, &cfg.Name, &cfg.Kind, &cfg.BaseURL, &cfg.APIKey,
		&cfg.Priority, &enabled, &timeoutMs, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Config{}, err
		}
		return Config{}, fmt.Errorf("scan provider: %w", err)
	}
	cfg.Enabled = enabled != 0
	cfg.Timeout = time.Duration(timeoutMs) * time.Millisecond
	cfg.CreatedAt = time.Unix(created, 0).UTC()
	cfg.UpdatedAt = time.Unix(updated, 0).UTC()
	return cfg, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation recognizes a UNIQUE constraint failure without importing
// the driver's error types, keeping the driver swappable.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}
