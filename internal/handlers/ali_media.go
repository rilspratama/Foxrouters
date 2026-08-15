package handlers

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"foxrouters/internal/upstream"
)

// Alibaba Media Studio handlers — OpenAI-shaped routes backed by DashScope.
// Routes: POST /v1/images/generations, POST /v1/images/edits,
// POST /v1/videos/generations, GET /v1/videos/:id.
// No session lifecycle (plain sk-ws-* keys) — no SSO/DPoP/Turnstile.

const aliMediaMaxReq = 48 << 20

// HandleAliImages implements POST /v1/images/generations (OpenAI Images API
// shape: {model, prompt, size, n, response_format}). Model defaults to
// qwen-image-3.0; unknown/grok-* ids fall back to the default. Returns
// {created, data:[{url, b64_json?}]}.
func HandleAliImages(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			Size           string `json:"size"`
			N              int    `json:"n"`
			ResponseFormat string `json:"response_format"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid request body"}})
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required"}})
			return
		}
		urls, err := aliAM.AliImageGen(req.Model, req.Prompt, req.Size, req.N)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error()}})
			return
		}
		data := make([]gin.H, 0, len(urls))
		for _, u := range urls {
			item := gin.H{"url": u}
			if req.ResponseFormat == "b64_json" {
				if b64, err := fetchB64(u); err == nil {
					item["b64_json"] = b64
				}
			}
			data = append(data, item)
		}
		c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": data})
	}
}

// HandleAliImagesEdit implements POST /v1/images/edits ({model, prompt,
// image}). image may be a base64 blob or a data: URI or a public URL.
// Returns {data:[{url}]}.
func HandleAliImagesEdit(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Image  string `json:"image"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid request body"}})
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required"}})
			return
		}
		if strings.TrimSpace(req.Image) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "image is required"}})
			return
		}
		url, err := aliAM.AliImageEdit(req.Model, req.Prompt, req.Image)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": []gin.H{{"url": url}}})
	}
}

// HandleAliVideoCreate implements POST /v1/videos/generations ({model,
// prompt}) → {id}. Video tasks are async; poll via GET /v1/videos/:id.
func HandleAliVideoCreate(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid request body"}})
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required"}})
			return
		}
		id, err := aliAM.AliVideoCreate(req.Model, req.Prompt)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id})
	}
}

// HandleAliVideoStatus implements GET /v1/videos/:id → {status, progress?,
// data:[{url}?]}. status maps DashScope task status to a stable API shape:
// "completed" / "processing" / "failed" / "unknown".
func HandleAliVideoStatus(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimPrefix(c.Request.URL.Path, "/v1/videos/")
		if id == "" || id == c.Request.URL.Path {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "video id is required"}})
			return
		}
		status, url, err := aliAM.AliVideoStatus(id)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error()}})
			return
		}
		out := gin.H{"id": id}
		switch status {
		case "succeeded":
			out["status"] = "completed"
			if url != "" {
				out["data"] = []gin.H{{"url": url}}
			}
		case "pending", "running":
			out["status"] = "processing"
			out["progress"] = 0
		case "failed":
			out["status"] = "failed"
		default:
			out["status"] = status
		}
		c.JSON(http.StatusOK, out)
	}
}

// fetchB64 downloads a public URL and returns its base64 content.
func fetchB64(url string) (string, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, aliMediaMaxReq))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
