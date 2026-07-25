package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CodeBuddy OAuth device/login flow (CLI platform).
//
// 1. StartDeviceAuth → POST CB_OAUTH_STATE_URL?platform=CLI
// 2. User opens AuthURL in browser (GitHub/Google)
// 3. PollDeviceAuth  → GET  CB_OAUTH_TOKEN_URL?state=...
//
// No server-side session store: handlers proxy each call. Network I/O
// never runs under any pool mutex.

// DeviceAuthStart is the result of starting a device/login OAuth flow.
type DeviceAuthStart struct {
	State   string `json:"state"`
	AuthURL string `json:"auth_url"`
}

// DeviceAuthPoll is the result of one poll for tokens.
// Status is "pending", "ready", or "error".
type DeviceAuthPoll struct {
	Status       string `json:"status"` // pending | ready | error
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Email        string `json:"email,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	UID          string `json:"uid,omitempty"`
	Error        string `json:"error,omitempty"`
}

// cbNoAuthHeaders are required by CodeBuddy plugin auth endpoints when
// the caller is not yet authenticated (state + token poll).
func cbNoAuthHeaders(req *http.Request) {
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
	req.Header.Set("Content-Type", "application/json")
}

// StartDeviceAuth requests a fresh OAuth state + browser auth URL from
// CodeBuddy. platform defaults to "CLI" when empty.
func StartDeviceAuth(platform string) (*DeviceAuthStart, error) {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		platform = "CLI"
	}
	u := CB_OAUTH_STATE_URL + "?platform=" + url.QueryEscape(platform)
	req, err := http.NewRequest("POST", u, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	cbNoAuthHeaders(req)

	client, proxyID := getClient(tokenRefreshClient, "codebuddy")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return nil, fmt.Errorf("cb oauth device start: %w", err)
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cb oauth device start [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
			// snake_case fallbacks
			AuthURLSnake string `json:"auth_url"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("cb oauth device start parse: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("cb oauth device start code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	authURL := envelope.Data.AuthURL
	if authURL == "" {
		authURL = envelope.Data.AuthURLSnake
	}
	state := strings.TrimSpace(envelope.Data.State)
	if state == "" && authURL != "" {
		// Extract state from authUrl query if missing in body.
		if parsed, err := url.Parse(authURL); err == nil {
			state = strings.TrimSpace(parsed.Query().Get("state"))
		}
	}
	if authURL == "" || state == "" {
		return nil, fmt.Errorf("cb oauth device start: missing authUrl/state in response")
	}
	slog.Info("cb oauth device start ok", "module", "cb-oauth-device", "state", truncateLog(state, 12))
	return &DeviceAuthStart{State: state, AuthURL: authURL}, nil
}

// PollDeviceAuth checks whether the user has completed browser login for state.
// Returns status "pending" while waiting, "ready" with tokens, or "error".
func PollDeviceAuth(state string) (*DeviceAuthPoll, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return &DeviceAuthPoll{Status: "error", Error: "state is required"}, nil
	}
	u := CB_OAUTH_TOKEN_URL + "?state=" + url.QueryEscape(state)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	cbNoAuthHeaders(req)

	client, proxyID := getClient(tokenRefreshClient, "codebuddy")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return nil, fmt.Errorf("cb oauth device poll: %w", err)
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Non-200 usually still means pending or a soft error from the plugin API.
	if resp.StatusCode != 200 {
		// Treat 404 / empty as pending so the client keeps polling.
		if resp.StatusCode == 404 || resp.StatusCode == 204 || resp.StatusCode == 202 {
			return &DeviceAuthPoll{Status: "pending"}, nil
		}
		return &DeviceAuthPoll{
			Status: "error",
			Error:  fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, truncateLog(string(body), 160)),
		}, nil
	}

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			AccessToken     string `json:"accessToken"`
			AccessTokenSnake string `json:"access_token"`
			RefreshToken    string `json:"refreshToken"`
			RefreshTokenSnake string `json:"refresh_token"`
			ExpiresIn       int64  `json:"expiresIn"`
			ExpiresInSnake  int64  `json:"expires_in"`
			Nickname        string `json:"nickname"`
			UID             string `json:"uid"`
			Email           string `json:"email"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		// Unparseable body while waiting is treated as pending.
		return &DeviceAuthPoll{Status: "pending"}, nil
	}

	at := envelope.Data.AccessToken
	if at == "" {
		at = envelope.Data.AccessTokenSnake
	}
	rt := envelope.Data.RefreshToken
	if rt == "" {
		rt = envelope.Data.RefreshTokenSnake
	}
	expIn := envelope.Data.ExpiresIn
	if expIn == 0 {
		expIn = envelope.Data.ExpiresInSnake
	}

	if at == "" {
		// code!=0 without tokens → still pending (user not finished) or soft fail.
		// Plugin often returns code=0 with empty data while waiting; non-zero
		// without tokens is also treated as pending so the UI keeps polling.
		return &DeviceAuthPoll{Status: "pending"}, nil
	}

	email := strings.TrimSpace(envelope.Data.Email)
	nickname := strings.TrimSpace(envelope.Data.Nickname)
	uid := strings.TrimSpace(envelope.Data.UID)

	// Prefer JWT claims for email when the poll body omits it.
	if email == "" {
		email = ParseJWTEmail(at)
	}
	if email == "" && nickname != "" {
		// nickname may itself be an email
		if strings.Contains(nickname, "@") {
			email = nickname
		}
	}

	slog.Info("cb oauth device poll ready",
		"module", "cb-oauth-device",
		"state", truncateLog(state, 12),
		"at", truncateLog(at, 16),
		"has_rt", rt != "",
		"expires_in", expIn,
		"email", email,
		"nickname", nickname,
	)

	return &DeviceAuthPoll{
		Status:       "ready",
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresIn:    expIn,
		Email:        email,
		Nickname:     nickname,
		UID:          uid,
	}, nil
}

// ParseJWTEmail extracts email / preferred_username from a JWT payload
// without verifying the signature (display / import identity only).
func ParseJWTEmail(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	for _, key := range []string{"email", "preferred_username", "upn", "unique_name"} {
		if v, ok := claims[key]; ok {
			if s, ok := v.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" && strings.Contains(s, "@") {
					return s
				}
			}
		}
	}
	// preferred_username without @ — still usable as local-part later by caller
	if v, ok := claims["preferred_username"].(string); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

// ResolveOAuthImportEmail picks a stable import identity for OAuth accounts.
// Order: explicit email → JWT email → nickname@oauth.local → uid@oauth.local → fallback.
func ResolveOAuthImportEmail(explicit, accessToken, nickname, uid string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	if e := ParseJWTEmail(accessToken); e != "" {
		if strings.Contains(e, "@") {
			return e
		}
		return e + "@oauth.local"
	}
	nickname = strings.TrimSpace(nickname)
	if nickname != "" {
		if strings.Contains(nickname, "@") {
			return nickname
		}
		return nickname + "@oauth.local"
	}
	uid = strings.TrimSpace(uid)
	if uid != "" {
		return uid + "@oauth.local"
	}
	// Last resort: short fingerprint of the access token so import still works.
	fp := accessToken
	if len(fp) > 12 {
		fp = fp[:12]
	}
	if fp == "" {
		fp = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "oauth-" + fp + "@oauth.local"
}
