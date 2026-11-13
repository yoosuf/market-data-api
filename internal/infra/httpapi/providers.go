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
)

// maxProviderBody bounds an admin request body.
const maxProviderBody = 64 << 10

// ProviderStore is the configuration CRUD capability, declared at the point
// of use. The annual read path does not use it, and it does not reach the
// annual read path.
type ProviderStore interface {
	ListProviders(ctx context.Context) ([]provider.Config, error)
	GetProvider(ctx context.Context, id int64) (provider.Config, error)
	CreateProvider(ctx context.Context, cfg provider.Config, at time.Time) (provider.Config, error)
	UpdateProvider(ctx context.Context, cfg provider.Config, at time.Time) (provider.Config, error)
	DeleteProvider(ctx context.Context, id int64) error
}

type providerHandler struct {
	store    ProviderStore
	registry *provider.Registry
	log      *slog.Logger
	now      func() time.Time
}

// providerPayload is the wire representation.
//
// NOTE: api_key is returned in full, by explicit configuration choice. Anyone
// who can reach these routes can read every provider credential. See the
// security note in README.md.
type providerPayload struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	Priority  int    `json:"priority"`
	Enabled   bool   `json:"enabled"`
	TimeoutMS int64  `json:"timeout_ms"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func toPayload(cfg provider.Config) providerPayload {
	return providerPayload{
		ID:        cfg.ID,
		Name:      cfg.Name,
		Kind:      cfg.Kind,
		BaseURL:   cfg.BaseURL,
		APIKey:    cfg.APIKey,
		Priority:  cfg.Priority,
		Enabled:   cfg.Enabled,
		TimeoutMS: cfg.Timeout.Milliseconds(),
		CreatedAt: formatTime(cfg.CreatedAt),
		UpdatedAt: formatTime(cfg.UpdatedAt),
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// decodeProvider reads a request body into a Config.
//
// Enabled defaults to true and timeout to 10s when omitted, so a minimal
// create body is name, kind, base_url and api_key.
func decodeProvider(w http.ResponseWriter, r *http.Request) (provider.Config, error) {
	var p struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
		Priority  *int   `json:"priority"`
		Enabled   *bool  `json:"enabled"`
		TimeoutMS *int64 `json:"timeout_ms"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProviderBody))
	dec.DisallowUnknownFields() // a typo in a field name must not silently no-op
	if err := dec.Decode(&p); err != nil {
		return provider.Config{}, errors.New("request body must be a JSON object with known fields")
	}

	cfg := provider.Config{
		Name:    p.Name,
		Kind:    p.Kind,
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Enabled: true,
		Timeout: 10 * time.Second,
	}
	if p.Priority != nil {
		cfg.Priority = *p.Priority
	} else {
		cfg.Priority = 100
	}
	if p.Enabled != nil {
		cfg.Enabled = *p.Enabled
	}
	if p.TimeoutMS != nil {
		cfg.Timeout = time.Duration(*p.TimeoutMS) * time.Millisecond
	}
	return cfg, nil
}

func (h *providerHandler) list(w http.ResponseWriter, r *http.Request) {
	configs, err := h.store.ListProviders(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]providerPayload, 0, len(configs))
	for _, c := range configs {
		out = append(out, toPayload(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (h *providerHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	cfg, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPayload(cfg))
}

func (h *providerHandler) create(w http.ResponseWriter, r *http.Request) {
	cfg, err := decodeProvider(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := provider.Validate(&cfg, h.registry); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateProvider(r.Context(), cfg, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.log.InfoContext(r.Context(), "provider created",
		"id", created.ID, "name", created.Name, "kind", created.Kind)
	writeJSON(w, http.StatusCreated, toPayload(created))
}

func (h *providerHandler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	cfg, err := decodeProvider(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.ID = id
	if err := provider.Validate(&cfg, h.registry); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.store.UpdateProvider(r.Context(), cfg, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.log.InfoContext(r.Context(), "provider updated", "id", id, "name", updated.Name)
	writeJSON(w, http.StatusOK, toPayload(updated))
}

func (h *providerHandler) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteProvider(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	h.log.InfoContext(r.Context(), "provider deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *providerHandler) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid provider id")
		return 0, false
	}
	return id, true
}

// fail maps configuration errors to status codes. Validation detail is
// deliberately returned here, unlike on the market data path: an operator
// editing configuration needs to know which field was wrong, and these
// messages describe the submitted request rather than the server's internals.
func (h *providerHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, "provider not found")
	case errors.Is(err, provider.ErrDuplicateName):
		writeError(w, http.StatusConflict, "a provider with that name already exists")
	case errors.Is(err, provider.ErrUnknownKind), errors.Is(err, provider.ErrInvalidConfig):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		h.log.ErrorContext(r.Context(), "provider admin request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
