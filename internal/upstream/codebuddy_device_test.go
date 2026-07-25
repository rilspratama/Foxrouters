package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// makeJWTWithClaims builds a minimal unsigned JWT with arbitrary claims.
func makeJWTWithClaims(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".sig"
}

// --- StartDeviceAuth ---

func TestStartDeviceAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v2/plugin/auth/state" {
			t.Errorf("expected /v2/plugin/auth/state, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("platform") != "CLI" {
			t.Errorf("expected platform=CLI, got %q", r.URL.Query().Get("platform"))
		}
		// Verify no-auth headers
		for _, h := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
			if r.Header.Get(h) == "" {
				t.Errorf("missing required header %s", h)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"state":   "test-state-uuid",
				"authUrl": "https://www.codebuddy.ai/login?platform=CLI&state=test-state-uuid",
			},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := StartDeviceAuth("")
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	if res.State != "test-state-uuid" {
		t.Errorf("state = %q, want test-state-uuid", res.State)
	}
	if !strings.Contains(res.AuthURL, "test-state-uuid") {
		t.Errorf("authUrl missing state: %s", res.AuthURL)
	}
}

func TestStartDeviceAuthStateFromURL(t *testing.T) {
	// Server returns authUrl but no state in body — should extract from URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"authUrl": "https://www.codebuddy.ai/login?platform=CLI&state=extracted-state-123",
			},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := StartDeviceAuth("CLI")
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	if res.State != "extracted-state-123" {
		t.Errorf("state = %q, want extracted-state-123", res.State)
	}
}

func TestStartDeviceAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	_, err := StartDeviceAuth("CLI")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

func TestStartDeviceAuthBadCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 1001,
			"msg":  "rate limited",
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	_, err := StartDeviceAuth("CLI")
	if err == nil {
		t.Fatal("expected error for code!=0")
	}
	if !strings.Contains(err.Error(), "1001") {
		t.Errorf("error should mention code 1001: %v", err)
	}
}

// --- PollDeviceAuth ---

func TestPollDeviceAuthReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v2/plugin/auth/token" {
			t.Errorf("expected /v2/plugin/auth/token, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("state") != "my-state" {
			t.Errorf("expected state=my-state, got %q", r.URL.Query().Get("state"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  "at_abc123",
				"refreshToken": "rt_def456",
				"expiresIn":     31535929,
				"nickname":     "testuser",
				"email":        "user@example.com",
			},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := PollDeviceAuth("my-state")
	if err != nil {
		t.Fatalf("PollDeviceAuth: %v", err)
	}
	if res.Status != "ready" {
		t.Fatalf("status = %q, want ready", res.Status)
	}
	if res.AccessToken != "at_abc123" {
		t.Errorf("accessToken = %q", res.AccessToken)
	}
	if res.RefreshToken != "rt_def456" {
		t.Errorf("refreshToken = %q", res.RefreshToken)
	}
	if res.ExpiresIn != 31535929 {
		t.Errorf("expiresIn = %d", res.ExpiresIn)
	}
	if res.Email != "user@example.com" {
		t.Errorf("email = %q", res.Email)
	}
	if res.Nickname != "testuser" {
		t.Errorf("nickname = %q", res.Nickname)
	}
}

func TestPollDeviceAuthReadySnakeCase(t *testing.T) {
	// Verify snake_case fallbacks work.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"access_token":  "at_snake",
				"refresh_token": "rt_snake",
				"expires_in":    7200,
			},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := PollDeviceAuth("st")
	if err != nil {
		t.Fatalf("PollDeviceAuth: %v", err)
	}
	if res.Status != "ready" {
		t.Fatalf("status = %q, want ready", res.Status)
	}
	if res.AccessToken != "at_snake" {
		t.Errorf("accessToken = %q, want at_snake", res.AccessToken)
	}
	if res.RefreshToken != "rt_snake" {
		t.Errorf("refreshToken = %q, want rt_snake", res.RefreshToken)
	}
	if res.ExpiresIn != 7200 {
		t.Errorf("expiresIn = %d, want 7200", res.ExpiresIn)
	}
}

func TestPollDeviceAuthPending(t *testing.T) {
	// code=0 but no tokens → pending
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := PollDeviceAuth("st")
	if err != nil {
		t.Fatalf("PollDeviceAuth: %v", err)
	}
	if res.Status != "pending" {
		t.Errorf("status = %q, want pending", res.Status)
	}
}

func TestPollDeviceAuth404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := PollDeviceAuth("st")
	if err != nil {
		t.Fatalf("PollDeviceAuth: %v", err)
	}
	if res.Status != "pending" {
		t.Errorf("status = %q, want pending for 404", res.Status)
	}
}

func TestPollDeviceAuthEmptyState(t *testing.T) {
	res, err := PollDeviceAuth("")
	if err != nil {
		t.Fatalf("PollDeviceAuth: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("status = %q, want error", res.Status)
	}
}

func TestPollDeviceAuthEmailFromJWT(t *testing.T) {
	// No email in poll response — should extract from JWT claims.
	jwt := makeJWTWithClaims(map[string]any{
		"email":               "fromjwt@example.com",
		"preferred_username":  "prefuser",
		"exp":                 time.Now().Add(3600 * time.Second).Unix(),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  jwt,
				"refreshToken": "rt",
				"expiresIn":    3600,
				"nickname":     "nick",
			},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	res, err := PollDeviceAuth("st")
	if err != nil {
		t.Fatalf("PollDeviceAuth: %v", err)
	}
	if res.Status != "ready" {
		t.Fatalf("status = %q, want ready", res.Status)
	}
	if res.Email != "fromjwt@example.com" {
		t.Errorf("email = %q, want fromjwt@example.com", res.Email)
	}
}

// --- ParseJWTEmail ---

func TestParseJWTEmail(t *testing.T) {
	jwt := makeJWTWithClaims(map[string]any{
		"email": "test@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	got := ParseJWTEmail(jwt)
	if got != "test@example.com" {
		t.Errorf("ParseJWTEmail = %q, want test@example.com", got)
	}
}

func TestParseJWTEmailPreferredUsername(t *testing.T) {
	jwt := makeJWTWithClaims(map[string]any{
		"preferred_username": "pref@example.com",
	})
	got := ParseJWTEmail(jwt)
	if got != "pref@example.com" {
		t.Errorf("ParseJWTEmail = %q, want pref@example.com", got)
	}
}

func TestParseJWTEmailNoEmail(t *testing.T) {
	jwt := makeJWTWithClaims(map[string]any{
		"sub": "12345",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	got := ParseJWTEmail(jwt)
	if got != "" {
		t.Errorf("ParseJWTEmail = %q, want empty", got)
	}
}

func TestParseJWTEmailInvalidToken(t *testing.T) {
	got := ParseJWTEmail("not-a-jwt")
	if got != "" {
		t.Errorf("ParseJWTEmail = %q, want empty for invalid token", got)
	}
}

// --- ResolveOAuthImportEmail ---

func TestResolveOAuthImportEmailExplicit(t *testing.T) {
	got := ResolveOAuthImportEmail("explicit@example.com", "at", "nick", "uid123")
	if got != "explicit@example.com" {
		t.Errorf("ResolveOAuthImportEmail = %q, want explicit@example.com", got)
	}
}

func TestResolveOAuthImportEmailFromJWT(t *testing.T) {
	jwt := makeJWTWithClaims(map[string]any{
		"email": "jwt@example.com",
	})
	got := ResolveOAuthImportEmail("", jwt, "", "")
	if got != "jwt@example.com" {
		t.Errorf("ResolveOAuthImportEmail = %q, want jwt@example.com", got)
	}
}

func TestResolveOAuthImportEmailNickname(t *testing.T) {
	got := ResolveOAuthImportEmail("", "non-jwt-token", "mynick", "")
	if got != "mynick@oauth.local" {
		t.Errorf("ResolveOAuthImportEmail = %q, want mynick@oauth.local", got)
	}
}

func TestResolveOAuthImportEmailNicknameIsEmail(t *testing.T) {
	got := ResolveOAuthImportEmail("", "non-jwt", "nick@example.com", "")
	if got != "nick@example.com" {
		t.Errorf("ResolveOAuthImportEmail = %q, want nick@example.com", got)
	}
}

func TestResolveOAuthImportEmailUID(t *testing.T) {
	got := ResolveOAuthImportEmail("", "non-jwt", "", "user123")
	if got != "user123@oauth.local" {
		t.Errorf("ResolveOAuthImportEmail = %q, want user123@oauth.local", got)
	}
}

func TestResolveOAuthImportEmailFallback(t *testing.T) {
	got := ResolveOAuthImportEmail("", "shorttoken", "", "")
	if !strings.HasSuffix(got, "@oauth.local") {
		t.Errorf("ResolveOAuthImportEmail = %q, should end with @oauth.local", got)
	}
}

// --- Platform parameter ---

func TestStartDeviceAuthCustomPlatform(t *testing.T) {
	var receivedPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPlatform = r.URL.Query().Get("platform")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"state":   "st",
				"authUrl": "https://example.com/login?state=st",
			},
		})
	}))
	defer srv.Close()

	restore := swapClients(srv.URL)
	defer restore()

	_, err := StartDeviceAuth("external")
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	if receivedPlatform != "external" {
		t.Errorf("platform = %q, want external", receivedPlatform)
	}
}
