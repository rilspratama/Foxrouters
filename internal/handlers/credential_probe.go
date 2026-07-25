package handlers

import (
	"log/slog"

	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
)

// HandleTestCBKey: POST /cb/keys/test
// Body: {"key":"<full|masked>"} or {"email":"..."} (OAuth).
// Probes the credential directly against CodeBuddy chat upstream.
// Always returns HTTP 200 with ok=false on upstream failure so the dashboard
// can show the result; 400/404 only for bad/missing request identity.
func HandleTestCBKey(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cbKM == nil {
			c.JSON(500, gin.H{"error": "cb key manager not initialised"})
			return
		}
		var body struct {
			Key   string `json:"key"`
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}
		id := body.Key
		if id == "" {
			id = body.Email
		}
		if id == "" {
			c.JSON(400, gin.H{"error": "key or email required"})
			return
		}

		key := cbKM.GetByKey(id)
		if key == nil {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}

		result := upstream.TestCBKey(key)
		slog.Info("cb key test",
			"module", "admin",
			"key", key.DisplayID(),
			"ok", result.OK,
			"status", result.Status,
			"latency_ms", result.LatencyMs)
		c.JSON(200, result)
	}
}

// HandleTestGrokAccount: POST /accounts/test
// Body: {"email":"..."}.
// Probes the Grok account directly against cli-chat-proxy (no RR).
func HandleTestGrokAccount(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if grokAM == nil {
			c.JSON(500, gin.H{"error": "grok account manager not initialised"})
			return
		}
		var body struct {
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}
		if body.Email == "" {
			c.JSON(400, gin.H{"error": "email required"})
			return
		}

		acc := grokAM.GetByEmail(body.Email)
		if acc == nil {
			c.JSON(404, gin.H{"error": "grok account not found"})
			return
		}

		result := upstream.TestGrokAccount(acc)
		slog.Info("grok account test",
			"module", "admin",
			"email", acc.Email,
			"ok", result.OK,
			"status", result.Status,
			"latency_ms", result.LatencyMs)
		c.JSON(200, result)
	}
}
