// Command api serves the annual market data endpoint.
//
// All dependencies are constructed here and passed down explicitly: there is
// no container, no service locator, and no package-level mutable state.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/core/provider"
	"github.com/yoosuf/market-data-api/internal/infra/graphqlapi"
	"github.com/yoosuf/market-data-api/internal/infra/httpapi"
	"github.com/yoosuf/market-data-api/internal/infra/sqlite"
)

type config struct {
	addr   string
	dbPath string
	// adminEnabled mounts the provider CRUD routes.
	adminEnabled bool
}

func loadConfig() (config, error) {
	cfg := config{
		addr:         env("ADDR", ":8080"),
		dbPath:       env("DB_PATH", "./market.db"),
		adminEnabled: true,
	}
	if v := os.Getenv("PROVIDER_ADMIN_API"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			// A typo here would silently leave an unauthenticated admin
			// surface mounted, so it is a startup failure rather than a
			// fallback to the default.
			return config{}, fmt.Errorf("PROVIDER_ADMIN_API must be a boolean, got %q", v)
		}
		cfg.adminEnabled = enabled
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := annual.Init(initCtx, db); err != nil {
		return err
	}
	if err := provider.Init(initCtx, db); err != nil {
		return err
	}
	annualStore := annual.NewSQLite(db)
	providerStore := provider.New(db)

	// Provider configuration lives in the database, managed over
	// /admin/providers. Nothing about an upstream is read from the
	// environment.
	registry := provider.DefaultRegistry()
	configured, err := providerStore.EnabledProviders(initCtx)
	if err != nil {
		return err
	}
	if len(configured) == 0 {
		// Not fatal: cached data is still servable, and an operator can add a
		// provider through the admin API without a restart.
		log.Warn("no enabled providers configured; cache misses will fail until one is added via POST /admin/providers",
			"admin_api", cfg.adminEnabled)
	}
	chain := provider.NewChain(providerStore, registry, log)

	svc := annual.New(annualStore, chain, annual.Config{Logger: log})

	// Two transports over one service: REST and GraphQL resolve through the
	// same cache-aside workflow, so their behavior cannot drift.
	gql, err := graphqlapi.New(svc, log, time.Now)
	if err != nil {
		return fmt.Errorf("build graphql schema: %w", err)
	}
	var restOpts []httpapi.Option
	if cfg.adminEnabled {
		restOpts = append(restOpts, httpapi.WithProviderAdmin(providerStore, registry))
	}

	mux := http.NewServeMux()
	mux.Handle("GET /graphql", gql)
	mux.Handle("POST /graphql", gql)
	mux.Handle("/", httpapi.New(svc, log, time.Now, restOpts...))
	handler := mux

	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: handler,
		// Explicit timeouts: http.ListenAndServe sets none, which lets a
		// single slow client hold a connection indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.addr, "database", cfg.dbPath,
			"providers", len(configured), "admin_api", cfg.adminEnabled)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh
	}
}
