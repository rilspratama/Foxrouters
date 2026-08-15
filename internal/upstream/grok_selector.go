package upstream

import (
	"fmt"
	"foxrouters/internal/db"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

func ExpandGrokAlias(model string) (string, bool) {
	switch model {
	case "grok-4.5-high", "grok-4.5-xhigh":
		return "high", true
	case "grok-4.5-medium":
		return "medium", true
	case "grok-4.5-low":
		return "low", true
	case "grok-4.5-auto":
		return "auto", true
	case "grok-4.5-none":
		return "none", true
	default:
		return "", false
	}
}

// IsGrokModel returns true if the model routes to the Grok upstream.
func IsGrokModel(model string) bool {
	return strings.HasPrefix(model, "grok-")
}

func grokHeaders(token, accept, model, userID, email string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", accept)
	h.Set("X-XAI-Token-Auth", "xai-grok-cli")
	h.Set("x-authenticateresponse", "authenticate-response")
	h.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	h.Set("x-grok-client-identifier", GROK_CLIENT_IDENTIFIER)
	h.Set("x-grok-client-mode", "tui")
	h.Set("User-Agent", fmt.Sprintf("grok-shell/%s (linux; x86_64)", GROK_CLIENT_VERSION))
	if userID != "" {
		h.Set("x-userid", userID)
	}
	if email != "" {
		h.Set("x-email", email)
	}
	convID := fmt.Sprintf("conv-%d", time.Now().UnixNano())
	reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	sessionID := fmt.Sprintf("sess-%d", time.Now().UnixNano())
	agentID := "agent-shell"
	h.Set("x-grok-conv-id", convID)
	h.Set("x-grok-req-id", reqID)
	h.Set("x-grok-model-override", model)
	h.Set("x-grok-session-id", sessionID)
	h.Set("x-grok-agent-id", agentID)
	return h
}

// ---------------------------------------------------------------------------
// Account selection modes (router-level strategy, runtime-configurable)
// ---------------------------------------------------------------------------

// GrokSelectorMode picks how ProxyGrok chooses the upstream account.
type GrokSelectorMode string

const (
	// GrokSelectorRR — classic round-robin over enabled accounts (no cache locality).
	GrokSelectorRR GrokSelectorMode = "rr"
	// GrokSelectorSticky — session-id header binds conversation to one account
	// until it dies (per-conversation cache locality; different sessions may
	// land on different accounts, re-caching the shared system prompt each time).
	GrokSelectorSticky GrokSelectorMode = "sticky"
	// GrokSelectorContentHash — hash(model + first system message) deterministically
	// maps to one account: every session sharing the same system prompt lands on
	// the same account → giant shared prefix cached once.
	GrokSelectorContentHash GrokSelectorMode = "content-hash"
	// GrokSelectorHybrid — content-hash picks a small bucket (~3 accounts);
	// session-id sticks to one account inside the bucket. Dead accounts rebind
	// within the bucket, keeping the shared system-prompt cache warm.
	GrokSelectorHybrid GrokSelectorMode = "hybrid"
)

// grokHybridBucketSize is how many enabled accounts form one hybrid bucket.
const grokHybridBucketSize = 3

// grokSelectorMode holds the active mode (atomic for lock-free hot-path reads).
var grokSelectorMode atomic.Value // stores GrokSelectorMode

func init() {
	m := GrokSelectorMode(os.Getenv("GROK_SELECTOR_MODE"))
	if !validGrokSelectorMode(m) {
		m = GrokSelectorSticky // default: sticky sessions (prompt-cache locality)
	}
	grokSelectorMode.Store(m)
}

func validGrokSelectorMode(m GrokSelectorMode) bool {
	switch m {
	case GrokSelectorRR, GrokSelectorSticky, GrokSelectorContentHash, GrokSelectorHybrid:
		return true
	}
	return false
}

// GetGrokSelectorMode returns the active Grok account selection mode.
func GetGrokSelectorMode() GrokSelectorMode { return grokSelectorMode.Load().(GrokSelectorMode) }

// SetGrokSelectorMode validates + stores the mode and persists it to Redis
// (grok:config hash) so restarts keep the operator's choice.
func SetGrokSelectorMode(store *db.Store, m GrokSelectorMode) error {
	if !validGrokSelectorMode(m) {
		return fmt.Errorf("invalid selector mode %q (valid: rr|sticky|content-hash|hybrid)", m)
	}
	grokSelectorMode.Store(m)
	if store != nil {
		if err := store.SetGrokConfig("selector_mode", string(m)); err != nil {
			slog.Warn("selector mode persist failed", "module", "grok", "error", err)
		}
	}
	slog.Info("grok selector mode changed", "module", "grok", "mode", m)
	return nil
}

// LoadGrokSelectorMode restores the persisted mode from Redis (called at startup).
func LoadGrokSelectorMode(store *db.Store) {
	if store == nil {
		return
	}
	if v, err := store.GetGrokConfig("selector_mode"); err == nil && validGrokSelectorMode(GrokSelectorMode(v)) {
		grokSelectorMode.Store(GrokSelectorMode(v))
		slog.Info("grok selector mode restored", "module", "grok", "mode", v)
	}
}

// NextForMode selects an account according to the active mode.
//   - sessionID: from x-session-id/x-conversation-id/x-chat-id header (may be "")
//   - sysHash:   hash of model + first system message content (may be "")
func (am *GrokAccountManager) NextForMode(mode GrokSelectorMode, sessionID, sysHash string) (*GrokAccount, error) {
	switch mode {
	case GrokSelectorRR:
		return am.Next()
	case GrokSelectorContentHash:
		return am.nextByHash(sysHash)
	case GrokSelectorHybrid:
		return am.nextHybrid(sessionID, sysHash)
	case GrokSelectorSticky:
		fallthrough
	default:
		if sessionID != "" {
			return am.NextSticky(sessionID)
		}
		return am.Next()
	}
}

// nextByHash: deterministic account from sysHash (FNV-1a over enabled accounts).
// All sessions with the same system prompt land on the same account.
func (am *GrokAccountManager) nextByHash(sysHash string) (*GrokAccount, error) {
	am.mu.RLock()
	enabled := make([]*GrokAccount, 0, len(am.accounts))
	for _, acc := range am.accounts {
		if !acc.IsDisabled() {
			enabled = append(enabled, acc)
		}
	}
	am.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all grok accounts disabled")
	}
	if sysHash == "" {
		return am.Next()
	}
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	return enabled[h.Sum64()%uint64(len(enabled))], nil
}

// nextHybrid: content-hash selects a bucket of grokHybridBucketSize enabled
// accounts; session-id sticks to one account inside the bucket (rebinding
// within the bucket when the account dies → shared system-prompt cache stays
// warm).
func (am *GrokAccountManager) nextHybrid(sessionID, sysHash string) (*GrokAccount, error) {
	am.mu.RLock()
	enabled := make([]*GrokAccount, 0, len(am.accounts))
	for _, acc := range am.accounts {
		if !acc.IsDisabled() {
			enabled = append(enabled, acc)
		}
	}
	am.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all grok accounts disabled")
	}
	if sysHash == "" {
		if sessionID != "" {
			return am.NextSticky(sessionID)
		}
		return am.Next()
	}

	// Bucket = consecutive slice of enabled accounts starting at hash position.
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	start := int(h.Sum64() % uint64(len(enabled)))
	bucket := make([]*GrokAccount, 0, grokHybridBucketSize)
	for i := 0; i < grokHybridBucketSize && i < len(enabled); i++ {
		bucket = append(bucket, enabled[(start+i)%len(enabled)])
	}

	// Session binding within bucket (reuse sticky map — but only accept the
	// bound account if it is in THIS bucket; otherwise rebind into the bucket).
	if sessionID != "" {
		am.stickyMu.Lock()
		if b, ok := am.sticky[sessionID]; ok && !b.acc.IsDisabled() {
			for _, acc := range bucket {
				if acc == b.acc {
					b.lastSeen = time.Now()
					am.stickyMu.Unlock()
					return acc, nil
				}
			}
		}
		delete(am.sticky, sessionID)
		// bind first bucket account (deterministic per session: hash sessionID)
		sh := fnv.New64a()
		sh.Write([]byte(sessionID))
		pick := bucket[sh.Sum64()%uint64(len(bucket))]
		am.sticky[sessionID] = &grokStickyBinding{acc: pick, lastSeen: time.Now()}
		am.stickyMu.Unlock()
		return pick, nil
	}
	return bucket[0], nil
}
