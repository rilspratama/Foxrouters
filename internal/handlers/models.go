package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/upstream"
)

// HandleModelsRefresh: POST /models/refresh — manually trigger the dynamic
// model registry refresh (admin). Returns per-upstream source + last-sync +
// count + error (empty on success). CodeBuddy is always static (no endpoint).
func HandleModelsRefresh(grokAM *upstream.GrokAccountManager, aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var fbErr, grokErr, aliErr error
		if err := upstream.RefreshFBModels(); err != nil {
			fbErr = err
		}
		if grokAM != nil {
			if err := upstream.RefreshGrokModels(grokAM); err != nil {
				grokErr = err
			}
		}
		if aliAM != nil {
			if err := upstream.RefreshAliModels(aliAM); err != nil {
				aliErr = err
			}
		}

		fbSrc, fbSynced, fbCount := upstream.FBModelsInfo()
		gSrc, gSynced, gCount := upstream.GrokModelsInfo()
		aSrc, aSynced, aCount := upstream.AliModelsInfo()

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
			"alibaba": gin.H{
				"source":    aSrc,
				"synced_at": aSynced,
				"count":     aCount,
				"error":     errOrEmpty(aliErr),
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
