package handlers

import (
	"net/http"

	"foxrouters/internal/upstream"
	"github.com/gin-gonic/gin"
)

// HandleAliModelUsage: GET /ali/models/usage — free-tier limit + accumulated
// per-model usage (limit vs used vs remaining), sorted by used % desc.
func HandleAliModelUsage(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if aliAM == nil {
			c.JSON(http.StatusOK, gin.H{"models": []upstream.AliModelUsage{}, "error": "alibaba not configured"})
			return
		}
		list := aliAM.AliModelUsageList()
		var totalUsed int64
		var totalRequests int64
		for _, m := range list {
			totalUsed += m.TokensIn + m.TokensOut
			totalRequests += m.Requests
		}
		c.JSON(http.StatusOK, gin.H{
			"models":        list,
			"total_used":    totalUsed,
			"total_requests": totalRequests,
			"limit_count":   upstream.AliQuotaCount(),
			"active_keys":   aliAM.ActiveKeyCount(),
		})
	}
}
