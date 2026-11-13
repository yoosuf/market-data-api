// Package market holds the domain types shared by every adapter.
// It performs no I/O and imports nothing outside the standard library.
package market

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scale is the fixed-point scale used for prices: four decimal places,
// matching the precision Alpha Vantage publishes for monthly series.
const Scale = 10000

// FractionDigits is the number of decimal places Scale represents.
const FractionDigits = 4

var (
	// ErrInvalidSymbol reports a symbol that fails normalization.
	ErrInvalidSymbol = errors.New("invalid symbol")
	// ErrInvalidYear reports a year outside the supported range.
	ErrInvalidYear = errors.New("invalid year")
	// ErrPrecision reports a price carrying more precision than Scale can
	// represent exactly. It is never rounded away silently.
	ErrPrecision = errors.New("price precision exceeds supported scale")
	// ErrPriceFormat reports a price string that is not a plain decimal.
	ErrPriceFormat = errors.New("malformed price")
	// ErrRange reports a value too large for exact int64 representation.
	ErrRange = errors.New("value out of range")
)

// MinYear is the earliest year the API will accept.
const MinYear = 1900

// Price is a monetary amount stored as an integer scaled by Scale.
// Binary floating point cannot represent values such as 0.01 exactly, so
// prices never pass through float64 anywhere in this codebase.
type Price int64

// ParsePrice converts a plain decimal string such as "188.4400" into a Price
// by string manipulation only. More than FractionDigits decimals is an error
// rather than a silent rounding.
func ParsePrice(s string) (Price, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrPriceFormat)
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("%w: %q", ErrPriceFormat, s)
	}
	if !allDigits(intPart) || (hasFrac && !allDigits(fracPart)) {
		return 0, fmt.Errorf("%w: %q", ErrPriceFormat, s)
	}
	if len(fracPart) > FractionDigits {
		// Trailing zeros carry no value, so "1.23000" is accepted as 1.2300.
		if strings.Trim(fracPart[FractionDigits:], "0") != "" {
			return 0, fmt.Errorf("%w: %q has %d decimals, want at most %d",
				ErrPrecision, s, len(fracPart), FractionDigits)
		}
		fracPart = fracPart[:FractionDigits]
	}

	whole := int64(0)
	if intPart != "" {
		v, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrRange, s)
		}
		whole = v
	}
	if whole > (1<<63-1)/Scale {
		return 0, fmt.Errorf("%w: %q", ErrRange, s)
	}

	frac := int64(0)
	if fracPart != "" {
		padded := fracPart + strings.Repeat("0", FractionDigits-len(fracPart))
		v, err := strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrPriceFormat, s)
		}
		frac = v
	}

	p := whole*Scale + frac
	if neg {
		p = -p
	}
	return Price(p), nil
}

// String renders the price with exactly FractionDigits decimals, matching the
// representation the provider publishes.
func (p Price) String() string {
	neg := p < 0
	v := int64(p)
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%0*d", v/Scale, FractionDigits, v%Scale)
	if neg {
		return "-" + s
	}
	return s
}

// allDigits reports whether s is made up only of ASCII digits.
// An empty string is reported as true; callers that disallow it check length
// separately (ParseYear requires four characters, ParsePrice rejects a bare
// decimal point).
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// MonthlyRecord is one calendar month of history for one symbol.
type MonthlyRecord struct {
	Symbol string // normalized
	Month  string // YYYY-MM
	Year   int
	High   Price
	Low    Price
	Volume int64
}

// AnnualSummary is the aggregate the API serves.
type AnnualSummary struct {
	Symbol string
	Year   int
	High   Price
	Low    Price
	Volume int64
	Months int // number of monthly rows behind the aggregate
}

// ImportState records that a provider fetch was attempted for a symbol, and
// whether it succeeded. It is what separates "never fetched" from "fetched
// successfully and the symbol genuinely has no data for that year".
type ImportState struct {
	Symbol string
	// LastSuccessAt is zero when no fetch has ever succeeded. This is the
	// only field any decision depends on.
	LastSuccessAt time.Time
	// LastStatus records the most recent outcome for diagnostics: it answers
	// "why does this symbol have no data" when reading the database by hand.
	// Deliberately not branched on — an outcome string is a poor substitute
	// for the timestamp above, which is what proves a fetch succeeded.
	LastStatus string
}

// NormalizeSymbol trims and uppercases a symbol and validates its charset.
// Dots and hyphens are legitimate (BRK.B, RDS-A) and are not rejected.
func NormalizeSymbol(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) == 0 || len(s) > 10 {
		return "", ErrInvalidSymbol
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return "", ErrInvalidSymbol
		}
	}
	return s, nil
}

// ParseYear validates a four-digit year string against the supported range.
// The four-digit rule is a property of the textual form: a path segment or a
// query string. Callers that already hold an integer use ValidateYear.
func ParseYear(s string, now time.Time) (int, error) {
	if len(s) != 4 || !allDigits(s) {
		return 0, ErrInvalidYear
	}
	y, err := strconv.Atoi(s)
	if err != nil {
		return 0, ErrInvalidYear
	}
	return y, ValidateYear(y, now)
}

// ValidateYear checks a year against the supported range. It is the single
// definition of that range, shared by every transport.
func ValidateYear(y int, now time.Time) error {
	if y < MinYear || y > now.Year() {
		return ErrInvalidYear
	}
	return nil
}

// MonthKey derives the YYYY-MM key and calendar year from a provider date
// such as "2024-01-31", whose day is the last trading day of the month.
func MonthKey(date string) (month string, year int, err error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(date))
	if err != nil {
		return "", 0, fmt.Errorf("malformed date %q", date)
	}
	return t.Format("2006-01"), t.Year(), nil
}

// Errors that cross adapter boundaries.
//
// These live here, in the shared domain package, so that the workflow and the
// HTTP layer can classify an outcome without importing a specific provider
// adapter. Swapping providers means implementing these semantics, not editing
// the layers above.
var (
	// ErrNoData means an upstream source answered successfully and reported
	// that the requested data does not exist. It is evidence of absence, and
	// may be cached.
	ErrNoData = errors.New("no data")

	// ErrUpstreamUnavailable means the upstream could not be reached, timed
	// out, or declined to serve. It is NOT evidence of absence and must never
	// be cached as one.
	ErrUpstreamUnavailable = errors.New("upstream unavailable")

	// ErrUpstreamPayload means the upstream answered with something that could
	// not be understood.
	ErrUpstreamPayload = errors.New("upstream returned an unusable payload")
)
