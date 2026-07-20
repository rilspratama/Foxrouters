// Package proxy — bufferedWriter: a gin.ResponseWriter that buffers status,
// headers, and body in memory instead of streaming to the real writer.
// Used by the combo fallback retry chain: we invoke the upstream through a
// buffered writer, inspect the resulting status, and either flush to the
// real writer (on success / 4xx) or discard + try the next model (on 5xx).
//
// Streaming (SSE) responses are NOT retried — see comboStrategy == "fallback"
// && !clientStream guard in ProxyRequest.
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// bufferedWriter satisfies gin.ResponseWriter but writes to an in-memory
// buffer. Headers are copied through to gin's underlying header map before
// flush so the caller sees the real Content-Type etc.
type bufferedWriter struct {
	real    gin.ResponseWriter
	buf     bytes.Buffer
	status  int
	headers http.Header
	written bool
}

func newBufferedWriter(real gin.ResponseWriter) *bufferedWriter {
	return &bufferedWriter{
		real:    real,
		status:  200,
		headers: http.Header{},
	}
}

// Header returns the pending header map. Headers written here are copied
// onto the real writer at flush time (so mutations before WriteHeader are
// preserved).
func (b *bufferedWriter) Header() http.Header { return b.headers }

// WriteHeader captures the status code. First call wins (matches
// http.ResponseWriter semantics).
func (b *bufferedWriter) WriteHeader(code int) {
	if b.written {
		return
	}
	b.status = code
	b.written = true
}

// Write appends bytes to the internal buffer. Returns len(data), nil —
// upstream code that checks the byte count still works.
func (b *bufferedWriter) Write(data []byte) (int, error) {
	if !b.written {
		b.written = true
	}
	return b.buf.Write(data)
}

// WriteString convenience — forwards to Write.
func (b *bufferedWriter) WriteString(s string) (int, error) {
	return b.Write([]byte(s))
}

// Status returns the captured status code (200 if WriteHeader was never
// called explicitly, matching net/http default).
func (b *bufferedWriter) Status() int { return b.status }

// Size returns the buffered body length.
func (b *bufferedWriter) Size() int { return b.buf.Len() }

// Written reports whether any Write / WriteHeader has happened.
func (b *bufferedWriter) Written() bool { return b.written }

// WriteHeaderNow is a gin extension — headers are always deferred until
// flush() runs, so this is a no-op for the buffered writer.
func (b *bufferedWriter) WriteHeaderNow() {}

// Pusher: HTTP/2 push isn't meaningful for buffered upstream responses.
func (b *bufferedWriter) Pusher() http.Pusher { return nil }

// Flush is a no-op — the buffered writer never streams. Use flush() (unexported)
// to actually push bytes onto the real writer once the combo loop settles on
// a terminal response.
func (b *bufferedWriter) Flush() {}

// CloseNotify: legacy hook. Return a channel that never fires; the real
// writer's context-based cancellation is what we actually care about.
func (b *bufferedWriter) CloseNotify() <-chan bool { return make(chan bool) }

// Hijack: buffered writer doesn't support connection hijacking.
func (b *bufferedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("bufferedWriter does not support hijack")
}

// flush copies the captured status + headers + body onto the real writer.
// After flush the buffered writer should not be used again — repeat flushes
// would double-write the body.
//
// C9 fix: merge instead of replace. The real header map may already carry
// entries set by upstream middleware (Vary, X-Request-ID, CSRF, etc.).
// A blanket `realHeaders[k] = v` clobbered those; we now Add each value
// so both middleware-set and upstream-set headers survive to the wire.
func (b *bufferedWriter) flush() {
	realHeaders := b.real.Header()
	for k, vs := range b.headers {
		for _, v := range vs {
			realHeaders.Add(k, v)
		}
	}
	b.real.WriteHeader(b.status)
	if b.buf.Len() > 0 {
		_, _ = b.real.Write(b.buf.Bytes())
	}
}
