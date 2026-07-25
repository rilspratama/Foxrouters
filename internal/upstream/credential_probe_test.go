package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetByKeyAndEmail(t *testing.T) {
	km := NewCBKeyManager(nil)
	api := NewCBKeyForTest("ck_abcdefghijklmnop")
	oauth := NewCBKeyForTest("user@example.com",
		WithCBOAuthTokens("at_live", "rt_live", time.Now().Add(time.Hour)))
	km.SetKeysForTest([]*CBKey{api, oauth})

	if got := km.GetByKey("ck_abcdefghijklmnop"); got == nil || got.Key != api.Key {
		t.Fatalf("full key resolve failed: %+v", got)
	}
	if got := km.GetByKey("ck_abcde...mnop"); got == nil || got.Key != api.Key {
		t.Fatalf("masked key resolve failed: %+v", got)
	}
	if got := km.GetByKey("user@example.com"); got == nil || got.Key != oauth.Key {
		t.Fatalf("oauth email resolve failed: %+v", got)
	}
	if got := km.GetByKey("missing"); got != nil {
		t.Fatalf("expected nil for missing key, got %+v", got)
	}

	am := NewGrokAccountManager(nil)
	acc := NewGrokAccountForTest("g@x.com", "atok", "rtok")
	am.SetAccountsForTest([]*GrokAccount{acc})
	if got := am.GetByEmail("g@x.com"); got == nil || got.Email != "g@x.com" {
		t.Fatalf("GetByEmail failed: %+v", got)
	}
	if got := am.GetByEmail("nope"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestTestCBKeyAPIKeyOK(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}],\"usage\":{\"credit\":0.01}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()
	// Also redirect healthCheckClient used by probes.
	origHealth := healthCheckClient
	healthCheckClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL, next: http.DefaultTransport},
	}
	defer func() { healthCheckClient = origHealth }()

	k := NewCBKeyForTest("ck_probe_api_key_xxxx")
	res := TestCBKey(k)
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	if res.Status != 200 {
		t.Fatalf("status=%d", res.Status)
	}
	if res.Content != "OK" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.Credit != 0.01 {
		t.Fatalf("credit=%v", res.Credit)
	}
	if res.CredType != string(CBAuthAPIKey) {
		t.Fatalf("cred_type=%q", res.CredType)
	}
	if res.Model != "gpt-5.5" {
		t.Fatalf("model=%q", res.Model)
	}
	if gotAuth != "Bearer ck_probe_api_key_xxxx" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotBody["model"] != "gpt-5.5" {
		t.Fatalf("body model=%v", gotBody["model"])
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream=%v", gotBody["stream"])
	}
}

func TestTestCBKeyOAuthOKAndDisabledStillTested(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()
	origHealth := healthCheckClient
	healthCheckClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL, next: http.DefaultTransport},
	}
	defer func() { healthCheckClient = origHealth }()

	k := NewCBKeyForTest("disabled@ex.com",
		WithCBOAuthTokens("at_oauth_token", "rt_oauth", time.Now().Add(time.Hour)),
		WithCBDisabledCooldown(time.Time{})) // permanent disable
	if !k.IsDisabled() {
		t.Fatal("expected permanently disabled")
	}
	res := TestCBKey(k)
	if !res.OK {
		t.Fatalf("disabled key should still be tested; got %+v", res)
	}
	if res.Email != "disabled@ex.com" {
		t.Fatalf("email=%q", res.Email)
	}
	if res.CredType != string(CBAuthOAuth) {
		t.Fatalf("cred_type=%q", res.CredType)
	}
	if gotAuth != "Bearer at_oauth_token" {
		t.Fatalf("auth=%q want Bearer at", gotAuth)
	}
}

func TestTestCBKeyUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()
	origHealth := healthCheckClient
	healthCheckClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL, next: http.DefaultTransport},
	}
	defer func() { healthCheckClient = origHealth }()

	k := NewCBKeyForTest("ck_bad_key_yyyyyyyy")
	res := TestCBKey(k)
	if res.OK {
		t.Fatalf("expected fail, got ok: %+v", res)
	}
	if res.Status != 401 {
		t.Fatalf("status=%d", res.Status)
	}
	if res.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestTestGrokAccountOK(t *testing.T) {
	var gotAuth, gotUA, gotVer, gotID string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path=%s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotVer = r.Header.Get("x-grok-client-version")
		gotID = r.Header.Get("x-grok-client-identifier")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()
	origHealth := healthCheckClient
	healthCheckClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL, next: http.DefaultTransport},
	}
	defer func() { healthCheckClient = origHealth }()

	acc := NewGrokAccountForTest("probe@x.ai", "grok_access_tok", "rt")
	res := TestGrokAccount(acc)
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	if res.Status != 200 || res.Content != "OK" {
		t.Fatalf("status=%d content=%q", res.Status, res.Content)
	}
	if res.Email != "probe@x.ai" {
		t.Fatalf("email=%q", res.Email)
	}
	if res.Model != "grok-4.5" {
		t.Fatalf("model=%q", res.Model)
	}
	if gotAuth != "Bearer grok_access_tok" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "grok-shell/") {
		t.Fatalf("ua=%q", gotUA)
	}
	if gotVer != GROK_CLIENT_VERSION || gotID != GROK_CLIENT_IDENTIFIER {
		t.Fatalf("headers ver=%q id=%q", gotVer, gotID)
	}
	if gotBody["model"] != "grok-4.5" || gotBody["stream"] != false {
		t.Fatalf("body=%v", gotBody)
	}
}

func TestTestGrokAccountUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"bad token"}`)
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()
	origHealth := healthCheckClient
	healthCheckClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL, next: http.DefaultTransport},
	}
	defer func() { healthCheckClient = origHealth }()

	acc := NewGrokAccountForTest("bad@x.ai", "dead", "rt")
	res := TestGrokAccount(acc)
	if res.OK || res.Status != 401 {
		t.Fatalf("expected 401 fail, got %+v", res)
	}
}

func TestParseCBProbeStream(t *testing.T) {
	r := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"usage\":{\"credit\":0.02}}\n\n" +
			"data: [DONE]\n\n",
	)
	content, credit := parseCBProbeStream(r)
	if content != "Hello" {
		t.Fatalf("content=%q", content)
	}
	if credit != 0.02 {
		t.Fatalf("credit=%v", credit)
	}
}
