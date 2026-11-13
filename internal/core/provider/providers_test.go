package provider_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/infra/sqlite"

	"github.com/yoosuf/market-data-api/internal/core/provider"
)

func newStore(t *testing.T) *provider.Store {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := provider.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	return provider.New(db)
}

func sampleConfig(name string, priority int) provider.Config {
	return provider.Config{
		Name:     name,
		Kind:     "alphavantage",
		BaseURL:  "https://example.test/query",
		APIKey:   "key-" + name,
		Priority: priority,
		Enabled:  true,
		Timeout:  10 * time.Second,
	}
}

func TestProviderCRUD(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	created, err := s.CreateProvider(ctx, sampleConfig("primary", 10), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("create did not assign an id")
	}
	if !created.CreatedAt.Equal(now) || !created.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v / %v", created.CreatedAt, created.UpdatedAt)
	}

	got, err := s.GetProvider(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "primary" || got.APIKey != "key-primary" || got.Timeout != 10*time.Second {
		t.Errorf("round trip lost data: %+v", got)
	}
	if !got.Enabled {
		t.Error("enabled flag lost")
	}

	// Update must change updated_at, which is what invalidates a running
	// chain's cached adapter.
	later := now.Add(time.Hour)
	got.BaseURL = "https://replacement.test/query"
	got.Priority = 5
	got.Enabled = false
	updated, err := s.UpdateProvider(ctx, got, later)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.BaseURL != "https://replacement.test/query" || updated.Priority != 5 || updated.Enabled {
		t.Errorf("update not applied: %+v", updated)
	}
	if !updated.UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %v, want %v", updated.UpdatedAt, later)
	}
	if !updated.CreatedAt.Equal(now) {
		t.Errorf("created_at changed to %v", updated.CreatedAt)
	}

	if err := s.DeleteProvider(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetProvider(ctx, created.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
}

func TestProviderNotFound(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.GetProvider(ctx, 999); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("get: %v", err)
	}
	if err := s.DeleteProvider(ctx, 999); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("delete: %v", err)
	}
	cfg := sampleConfig("ghost", 1)
	cfg.ID = 999
	if _, err := s.UpdateProvider(ctx, cfg, time.Now()); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("update: %v", err)
	}
}

func TestProviderNamesAreUnique(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()

	if _, err := s.CreateProvider(ctx, sampleConfig("dup", 1), now); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateProvider(ctx, sampleConfig("dup", 2), now); !errors.Is(err, provider.ErrDuplicateName) {
		t.Fatalf("second create: %v, want ErrDuplicateName", err)
	}
}

// The chain depends on this order, so it is the store's job to guarantee it.
func TestEnabledProvidersAreOrderedByPriority(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()

	for _, c := range []provider.Config{
		sampleConfig("third", 30),
		sampleConfig("first", 10),
		sampleConfig("second", 20),
	} {
		if _, err := s.CreateProvider(ctx, c, now); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
	}
	disabled := sampleConfig("disabled", 1) // lowest priority number, but off
	disabled.Enabled = false
	if _, err := s.CreateProvider(ctx, disabled, now); err != nil {
		t.Fatalf("create disabled: %v", err)
	}

	got, err := s.EnabledProviders(ctx)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("got %d providers, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d = %s, want %s", i, got[i].Name, name)
		}
	}

	all, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("ListProviders returned %d, want 4 including the disabled one", len(all))
	}
}
