// Package provider turns stored provider configuration into something the
// annual workflow can fetch from.
//
// Configuration lives in the database, not in the environment, so providers
// can be added, reordered, disabled and removed at runtime. What stays in code
// is the *shape* of each upstream API: a row names a `kind`, and the registry
// maps that kind to the adapter that knows how to talk to it.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/yoosuf/market-data-api/internal/shared/market"
)

// Errors returned when configuration is unusable.
var (
	ErrUnknownKind   = errors.New("unknown provider kind")
	ErrInvalidConfig = errors.New("invalid provider configuration")
	ErrNotFound      = errors.New("provider not found")
	ErrDuplicateName = errors.New("provider name already exists")
)

// Config is one upstream provider as stored.
type Config struct {
	ID        int64
	Name      string        // operator-chosen label, unique
	Kind      string        // selects the adapter: "alphavantage"
	BaseURL   string        // endpoint the adapter will call
	APIKey    string        // credential passed to the adapter
	Priority  int           // lower is tried first
	Enabled   bool          // disabled providers are never called
	Timeout   time.Duration // per-request budget
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Fetcher is what an adapter must implement to serve monthly history.
type Fetcher interface {
	MonthlyHistory(ctx context.Context, symbol string) ([]market.MonthlyRecord, error)
}

// Factory builds a Fetcher from stored configuration.
type Factory func(Config) (Fetcher, error)

// Registry maps a stored `kind` to the adapter that implements it.
//
// A replacement provider is a new entry here plus one adapter package; the
// annual workflow and both transports are untouched.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds a kind. Registering a kind twice replaces it, which keeps
// wiring order from mattering.
func (r *Registry) Register(kind string, f Factory) {
	r.factories[strings.ToLower(strings.TrimSpace(kind))] = f
}

// Kinds lists the registered kinds, for validation and for error messages
// that tell an operator what they may actually choose.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	return out
}

// Knows reports whether a kind is registered.
func (r *Registry) Knows(kind string) bool {
	_, ok := r.factories[strings.ToLower(strings.TrimSpace(kind))]
	return ok
}

// Build constructs the adapter for a config.
func (r *Registry) Build(cfg Config) (Fetcher, error) {
	f, ok := r.factories[strings.ToLower(strings.TrimSpace(cfg.Kind))]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, cfg.Kind)
	}
	return f(cfg)
}

// Validate checks a configuration before it is stored. Rejecting bad input at
// write time means the fetch path never has to cope with it.
func Validate(cfg *Config, known *Registry) error {
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Kind = strings.ToLower(strings.TrimSpace(cfg.Kind))
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)

	switch {
	case cfg.Name == "":
		return fmt.Errorf("%w: name is required", ErrInvalidConfig)
	case len(cfg.Name) > 64:
		return fmt.Errorf("%w: name must be 64 characters or fewer", ErrInvalidConfig)
	case cfg.Kind == "":
		return fmt.Errorf("%w: kind is required", ErrInvalidConfig)
	case known != nil && !known.Knows(cfg.Kind):
		return fmt.Errorf("%w: %q (known kinds: %s)", ErrUnknownKind, cfg.Kind,
			strings.Join(known.Kinds(), ", "))
	}

	// The server will issue requests to this URL with a stored credential, so
	// it must at least be a well-formed absolute http(s) endpoint.
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%w: base_url must be an absolute URL", ErrInvalidConfig)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: base_url scheme must be http or https", ErrInvalidConfig)
	}

	if cfg.Priority < 0 {
		return fmt.Errorf("%w: priority must not be negative", ErrInvalidConfig)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Timeout > time.Minute {
		return fmt.Errorf("%w: timeout must be one minute or less", ErrInvalidConfig)
	}
	return nil
}
