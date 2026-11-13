package annual_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/shared/market"
	"github.com/yoosuf/market-data-api/internal/shared/markettest"
)

func TestColdRequestFetchesWarmRequestDoesNot(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(store, p, annual.Config{Now: markettest.Now()})

	got, err := svc.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("cold request: %v", err)
	}
	if p.Calls() != 1 {
		t.Fatalf("cold request made %d provider calls, want 1", p.Calls())
	}
	if got.High.String() != "153.2100" || got.Low.String() != "129.0000" || got.Volume != 132687444 {
		t.Fatalf("aggregate wrong: high=%s low=%s volume=%d", got.High, got.Low, got.Volume)
	}

	// Warm request: the cache answers, and the provider is not contacted.
	got2, err := svc.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("warm request: %v", err)
	}
	if p.Calls() != 1 {
		t.Fatalf("warm request made an extra provider call (total %d)", p.Calls())
	}
	if got2 != got {
		t.Fatalf("warm result %+v differs from cold %+v", got2, got)
	}
}

// The provider returns history by symbol, so a second year must be served
// from the history already imported.
func TestSecondYearReusesTheSameImport(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(store, p, annual.Config{Now: markettest.Now()})

	if _, err := svc.AnnualSummary(ctx, "IBM", 2023); err != nil {
		t.Fatalf("2023: %v", err)
	}
	got, err := svc.AnnualSummary(ctx, "IBM", 2022)
	if err != nil {
		t.Fatalf("2022: %v", err)
	}
	if p.Calls() != 1 {
		t.Fatalf("second year triggered another fetch (calls=%d)", p.Calls())
	}
	if got.Volume != 50 || got.High.String() != "99.0000" {
		t.Fatalf("2022 aggregate wrong: high=%s volume=%d", got.High, got.Volume)
	}
}

// A successful fetch that yields nothing for the year is no-data, and the
// result is remembered so repeated requests do not burn quota.
func TestNoDataIsRememberedForTheTTL(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(store, p, annual.Config{Now: markettest.Now(), NegativeTTL: time.Hour})

	for i := 0; i < 3; i++ {
		_, err := svc.AnnualSummary(ctx, "IBM", 2019) // predates this history
		if !errors.Is(err, market.ErrNoData) {
			t.Fatalf("attempt %d: got %v, want ErrNoData", i, err)
		}
	}
	if p.Calls() != 1 {
		t.Fatalf("negative result not cached: %d provider calls", p.Calls())
	}
}

func TestNoDataIsRefetchedAfterTTL(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	svc := annual.New(store, p, annual.Config{
		NegativeTTL: time.Hour,
		Now:         func() time.Time { return now },
	})

	if _, err := svc.AnnualSummary(ctx, "IBM", 2019); !errors.Is(err, market.ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}
	now = now.Add(2 * time.Hour) // TTL expires
	if _, err := svc.AnnualSummary(ctx, "IBM", 2019); !errors.Is(err, market.ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}
	if p.Calls() != 2 {
		t.Fatalf("expected a refetch after the TTL, got %d calls", p.Calls())
	}
}

// An upstream failure is never evidence of absence, and must never be
// remembered as one.
func TestProviderFailureIsNotCachedAsNoData(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.FailingProvider(market.ErrUpstreamUnavailable)
	svc := annual.New(store, p, annual.Config{Now: markettest.Now()})

	for i := 0; i < 2; i++ {
		_, err := svc.AnnualSummary(ctx, "IBM", 2023)
		if !errors.Is(err, market.ErrUpstreamUnavailable) {
			t.Fatalf("attempt %d: got %v, want ErrUnavailable", i, err)
		}
		if errors.Is(err, market.ErrNoData) {
			t.Fatal("an outage was reported as no-data")
		}
	}
	// Each request must retry: a failure does not create a negative entry.
	if p.Calls() != 2 {
		t.Fatalf("failure suppressed the retry: %d calls, want 2", p.Calls())
	}
}

// Cached data stays servable when the provider is down.
func TestWarmRequestSurvivesProviderOutage(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(store, p, annual.Config{Now: markettest.Now()})

	want, err := svc.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}

	p.SetError(market.ErrUpstreamUnavailable) // upstream goes down
	got, err := svc.AnnualSummary(ctx, "IBM", 2023)
	if err != nil {
		t.Fatalf("warm request failed during outage: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// The current year is partial, so a stale cache entry is refreshed.
func TestCurrentYearRefreshesWhenStale(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	p := markettest.NewProvider([]market.MonthlyRecord{
		{Month: "2026-01", Year: 2026, High: markettest.MustPrice(t, "10.0000"), Low: markettest.MustPrice(t, "5.0000"), Volume: 100},
	})
	svc := annual.New(store, p, annual.Config{
		CurrentYearTTL: time.Hour,
		Now:            func() time.Time { return now },
	})

	if _, err := svc.AnnualSummary(ctx, "IBM", 2026); err != nil {
		t.Fatalf("cold: %v", err)
	}
	// Within the freshness window: no new call.
	if _, err := svc.AnnualSummary(ctx, "IBM", 2026); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if p.Calls() != 1 {
		t.Fatalf("fresh current year refetched: %d calls", p.Calls())
	}

	now = now.Add(2 * time.Hour)
	p.SetRecords(append(p.Records(), market.MonthlyRecord{
		Month: "2026-02", Year: 2026, High: markettest.MustPrice(t, "20.0000"), Low: markettest.MustPrice(t, "1.0000"), Volume: 400,
	}))
	got, err := svc.AnnualSummary(ctx, "IBM", 2026)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if p.Calls() != 2 {
		t.Fatalf("stale current year not refreshed: %d calls", p.Calls())
	}
	if got.Volume != 500 || got.High.String() != "20.0000" {
		t.Fatalf("refresh not reflected: high=%s volume=%d", got.High, got.Volume)
	}
}

// A completed year never expires: its history cannot change.
func TestCompletedYearNeverRefetches(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(store, p, annual.Config{
		CurrentYearTTL: time.Minute,
		Now:            func() time.Time { return now },
	})

	if _, err := svc.AnnualSummary(ctx, "IBM", 2023); err != nil {
		t.Fatalf("cold: %v", err)
	}
	now = now.Add(30 * 24 * time.Hour)
	if _, err := svc.AnnualSummary(ctx, "IBM", 2023); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if p.Calls() != 1 {
		t.Fatalf("completed year refetched: %d calls", p.Calls())
	}
}

// A failed current-year refresh must leave the cached aggregate servable.
func TestStaleCurrentYearSurvivesFailedRefresh(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	p := markettest.NewProvider([]market.MonthlyRecord{
		{Month: "2026-01", Year: 2026, High: markettest.MustPrice(t, "10.0000"), Low: markettest.MustPrice(t, "5.0000"), Volume: 100},
	})
	svc := annual.New(store, p, annual.Config{
		CurrentYearTTL: time.Hour,
		Now:            func() time.Time { return now },
	})

	want, err := svc.AnnualSummary(ctx, "IBM", 2026)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}

	now = now.Add(2 * time.Hour)
	p.SetError(market.ErrUpstreamUnavailable)
	got, err := svc.AnnualSummary(ctx, "IBM", 2026)
	if err != nil {
		t.Fatalf("stale refresh failure must not fail the request: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want cached %+v", got, want)
	}
}

// Concurrent misses may duplicate the upstream call, but must never
// duplicate rows or inflate volume: the (symbol, month) key guarantees it.
func TestConcurrentMissesDoNotCorruptTheAggregate(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t)).WithDelay(20 * time.Millisecond)
	svc := annual.New(store, p, annual.Config{Now: markettest.Now()})

	const n = 8
	results := make([]market.AnnualSummary, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.AnnualSummary(ctx, "IBM", 2023)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if results[i].Volume != 132687444 {
			t.Fatalf("goroutine %d saw volume %d, want 132687444", i, results[i].Volume)
		}
		if results[i].Months != 2 {
			t.Fatalf("goroutine %d saw %d months, want 2", i, results[i].Months)
		}
	}
}

func TestImporterAlwaysFetches(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))
	imp := annual.NewImporter(store, p, time.Now)

	for i := 1; i <= 2; i++ {
		n, err := imp.Import(ctx, "IBM")
		if err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
		if n != 3 {
			t.Fatalf("import %d persisted %d months, want 3", i, n)
		}
		if p.Calls() != i {
			t.Fatalf("import %d made %d calls, want %d", i, p.Calls(), i)
		}
	}

	// Re-running the job must not inflate volume.
	got, found, err := store.AnnualSummary(ctx, "IBM", 2023)
	if err != nil || !found {
		t.Fatalf("summary: found=%v err=%v", found, err)
	}
	if got.Volume != 132687444 {
		t.Fatalf("volume = %d after two refresh runs, want 132687444", got.Volume)
	}
}

func TestImporterFailurePreservesCachedData(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.NewProvider(markettest.History(t))
	imp := annual.NewImporter(store, p, time.Now)

	if _, err := imp.Import(ctx, "IBM"); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	p.SetError(market.ErrUpstreamUnavailable)
	if _, err := imp.Import(ctx, "IBM"); !errors.Is(err, market.ErrUpstreamUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}

	got, found, err := store.AnnualSummary(ctx, "IBM", 2023)
	if err != nil || !found {
		t.Fatalf("cached data lost after failed refresh: found=%v err=%v", found, err)
	}
	if got.Volume != 132687444 {
		t.Fatalf("volume = %d, want 132687444", got.Volume)
	}
}

// A symbol the provider does not know is a determination, not a failure: the
// result must be remembered, or every request for a bogus symbol spends an
// upstream call from a ~25/day quota.
func TestUnknownSymbolIsRemembered(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.FailingProvider(market.ErrNoData)
	svc := annual.New(store, p, annual.Config{Now: markettest.Now(), NegativeTTL: time.Hour})

	for i := 0; i < 3; i++ {
		_, err := svc.AnnualSummary(ctx, "NOSUCH", 2023)
		if !errors.Is(err, market.ErrNoData) {
			t.Fatalf("attempt %d: got %v, want ErrNoData", i, err)
		}
	}
	if p.Calls() != 1 {
		t.Fatalf("unknown symbol cost %d upstream calls, want 1", p.Calls())
	}
}

// ...but it is still re-checked once the window closes, so a newly listed
// symbol becomes available without operator intervention.
func TestUnknownSymbolIsRecheckedAfterTTL(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	p := markettest.FailingProvider(market.ErrNoData)
	svc := annual.New(store, p, annual.Config{
		NegativeTTL: time.Hour,
		Now:         func() time.Time { return now },
	})

	if _, err := svc.AnnualSummary(ctx, "NEW", 2023); !errors.Is(err, market.ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}

	now = now.Add(2 * time.Hour)
	p.SetError(nil)
	p.SetRecords(markettest.History(t))

	got, err := svc.AnnualSummary(ctx, "NEW", 2023)
	if err != nil {
		t.Fatalf("after TTL: %v", err)
	}
	if p.Calls() != 2 {
		t.Fatalf("calls = %d, want 2", p.Calls())
	}
	if got.Volume != 132687444 {
		t.Fatalf("volume = %d, want 132687444", got.Volume)
	}
}

// A cached completed year must not pay for an import-state lookup.
func TestCachedCompletedYearSkipsImportStateLookup(t *testing.T) {
	ctx := context.Background()
	inner := markettest.NewStore(t)
	counting := &countingStore{SQLiteStore: inner}
	p := markettest.NewProvider(markettest.History(t))
	svc := annual.New(counting, p, annual.Config{Now: markettest.Now()})

	if _, err := svc.AnnualSummary(ctx, "IBM", 2023); err != nil {
		t.Fatalf("cold: %v", err)
	}
	before := counting.stateCalls
	if _, err := svc.AnnualSummary(ctx, "IBM", 2023); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if counting.stateCalls != before {
		t.Fatalf("warm hit queried import state %d extra times", counting.stateCalls-before)
	}
}

type countingStore struct {
	*annual.SQLiteStore
	stateCalls int
}

func (c *countingStore) ImportState(ctx context.Context, symbol string) (market.ImportState, bool, error) {
	c.stateCalls++
	return c.SQLiteStore.ImportState(ctx, symbol)
}

// The refresh job and the serving path must agree about what a definitive
// "no such symbol" means, or the same fact would produce different cache
// state depending on which path discovered it.
func TestImporterAndServiceAgreeOnUnknownSymbol(t *testing.T) {
	ctx := context.Background()
	store := markettest.NewStore(t)
	p := markettest.FailingProvider(market.ErrNoData)

	imp := annual.NewImporter(store, p, markettest.Now())
	if _, err := imp.Import(ctx, "NOSUCH"); !errors.Is(err, market.ErrNoData) {
		t.Fatalf("import: got %v, want ErrNoData", err)
	}

	// The job established the absence, so the serving path must now answer
	// from that determination instead of asking again.
	svc := annual.New(store, p, annual.Config{Now: markettest.Now(), NegativeTTL: time.Hour})
	if _, err := svc.AnnualSummary(ctx, "NOSUCH", 2023); !errors.Is(err, market.ErrNoData) {
		t.Fatalf("serve: got %v, want ErrNoData", err)
	}
	if p.Calls() != 1 {
		t.Fatalf("provider called %d times; the job's determination was not reused", p.Calls())
	}
}
