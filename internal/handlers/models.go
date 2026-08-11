package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/upstream"
)

// HandleModelsRefresh: POST /models/refresh — manually trigger the dynamic
// model registry refresh (admin). Returns per-upstream source + last-sync +
// count + error (empty on success). CodeBuddy is always static (no endpoint).
func HandleModelsRefresh(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var fbErr, grokErr error
		if err := upstream.RefreshFBModels(); err != nil {
			fbErr = err
		}
		if grokAM != nil {
			if err := upstream.RefreshGrokModels(grokAM); err != nil {
				grokErr = err
			}
		}

		fbSrc, fbSynced, fbCount := upstream.FBModelsInfo()
		gSrc, gSynced, gCount := upstream.GrokModelsInfo()

		c.JSON(http.StatusOK, gin.H{
			"freebuff": gin.H{
				"source":    fbSrc,
				"synced_at": fbSynced,
				"count":     fbCount,
				"error":     errOrEmpty(fbErr),
			},
			"grok": gin.H{
				"source":    gSrc,
				"synced_at": gSynced,
				"count":     gCount,
				"error":     errOrEmpty(grokErr),
			},
			"codebuddy": gin.H{
				"source": "static (no models endpoint)",
				"count":  0,
				"error":  "",
			},
		})
	}
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
