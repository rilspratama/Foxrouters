package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStickySessionID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no headers", nil, ""},
		{"opencode x-session-id", map[string]string{"X-Session-Id": "ses_abc123"}, "ses_abc123"},
		{"codex session-id", map[string]string{"Session-Id": "01a005ba-147d-7c62-b460-2e97b7216633"}, "01a005ba-147d-7c62-b460-2e97b7216633"},
		{"codex thread-id wins over session-id", map[string]string{"Session-Id": "ses-root", "Thread-Id": "thr-subagent"}, "thr-subagent"},
		{"claude code session", map[string]string{"X-Claude-Code-Session-Id": "bc1212c1-4555-4552-a770-b436a9353d5d"}, "bc1212c1-4555-4552-a770-b436a9353d5d"},
		{"generic conversation-id fallback", map[string]string{"X-Conversation-Id": "conv-1"}, "conv-1"},
		{"priority: x-session-id over thread-id", map[string]string{"X-Session-Id": "ses_a", "Thread-Id": "thr_b"}, "ses_a"},
		{"trimmed", map[string]string{"Session-Id": "  uuid-1  "}, "uuid-1"},
		{"capped at 128", map[string]string{"Session-Id": repeatStr("x", 300)}, repeatStr("x", 128)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			if got := StickySessionID(c); got != tc.want {
				t.Fatalf("StickySessionID = %q, want %q", got, tc.want)
			}
		})
	}
}

func repeatStr(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
