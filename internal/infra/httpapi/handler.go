// Package httpapi adapts the annual workflow to HTTP: it validates route
// input, maps domain outcomes to status codes, and renders JSON. It holds no
// business rules.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yoosuf/market-data-api/internal/core/provider"
	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// AnnualService is the capability the handler needs.
type AnnualService interface {
	AnnualSummary(ctx context.Context, symbol string, year int) (market.AnnualSummary, error)
}

// handler serves the market data routes.
type handler struct {
	svc AnnualService
	log *slog.Logger
	now func() time.Time
}

// Option customizes the router.
type Option func(*options)

type options struct {
	providers ProviderStore
	registry  *provider.Registry
}

// WithProviderAdmin mounts the provider configuration CRUD routes under
// /admin/providers. It is an option rather than a parameter because the
// market data endpoint is complete without it: a deployment that configures
// providers out of band can leave the admin surface off entirely.
func WithProviderAdmin(store ProviderStore, registry *provider.Registry) Option {
	return func(o *options) {
		o.providers = store
		o.registry = registry
	}
}

// New builds the router. Routing uses net/http patterns; no third-party
// router is needed for this many routes.
func New(svc AnnualService, log *slog.Logger, now func() time.Time, opts ...Option) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	var cfg options
	for _, o := range opts {
		o(&cfg)
	}
	h := &handler{svc: svc, log: log, now: now}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /symbols/{symbol}/annual/{year}", h.annual)

	if cfg.providers != nil {
		p := &providerHandler{store: cfg.providers, registry: cfg.registry, log: log, now: now}
		mux.HandleFunc("GET /admin/providers", p.list)
		mux.HandleFunc("POST /admin/providers", p.create)
		mux.HandleFunc("GET /admin/providers/{id}", p.get)
		mux.HandleFunc("PUT /admin/providers/{id}", p.update)
		mux.HandleFunc("DELETE /admin/providers/{id}", p.remove)
	}
	return mux
}

// annualResponse is the wire format. High, Low and Volume are strings so that
// no client parses them as IEEE-754 doubles and loses the exact precision the
// service went to some trouble to preserve.
type annualResponse struct {
	Symbol string `json:"symbol"`
	Year   int    `json:"year"`
	High   string `json:"high"`
	Low    string `json:"low"`
	Volume string `json:"volume"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) annual(w http.ResponseWriter, r *http.Request) {
	symbol, err := market.NormalizeSymbol(r.PathValue("symbol"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid symbol")
		return
	}
	year, err := market.ParseYear(r.PathValue("year"), h.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid year")
		return
	}

	summary, err := h.svc.AnnualSummary(r.Context(), symbol, year)
	if err != nil {
		h.writeFailure(w, r, symbol, year, err)
		return
	}

	writeJSON(w, http.StatusOK, annualResponse{
		Symbol: summary.Symbol,
		Year:   summary.Year,
		High:   summary.High.String(),
		Low:    summary.Low.String(),
		Volume: strconv.FormatInt(summary.Volume, 10),
	})
}

// writeFailure maps domain errors to status codes. The mapping that matters:
// a 404 is a claim that the data does not exist, so it is reserved for a
// successful upstream answer. Every upstream problem is a 5xx.
func (h *handler) writeFailure(w http.ResponseWriter, r *http.Request, symbol string, year int, err error) {
	switch {
	case errors.Is(err, market.ErrNoData):
		writeError(w, http.StatusNotFound, "no data")
	case errors.Is(err, market.ErrUpstreamUnavailable):
		h.log.WarnContext(r.Context(), "upstream unavailable",
			"symbol", symbol, "year", year, "error", err)
		writeError(w, http.StatusServiceUnavailable, "upstream unavailable")
	case errors.Is(err, market.ErrUpstreamPayload), errors.Is(err, market.ErrPrecision):
		h.log.ErrorContext(r.Context(), "upstream payload rejected",
			"symbol", symbol, "year", year, "error", err)
		writeError(w, http.StatusBadGateway, "upstream error")
	case errors.Is(err, context.Canceled):
		// The client hung up; nothing useful to send.
		return
	default:
		// Database and unclassified failures. The detail stays in the log:
		// SQL text and driver messages are not the caller's business.
		h.log.ErrorContext(r.Context(), "request failed",
			"symbol", symbol, "year", year, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
