// Command refresh warms the SQLite cache by fetching monthly history for a
// configured symbol list. It is optional: the API works correctly whether or
// not this ever runs.
//
// Run it manually or from cron. It is a one-shot process, so it carries no
// scheduler of its own.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/core/provider"
	"github.com/yoosuf/market-data-api/internal/infra/sqlite"
	"github.com/yoosuf/market-data-api/internal/shared/market"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("refresh failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./market.db"
	}
	symbols, err := parseSymbols(os.Getenv("REFRESH_SYMBOLS"))
	if err != nil {
		return err
	}
	timeout := 2 * time.Minute
	if v := os.Getenv("REFRESH_TIMEOUT_SECONDS"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs <= 0 {
			return errors.New("REFRESH_TIMEOUT_SECONDS must be a positive integer")
		}
		timeout = time.Duration(secs) * time.Second
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	db, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := annual.Init(ctx, db); err != nil {
		return err
	}
	if err := provider.Init(ctx, db); err != nil {
		return err
	}
	annualStore := annual.NewSQLite(db)
	providerStore := provider.New(db)

	// Providers come from the same database the API reads, so the job cannot
	// drift onto a different upstream than the one being served.
	configured, err := providerStore.EnabledProviders(ctx)
	if err != nil {
		return err
	}
	if len(configured) == 0 {
		return errors.New("no enabled providers configured; add one via POST /admin/providers")
	}
	chain := provider.NewChain(providerStore, provider.DefaultRegistry(), log)

	// The importer takes the narrow write interface: this command must not be
	// able to call the annual read path, which can return without fetching.
	importer := annual.NewImporter(annualStore, chain, time.Now)

	var failed []string
	for _, symbol := range symbols {
		// One fetch per symbol per run. The provider returns full history, so
		// fetching per year would multiply quota use for no extra data.
		months, err := importer.Import(ctx, symbol)
		if err != nil {
			// Keep going: one bad symbol or one rate-limited call should not
			// deny the remaining symbols their refresh. Already-cached data
			// for this symbol is left untouched and remains servable.
			log.Error("symbol refresh failed", "symbol", symbol, "error", err)
			failed = append(failed, symbol)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		log.Info("symbol refreshed", "symbol", symbol, "months", months)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d of %d symbols failed: %s",
			len(failed), len(symbols), strings.Join(failed, ", "))
	}
	log.Info("refresh complete", "symbols", len(symbols))
	return nil
}

// parseSymbols reads the configurable symbol list, defaulting to a small set.
func parseSymbols(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "IBM,AAPL,MSFT"
	}
	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		sym, err := market.NormalizeSymbol(part)
		if err != nil {
			return nil, fmt.Errorf("REFRESH_SYMBOLS contains an invalid symbol %q", strings.TrimSpace(part))
		}
		if seen[sym] {
			continue // a duplicate would spend quota twice for the same data
		}
		seen[sym] = true
		out = append(out, sym)
	}
	if len(out) == 0 {
		return nil, errors.New("REFRESH_SYMBOLS contains no usable symbols")
	}
	return out, nil
}
