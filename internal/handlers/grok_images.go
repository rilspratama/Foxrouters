package handlers

// Grok image generation via console.x.ai DPoP (free tier) — PURE GO.
// No sidecar needed: console.x.ai accepts Go's plain net/http as long as the
// SSO cookie header is complete (sso + sso-rw).
//
// Lazy-auth design (per operator directive, Aug 2026):
//   - Cookies are CACHED in Redis (never proactively refreshed).
//   - 401 invalid SSO → pure-HTTP re-login (Turnstile solver + /api/auth/sign-in,
//     ~3s, in-gateway) → retry once with fresh cookies.
//   - 429 resource-exhausted → mark account exhausted (5-img quota counted
//     lazily locally) → rotate to next account.
//   - NO /v1/usage round-trip polling (cookies die too often for that to be
//     reliable) — the old GrokImageQuotaWorker was removed.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"foxrouters/internal/upstream"
	"github.com/gin-gonic/gin"
)

// bindJSONLimited binds a JSON body with a hard size cap. Prevents
// memory-exhaustion DoS via oversized base64/image payloads (FR-WEB-001).
// Caller handles the returned error (413 for MaxBytesError, else 400).
func bindJSONLimited(c *gin.Context, v any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, upstream.MAX_REQUEST_BODY)
	return c.ShouldBindJSON(v)
}

var grokImgRR atomic.Int64

// HandleGrokImages implements POST /v1/images/generations (OpenAI Images API
// shape) backed by the Grok free console.x.ai DPoP path.
func HandleGrokImages(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			N              int    `json:"n"`
			AspectRatio    string `json:"aspect_ratio"`
			Size           string `json:"size"`
			ResponseFormat string `json:"response_format"`
			Quality        string `json:"quality"`
		}
		if err := bindJSONLimited(c, &req); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				c.JSON(413, gin.H{"error": "request body too large"})
			} else {
				c.JSON(400, gin.H{"error": "invalid JSON body"})
			}
			return
		}
		if req.Prompt == "" {
			c.JSON(400, gin.H{"error": "prompt is required"})
			return
		}
		aspect := req.AspectRatio
		if aspect == "" {
			switch req.Size {
			case "256x256", "512x512", "1024x1024":
				aspect = "1:1"
			case "1792x1024", "1536x1024":
				aspect = "3:2"
			case "1024x1792", "1024x1536":
				aspect = "2:3"
			default:
				aspect = "1:1"
			}
		}
		n := req.N
		if n <= 0 {
			n = 1
		}
		if n > 4 {
			n = 4
		}
		wantURL := req.ResponseFormat == "url"

		var lastErr string
		start := int(grokImgRR.Load())
		total := grokAM.Len()
		for attempt := 0; attempt < total; attempt++ {
			idx, snap := grokAM.PickImageAccount(start)
			if snap == nil {
				lastErr = "no grok account with image-gen (SSO) available"
				break
			}
			grokImgRR.Store(int64(idx) + 1)
			start = idx + 1

			res := upstream.GenerateImage(snap.SSO, snap.SSORW, req.Prompt, aspect)
			switch res.StatusCode {
			case 200:
				grokAM.IncrImgUsed(snap.Email) // lazy local quota count (limit 5)
				if !wantURL {
					c.JSON(200, gin.H{
						"created": time.Now().Unix(),
						"data":    []gin.H{{"b64_json": res.B64}},
					})
				} else {
					c.JSON(200, gin.H{
						"created": time.Now().Unix(),
						"data":    []gin.H{{"url": "data:image/jpeg;base64," + res.B64}},
					})
				}
				return
			case 429:
				// Quota exhausted — lazy count reached limit. Rotate.
				grokAM.MarkImgExhausted(snap.Email)
				lastErr = fmt.Sprintf("%s: image quota exhausted", snap.Email)
			case 401:
				// Cookie dead → pure-HTTP re-login (~3s) → retry once.
				if err := grokAM.RefreshSSO(snap.Email); err != nil {
					// Login failed (bad pwd / solver down / IP flag) — back off this
					// account for a bit, don't hammer.
					grokAM.SetImgCooldown(snap.Email, time.Now().Add(10*time.Minute))
					lastErr = fmt.Sprintf("%s: sso refresh failed: %v", snap.Email, err)
					continue
				}
				refreshed := grokAM.GetSnapshot(snap.Email)
				if refreshed == nil {
					lastErr = fmt.Sprintf("%s: account vanished after refresh", snap.Email)
					continue
				}
				res2 := upstream.GenerateImage(refreshed.SSO, refreshed.SSORW, req.Prompt, aspect)
				switch res2.StatusCode {
				case 200:
					grokAM.IncrImgUsed(refreshed.Email)
					if !wantURL {
						c.JSON(200, gin.H{
							"created": time.Now().Unix(),
							"data":    []gin.H{{"b64_json": res2.B64}},
						})
					} else {
						c.JSON(200, gin.H{
							"created": time.Now().Unix(),
							"data":    []gin.H{{"url": "data:image/jpeg;base64," + res2.B64}},
						})
					}
					return
				case 429:
					grokAM.MarkImgExhausted(refreshed.Email)
					lastErr = fmt.Sprintf("%s: quota exhausted after refresh", refreshed.Email)
				case 401:
					grokAM.SetImgCooldown(refreshed.Email, time.Now().Add(10*time.Minute))
					lastErr = fmt.Sprintf("%s: still 401 after refresh", refreshed.Email)
				default:
					lastErr = fmt.Sprintf("%s: upstream %d after refresh (%s)", refreshed.Email, res2.StatusCode, res2.Error)
				}
			default:
				lastErr = fmt.Sprintf("%s: upstream %d (%s)", snap.Email, res.StatusCode, res.Error)
			}
		}
		c.JSON(429, gin.H{"error": "all grok image accounts exhausted: " + lastErr})
	}
}

// HandleGrokQuotaSync — POST /grok/quota/sync (admin). Best-effort on-demand
// /v1/usage fetch (diagnostics only — the lazy local counter is the real
// source of truth for image quota now).
func HandleGrokQuotaSync(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		synced, failed := grokAM.SyncAllQuota()
		c.JSON(200, gin.H{"status": "ok", "synced": synced, "failed": failed, "total": grokAM.Len()})
	}
}

var grokImgEditRR atomic.Int64

// HandleGrokImagesEdit implements POST /v1/images/edits (OpenAI edits shape:
// {model, prompt, image (b64)} → {created, data:[{url}]}). Shares the image
// quota (gen+edit = 5/account) and the same lazy-auth rotation as generation.
func HandleGrokImagesEdit(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Image  string `json:"image"`
		}
		if err := bindJSONLimited(c, &req); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				c.JSON(413, gin.H{"error": "request body too large"})
			} else {
				c.JSON(400, gin.H{"error": "invalid JSON body"})
			}
			return
		}
		if req.Prompt == "" {
			c.JSON(400, gin.H{"error": "prompt is required"})
			return
		}
		if req.Image == "" {
			c.JSON(400, gin.H{"error": "image (base64) is required"})
			return
		}

		var lastErr string
		start := int(grokImgEditRR.Load())
		total := grokAM.Len()
		for attempt := 0; attempt < total; attempt++ {
			idx, snap := grokAM.PickImageAccount(start)
			if snap == nil {
				lastErr = "no grok account with image-gen (SSO) available"
				break
			}
			grokImgEditRR.Store(int64(idx) + 1)
			start = idx + 1

			res := upstream.EditImage(snap.SSO, snap.SSORW, req.Prompt, req.Image)
			switch res.StatusCode {
			case 200:
				grokAM.IncrImgUsed(snap.Email) // edits share the 5-img quota
				c.JSON(200, gin.H{
					"created": time.Now().Unix(),
					"data":    []gin.H{{"url": res.URL}},
				})
				return
			case 429:
				grokAM.MarkImgExhausted(snap.Email)
				lastErr = fmt.Sprintf("%s: image quota exhausted", snap.Email)
			case 401:
				if err := grokAM.RefreshSSO(snap.Email); err != nil {
					grokAM.SetImgCooldown(snap.Email, time.Now().Add(10*time.Minute))
					lastErr = fmt.Sprintf("%s: sso refresh failed: %v", snap.Email, err)
					continue
				}
				refreshed := grokAM.GetSnapshot(snap.Email)
				if refreshed == nil {
					lastErr = fmt.Sprintf("%s: account vanished after refresh", snap.Email)
					continue
				}
				res2 := upstream.EditImage(refreshed.SSO, refreshed.SSORW, req.Prompt, req.Image)
				switch res2.StatusCode {
				case 200:
					grokAM.IncrImgUsed(refreshed.Email)
					c.JSON(200, gin.H{
						"created": time.Now().Unix(),
						"data":    []gin.H{{"url": res2.URL}},
					})
					return
				case 429:
					grokAM.MarkImgExhausted(refreshed.Email)
					lastErr = fmt.Sprintf("%s: quota exhausted after refresh", refreshed.Email)
				case 401:
					grokAM.SetImgCooldown(refreshed.Email, time.Now().Add(10*time.Minute))
					lastErr = fmt.Sprintf("%s: still 401 after refresh", refreshed.Email)
				default:
					lastErr = fmt.Sprintf("%s: upstream %d after refresh (%s)", refreshed.Email, res2.StatusCode, res2.Error)
				}
			default:
				lastErr = fmt.Sprintf("%s: upstream %d (%s)", snap.Email, res.StatusCode, res.Error)
			}
		}
		c.JSON(429, gin.H{"error": "all grok image accounts exhausted: " + lastErr})
	}
}

var grokVidRR atomic.Int64

// HandleGrokVideoCreate implements POST /v1/videos/generations ({model, prompt}
// → {id}). Starts an async video job; the dashboard polls GET /v1/videos/:id.
func HandleGrokVideoCreate(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := bindJSONLimited(c, &req); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				c.JSON(413, gin.H{"error": "request body too large"})
			} else {
				c.JSON(400, gin.H{"error": "invalid JSON body"})
			}
			return
		}
		if req.Prompt == "" {
			c.JSON(400, gin.H{"error": "prompt is required"})
			return
		}

		var lastErr string
		start := int(grokVidRR.Load())
		total := grokAM.Len()
		for attempt := 0; attempt < total; attempt++ {
			idx, snap := grokAM.PickVideoAccount(start)
			if snap == nil {
				lastErr = "no grok account with video-gen (SSO) available"
				break
			}
			grokVidRR.Store(int64(idx) + 1)
			start = idx + 1

			rid, code, errMsg := upstream.GenerateVideo(snap.SSO, snap.SSORW, req.Prompt)
			switch code {
			case 200:
				grokAM.IncrVidUsed(snap.Email) // lazy video quota count (limit 2)
				grokAM.RecordVideoOwner(rid, snap.Email)
				c.JSON(200, gin.H{"id": rid, "status": "pending"})
				return
			case 429:
				grokAM.MarkVidExhausted(snap.Email)
				lastErr = fmt.Sprintf("%s: video quota exhausted", snap.Email)
			case 401:
				if err := grokAM.RefreshSSO(snap.Email); err != nil {
					grokAM.SetImgCooldown(snap.Email, time.Now().Add(10*time.Minute))
					lastErr = fmt.Sprintf("%s: sso refresh failed: %v", snap.Email, err)
					continue
				}
				refreshed := grokAM.GetSnapshot(snap.Email)
				if refreshed == nil {
					lastErr = fmt.Sprintf("%s: account vanished after refresh", snap.Email)
					continue
				}
				rid2, code2, errMsg2 := upstream.GenerateVideo(refreshed.SSO, refreshed.SSORW, req.Prompt)
				switch code2 {
				case 200:
					grokAM.IncrVidUsed(refreshed.Email)
					grokAM.RecordVideoOwner(rid2, refreshed.Email)
					c.JSON(200, gin.H{"id": rid2, "status": "pending"})
					return
				case 429:
					grokAM.MarkVidExhausted(refreshed.Email)
					lastErr = fmt.Sprintf("%s: quota exhausted after refresh", refreshed.Email)
				case 401:
					grokAM.SetImgCooldown(refreshed.Email, time.Now().Add(10*time.Minute))
					lastErr = fmt.Sprintf("%s: still 401 after refresh", refreshed.Email)
				default:
					lastErr = fmt.Sprintf("%s: upstream %d after refresh (%s)", refreshed.Email, code2, errMsg2)
				}
			default:
				lastErr = fmt.Sprintf("%s: upstream %d (%s)", snap.Email, code, errMsg)
			}
		}
		c.JSON(429, gin.H{"error": "all grok video accounts exhausted: " + lastErr})
	}
}

// HandleGrokVideoStatus implements GET /v1/videos/:id — polls the async job
// with the creating account's SSO. 202 → {status, progress}; 200 → {data:[{url}]}.
func HandleGrokVideoStatus(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimPrefix(c.Request.URL.Path, "/v1/videos/")
		if id == "" || id == c.Request.URL.Path {
			c.JSON(400, gin.H{"error": "video id required"})
			return
		}
		owner := grokAM.VideoOwner(id)
		if owner == "" {
			c.JSON(404, gin.H{"error": "unknown video job (created on a previous gateway restart?)"})
			return
		}
		snap := grokAM.GetSnapshot(owner)
		if snap == nil {
			c.JSON(404, gin.H{"error": "owner account no longer exists"})
			return
		}

		status, progress, videoURL, code, errMsg := upstream.GetVideo(snap.SSO, snap.SSORW, id)
		if code == 401 {
			// Cookie died while the job was running — re-login + retry once.
			if err := grokAM.RefreshSSO(owner); err == nil {
				if refreshed := grokAM.GetSnapshot(owner); refreshed != nil {
					status, progress, videoURL, code, errMsg = upstream.GetVideo(refreshed.SSO, refreshed.SSORW, id)
				}
			}
		}
		switch code {
		case 202:
			c.JSON(202, gin.H{"id": id, "status": status, "progress": progress})
		case 200:
			if videoURL != "" {
				c.JSON(200, gin.H{"id": id, "status": "completed", "data": []gin.H{{"url": videoURL}}})
			} else {
				c.JSON(502, gin.H{"error": "video done but no url: " + errMsg})
			}
		case 401:
			c.JSON(401, gin.H{"error": "video auth failed: " + errMsg})
		default:
			c.JSON(502, gin.H{"error": fmt.Sprintf("video poll %d (%s)", code, errMsg)})
		}
	}
}
