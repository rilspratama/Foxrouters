// Package proxy — ComboRegistry: groups multiple models under one logical
// name ("combo/<name>") with a routing strategy applied per request. Backed
// by Redis (see internal/db) and cached in memory behind a sync.RWMutex.
//
// Two strategies:
//
//   fallback     Try models in order — models[0] first; on upstream failure
//                the proxy caller walks NextInFallback until either one
//                succeeds or the list is exhausted. Resolve() returns the
//                head of the list.
//
//   round_robin  Rotate models across requests. Resolve() calls
//                IncrComboCounter and picks models[counter % len(Models)].
//                Atomic INCR gives cluster-safe fairness even under
//                concurrent traffic.
//
// Combos are addressed as "combo/<name>" — the proxy strips the prefix,
// looks up the combo, and rewrites the outgoing model field before default
// routing. See proxy.ProxyRequest for the wiring.
package proxy

import (
	"fmt"
	"strings"
	"sync"

	"foxrouters/internal/db"
)

// Combo is re-exported from internal/db for callers that only import proxy.
type Combo = db.Combo

// ComboRegistry is a thread-safe in-memory cache of combos.
type ComboRegistry struct {
	mu     sync.RWMutex
	combos map[string]Combo
	store  *db.Store
}

// NewComboRegistry builds an empty registry bound to the given DB store.
// Call Load() before serving requests.
func NewComboRegistry(store *db.Store) *ComboRegistry {
	return &ComboRegistry{
		combos: map[string]Combo{},
		store:  store,
	}
}

// Load pulls the current state from Redis. Safe to call at startup and on
// mutation (functions AddCombo/DeleteCombo already do). With a nil store
// the call is a no-op (used by tests that seed the cache directly).
func (r *ComboRegistry) Load() error {
	if r == nil || r.store == nil {
		return nil
	}
	combos, err := r.store.LoadCombos()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.combos = combos
	r.mu.Unlock()
	return nil
}

// Reload is an alias for Load — sugar for mutation call sites.
func (r *ComboRegistry) Reload() error { return r.Load() }

// Resolve maps an incoming model name onto the next model to route to.
//
// If model starts with "combo/", the trailing segment is looked up. On hit
// the strategy picks the next model:
//   - fallback     → models[0] (retry chain handled by proxy caller)
//   - round_robin  → models[INCR(counter) % len(models)] (Redis atomic)
//
// Returns (nextModel, true) on a combo hit, ("", false) otherwise.
//
// A nil registry, an unknown combo, or a combo with zero models all yield
// ("", false) so callers can fall through to the built-in routing without
// special-casing.
func (r *ComboRegistry) Resolve(model string) (string, bool) {
	if r == nil {
		return "", false
	}
	if !strings.HasPrefix(model, "combo/") {
		return "", false
	}
	name := strings.TrimPrefix(model, "combo/")
	r.mu.RLock()
	c, ok := r.combos[name]
	r.mu.RUnlock()
	if !ok || len(c.Models) == 0 {
		return "", false
	}
	switch c.Strategy {
	case "round_robin":
		// Fall back to in-process rotation when Redis is unavailable — keeps
		// tests + local dev functional. The mod is signed-safe: even on a
		// negative INCR result the (n%L + L)%L keeps the index in range.
		var counter int64
		if r.store != nil {
			if v, err := r.store.IncrComboCounter(name); err == nil {
				counter = v
			}
		}
		if counter == 0 {
			// No Redis (or first call after wraparound) — use a package-level
			// rotating fallback based on a hash of name so tests are
			// deterministic per-key.
			counter = fallbackCounter(name)
		}
		idx := int((counter - 1) % int64(len(c.Models)))
		if idx < 0 {
			idx += len(c.Models)
		}
		return c.Models[idx], true
	default:
		// "fallback" (also the default) — return head; caller can call
		// NextInFallback on upstream error.
		return c.Models[0], true
	}
}

// fallbackCounter is a per-process monotonic sequence used only when Redis
// is unavailable (tests, cold start with dead Redis). Not cluster-safe.
var (
	fallbackMu   sync.Mutex
	fallbackSeq  = map[string]int64{}
)

func fallbackCounter(name string) int64 {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	fallbackSeq[name]++
	return fallbackSeq[name]
}

// NextInFallback returns the model that comes after failedModel in the
// combo's list. Returns ("", false) when the failed model was the last
// entry, when the combo doesn't exist, or when it isn't a fallback combo.
func (r *ComboRegistry) NextInFallback(name, failedModel string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	c, ok := r.combos[name]
	r.mu.RUnlock()
	if !ok || c.Strategy != "fallback" || len(c.Models) == 0 {
		return "", false
	}
	for i, m := range c.Models {
		if m == failedModel && i+1 < len(c.Models) {
			return c.Models[i+1], true
		}
	}
	return "", false
}

// ListCombos returns a snapshot of every combo (order is map-random, but
// stable per snapshot). Used by /v1/models and the admin GET /api/combos.
func (r *ComboRegistry) ListCombos() []Combo {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]Combo, 0, len(r.combos))
	for _, c := range r.combos {
		out = append(out, c)
	}
	r.mu.RUnlock()
	return out
}

// GetCombo returns a single combo by name (case-sensitive).
func (r *ComboRegistry) GetCombo(name string) (Combo, bool) {
	if r == nil {
		return Combo{}, false
	}
	r.mu.RLock()
	c, ok := r.combos[name]
	r.mu.RUnlock()
	return c, ok
}

// AddCombo validates + persists a combo and refreshes the cache. Cache is
// updated only after the Redis write succeeds so a persistence error does
// not silently diverge the in-memory view.
func (r *ComboRegistry) AddCombo(c Combo) error {
	if r == nil {
		return fmt.Errorf("registry not initialised")
	}
	c.Name = strings.TrimSpace(c.Name)
	c.Strategy = strings.TrimSpace(c.Strategy)
	if err := validateName(c.Name, "name"); err != nil {
		return err
	}
	// Reserve the "combo/" prefix — combos are addressed as combo/<name>, so
	// letting the name itself start with "combo/" would create combo/combo/x.
	if strings.HasPrefix(c.Name, "combo/") {
		return fmt.Errorf("name must not start with 'combo/'")
	}
	switch c.Strategy {
	case "":
		c.Strategy = "fallback"
	case "fallback", "round_robin":
	default:
		return fmt.Errorf("strategy must be 'fallback' or 'round_robin'")
	}
	// Trim + drop empty model entries.
	cleaned := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		m = strings.TrimSpace(m)
		if m != "" {
			cleaned = append(cleaned, m)
		}
	}
	// P3-2: cap combo size to prevent Redis memory DoS.
	if len(cleaned) > 32 {
		return fmt.Errorf("combo models list too long (max 32, got %d)", len(cleaned))
	}
	if len(cleaned) == 0 {
		return fmt.Errorf("at least one model required")
	}
	c.Models = cleaned
	c.Description = strings.TrimSpace(c.Description)

	if r.store != nil {
		if err := r.store.SaveCombo(c); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.combos[c.Name] = c
	r.mu.Unlock()
	return nil
}

// DeleteCombo removes one combo (and its round-robin counter) from Redis +
// cache.
func (r *ComboRegistry) DeleteCombo(name string) error {
	if r == nil {
		return fmt.Errorf("registry not initialised")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	if r.store != nil {
		if err := r.store.DeleteCombo(name); err != nil {
			return err
		}
	}
	r.mu.Lock()
	delete(r.combos, name)
	r.mu.Unlock()
	return nil
}
