package provider_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/provider"
	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// stubFetcher honors the adapter contract: history, or a classified error.
type stubFetcher struct {
	mu      sync.Mutex
	calls   int
	records []market.MonthlyRecord
	err     error
}

func (s *stubFetcher) MonthlyHistory(_ context.Context, symbol string) ([]market.MonthlyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]market.MonthlyRecord, len(s.records))
	copy(out, s.records)
	for i := range out {
		out[i].Symbol = symbol
	}
	return out, nil
}

func (s *stubFetcher) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type staticStore struct {
	configs []provider.Config
	err     error
}

func (s staticStore) EnabledProviders(context.Context) ([]provider.Config, error) {
	return s.configs, s.err
}

func records(t *testing.T) []market.MonthlyRecord {
	t.Helper()
	high, err := market.ParsePrice("10.0000")
	if err != nil {
		t.Fatalf("ParsePrice: %v", err)
	}
	low, err := market.ParsePrice("5.0000")
	if err != nil {
		t.Fatalf("ParsePrice: %v", err)
	}
	return []market.MonthlyRecord{{Month: "2023-01", Year: 2023, High: high, Low: low, Volume: 100}}
}

// chainOf wires stub fetchers as kinds k0, k1, ... in the given order.
func chainOf(t *testing.T, stubs ...*stubFetcher) *provider.Chain {
	t.Helper()
	reg := provider.NewRegistry()
	var configs []provider.Config
	for i, s := range stubs {
		kind := "k" + string(rune('0'+i))
		stub := s
		reg.Register(kind, func(provider.Config) (provider.Fetcher, error) { return stub, nil })
		configs = append(configs, provider.Config{
			ID: int64(i + 1), Name: kind, Kind: kind,
			BaseURL: "https://example.test", Priority: i, Enabled: true,
			Timeout: time.Second, UpdatedAt: time.Unix(int64(i), 0),
		})
	}
	return provider.NewChain(staticStore{configs: configs}, reg, nil)
}

func TestFirstProviderWinsAndSecondIsNotCalled(t *testing.T) {
	first := &stubFetcher{records: records(t)}
	second := &stubFetcher{records: records(t)}
	chain := chainOf(t, first, second)

	recs, err := chain.MonthlyHistory(context.Background(), "IBM")
	if err != nil {
		t.Fatalf("MonthlyHistory: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if first.callCount() != 1 || second.callCount() != 0 {
		t.Fatalf("calls: first=%d second=%d, want 1 and 0", first.callCount(), second.callCount())
	}
}

func TestFailoverToNextProvider(t *testing.T) {
	first := &stubFetcher{err: market.ErrUpstreamUnavailable}
	second := &stubFetcher{records: records(t)}
	chain := chainOf(t, first, second)

	recs, err := chain.MonthlyHistory(context.Background(), "IBM")
	if err != nil {
		t.Fatalf("failover did not happen: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if second.callCount() != 1 {
		t.Fatal("second provider was never tried")
	}
}

// One provider not knowing a symbol says nothing about the others.
func TestNoDataFallsThroughToNextProvider(t *testing.T) {
	first := &stubFetcher{err: market.ErrNoData}
	second := &stubFetcher{records: records(t)}
	chain := chainOf(t, first, second)

	if _, err := chain.MonthlyHistory(context.Background(), "IBM"); err != nil {
		t.Fatalf("got %v, want success from the second provider", err)
	}
}

// Absence is only established when every provider actually answered.
func TestAllProvidersAgreeOnNoData(t *testing.T) {
	chain := chainOf(t,
		&stubFetcher{err: market.ErrNoData},
		&stubFetcher{err: market.ErrNoData})

	_, err := chain.MonthlyHistory(context.Background(), "NOSUCH")
	if !errors.Is(err, market.ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}
}

// The invariant that protects the negative cache: if any provider could not
// answer, the chain must not claim the symbol does not exist.
func TestOutageMixedWithNoDataIsUnavailable(t *testing.T) {
	chain := chainOf(t,
		&stubFetcher{err: market.ErrNoData},
		&stubFetcher{err: market.ErrUpstreamUnavailable})

	_, err := chain.MonthlyHistory(context.Background(), "IBM")
	if !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUpstreamUnavailable", err)
	}
	if errors.Is(err, market.ErrNoData) {
		t.Fatal("a partial outage was reported as proven absence")
	}
}

func TestAllProvidersFailing(t *testing.T) {
	chain := chainOf(t,
		&stubFetcher{err: market.ErrUpstreamUnavailable},
		&stubFetcher{err: market.ErrUpstreamPayload})

	_, err := chain.MonthlyHistory(context.Background(), "IBM")
	if !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUpstreamUnavailable", err)
	}
}

func TestNoProvidersConfigured(t *testing.T) {
	chain := provider.NewChain(staticStore{}, provider.NewRegistry(), nil)

	_, err := chain.MonthlyHistory(context.Background(), "IBM")
	if !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUpstreamUnavailable", err)
	}
	if errors.Is(err, market.ErrNoData) {
		t.Fatal("an unconfigured service must not claim the symbol does not exist")
	}
}

func TestDisabledProvidersAreNeverCalled(t *testing.T) {
	stub := &stubFetcher{records: records(t)}
	reg := provider.NewRegistry()
	reg.Register("k0", func(provider.Config) (provider.Fetcher, error) { return stub, nil })

	// EnabledProviders is the store's job; a chain over an empty list must
	// not reach for anything.
	chain := provider.NewChain(staticStore{}, reg, nil)
	if _, err := chain.MonthlyHistory(context.Background(), "IBM"); err == nil {
		t.Fatal("expected an error")
	}
	if stub.callCount() != 0 {
		t.Fatalf("disabled provider was called %d times", stub.callCount())
	}
}

// A row naming a kind this build cannot serve is operator error, not absence.
func TestUnknownKindIsUnavailableNotNoData(t *testing.T) {
	chain := provider.NewChain(staticStore{configs: []provider.Config{{
		ID: 1, Name: "mystery", Kind: "nosuchkind",
		BaseURL: "https://example.test", Enabled: true, Timeout: time.Second,
	}}}, provider.NewRegistry(), nil)

	_, err := chain.MonthlyHistory(context.Background(), "IBM")
	if !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUpstreamUnavailable", err)
	}
}

func TestCancelledContextStopsTheChain(t *testing.T) {
	first := &stubFetcher{err: context.Canceled}
	second := &stubFetcher{records: records(t)}
	chain := chainOf(t, first, second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := chain.MonthlyHistory(ctx, "IBM"); err == nil {
		t.Fatal("expected an error")
	}
	if second.callCount() != 0 {
		t.Fatal("chain kept trying after the caller gave up")
	}
}

// An edit through the CRUD API must take effect without a restart.
func TestAdapterIsRebuiltWhenConfigChanges(t *testing.T) {
	oldStub := &stubFetcher{err: market.ErrUpstreamUnavailable}
	newStub := &stubFetcher{records: records(t)}
	active := oldStub

	reg := provider.NewRegistry()
	reg.Register("k0", func(provider.Config) (provider.Fetcher, error) { return active, nil })

	cfg := provider.Config{
		ID: 1, Name: "one", Kind: "k0", BaseURL: "https://example.test",
		Enabled: true, Timeout: time.Second, UpdatedAt: time.Unix(1, 0),
	}
	store := &mutableStore{configs: []provider.Config{cfg}}
	chain := provider.NewChain(store, reg, nil)

	if _, err := chain.MonthlyHistory(context.Background(), "IBM"); err == nil {
		t.Fatal("expected the first adapter to fail")
	}

	// Same config: the cached adapter is reused, so the swap is not picked up.
	active = newStub
	if _, err := chain.MonthlyHistory(context.Background(), "IBM"); err == nil {
		t.Fatal("cached adapter should still be the failing one")
	}

	// An update bumps updated_at, which invalidates the cached adapter.
	store.configs[0].UpdatedAt = time.Unix(2, 0)
	if _, err := chain.MonthlyHistory(context.Background(), "IBM"); err != nil {
		t.Fatalf("adapter was not rebuilt after a config change: %v", err)
	}
}

type mutableStore struct {
	configs []provider.Config
}

func (m *mutableStore) EnabledProviders(context.Context) ([]provider.Config, error) {
	return m.configs, nil
}

func TestValidate(t *testing.T) {
	reg := provider.DefaultRegistry()

	ok := provider.Config{Name: " av ", Kind: " AlphaVantage ", BaseURL: "https://example.test/query"}
	if err := provider.Validate(&ok, reg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if ok.Name != "av" || ok.Kind != "alphavantage" {
		t.Errorf("not normalized: name=%q kind=%q", ok.Name, ok.Kind)
	}
	if ok.Timeout != 10*time.Second {
		t.Errorf("default timeout = %v", ok.Timeout)
	}

	bad := map[string]provider.Config{
		"no name":       {Kind: "alphavantage", BaseURL: "https://e.test"},
		"no kind":       {Name: "x", BaseURL: "https://e.test"},
		"unknown kind":  {Name: "x", Kind: "bloomberg", BaseURL: "https://e.test"},
		"relative url":  {Name: "x", Kind: "alphavantage", BaseURL: "/query"},
		"no scheme":     {Name: "x", Kind: "alphavantage", BaseURL: "example.test/query"},
		"file scheme":   {Name: "x", Kind: "alphavantage", BaseURL: "file:///etc/passwd"},
		"negative prio": {Name: "x", Kind: "alphavantage", BaseURL: "https://e.test", Priority: -1},
		"huge timeout":  {Name: "x", Kind: "alphavantage", BaseURL: "https://e.test", Timeout: time.Hour},
	}
	for name, cfg := range bad {
		c := cfg
		if err := provider.Validate(&c, reg); err == nil {
			t.Errorf("%s: accepted, want rejection", name)
		}
	}
}
