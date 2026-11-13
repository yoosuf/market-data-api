// Package sqlite opens the SQLite database. Schema ownership lives in each
// module, which calls its own Init against the handle returned here.
package sqlite

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

// Open opens the SQLite database at path with the pragmas this service needs.
func Open(path string) (*sql.DB, error) {
	dsn := (&url.URL{
		Scheme: "file",
		Opaque: (&url.URL{Path: path}).EscapedPath(),
		RawQuery: "_pragma=busy_timeout(5000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=foreign_keys(on)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}
