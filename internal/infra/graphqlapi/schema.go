// Package graphqlapi exposes the same annual aggregate over GraphQL.
//
// It is a second transport adapter, not a second implementation: resolvers
// call the identical annual service the REST handler uses, so cache behavior,
// validation rules and upstream semantics cannot drift between the two.
package graphqlapi

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// AnnualService is the capability this adapter needs, declared at the point
// of use.
type AnnualService interface {
	AnnualSummary(ctx context.Context, symbol string, year int) (market.AnnualSummary, error)
}

// Error codes surfaced in each GraphQL error's `extensions.code`. GraphQL has
// no status line, so the code is how a client distinguishes bad input from
// absence from an upstream outage — the same four-way split the REST adapter
// expresses with 400/404/503/500.
const (
	codeBadInput    = "BAD_USER_INPUT"
	codeNotFound    = "NOT_FOUND"
	codeUnavailable = "UPSTREAM_UNAVAILABLE"
	codeUpstream    = "UPSTREAM_ERROR"
	codeInternal    = "INTERNAL_ERROR"
)

// resolverError carries a safe message and a machine-readable code.
// graphql-go renders Extensions() into the error entry, and nothing else of
// the underlying error escapes: SQL text, driver detail and the API key stay
// in the server log.
type resolverError struct {
	msg  string
	code string
}

func (e resolverError) Error() string { return e.msg }

func (e resolverError) Extensions() map[string]any {
	return map[string]any{"code": e.code}
}

// newSchema builds the executable schema.
//
// high, low and volume are String, not Float or Int: GraphQL's Float is IEEE
// 754 double precision, which would discard the exactness the service stores,
// and volume can exceed GraphQL's 32-bit Int.
func newSchema(svc AnnualService, now func() time.Time) (graphql.Schema, error) {
	summaryType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "AnnualSummary",
		Description: "Aggregated market data for one symbol over one calendar year.",
		Fields: graphql.Fields{
			"symbol": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.String),
				Description: "Normalized ticker symbol.",
			},
			"year": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.Int),
				Description: "Calendar year.",
			},
			"high": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.String),
				Description: "Highest monthly high, with four decimals. A string to preserve exact precision.",
			},
			"low": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.String),
				Description: "Lowest monthly low, with four decimals. A string to preserve exact precision.",
			},
			"volume": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.String),
				Description: "Total traded volume. A string because it can exceed a 32-bit Int.",
			},
			"months": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.Int),
				Description: "How many monthly records back this aggregate. Fewer than 12 is valid for an IPO, delisting, or the current year.",
			},
		},
	})

	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"annual": &graphql.Field{
				Type:        summaryType, // nullable: absence is data, not a crash
				Description: "Annual aggregate for a symbol. Null with a NOT_FOUND error when the symbol has no history for that year.",
				Args: graphql.FieldConfigArgument{
					"symbol": &graphql.ArgumentConfig{
						Type:        graphql.NewNonNull(graphql.String),
						Description: "Ticker symbol, case-insensitive. Punctuation such as BRK.B is accepted.",
					},
					"year": &graphql.ArgumentConfig{
						Type:        graphql.NewNonNull(graphql.Int),
						Description: "Four-digit calendar year, 1900 through the current year.",
					},
				},
				Resolve: resolveAnnual(svc, now),
			},
		},
	})

	return graphql.NewSchema(graphql.SchemaConfig{Query: query})
}

// annualResult is what the resolver returns. Field values are pre-rendered so
// the field resolvers stay trivial and the wire format matches REST exactly.
type annualResult struct {
	Symbol string `json:"symbol"`
	Year   int    `json:"year"`
	High   string `json:"high"`
	Low    string `json:"low"`
	Volume string `json:"volume"`
	Months int    `json:"months"`
}

func resolveAnnual(svc AnnualService, now func() time.Time) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (any, error) {
		rawSymbol, _ := p.Args["symbol"].(string)
		symbol, err := market.NormalizeSymbol(rawSymbol)
		if err != nil {
			return nil, resolverError{msg: "invalid symbol", code: codeBadInput}
		}
		year, ok := p.Args["year"].(int)
		if !ok {
			return nil, resolverError{msg: "invalid year", code: codeBadInput}
		}
		if err := market.ValidateYear(year, now()); err != nil {
			return nil, resolverError{msg: "invalid year", code: codeBadInput}
		}

		summary, err := svc.AnnualSummary(p.Context, symbol, year)
		if err != nil {
			return nil, classify(err)
		}

		return annualResult{
			Symbol: summary.Symbol,
			Year:   summary.Year,
			High:   summary.High.String(),
			Low:    summary.Low.String(),
			Volume: strconv.FormatInt(summary.Volume, 10),
			Months: summary.Months,
		}, nil
	}
}

// classify maps domain outcomes to safe GraphQL errors. It mirrors the REST
// status mapping deliberately: NOT_FOUND is a claim that the data does not
// exist, so an upstream failure never produces it.
func classify(err error) error {
	switch {
	case errors.Is(err, market.ErrNoData):
		return resolverError{msg: "no data", code: codeNotFound}
	case errors.Is(err, market.ErrUpstreamUnavailable):
		return resolverError{msg: "upstream unavailable", code: codeUnavailable}
	case errors.Is(err, market.ErrUpstreamPayload), errors.Is(err, market.ErrPrecision):
		return resolverError{msg: "upstream error", code: codeUpstream}
	case errors.Is(err, context.Canceled):
		return resolverError{msg: "request cancelled", code: codeInternal}
	default:
		return resolverError{msg: "internal error", code: codeInternal}
	}
}
