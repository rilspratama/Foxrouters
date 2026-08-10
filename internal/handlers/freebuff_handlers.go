package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/upstream"
)

// HandleFBImport adds a Freebuff auth token to the pool.
// POST /fb/import body: {"token":"dc8e0cd5-..."}
// HandleFBImport imports a single Freebuff token (manual paste).
// POST /fb/import {token} → probes /api/v1/me for userID.
func HandleFBImport(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Token string `json:"token" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing token field"})
			return
		}

		added, total, err := fbAM.AddAccount(req.Token)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{
				"error":  "failed to add freebuff token",
				"detail": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"added": added,
			"total": total,
		})
	}
}

// HandleFBImportBulk imports multiple Freebuff tokens at once.
// POST /fb/import/bulk {"tokens":["uuid1","uuid2",...]} or {"raw":"uuid1\nuuid2\n..."}
func HandleFBImportBulk(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Tokens []string `json:"tokens"`
			Raw    string   `json:"raw"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}

		tokens := req.Tokens
		// Parse raw input — supports two formats per line:
		//   1. token|email|userid   (pipe-separated, email + userid optional)
		//   2. token                (bare UUID)
		type fbEntry struct {
			token  string
			email  string
			userID string
		}
		var entries []fbEntry
		if len(tokens) == 0 && strings.TrimSpace(req.Raw) != "" {
			for _, line := range strings.Split(req.Raw, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// Also split by comma for backward compat
				for _, part := range strings.Split(line, ",") {
					part = strings.TrimSpace(part)
					if part == "" {
						continue
					}
					// Check for pipe separator: token|email|userid
					pipeParts := strings.Split(part, "|")
					e := fbEntry{token: strings.TrimSpace(pipeParts[0])}
					if len(pipeParts) > 1 {
						e.email = strings.TrimSpace(pipeParts[1])
					}
					if len(pipeParts) > 2 {
						e.userID = strings.TrimSpace(pipeParts[2])
					}
					if e.token != "" {
						entries = append(entries, e)
					}
				}
			}
		} else {
			for _, t := range tokens {
				t = strings.TrimSpace(t)
				if t != "" {
					entries = append(entries, fbEntry{token: t})
				}
			}
		}

		if len(entries) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no tokens provided"})
			return
		}

		added := 0
		failed := 0
		var errors []string
		for _, e := range entries {
			ok, _, err := fbAM.AddAccountWithInfo(e.token, e.userID, e.email)
			if err != nil {
				failed++
				tokenDisplay := e.token
				if len(tokenDisplay) > 12 {
					tokenDisplay = tokenDisplay[:12] + "..."
				}
				errors = append(errors, tokenDisplay+": "+err.Error())
				continue
			}
			if ok {
				added++
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"added":  added,
			"failed": failed,
			"total":  fbAM.Len(),
			"errors": errors,
		})
	}
}

// HandleFBAccounts lists all Freebuff accounts.
// GET /fb/accounts
func HandleFBAccounts(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts := fbAM.ListAccounts()
		c.JSON(http.StatusOK, gin.H{
			"accounts": accounts,
			"total":    len(accounts),
		})
	}
}

// HandleFBDeviceStart starts a Freebuff device/login OAuth flow.
// POST /fb/oauth/device/start → {state, auth_url, fingerprint_id, fingerprint_hash, expires_at}
func HandleFBDeviceStart(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := upstream.StartFreebuffDeviceAuth()
		if err != nil {
			c.JSON(502, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"state":            res.State,
			"auth_url":         res.AuthURL,
			"fingerprint_id":   res.FingerprintID,
			"fingerprint_hash": res.FingerprintHash,
			"expires_at":       res.ExpiresAt,
		})
	}
}

// HandleFBDevicePoll polls Freebuff for tokens after browser login.
// GET /fb/oauth/device/poll?fingerprint_id=...&fingerprint_hash=...&expires_at=...
// Returns {status:"pending"} | {status:"ready", auth_token, user_id, email} | {status:"error", error}
func HandleFBDevicePoll(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		fpID := c.Query("fingerprint_id")
		fpHash := c.Query("fingerprint_hash")
		var expiresAt int64
		fmt.Sscanf(c.Query("expires_at"), "%d", &expiresAt)
		if fpID == "" || fpHash == "" {
			c.JSON(400, gin.H{"error": "fingerprint_id and fingerprint_hash are required"})
			return
		}
		res, err := upstream.PollFreebuffDeviceAuth(fpID, fpHash, expiresAt)
		if err != nil {
			c.JSON(502, gin.H{"status": "error", "error": err.Error()})
			return
		}
		switch res.Status {
		case "ready":
			// Auto-import the token to the pool with email + userID from poll response
			fbAM.AddAccountWithInfo(res.AuthToken, res.UserID, res.Email)
			c.JSON(200, gin.H{
				"status":     "ready",
				"auth_token": res.AuthToken,
				"user_id":    res.UserID,
				"email":      res.Email,
			})
		case "error":
			c.JSON(200, gin.H{"status": "error", "error": res.Error})
		default:
			c.JSON(200, gin.H{"status": "pending"})
		}
	}
}

// HandleFBQuotaSync syncs quota for all or one Freebuff account.
// POST /fb/quota/sync {"token":"optional"} (empty = sync all)
func HandleFBQuotaSync(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Token string `json:"token"`
		}
		_ = c.ShouldBindJSON(&req)

		if strings.TrimSpace(req.Token) != "" {
			// Sync single account
			acc := fbAM.GetAccount(req.Token)
			if acc == nil {
				c.JSON(404, gin.H{"error": "account not found"})
				return
			}
			if err := fbAM.SyncQuota(acc); err != nil {
				c.JSON(502, gin.H{"error": err.Error()})
				return
			}
			c.JSON(200, gin.H{"status": "ok", "synced": 1})
			return
		}

		// Sync all
		fbAM.SyncAllQuota()
		c.JSON(200, gin.H{"status": "ok", "synced": fbAM.Len()})
	}
}

// HandleFBDeleteAccount removes a Freebuff account.
// DELETE /fb/accounts/:token
func HandleFBDeleteAccount(fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.Param("token")
		if token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing token parameter"})
			return
		}
		fbAM.RemoveAccount(token)
		c.JSON(http.StatusOK, gin.H{"deleted": true})
	}
}
