package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/upstream"
)

// fakeFBConfigStore is a minimal in-memory store for the /fb/config handler.
type fakeFBConfigStore struct {
	m map[string]string
}

func (f *fakeFBConfigStore) GetFBConfig(field string) (string, error) {
	if f.m == nil {
		return "", nil
	}
	return f.m[field], nil
}

func (f *fakeFBConfigStore) SetFBConfig(field, value string) error {
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[field] = value
	return nil
}

func setupFBConfigTest() (*gin.Engine, *fakeFBConfigStore) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	st := &fakeFBConfigStore{}
	h := HandleFBConfig(st)
	r.GET("/fb/config", h)
	r.PUT("/fb/config", h)
	return r, st
}

func TestHandleFBConfigGet(t *testing.T) {
	r, _ := setupFBConfigTest()
	orig := upstream.FreebuffAPIBase()

	// GET returns the effective + persisted value.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fb/config", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp["api_base"] != orig {
		t.Fatalf("GET api_base = %v, want %v", resp["api_base"], orig)
	}
	if resp["device_base"] != upstream.FREEBUFF_DEVICE_BASE {
		t.Fatalf("GET device_base = %v, want %v", resp["device_base"], upstream.FREEBUFF_DEVICE_BASE)
	}
}

func TestHandleFBConfigPutPersists(t *testing.T) {
	r, st := setupFBConfigTest()
	orig := upstream.FreebuffAPIBase()
	defer upstream.SetFreebuffAPIBase(orig)

	body, _ := json.Marshal(map[string]string{"api_base": "https://1.1.1.1"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/fb/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := upstream.FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("in-memory base not updated: %q", got)
	}
	if st.m["api_base"] != "https://1.1.1.1" {
		t.Fatalf("not persisted to store: %v", st.m)
	}
}

func TestHandleFBConfigPutValidation(t *testing.T) {
	r, st := setupFBConfigTest()
	orig := upstream.FreebuffAPIBase()
	defer upstream.SetFreebuffAPIBase(orig)

	// Empty api_base → 400.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/fb/config", bytes.NewReader([]byte(`{"api_base":""}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty api_base status %d, want 400", w.Code)
	}

	// Non-http(s) scheme → 400.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/fb/config", bytes.NewReader([]byte(`{"api_base":"ftp://x"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad scheme status %d, want 400", w.Code)
	}

	// http:// (insecure) → 400 by default (https-only policy).
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/fb/config", bytes.NewReader([]byte(`{"api_base":"http://1.1.1.1"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("http scheme status %d, want 400 (https-only)", w.Code)
	}

	// path in URL → 400 (bare origin only).
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/fb/config", bytes.NewReader([]byte(`{"api_base":"https://1.1.1.1/evil/path"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("path-in-url status %d, want 400", w.Code)
	}

	// Base unchanged after rejected sets + nothing persisted.
	if got := upstream.FreebuffAPIBase(); got != orig {
		t.Fatalf("rejected set changed base to %q (want %q)", got, orig)
	}
	if st.m["api_base"] != "" {
		t.Fatalf("rejected set persisted something: %v", st.m)
	}
	if w.Code == http.StatusBadRequest && w.Body.Len() == 0 {
		t.Fatalf("error body should contain a message: %s", w.Body.String())
	}
}

// TestHandleFBConfigPutPersistFailure verifies the persist-first ordering:
// when Redis persist fails, the in-memory base must NOT change (no
// split-brain between running state and persisted state).
type failingFBConfigStore struct {
	fakeFBConfigStore
	fail bool
}

func (f *failingFBConfigStore) SetFBConfig(field, value string) error {
	if f.fail {
		return errPersistFail
	}
	return f.fakeFBConfigStore.SetFBConfig(field, value)
}

var errPersistFail = &persistFailErr{}

type persistFailErr struct{}

func (e *persistFailErr) Error() string { return "redis down" }

func TestHandleFBConfigPutPersistFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	st := &failingFBConfigStore{fail: true}
	h := HandleFBConfig(st)
	r.PUT("/fb/config", h)

	orig := upstream.FreebuffAPIBase()
	defer upstream.SetFreebuffAPIBase(orig)

	body, _ := json.Marshal(map[string]string{"api_base": "https://1.1.1.1"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/fb/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("persist-failure status %d, want 500", w.Code)
	}
	if got := upstream.FreebuffAPIBase(); got != orig {
		t.Fatalf("in-memory base changed on persist failure: got %q, want %q (split-brain!)", got, orig)
	}
}
