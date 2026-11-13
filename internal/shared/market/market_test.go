package market_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

func TestParsePriceExact(t *testing.T) {
	cases := []struct {
		in   string
		want market.Price
	}{
		{"188.4400", 1884400},
		{"0.0001", 1},
		{"10.0000", 100000},
		{"9.9000", 99000},
		{"1", 10000},
		{".5", 5000},
		{"0.1", 1000},
		{"1.23000", 12300}, // trailing zeros beyond scale carry no value
		{" 42.5 ", 425000},
	}
	for _, c := range cases {
		got, err := market.ParsePrice(c.in)
		if err != nil {
			t.Errorf("ParsePrice(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePrice(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The classic float failure: 0.1+0.2 != 0.3 in binary floating point.
// Scaled integers must not reproduce it.
func TestParsePriceAvoidsFloatError(t *testing.T) {
	a, _ := market.ParsePrice("0.1")
	b, _ := market.ParsePrice("0.2")
	c, _ := market.ParsePrice("0.3")
	if a+b != c {
		t.Fatalf("0.1 + 0.2 = %s, want %s", (a + b).String(), c.String())
	}
}

func TestParsePriceRejectsExcessPrecision(t *testing.T) {
	// Five significant decimals cannot be stored exactly, and must not be
	// silently rounded into the cache.
	if _, err := market.ParsePrice("1.23456"); !errors.Is(err, market.ErrPrecision) {
		t.Fatalf("got %v, want ErrPrecision", err)
	}
}

func TestParsePriceRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "1.2.3", "1,000.00", "1e5", ".", "12a.00"} {
		if _, err := market.ParsePrice(in); err == nil {
			t.Errorf("ParsePrice(%q) succeeded, want error", in)
		}
	}
}

func TestPriceStringRoundTrip(t *testing.T) {
	for _, in := range []string{"188.4400", "0.0001", "1000000.0000", "9.9000"} {
		p, err := market.ParsePrice(in)
		if err != nil {
			t.Fatalf("ParsePrice(%q): %v", in, err)
		}
		if got := p.String(); got != in {
			t.Errorf("round trip %q -> %q", in, got)
		}
	}
}

func TestNormalizeSymbol(t *testing.T) {
	ok := map[string]string{
		"ibm":    "IBM",
		" msft ": "MSFT",
		"BRK.B":  "BRK.B", // dots are legitimate
		"rds-a":  "RDS-A", // so are hyphens
		"7203":   "7203",
	}
	for in, want := range ok {
		got, err := market.NormalizeSymbol(in)
		if err != nil {
			t.Errorf("NormalizeSymbol(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeSymbol(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "   ", "TOOLONGSYMBOL", "IB M", "DROP;TABLE", "IBM/", "ibm'"} {
		if _, err := market.NormalizeSymbol(in); !errors.Is(err, market.ErrInvalidSymbol) {
			t.Errorf("NormalizeSymbol(%q) accepted or wrong error", in)
		}
	}
}

func TestParseYear(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if y, err := market.ParseYear("2023", now); err != nil || y != 2023 {
		t.Fatalf("ParseYear(2023) = %d, %v", y, err)
	}
	if _, err := market.ParseYear("2026", now); err != nil {
		t.Fatalf("current year must be accepted: %v", err)
	}
	for _, in := range []string{"202", "20233", "abcd", "2027", "1899", "-123", "20 3"} {
		if _, err := market.ParseYear(in, now); !errors.Is(err, market.ErrInvalidYear) {
			t.Errorf("ParseYear(%q) accepted or wrong error", in)
		}
	}
}

func TestMonthKey(t *testing.T) {
	// The provider keys months by their last trading day.
	month, year, err := market.MonthKey("2024-01-31")
	if err != nil || month != "2024-01" || year != 2024 {
		t.Fatalf("MonthKey = %q, %d, %v", month, year, err)
	}
	if _, _, err := market.MonthKey("2024-13-01"); err == nil {
		t.Fatal("expected error for invalid month")
	}
}
