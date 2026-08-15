package handlers

import (
	"foxrouters/internal/db"
	"log/slog"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func HandleHistory(store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		hours := 24
		if h := c.Query("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 {
				hours = v
			}
		}
		since := time.Now().Add(-time.Duration(hours) * time.Hour)

		stats, err := store.GetRequestStats(since)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err)
			c.JSON(500, gin.H{"error": "internal server error"})
			return
		}

		modelStats, err := store.GetModelStats(since, 20)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err)
			c.JSON(500, gin.H{"error": "internal server error"})
			return
		}

		c.JSON(200, gin.H{
			"period_hours":     hours,
			"since":            since.Format(time.RFC3339),
			"total_requests":   stats.TotalRequests,
			"total_errors":     stats.TotalErrors,
			"error_rate_pct":   stats.ErrorRate,
			"avg_latency_ms":   stats.AvgLatencyMs,
			"total_tokens_in":  stats.TotalTokensIn,
			"total_tokens_out": stats.TotalTokensOut,
			"total_tokens":     stats.TotalTokens,
			"by_model":         modelStats,
		})
	}
}

// HandleRecentRequests returns recent request previews (id as string for JS).
// Optional query filters: model, upstream, status (exact or 2xx/3xx/4xx/5xx),
// errors=1 (only errored rows), hours=N (within last N hours), limit=N.
func HandleRecentRequests(store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 50
		if l := c.Query("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 500 {
				limit = v
			}
		}
		f := db.RecentFilter{
			Model:    c.Query("model"),
			Upstream: c.Query("upstream"),
			Status:   c.Query("status"),
		}
		if c.Query("errors") == "1" || c.Query("errors") == "true" {
			f.ErrorOnly = true
		}
		if h := c.Query("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 && v <= 24*365 {
				f.Hours = v
			}
		}
		logs, err := store.GetRecentRequests(limit, f)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err)
			c.JSON(500, gin.H{"error": "internal server error"})
			return
		}
		c.JSON(200, gin.H{"recent_requests": logs, "count": len(logs)})
	}
}

// HandleHistoryDetail returns the full request/response JSON for a single log.
func HandleHistoryDetail(store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil || id == 0 {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		detail, err := store.GetRequestDetail(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "request not found"})
			return
		}
		c.JSON(200, detail)
	}
}
