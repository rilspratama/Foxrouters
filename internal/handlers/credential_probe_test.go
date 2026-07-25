package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// rewriteTransport rewrites host to the test server (same pattern as
// internal/upstream oauth tests) so CB_UPSTREAM_URL / XAI_UPSTREAM_URL paths
// still hit the httptest server.
type rewriteTransport struct {
	base string
	next http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	baseReq, err := http.NewRequest("GET", t.base, nil)
	if err != nil {
		return nil, err
	}
	u.Scheme = baseReq.URL.Scheme
	u.Host = baseReq.URL.Host
	newReq := req.Clone(req.Context())
	newReq.URL = &u
	newReq.Host = u.Host
	return t.next.RoundTrip(newReq)
}

func swapHealthClient(baseURL string) (restore func()) {
	orig := upstream.HealthCheckClient()
	c := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteTransport{base: baseURL, next: http.DefaultTransport},
	}
	upstream.SetHealthCheckClient(c)
	// Also swap the shared upstream/token clients so EnsureValid refresh (if
	// triggered) stays inside the test server.
	origUp := upstream.UpstreamClient()
	origTok := upstream.TokenRefreshClient()
	upstream.SetUpstreamClient(c)
	upstream.SetTokenRefreshClient(c)
	return func() {
		upstream.SetHealthCheckClient(orig)
		upstream.SetUpstreamClient(origUp)
		upstream.SetTokenRefreshClient(origTok)
	}
}

func TestHandleTestCBKey_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}],\"usage\":{\"credit\":0.01}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	restore := swapHealthClient(srv.URL)
	defer restore()

	km := upstream.NewCBKeyManager(nil)
	km.SetKeysForTest([]*upstream.CBKey{
		upstream.NewCBKeyForTest("ck_handler_test_keyxx"),
	})

	r := gin.New()
	r.POST("/cb/keys/test", HandleTestCBKey(km))

	body := []byte(`{"key":"ck_handler_test_keyxx"}`)
	req := httptest.NewRequest("POST", "/cb/keys/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("json: %v", err)
	}
	if res["ok"] != true {
		t.Fatalf("ok=%v full=%v", res["ok"], res)
	}
	if res["model"] != "gpt-5.5" {
		t.Fatalf("model=%v", res["model"])
	}
	if res["content"] != "OK" {
		t.Fatalf("content=%v", res["content"])
	}
}

func TestHandleTestCBKey_MaskedAndMissing(t *testing.T) {
	km := upstream.NewCBKeyManager(nil)
	full := "ck_abcdefghijklmnop"
	km.SetKeysForTest([]*upstream.CBKey{upstream.NewCBKeyForTest(full)})

	// Missing identity → 400
	{
		r := gin.New()
		r.POST("/cb/keys/test", HandleTestCBKey(km))
		req := httptest.NewRequest("POST", "/cb/keys/test", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("empty body status=%d", w.Code)
		}
	}

	// Unknown key → 404
	{
		r := gin.New()
		r.POST("/cb/keys/test", HandleTestCBKey(km))
		req := httptest.NewRequest("POST", "/cb/keys/test", bytes.NewReader([]byte(`{"key":"ck_nope"}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 404 {
			t.Fatalf("missing key status=%d", w.Code)
		}
	}

	// Masked key resolves → needs live upstream; mock it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	restore := swapHealthClient(srv.URL)
	defer restore()

	r := gin.New()
	r.POST("/cb/keys/test", HandleTestCBKey(km))
	masked := full[:8] + "..." + full[len(full)-4:]
	req := httptest.NewRequest("POST", "/cb/keys/test", bytes.NewReader([]byte(`{"key":"`+masked+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("masked status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Fatalf("masked ok=%v", res)
	}
}

func TestHandleTestCBKey_EmailOAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"oauth-ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	restore := swapHealthClient(srv.URL)
	defer restore()

	km := upstream.NewCBKeyManager(nil)
	km.SetKeysForTest([]*upstream.CBKey{
		upstream.NewCBKeyForTest("o@ex.com",
			upstream.WithCBOAuthTokens("at1", "rt1", time.Now().Add(time.Hour))),
	})

	r := gin.New()
	r.POST("/cb/keys/test", HandleTestCBKey(km))
	req := httptest.NewRequest("POST", "/cb/keys/test", bytes.NewReader([]byte(`{"email":"o@ex.com"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["ok"] != true || res["cred_type"] != "oauth" {
		t.Fatalf("res=%v", res)
	}
	if res["email"] != "o@ex.com" {
		t.Fatalf("email=%v", res["email"])
	}
}

func TestHandleTestGrokAccount_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer srv.Close()
	restore := swapHealthClient(srv.URL)
	defer restore()

	am := upstream.NewGrokAccountManager(nil)
	am.SetAccountsForTest([]*upstream.GrokAccount{
		upstream.NewGrokAccountForTest("g@x.ai", "atok", "rtok"),
	})

	r := gin.New()
	r.POST("/accounts/test", HandleTestGrokAccount(am))
	req := httptest.NewRequest("POST", "/accounts/test", bytes.NewReader([]byte(`{"email":"g@x.ai"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Fatalf("res=%v", res)
	}
	if res["model"] != "grok-4.5" || res["email"] != "g@x.ai" {
		t.Fatalf("res=%v", res)
	}
	if res["content"] != "OK" {
		t.Fatalf("content=%v", res["content"])
	}
}

func TestHandleTestGrokAccount_Missing(t *testing.T) {
	am := upstream.NewGrokAccountManager(nil)
	r := gin.New()
	r.POST("/accounts/test", HandleTestGrokAccount(am))

	req := httptest.NewRequest("POST", "/accounts/test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("empty email status=%d", w.Code)
	}

	req = httptest.NewRequest("POST", "/accounts/test", bytes.NewReader([]byte(`{"email":"nope@x.ai"}`)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("missing account status=%d", w.Code)
	}
}

func TestHandleTestGrokAccount_UpstreamFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":"forbidden"}`)
	}))
	defer srv.Close()
	restore := swapHealthClient(srv.URL)
	defer restore()

	am := upstream.NewGrokAccountManager(nil)
	am.SetAccountsForTest([]*upstream.GrokAccount{
		upstream.NewGrokAccountForTest("bad@x.ai", "atok", "rtok"),
	})

	r := gin.New()
	r.POST("/accounts/test", HandleTestGrokAccount(am))
	req := httptest.NewRequest("POST", "/accounts/test", bytes.NewReader([]byte(`{"email":"bad@x.ai"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Handler always returns 200 with ok=false for upstream failures.
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["ok"] != false {
		t.Fatalf("expected ok=false, got %v", res)
	}
	if int(res["status"].(float64)) != 403 {
		t.Fatalf("status field=%v", res["status"])
	}
}
