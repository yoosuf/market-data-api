package graphqlapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/graphql-go/graphql"
)

// maxBody bounds a query document. GraphQL requests are small; anything
// larger is a mistake or an attack.
const maxBody = 64 << 10

// New builds the GraphQL HTTP handler. It accepts POST with a JSON body and
// GET with query parameters, which is what the GraphQL-over-HTTP convention
// expects and what makes a query shareable as a URL.
func New(svc AnnualService, log *slog.Logger, now func() time.Time) (http.Handler, error) {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	schema, err := newSchema(svc, now)
	if err != nil {
		return nil, err
	}
	return &handler{schema: schema, log: log}, nil
}

type handler struct {
	schema graphql.Schema
	log    *slog.Logger
}

type request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, err := parseRequest(w, r)
	if err != nil {
		// A request that cannot be parsed never reached execution, so it gets
		// a 400. Errors raised *during* execution return 200 with an errors
		// array, as GraphQL clients expect.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]any{{
				"message":    err.Error(),
				"extensions": map[string]any{"code": codeBadInput},
			}},
		})
		return
	}

	result := graphql.Do(graphql.Params{
		Schema:         h.schema,
		RequestString:  req.Query,
		VariableValues: req.Variables,
		OperationName:  req.OperationName,
		Context:        r.Context(),
	})

	for _, e := range result.Errors {
		h.log.WarnContext(r.Context(), "graphql error", "message", e.Message, "path", e.Path)
	}
	writeJSON(w, http.StatusOK, result)
}

func parseRequest(w http.ResponseWriter, r *http.Request) (request, error) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		req := request{
			Query:         q.Get("query"),
			OperationName: q.Get("operationName"),
		}
		if raw := q.Get("variables"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &req.Variables); err != nil {
				return request{}, errors.New("variables must be a JSON object")
			}
		}
		if req.Query == "" {
			return request{}, errors.New("missing query parameter")
		}
		return req, nil

	case http.MethodPost:
		if ct := r.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
			return request{}, errors.New("content-type must be application/json")
		}
		var req request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		if err := dec.Decode(&req); err != nil {
			return request{}, errors.New("request body must be a JSON object")
		}
		if req.Query == "" {
			return request{}, errors.New("missing query field")
		}
		return req, nil

	default:
		return request{}, errors.New("method must be GET or POST")
	}
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || mediaType == "application/graphql+json"
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
