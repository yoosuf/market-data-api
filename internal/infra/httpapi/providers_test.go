package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yoosuf/market-data-api/internal/core/annual"
	"github.com/yoosuf/market-data-api/internal/core/provider"
	"github.com/yoosuf/market-data-api/internal/infra/httpapi"
	"github.com/yoosuf/market-data-api/internal/infra/sqlite"
	"github.com/yoosuf/market-data-api/internal/shared/markettest"
)

// newAdminAPI wires the real store and the real registry, so the tests cover
// validation against the kinds this build can actually serve.
func newAdminAPI(t *testing.T) (http.Handler, *provider.Store) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := annual.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := provider.Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	annualStore := annual.NewSQLite(db)
	providerStore := provider.New(db)
	now := markettest.Now()
	svc := annual.New(annualStore, markettest.NewProvider(nil), annual.Config{Now: now})
	h := httpapi.New(svc, nil, now,
		httpapi.WithProviderAdmin(providerStore, provider.DefaultRegistry()))
	return h, providerStore
}

func do(t *testing.T, h http.Handler, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			r = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			r = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = resp.Body.Close()
	return resp, raw
}

func validCreate() map[string]any {
	return map[string]any{
		"name":       "primary",
		"kind":       "alphavantage",
		"base_url":   "https://www.alphavantage.co/query",
		"api_key":    "secret-key",
		"priority":   10,
		"enabled":    true,
		"timeout_ms": 8000,
	}
}

func TestProviderCreateReadUpdateDelete(t *testing.T) {
	h, _ := newAdminAPI(t)

	resp, raw := do(t, h, http.MethodPost, "/admin/providers", validCreate())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := int64(created["id"].(float64))
	if id == 0 {
		t.Fatal("no id assigned")
	}
	if created["priority"].(float64) != 10 || created["timeout_ms"].(float64) != 8000 {
		t.Errorf("fields not stored: %+v", created)
	}

	resp, raw = do(t, h, http.MethodGet, "/admin/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var listed struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Providers) != 1 {
		t.Fatalf("listed %d providers, want 1", len(listed.Providers))
	}

	resp, _ = do(t, h, http.MethodGet, "/admin/providers/"+strconv.FormatInt(id, 10), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}

	body := validCreate()
	body["name"] = "renamed"
	body["enabled"] = false
	resp, raw = do(t, h, http.MethodPut, "/admin/providers/"+strconv.FormatInt(id, 10), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, body %s", resp.StatusCode, raw)
	}
	var updated map[string]any
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated["name"] != "renamed" || updated["enabled"] != false {
		t.Errorf("update not applied: %+v", updated)
	}

	resp, _ = do(t, h, http.MethodDelete, "/admin/providers/"+strconv.FormatInt(id, 10), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp, _ = do(t, h, http.MethodGet, "/admin/providers/"+strconv.FormatInt(id, 10), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", resp.StatusCode)
	}
}

// Documents the configured behavior: the key is returned in full on read.
// Anyone who can reach these routes can read every provider credential.
func TestAPIKeyIsReturnedInFull(t *testing.T) {
	h, _ := newAdminAPI(t)

	if resp, raw := do(t, h, http.MethodPost, "/admin/providers", validCreate()); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	_, raw := do(t, h, http.MethodGet, "/admin/providers", nil)
	if !strings.Contains(string(raw), "secret-key") {
		t.Fatalf("api_key not returned: %s", raw)
	}
}

func TestProviderValidation(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing name":   func(m map[string]any) { delete(m, "name") },
		"missing kind":   func(m map[string]any) { delete(m, "kind") },
		"unknown kind":   func(m map[string]any) { m["kind"] = "bloomberg" },
		"relative url":   func(m map[string]any) { m["base_url"] = "/query" },
		"missing scheme": func(m map[string]any) { m["base_url"] = "example.test" },
		"file scheme":    func(m map[string]any) { m["base_url"] = "file:///etc/passwd" },
		"negative prio":  func(m map[string]any) { m["priority"] = -5 },
		"absurd timeout": func(m map[string]any) { m["timeout_ms"] = 3_600_000 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newAdminAPI(t)
			body := validCreate()
			mutate(body)
			resp, raw := do(t, h, http.MethodPost, "/admin/providers", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", resp.StatusCode, raw)
			}
		})
	}
}

// A misspelled field must not be silently ignored, leaving an operator
// believing they configured something they did not.
func TestUnknownFieldsAreRejected(t *testing.T) {
	h, _ := newAdminAPI(t)
	body := validCreate()
	body["timout_ms"] = 5000 // typo
	resp, _ := do(t, h, http.MethodPost, "/admin/providers", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDuplicateNameIsConflict(t *testing.T) {
	h, _ := newAdminAPI(t)
	if resp, _ := do(t, h, http.MethodPost, "/admin/providers", validCreate()); resp.StatusCode != http.StatusCreated {
		t.Fatal("first create failed")
	}
	resp, _ := do(t, h, http.MethodPost, "/admin/providers", validCreate())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestMalformedBodyAndID(t *testing.T) {
	h, _ := newAdminAPI(t)

	if resp, _ := do(t, h, http.MethodPost, "/admin/providers", `{nope`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: %d", resp.StatusCode)
	}
	if resp, _ := do(t, h, http.MethodGet, "/admin/providers/abc", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: %d", resp.StatusCode)
	}
	if resp, _ := do(t, h, http.MethodGet, "/admin/providers/0", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("zero id: %d", resp.StatusCode)
	}
}

// Defaults keep a minimal create body usable.
func TestCreateDefaults(t *testing.T) {
	h, _ := newAdminAPI(t)
	resp, raw := do(t, h, http.MethodPost, "/admin/providers", map[string]any{
		"name":     "minimal",
		"kind":     "alphavantage",
		"base_url": "https://example.test/query",
		"api_key":  "k",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != true {
		t.Error("enabled should default to true")
	}
	if got["timeout_ms"].(float64) != 10000 {
		t.Errorf("timeout_ms = %v, want 10000", got["timeout_ms"])
	}
	if got["priority"].(float64) != 100 {
		t.Errorf("priority = %v, want 100", got["priority"])
	}
}

// The admin surface is opt-in: without the option, the routes do not exist.
func TestAdminRoutesAbsentWhenNotMounted(t *testing.T) {
	h := newAPI(t, markettest.NewProvider(nil))
	if resp := get(t, h, "/admin/providers"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
