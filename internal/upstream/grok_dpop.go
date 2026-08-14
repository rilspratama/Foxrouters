package upstream

// Grok console.x.ai DPoP image generation — pure Go, plain net/http.
// NOTE: the SSO cookie value for `sso-rw` must be sent; the console.x.ai
// mint endpoint returns 401 when the cookie header is malformed (e.g. the
// sso-rw value is dropped). No TLS impersonation needed — console.x.ai
// accepts Go's plain net/http stack with the correct cookie.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	consoleBase           = "https://console.x.ai"
	grokImagineModel      = "grok-imagine-image"
	grokImagineVideoModel = "grok-imagine-video"
)

// Turnstile solver configuration (runtime-switchable via dashboard Settings).
// Defaults target the local Cloudflare-Turnstile-Bypass service + the
// console.x.ai sitekey; overridable by env (TURNSTILE_SOLVER_URL,
// TURNSTILE_SITEKEY) at startup, then by Redis gw:config (Settings page),
// then live via SetTurnstileConfig.
var (
	turnstileSolver  = "http://127.0.0.1:8742/cloudflare"
	turnstileSiteKey = "0x4AAAAAAAhr9JGVDZbrZOo0"
)

// GetTurnstileConfig returns the active (solverURL, siteKey).
func GetTurnstileConfig() (string, string) { return turnstileSolver, turnstileSiteKey }

// SetTurnstileConfig swaps the runtime solver URL + sitekey (no restart).
func SetTurnstileConfig(solverURL, siteKey string) {
	turnstileSolver = solverURL
	turnstileSiteKey = siteKey
}

// LoadTurnstileConfig applies env overrides then Redis-persisted values
// (gw:config hash) so the dashboard Settings survive restarts.
func LoadTurnstileConfig(store interface {
	GetGWConfig(field string) (string, error)
}) {
	if v := os.Getenv("TURNSTILE_SOLVER_URL"); v != "" {
		turnstileSolver = v
	}
	if v := os.Getenv("TURNSTILE_SITEKEY"); v != "" {
		turnstileSiteKey = v
	}
	if store != nil {
		if v, err := store.GetGWConfig("turnstile_solver_url"); err == nil && v != "" {
			turnstileSolver = v
		}
		if v, err := store.GetGWConfig("turnstile_sitekey"); err == nil && v != "" {
			turnstileSiteKey = v
		}
	}
}

// TestTurnstile solves one token against the CONFIGURED solver+sitekey and
// returns elapsed ms + token length — the Settings page "Test" button.
func TestTurnstile() (elapsedMS int64, tokenLen int, err error) {
	body, _ := json.Marshal(map[string]string{
		"mode": "turnstile", "domain": "https://console.x.ai", "siteKey": turnstileSiteKey,
	})
	start := time.Now()
	client := &http.Client{Timeout: 60 * time.Second}
	sreq, err := http.NewRequest(http.MethodPost, turnstileSolver, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	sreq.Header.Set("Content-Type", "application/json")
	sresp, err := client.Do(sreq)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return elapsed, 0, fmt.Errorf("solver unreachable: %w", err)
	}
	defer sresp.Body.Close()
	var sv struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(sresp.Body, 1<<20)).Decode(&sv); err != nil {
		return elapsed, 0, fmt.Errorf("bad solver response: %w", err)
	}
	if sv.Token == "" {
		return elapsed, 0, fmt.Errorf("solver error: %s", sv.Error)
	}
	return elapsed, len(sv.Token), nil
}

// consoleHeaders are the shared headers for console.x.ai API calls.
func consoleHeaders(extra map[string]string) http.Header {
	h := http.Header{
		"User-Agent":      {"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
		"Accept":          {"application/json, text/plain, */*"},
		"Accept-Language": {"en-US,en;q=0.9"},
		"Origin":          {"https://console.x.ai"},
		"Referer":         {"https://console.x.ai/"},
		"Content-Type":    {"application/json"},
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// LoginSSO performs a pure-HTTP console.x.ai login — NO browser. It solves the
// Cloudflare Turnstile via the local solver service (127.0.0.1:8742), then
// POSTs /api/auth/sign-in and returns fresh sso/sso-rw cookies (~3s/account).
// console.x.ai accepts Go's plain net/http stack here (verified live).
func LoginSSO(email, password string) (sso, ssoRW string, err error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 45 * time.Second, Jar: jar}
	base := &url.URL{Scheme: "https", Host: "console.x.ai"}

	// 1. GET /login → session cookies
	req, _ := http.NewRequest(http.MethodGet, consoleBase+"/login", nil)
	req.Header = consoleHeaders(nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("login page: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 2. solve Turnstile via local solver
	solverBody, _ := json.Marshal(map[string]string{
		"mode": "turnstile", "domain": "https://console.x.ai", "siteKey": turnstileSiteKey,
	})
	sreq, err := http.NewRequest(http.MethodPost, turnstileSolver, bytes.NewReader(solverBody))
	if err != nil {
		return "", "", err
	}
	sreq.Header.Set("Content-Type", "application/json")
	sresp, err := client.Do(sreq)
	if err != nil {
		return "", "", fmt.Errorf("turnstile solver: %w", err)
	}
	defer sresp.Body.Close()
	var sv struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(sresp.Body, 1<<20)).Decode(&sv); err != nil {
		return "", "", fmt.Errorf("turnstile decode: %w", err)
	}
	if sv.Token == "" {
		return "", "", fmt.Errorf("turnstile solver: %s", sv.Error)
	}

	// 3. POST /api/auth/sign-in → sets sso + sso-rw cookies
	loginBody, _ := json.Marshal(map[string]string{
		"method": "email", "email": email, "password": password, "turnstileToken": sv.Token,
	})
	lreq, _ := http.NewRequest(http.MethodPost, consoleBase+"/api/auth/sign-in", bytes.NewReader(loginBody))
	lreq.Header = consoleHeaders(nil)
	lreq.Header.Set("Referer", "https://console.x.ai/login")
	lresp, err := client.Do(lreq)
	if err != nil {
		return "", "", fmt.Errorf("sign-in: %w", err)
	}
	io.Copy(io.Discard, lresp.Body)
	lresp.Body.Close()
	if lresp.StatusCode != 200 {
		return "", "", fmt.Errorf("sign-in %d", lresp.StatusCode)
	}
	for _, c := range jar.Cookies(base) {
		if c.Name == "sso" {
			sso = c.Value
		}
		if c.Name == "sso-rw" {
			ssoRW = c.Value
		}
	}
	if sso == "" {
		return "", "", fmt.Errorf("no sso cookie in sign-in response")
	}
	return sso, ssoRW, nil
}

// GrokImageResult carries a generated image back to the caller.
type GrokImageResult struct {
	Bytes      []byte
	B64        string
	URL        string // edits return imgen.x.ai URL (not b64)
	StatusCode int    // 200 = ok, 429 = quota exhausted, 401 = invalid SSO, else error
	Error      string
}

type dpopClient struct {
	priv *ecdsa.PrivateKey
	jwk  map[string]string
	http *http.Client
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func makeJWK(pub *ecdsa.PublicKey) map[string]string {
	x := make([]byte, 32)
	y := make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	return map[string]string{"kty": "EC", "crv": "P-256", "x": b64u(x), "y": b64u(y)}
}

func rawSig(r, s *big.Int) []byte {
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func newDPoPClient() (*dpopClient, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &dpopClient{
		priv: priv,
		jwk:  makeJWK(&priv.PublicKey),
		http: &http.Client{Timeout: 120 * time.Second},
	}, nil
}

func (d *dpopClient) headers(cookie string, extra map[string]string) http.Header {
	h := http.Header{
		"User-Agent":      {"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
		"Accept":          {"application/json, text/plain, */*"},
		"Accept-Language": {"en-US,en;q=0.9"},
		"Origin":          {"https://console.x.ai"},
		"Referer":         {"https://console.x.ai/"},
		"Content-Type":    {"application/json"},
		"Cookie":          {cookie},
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

func (d *dpopClient) mint(cookie string) (string, error) {
	body, _ := json.Marshal(map[string]any{"jwk": d.jwk})
	req, _ := http.NewRequest(http.MethodPost, consoleBase+"/v1/dpop/token", bytes.NewReader(body))
	req.Header = d.headers(cookie, nil)
	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("mint %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(raw, &out)
	return out.AccessToken, nil
}

func (d *dpopClient) proof(htm, htu, at string) (string, error) {
	ath := sha256.Sum256([]byte(at))
	header, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": d.jwk})
	claims, _ := json.Marshal(map[string]any{
		"jti": fmt.Sprintf("%d", time.Now().UnixNano()),
		"htm": htm, "htu": htu, "iat": time.Now().Unix(),
		"ath": b64u(ath[:]),
	})
	h, c := b64u(header), b64u(claims)
	signing := h + "." + c
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, d.priv, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64u(rawSig(r, s)), nil
}

// GrokUsageQuota is one quota bucket from GET /v1/usage.
type GrokUsageQuota struct {
	Limit          int   `json:"limit"`
	Used           int   `json:"used"`
	Remaining      int   `json:"remaining"`
	LastConsumedAt int64 `json:"last_consumed_at"`
}

// GrokUsage is the full free-tier quota snapshot for one account.
type GrokUsage struct {
	Chat  GrokUsageQuota
	Image GrokUsageQuota
	Video GrokUsageQuota
}

// SyncUsage fetches GET /v1/usage for one account — SSO cookie only,
// NO DPoP proof needed. Returns nil quota buckets when absent.
func SyncUsage(sso, ssoRW string) (*GrokUsage, error) {
	cookie := "sso=" + sso + "; sso-rw=" + ssoRW
	req, _ := http.NewRequest(http.MethodGet, consoleBase+"/v1/usage", nil)
	req.Header = http.Header{
		"User-Agent":      {"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
		"Accept":          {"application/json, text/plain, */*"},
		"Accept-Language": {"en-US,en;q=0.9"},
		"Origin":          {"https://console.x.ai"},
		"Referer":         {"https://console.x.ai/"},
		"Cookie":          {cookie},
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("usage %d", resp.StatusCode)
	}
	var out struct {
		Quotas []struct {
			Kind           string `json:"kind"`
			Limit          int    `json:"limit"`
			Used           int    `json:"used"`
			Remaining      int    `json:"remaining"`
			LastConsumedAt int64  `json:"last_consumed_at"`
		} `json:"quotas"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	u := &GrokUsage{}
	for _, q := range out.Quotas {
		switch q.Kind {
		case "chat":
			u.Chat = GrokUsageQuota{Limit: q.Limit, Used: q.Used, Remaining: q.Remaining, LastConsumedAt: q.LastConsumedAt}
		case "image":
			u.Image = GrokUsageQuota{Limit: q.Limit, Used: q.Used, Remaining: q.Remaining, LastConsumedAt: q.LastConsumedAt}
		case "video":
			u.Video = GrokUsageQuota{Limit: q.Limit, Used: q.Used, Remaining: q.Remaining, LastConsumedAt: q.LastConsumedAt}
		}
	}
	return u, nil
}

// GenerateImage runs mint + DPoP proof + images/generations for one account.
// Returns a result with StatusCode semantics used by the handler:
//   200 → Bytes/B64 populated; 429 → quota; 401 → invalid SSO.
func GenerateImage(sso, ssoRW, prompt, aspect string) GrokImageResult {
	cookie := "sso=" + sso + "; sso-rw=" + ssoRW
	client, err := newDPoPClient()
	if err != nil {
		return GrokImageResult{StatusCode: 500, Error: err.Error()}
	}

	at, err := client.mint(cookie)
	if err != nil {
		code := 500
		if strings.Contains(err.Error(), "mint 401") {
			code = 401
		} else if strings.Contains(err.Error(), "mint 429") {
			code = 429
		}
		return GrokImageResult{StatusCode: code, Error: err.Error()}
	}

	htu := consoleBase + "/v1/images/generations"
	proof, err := client.proof("POST", htu, at)
	if err != nil {
		return GrokImageResult{StatusCode: 500, Error: err.Error()}
	}

	body, _ := json.Marshal(map[string]any{
		"model": grokImagineModel, "prompt": prompt, "n": 1,
		"aspect_ratio": aspect, "resolution": "1k", "response_format": "b64_json",
	})
	req, _ := http.NewRequest(http.MethodPost, htu, bytes.NewReader(body))
	req.Header = client.headers(cookie, map[string]string{
		"Authorization": "DPoP " + at,
		"DPoP":          proof,
	})
	resp, err := client.http.Do(req)
	if err != nil {
		return GrokImageResult{StatusCode: 500, Error: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case 200:
		var out struct {
			Data []struct {
				B64 string `json:"b64_json"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 || out.Data[0].B64 == "" {
			return GrokImageResult{StatusCode: 500, Error: "no b64_json in response"}
		}
		img, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
		if err != nil {
			return GrokImageResult{StatusCode: 500, Error: "bad b64: " + err.Error()}
		}
		return GrokImageResult{StatusCode: 200, Bytes: img, B64: out.Data[0].B64}
	case 429:
		return GrokImageResult{StatusCode: 429, Error: "resource-exhausted"}
	case 401:
		return GrokImageResult{StatusCode: 401, Error: "invalid sso"}
	default:
		return GrokImageResult{StatusCode: resp.StatusCode, Error: clampErr(strings.TrimSpace(string(raw)), 200)}
	}
}

// EditImage runs mint + DPoP proof + POST /v1/images/edits for one account.
// Edits SHARE the image quota (gen+edit = 5/account). Returns the resulting
// image URL (imgen.x.ai) — the upstream responds with url, not b64_json.
func EditImage(sso, ssoRW, prompt, imageB64 string) GrokImageResult {
	cookie := "sso=" + sso + "; sso-rw=" + ssoRW
	client, err := newDPoPClient()
	if err != nil {
		return GrokImageResult{StatusCode: 500, Error: err.Error()}
	}

	at, err := client.mint(cookie)
	if err != nil {
		code := 500
		if strings.Contains(err.Error(), "mint 401") {
			code = 401
		} else if strings.Contains(err.Error(), "mint 429") {
			code = 429
		}
		return GrokImageResult{StatusCode: code, Error: err.Error()}
	}

	htu := consoleBase + "/v1/images/edits"
	proof, err := client.proof("POST", htu, at)
	if err != nil {
		return GrokImageResult{StatusCode: 500, Error: err.Error()}
	}

	body, _ := json.Marshal(map[string]any{
		"model": grokImagineModel, "prompt": prompt, "image_b64": imageB64,
	})
	req, _ := http.NewRequest(http.MethodPost, htu, bytes.NewReader(body))
	req.Header = client.headers(cookie, map[string]string{
		"Authorization": "DPoP " + at,
		"DPoP":          proof,
	})
	resp, err := client.http.Do(req)
	if err != nil {
		return GrokImageResult{StatusCode: 500, Error: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case 200:
		var out struct {
			Data []struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 || out.Data[0].URL == "" {
			return GrokImageResult{StatusCode: 500, Error: "no url in edit response"}
		}
		return GrokImageResult{StatusCode: 200, URL: out.Data[0].URL}
	case 429:
		return GrokImageResult{StatusCode: 429, Error: "resource-exhausted"}
	case 401:
		return GrokImageResult{StatusCode: 401, Error: "invalid sso"}
	default:
		return GrokImageResult{StatusCode: resp.StatusCode, Error: clampErr(strings.TrimSpace(string(raw)), 200)}
	}
}

// videoOwners maps a console.x.ai video request_id → the email of the account
// that created it. GET /v1/videos/{id} requires THAT account's SSO cookie, so
// the polling handler needs the owner. Bounded (drops oldest on overflow).
var videoOwners = struct {
	sync.Mutex
	m map[string]string
}{m: make(map[string]string)}

var urlRe = regexp.MustCompile(`https://[^\s"']+`)

// firstNonEmpty returns the first non-empty string value from the map for the
// given keys.
func firstNonEmpty(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clampErr(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return s
}


// getNestedStr digs for a string at data[i].<key>.
func getNestedStr(data []map[string]any, key string) string {
	for _, d := range data {
		if v, ok := d[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// RecordVideoOwner binds a request_id to its creating account.
func RecordVideoOwner(requestID, email string) {
	videoOwners.Lock()
	defer videoOwners.Unlock()
	if len(videoOwners.m) > 5000 {
		videoOwners.m = make(map[string]string)
	}
	videoOwners.m[requestID] = email
}

// VideoOwner returns the account that created a video request ("" = unknown).
func VideoOwner(requestID string) string {
	videoOwners.Lock()
	defer videoOwners.Unlock()
	return videoOwners.m[requestID]
}

// GenerateVideo starts an async video generation (quota 2/account). Returns
// the requestID for polling via GetVideo. StatusCode semantics match
// GrokImageResult (200/401/429/other).
func GenerateVideo(sso, ssoRW, prompt string) (requestID string, statusCode int, errMsg string) {
	cookie := "sso=" + sso + "; sso-rw=" + ssoRW
	client, err := newDPoPClient()
	if err != nil {
		return "", 500, err.Error()
	}
	at, err := client.mint(cookie)
	if err != nil {
		code := 500
		if strings.Contains(err.Error(), "mint 401") {
			code = 401
		} else if strings.Contains(err.Error(), "mint 429") {
			code = 429
		}
		return "", code, err.Error()
	}
	htu := consoleBase + "/v1/videos/generations"
	proof, err := client.proof("POST", htu, at)
	if err != nil {
		return "", 500, err.Error()
	}
	body, _ := json.Marshal(map[string]any{"model": grokImagineVideoModel, "prompt": prompt})
	req, _ := http.NewRequest(http.MethodPost, htu, bytes.NewReader(body))
	req.Header = client.headers(cookie, map[string]string{
		"Authorization": "DPoP " + at,
		"DPoP":          proof,
	})
	resp, err := client.http.Do(req)
	if err != nil {
		return "", 500, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case 200, 202:
		var out struct {
			RequestID string `json:"request_id"`
			ID        string `json:"id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", 500, "bad video create response"
		}
		rid := out.RequestID
		if rid == "" {
			rid = out.ID
		}
		if rid == "" {
			// some responses nest under data[0].{request_id,id}
			var out2 struct {
				Data []struct {
					RequestID string `json:"request_id"`
					ID        string `json:"id"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &out2) == nil && len(out2.Data) > 0 {
				rid = out2.Data[0].RequestID
				if rid == "" {
					rid = out2.Data[0].ID
				}
			}
		}
		if rid == "" {
			return "", 500, "no request_id in video create response"
		}
		return rid, 200, ""
	case 429:
		return "", 429, "resource-exhausted"
	case 401:
		return "", 401, "invalid sso"
	default:
		return "", resp.StatusCode, clampErr(strings.TrimSpace(string(raw)), 200)
	}
}

// GetVideo polls an async video job — DPoP GET proof (mirrors the verified
// probe_video_poll.py flow). Returns 202 while pending (status+progress) and
// 200 when done (url).
func GetVideo(sso, ssoRW, requestID string) (status string, progress int, videoURL string, statusCode int, errMsg string) {
	cookie := "sso=" + sso + "; sso-rw=" + ssoRW
	client, err := newDPoPClient()
	if err != nil {
		return "", 0, "", 500, err.Error()
	}
	at, err := client.mint(cookie)
	if err != nil {
		code := 500
		if strings.Contains(err.Error(), "mint 401") {
			code = 401
		} else if strings.Contains(err.Error(), "mint 429") {
			code = 429
		}
		return "", 0, "", code, err.Error()
	}
	htu := consoleBase + "/v1/videos/" + requestID
	proof, err := client.proof("GET", htu, at)
	if err != nil {
		return "", 0, "", 500, err.Error()
	}
	req, _ := http.NewRequest(http.MethodGet, htu, nil)
	req.Header = client.headers(cookie, map[string]string{
		"Authorization": "DPoP " + at,
		"DPoP":          proof,
	})
	resp, err := client.http.Do(req)
	if err != nil {
		return "", 0, "", 500, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	switch resp.StatusCode {
	case 202:
		var out struct {
			Status   string `json:"status"`
			Progress int    `json:"progress"`
		}
		_ = json.Unmarshal(raw, &out)
		return out.Status, out.Progress, "", 202, ""
	case 200:
		// Tolerate multiple response shapes: data[0].{url,video_url,file_url,output},
		// top-level {url,video_url,media_url,result}, video.{url} (console.x.ai
		// done shape: {"status":"done","video":{"url":...},"progress":100}).
		var rawShape struct {
			Data []map[string]any `json:"data"`
			URL  string           `json:"url"`
			Video struct {
				URL string `json:"url"`
			} `json:"video"`
		}
		videoURL = ""
		if err := json.Unmarshal(raw, &rawShape); err == nil {
			videoURL = rawShape.Video.URL
			if videoURL == "" {
				for _, d := range rawShape.Data {
					videoURL = firstNonEmpty(d, "url", "video_url", "file_url", "output", "media_url", "result", "mp4_url")
					if videoURL != "" {
						break
					}
				}
			}
			if videoURL == "" {
				videoURL = firstNonEmptyStr(rawShape.URL, getNestedStr(rawShape.Data, "url"), getNestedStr(rawShape.Data, "video_url"))
			}
		}
		if videoURL == "" {
			// last resort: search for any https URL in the raw body
			if m := urlRe.FindSubmatch(raw); m != nil {
				videoURL = string(m[0])
			}
		}
		if videoURL != "" {
			return "completed", 100, videoURL, 200, ""
		}
		s := strings.TrimSpace(string(raw))
		if len(s) > 400 {
			s = s[:400]
		}
		return "completed", 100, "", 200, "no url in video response: " + s
	case 401:
		return "", 0, "", 401, "invalid sso"
	default:
		return "", 0, "", resp.StatusCode, clampErr(strings.TrimSpace(string(raw)), 200)
	}
}
