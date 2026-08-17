package upstream

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// stickySessionHeaders lists request headers (priority order) that identify a
// client conversation for sticky routing across harnesses:
//
//	x-session-id / x-session-affinity — OpenCode (ses_<base62>)
//	x-conversation-id / x-chat-id     — generic OpenAI-compatible clients
//	thread-id / session-id            — Codex CLI (UUID; thread diverges per
//	                                    subagent — best locality + parallelism)
//	x-claude-code-session-id          — Claude Code CLI (UUID)
//
// NOTE: Codex also sends x-codex-turn-metadata with a per-turn turn_id. That
// MUST NOT be used as a sticky key — turn_id rotates every request and would
// pin every request to a fresh account, destroying cache locality.
var stickySessionHeaders = []string{
	"x-session-id",
	"x-conversation-id",
	"x-chat-id",
	"thread-id",
	"session-id",
	"x-claude-code-session-id",
	"x-session-affinity",
}

// maxStickySessionLen bounds the sticky map key size.
const maxStickySessionLen = 128

// StickySessionID extracts the conversation identifier from request headers.
// First non-empty header wins (priority per stickySessionHeaders). Returns ""
// when the client sends none — caller then falls back to round-robin or the
// active mode's default.
func StickySessionID(c *gin.Context) string {
	for _, h := range stickySessionHeaders {
		if v := c.GetHeader(h); v != "" {
			v = strings.TrimSpace(v)
			if len(v) > maxStickySessionLen {
				v = v[:maxStickySessionLen]
			}
			return v
		}
	}
	return ""
}
