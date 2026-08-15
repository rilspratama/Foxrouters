package upstream

import (
	"errors"
	"strings"
	"time"
)

const (
	FREEBUFF_API_BASE     = "https://www.codebuff.com"
	FREEBUFF_CHAT_PATH    = "/api/v1/chat/completions"
	FREEBUFF_SESSION_PATH = "/api/v1/freebuff/session"
	FREEBUFF_RUN_PATH     = "/api/v1/agent-runs"
	FREEBUFF_ME_PATH      = "/api/v1/me"
	FREEBUFF_STREAK_PATH  = "/api/v1/freebuff/streak"
	FREEBUFF_ADS_PATH     = "/api/v1/ads"

	FREEBUFF_BUFFY_PREFIX = "You are Buffy, the strategic coding assistant."

	// Timeouts
	FREEBUFF_UPSTREAM_TIMEOUT  = 25 * time.Second
	FREEBUFF_SESSION_TIMEOUT   = 12 * time.Second
	FREEBUFF_NONSTREAM_TIMEOUT = 50 * time.Second

	// Session cache: reuse if >60s remaining
	FREEBUFF_SESSION_MIN_REMAINING = 60 * time.Second

	// Run cache: reuse runId for 10 minutes
	FREEBUFF_RUN_CACHE_TTL = 10 * time.Minute

	// Redis key prefixes (persistent session/run cache — survives gateway restart)
	fbSessionKeyPrefix = "fb:session:"
	fbRunKeyPrefix     = "fb:run:"
)

// ErrFBBanned marks an account permanently banned by Freebuff upstream.
// Detected from POST /session 403 {"status":"banned"} or GET /session status:"banned".
var ErrFBBanned = errors.New("freebuff account banned")

// FreebuffModels maps gateway model IDs (fb/ prefix) to upstream config.
type FreebuffModelConfig struct {
	GatewayID string // e.g. "fb/deepseek-v4-flash"
	Upstream  string // e.g. "deepseek/deepseek-v4-flash"
	Agent     string // e.g. "base2-free-deepseek-flash"
	Reasoning bool
	FullMode  bool // true = only available in full access mode (US/EU exit)
}

var FreebuffModels = []FreebuffModelConfig{
	{"fb/deepseek-v4-flash", "deepseek/deepseek-v4-flash", "base2-free-deepseek-flash", true, false},
	{"fb/mimo-v2.5", "mimo/mimo-v2.5", "base2-free-mimo", false, false},
	{"fb/deepseek-v4-pro", "deepseek/deepseek-v4-pro", "base2-free-deepseek", true, true},
	{"fb/minimax-m3", "minimax/minimax-m3", "base2-free-minimax-m3", false, true},
	{"fb/gpt-5.6-luna", "openai/gpt-5.6-luna", "base2-free-luna", false, true},
	{"fb/glm-5.2", "z-ai/glm-5.2", "base2-free-glm", true, true},
}

// IsFreebuffModel returns true if the model routes to the Freebuff upstream.
func IsFreebuffModel(model string) bool {
	return strings.HasPrefix(model, "fb/")
}

// fbStripPrefix removes the "fb/" prefix → upstream model name.
func fbStripPrefix(model string) string {
	return strings.TrimPrefix(model, "fb/")
}

// fbModelConfig looks up the model config by gateway ID (dynamic registry
// first, static fallback).
func fbModelConfig(gatewayID string) *FreebuffModelConfig {
	models := GetFBModels()
	for i := range models {
		if models[i].GatewayID == gatewayID {
			return &models[i]
		}
	}
	// Default to deepseek-v4-flash
	return &models[0]
}
