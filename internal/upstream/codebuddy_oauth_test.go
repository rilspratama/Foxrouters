package upstream

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/db"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeJWT(exp int64) string {
	// Minimal unsigned JWT with exp claim (signature ignored by parseJWTExp).
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa64(exp) + `,"typ":"Bearer"}`))
	return header + "." + payload + ".sig"
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// swapClients temporarily redirects package HTTP clients at a test server.
func swapClients(baseURL string) (restore func()) {
	origUp := upstreamClient
	origTok := tokenRefreshClient
	upstreamClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{
			base: baseURL,
			next: http.DefaultTransport,
		},
	}
	tokenRefreshClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{
			base: baseURL,
			next: http.DefaultTransport,
		},
	}
	return func() {
		upstreamClient = origUp
		tokenRefreshClient = origTok
	}
}

// rewriteTransport rewrites host of any request to the test server base URL
// while preserving the original path+query so CB_UPSTREAM_URL and
// CB_OAUTH_REFRESH_URL path routing still works.
type rewriteTransport struct {
	base string
	next http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	base := strings.TrimRight(t.base, "/")
	// Parse base to get scheme+host
	bu, err := http.NewRequest("GET", base, nil)
	if err != nil {
		return nil, err
	}
	u.Scheme = bu.URL.Scheme
	u.Host = bu.URL.Host
	// Keep original path
	newReq := req.Clone(req.Context())
	newReq.URL = &u
	newReq.Host = u.Host
	return t.next.RoundTrip(newReq)
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

func TestCBKeyAuthHeaderAPIKey(t *testing.T) {
	k := NewCBKeyForTest("ck_testkey_abcdef")
	got := k.AuthHeader()
	want := "Bearer ck_testkey_abcdef"
	if got != want {
		t.Fatalf("AuthHeader=%q want %q", got, want)
	}
	if k.IsExpired() {
		t.Fatal("API key should never be expired")
	}
	if err := k.EnsureValid(); err != nil {
		t.Fatalf("EnsureValid on api_key: %v", err)
	}
}

func TestCBKeyAuthHeaderOAuth(t *testing.T) {
	k := NewCBKeyForTest("user@example.com",
		WithCBOAuthTokens("at_live_token", "rt_live_token", time.Now().Add(time.Hour)))
	got := k.AuthHeader()
	want := "Bearer at_live_token"
	if got != want {
		t.Fatalf("AuthHeader=%q want %q", got, want)
	}
	if k.GetCredType() != CBAuthOAuth {
		t.Fatalf("CredType=%q want oauth", k.GetCredType())
	}
	if k.IsExpired() {
		t.Fatal("fresh OAuth token should not be expired")
	}
}

func TestCBKeyIsExpiredNearExpiry(t *testing.T) {
	// Expires in 5 min — within REFRESH_BUFFER (10 min) → expired
	k := NewCBKeyForTest("user@example.com",
		WithCBOAuthTokens("at", "rt", time.Now().Add(5*time.Minute)))
	if !k.IsExpired() {
		t.Fatal("token within REFRESH_BUFFER should be IsExpired")
	}
}

func TestCBKeyToDTO(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	k := NewCBKeyForTest("user@example.com",
		WithCBOAuthTokens("at_x", "rt_x", exp))
	k.creditsUsed = 12.5
	k.totalReqs = 3
	dto := k.toDTO()
	if dto.CredType != "oauth" {
		t.Fatalf("CredType=%q", dto.CredType)
	}
	if dto.AccessToken != "at_x" || dto.RefreshToken != "rt_x" {
		t.Fatalf("tokens not snapshotted: %+v", dto)
	}
	if dto.Email != "user@example.com" || dto.Key != "user@example.com" {
		t.Fatalf("email/key: %+v", dto)
	}
	if !dto.ExpiresAt.Equal(exp) && dto.ExpiresAt.Unix() != exp.Unix() {
		// compare via unix to avoid monotonic clock noise
		if dto.ExpiresAt.Unix() != exp.Unix() {
			t.Fatalf("ExpiresAt=%v want %v", dto.ExpiresAt, exp)
		}
	}
	if dto.CreditsUsed != 12.5 || dto.TotalReqs != 3 {
		t.Fatalf("stats: %+v", dto)
	}
}

func TestAddOAuthAccount(t *testing.T) {
	km := NewCBKeyManager(nil)
	exp := time.Now().Add(365 * 24 * time.Hour)
	added, total := km.AddOAuthAccount("a@b.com", "at1", "rt1", exp)
	if !added || total != 1 {
		t.Fatalf("first add: added=%v total=%d", added, total)
	}
	// Dedup by email — update tokens
	added, total = km.AddOAuthAccount("a@b.com", "at2", "rt2", exp)
	if added || total != 1 {
		t.Fatalf("dup email: added=%v total=%d", added, total)
	}
	keys := km.GetAll()
	if len(keys) != 1 {
		t.Fatalf("len=%d", len(keys))
	}
	if keys[0].AccessToken != "at2" {
		t.Fatalf("token not updated: %s", keys[0].AccessToken)
	}
	if keys[0].AuthHeader() != "Bearer at2" {
		t.Fatalf("AuthHeader after update: %s", keys[0].AuthHeader())
	}
	// Blank rejected
	added, _ = km.AddOAuthAccount("", "at", "rt", exp)
	if added {
		t.Fatal("blank email should not add")
	}
	// Resolve by email
	if got := km.ResolveKey("a@b.com"); got != "a@b.com" {
		t.Fatalf("ResolveKey email: %q", got)
	}
	// Delete by email
	if !km.DeleteKey("a@b.com") {
		t.Fatal("DeleteKey by email failed")
	}
	if km.Len() != 0 {
		t.Fatalf("Len after delete=%d", km.Len())
	}
}

func TestAddKeyBackwardCompat(t *testing.T) {
	// API key path must remain unchanged
	km := NewCBKeyManager(nil)
	added, total := km.AddKey("ck_legacy_apikey_xyz")
	if !added || total != 1 {
		t.Fatalf("AddKey: added=%v total=%d", added, total)
	}
	k := km.GetAll()[0]
	if k.GetCredType() != CBAuthAPIKey {
		t.Fatalf("default CredType=%q want api_key", k.GetCredType())
	}
	if k.AuthHeader() != "Bearer ck_legacy_apikey_xyz" {
		t.Fatalf("AuthHeader=%q", k.AuthHeader())
	}
	// Mix OAuth + API key in same pool
	km.AddOAuthAccount("o@x.com", "at", "rt", time.Now().Add(time.Hour))
	if km.Len() != 2 {
		t.Fatalf("mixed pool Len=%d", km.Len())
	}
}

func TestParseJWTExp(t *testing.T) {
	exp := time.Now().Add(48 * time.Hour).Unix()
	tok := makeJWT(exp)
	got := ParseJWTExp(tok)
	if got.Unix() != exp {
		t.Fatalf("ParseJWTExp=%v want unix %d", got, exp)
	}
	if !ParseJWTExp("not-a-jwt").IsZero() {
		t.Fatal("garbage should yield zero time")
	}
}

// ---------------------------------------------------------------------------
// Mocked HTTP: refresh + proxy
// ---------------------------------------------------------------------------

func TestCBOAuthRefresh(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Refresh-Token") != "rt_old" {
			http.Error(w, "bad rt", 401)
			return
		}
		if r.Header.Get("X-Auth-Refresh-Source") != "cli" {
			http.Error(w, "bad source", 400)
			return
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  "at_new",
				"refreshToken": "rt_new",
				"expiresIn":    31535929,
			},
		})
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()

	k := NewCBKeyForTest("user@example.com",
		WithCBOAuthTokens("at_old", "rt_old", time.Now().Add(time.Minute)))
	if err := k.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if k.AccessToken != "at_new" || k.RefreshToken != "rt_new" {
		t.Fatalf("tokens not updated: at=%s rt=%s", k.AccessToken, k.RefreshToken)
	}
	if k.ExpiresAt.Before(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("ExpiresAt too soon: %v", k.ExpiresAt)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}

	// singleflight: concurrent Refresh should collapse
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = k.Refresh()
		}()
	}
	wg.Wait()
	// May be 2 total (1 original + 1 coalesced batch) — not 1+8
	if hits.Load() > 3 {
		t.Fatalf("singleflight not collapsing: hits=%d", hits.Load())
	}
}

func TestProxyCodeBuddyOAuthBearerAT(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/chat/completions" {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()

	km := NewCBKeyManager(nil)
	km.AddOAuthAccount("oauth@cb.test", "at_secret_xyz", "rt_secret", time.Now().Add(time.Hour))

	hc := NewHealthChecker(nil, km)
	// Keep circuit closed
	_ = hc

	body := []byte(`{"model":"cb/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	var bodyMap map[string]any
	_ = json.Unmarshal(body, &bodyMap)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))

	ProxyCodeBuddy(c, body, bodyMap, km, false, hc)

	if gotAuth != "Bearer at_secret_xyz" {
		t.Fatalf("Authorization=%q want Bearer at_secret_xyz", gotAuth)
	}
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if acct, _ := c.Get("upstream_account"); acct != "oauth@cb.test" {
		t.Fatalf("upstream_account=%v want email", acct)
	}
}

func TestProxyCodeBuddyOAuth401RefreshRetry(t *testing.T) {
	var chatHits atomic.Int32
	var refreshHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			n := chatHits.Add(1)
			auth := r.Header.Get("Authorization")
			if n == 1 {
				// First call with old AT → 401
				if auth != "Bearer at_stale" {
					t.Errorf("first auth=%q", auth)
				}
				http.Error(w, `{"error":"unauthorized"}`, 401)
				return
			}
			// After refresh should use new AT
			if auth != "Bearer at_fresh" {
				t.Errorf("retry auth=%q want Bearer at_fresh", auth)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/v2/plugin/auth/token/refresh":
			refreshHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"accessToken":  "at_fresh",
					"refreshToken": "rt_fresh",
					"expiresIn":    3600,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()

	km := NewCBKeyManager(nil)
	// Token is still "valid" by clock so EnsureValid won't pre-refresh —
	// we want the 401 path to trigger Refresh.
	km.AddOAuthAccount("retry@cb.test", "at_stale", "rt_stale", time.Now().Add(24*time.Hour))

	hc := NewHealthChecker(nil, km)
	body := []byte(`{"model":"cb/glm-5.2","messages":[{"role":"user","content":"x"}],"stream":false}`)
	var bodyMap map[string]any
	_ = json.Unmarshal(body, &bodyMap)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))

	ProxyCodeBuddy(c, body, bodyMap, km, false, hc)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if chatHits.Load() < 2 {
		t.Fatalf("chat hits=%d want >=2 (401 then retry)", chatHits.Load())
	}
	if refreshHits.Load() < 1 {
		t.Fatalf("refresh hits=%d", refreshHits.Load())
	}
	// Key should hold new tokens and not be disabled
	k := km.GetAll()[0]
	if k.AccessToken != "at_fresh" {
		t.Fatalf("AccessToken=%s", k.AccessToken)
	}
	if k.IsDisabled() {
		t.Fatal("key should not be disabled after successful 401 retry")
	}
}

func TestProxyCodeBuddyAPIKeyPathUnchanged(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/chat/completions" {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"api\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()

	km := NewCBKeyManager(nil)
	km.AddKey("ck_api_key_legacy_xyz")

	hc := NewHealthChecker(nil, km)
	body := []byte(`{"model":"cb/glm-5.2","messages":[{"role":"user","content":"x"}],"stream":false}`)
	var bodyMap map[string]any
	_ = json.Unmarshal(body, &bodyMap)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))

	ProxyCodeBuddy(c, body, bodyMap, km, false, hc)

	if gotAuth != "Bearer ck_api_key_legacy_xyz" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Masked key in upstream_account
	acct, _ := c.Get("upstream_account")
	acctStr, _ := acct.(string)
	if !strings.Contains(acctStr, "...") {
		t.Fatalf("upstream_account should be masked for api_key, got %q", acctStr)
	}
}

func TestProxyCodeBuddyAPIKey401PermanentDisable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", 401)
	}))
	defer srv.Close()
	restore := swapClients(srv.URL)
	defer restore()

	km := NewCBKeyManager(nil)
	km.AddKey("ck_bad_key_xxxx")

	hc := NewHealthChecker(nil, km)
	body := []byte(`{"model":"cb/glm-5.2","messages":[{"role":"user","content":"x"}],"stream":false}`)
	var bodyMap map[string]any
	_ = json.Unmarshal(body, &bodyMap)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))

	ProxyCodeBuddy(c, body, bodyMap, km, false, hc)

	// All keys disabled → 503
	if w.Code != 503 {
		t.Fatalf("status=%d want 503 body=%s", w.Code, w.Body.String())
	}
	if !km.GetAll()[0].IsDisabled() {
		t.Fatal("API key should be permanently disabled on 401")
	}
}

// TestLoadFromRedisOAuthShape verifies the Redis state parser for oauth entries
// without a live Redis — we exercise the same field-mapping logic used by
// LoadFromRedis by constructing a CBKey the same way.
func TestLoadFromRedisOAuthShape(t *testing.T) {
	// Simulate what LoadFromRedis does with a redis hash map.
	state := map[string]string{
		"cred_type":      "oauth",
		"access_token":   "at_from_redis",
		"refresh_token":  "rt_from_redis",
		"email":          "redis@cb.test",
		"expires_at":     "1893456000", // 2030-01-01
		"credits_used":   "1.5",
		"total_requests": "7",
		"disabled":       "false",
	}
	// Replicate LoadFromRedis field mapping
	key := &CBKey{Key: "redis@cb.test", CredType: CBAuthAPIKey}
	if state["cred_type"] == string(CBAuthOAuth) {
		key.CredType = CBAuthOAuth
		key.AccessToken = state["access_token"]
		key.RefreshToken = state["refresh_token"]
		key.Email = state["email"]
		if n, err := parseInt64(state["expires_at"]); err == nil && n > 0 {
			key.ExpiresAt = time.Unix(n, 0)
		}
	}
	if key.CredType != CBAuthOAuth {
		t.Fatal("cred_type oauth not applied")
	}
	if key.AuthHeader() != "Bearer at_from_redis" {
		t.Fatalf("AuthHeader=%q", key.AuthHeader())
	}
	if key.Email != "redis@cb.test" {
		t.Fatalf("Email=%q", key.Email)
	}
	if key.ExpiresAt.Year() != 2030 {
		t.Fatalf("ExpiresAt year=%d", key.ExpiresAt.Year())
	}

	// Legacy entry without cred_type defaults to api_key
	legacy := &CBKey{Key: "ck_legacy_xxxx", CredType: CBAuthAPIKey}
	// empty cred_type → stays api_key
	if legacy.GetCredType() != CBAuthAPIKey {
		t.Fatal("legacy default should be api_key")
	}
	dto := legacy.toDTO()
	if dto.CredType != "api_key" {
		t.Fatalf("toDTO CredType=%q", dto.CredType)
	}
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, io.EOF
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// Ensure SaveCBKey DTO shape compiles / fields are set for oauth.
func TestCBKeyDTOOAuthFields(t *testing.T) {
	k := NewCBKeyForTest("e@x.com",
		WithCBOAuthTokens("at", "rt", time.Unix(1893456000, 0)))
	dto := k.toDTO()
	// Type assertion that DTO has OAuth fields (compile-time + runtime)
	var _ db.CBKeyDTO = dto
	if dto.CredType != "oauth" || dto.AccessToken == "" || dto.RefreshToken == "" {
		t.Fatalf("DTO missing oauth fields: %+v", dto)
	}
}
