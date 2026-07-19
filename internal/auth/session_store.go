package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// SessionStore is an in-memory session token → API key map.
// Cookie value is now a random session token (P3-3), NOT the raw API key.
// Sessions expire after 7 days (sliding). Tokens are 256-bit random.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	key       string    // underlying gateway API key
	createdAt time.Time
	lastSeen  time.Time
}

// SessionTTL is the maximum lifetime of a session (7 days).
const SessionTTL = 7 * 24 * time.Hour

// NewSessionStore creates a session store with a background cleanup goroutine.
func NewSessionStore() *SessionStore {
	s := &SessionStore{sessions: make(map[string]*session)}
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			s.cleanup()
		}
	}()
	return s
}

// Create issues a new session token bound to the given API key.
func (s *SessionStore) Create(apiKey string) (string, error) {
	buf := make([]byte, 32) // 256-bit
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	s.mu.Lock()
	s.sessions[token] = &session{key: apiKey, createdAt: now, lastSeen: now}
	s.mu.Unlock()
	return token, nil
}

// Lookup returns the API key bound to the session token, or empty string.
// Slides the lastSeen forward on hit (extends TTL while in use).
func (s *SessionStore) Lookup(token string) string {
	if token == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return ""
	}
	if time.Since(sess.createdAt) > SessionTTL {
		delete(s.sessions, token)
		return ""
	}
	sess.lastSeen = time.Now()
	return sess.key
}

// Revoke deletes a session (logout).
func (s *SessionStore) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *SessionStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for token, sess := range s.sessions {
		if now.Sub(sess.createdAt) > SessionTTL {
			delete(s.sessions, token)
		}
	}
}
