// Package proxy — CustomRegistry: runtime-configurable custom model list and
// alias table. Backed by Redis (see internal/db) and cached in memory behind
// a sync.RWMutex so the hot path is a single lock-free map lookup after Load.
//
// Two mechanisms:
//
//	Custom model    "cb/kimi-k3" → {upstream: codebuddy, model_name: "kimi-k3",
//	                                owned_by: "codebuddy"}
//	  Adds a new routable model id that the proxy will forward to a specific
//	  upstream, overriding the default cb/ vs grok- prefix routing.
//
//	Alias           "my-claude" → "cb/claude-sonnet-4.6"
//	  Rewrites the incoming model field to the target before routing.
//
// Resolve() first walks the alias table (single hop, no recursion), then
// checks whether the resolved id is a custom model, and returns whichever
// upstream + model_name pair to use. Callers that get an empty upstream fall
// back to the built-in grok-* / cb/* routing.
package proxy

import (
	"fmt"
	"strings"
	"sync"

	"foxrouters/internal/db"
)

// CustomRegistry is a thread-safe in-memory cache of custom models + aliases.
type CustomRegistry struct {
	mu      sync.RWMutex
	models  map[string]db.CustomModel // model_id → config
	aliases map[string]string         // alias → target model_id
	store   *db.Store
}

// NewCustomRegistry builds an empty registry bound to the given DB store.
// Call Load() before serving requests.
func NewCustomRegistry(store *db.Store) *CustomRegistry {
	return &CustomRegistry{
		models:  map[string]db.CustomModel{},
		aliases: map[string]string{},
		store:   store,
	}
}

// Load pulls the current state from Redis. Safe to call at startup and on
// mutation (functions AddModel/AddAlias/DeleteModel/DeleteAlias already do).
// With a nil store the call is a no-op (used by tests that seed the cache
// directly).
func (r *CustomRegistry) Load() error {
	if r == nil || r.store == nil {
		return nil
	}
	models, err := r.store.LoadCustomModels()
	if err != nil {
		return err
	}
	aliases, err := r.store.LoadCustomAliases()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.models = models
	r.aliases = aliases
	r.mu.Unlock()
	return nil
}

// Reload is an alias for Load — semantic sugar for callers that want to make
// the "post-mutation refresh" intent obvious.
func (r *CustomRegistry) Reload() error { return r.Load() }

// Resolve applies alias + custom-model lookup to an incoming model name.
//
// Returns:
//
//	resolvedModel — model id to place in the outgoing request body. If an
//	                alias was hit this is the alias target; otherwise the
//	                input is returned unchanged.
//	upstream      — "codebuddy" | "grok" if a custom model matched, empty
//	                otherwise (caller falls back to default routing).
//	modelName     — the actual model string the upstream should see (custom
//	                model's ModelName override). Empty when upstream is empty.
//
// Aliases resolve only one hop deep — chained aliases (a→b→c) are intentional
// non-supported to keep resolution O(1) and avoid loops.
func (r *CustomRegistry) Resolve(model string) (resolvedModel, upstream, modelName string) {
	if r == nil {
		return model, "", ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	resolvedModel = model
	if target, ok := r.aliases[model]; ok && target != "" {
		resolvedModel = target
	}
	if cm, ok := r.models[resolvedModel]; ok {
		return resolvedModel, cm.Upstream, cm.ModelName
	}
	return resolvedModel, "", ""
}

// ListModelsEntry is a flat view of one custom model for /v1/models rendering.
type ListModelsEntry struct {
	ID      string
	OwnedBy string
}

// ListModels returns a stable-ordered slice of custom-model entries so the
// /v1/models endpoint can append them to its hardcoded list.
func (r *CustomRegistry) ListModels() []ListModelsEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]ListModelsEntry, 0, len(r.models))
	for id, cm := range r.models {
		owner := cm.OwnedBy
		if owner == "" {
			owner = cm.Upstream
		}
		out = append(out, ListModelsEntry{ID: id, OwnedBy: owner})
	}
	r.mu.RUnlock()
	return out
}

// SnapshotModels returns a copy of the custom-model map for dashboard reads.
func (r *CustomRegistry) SnapshotModels() map[string]db.CustomModel {
	out := map[string]db.CustomModel{}
	if r == nil {
		return out
	}
	r.mu.RLock()
	for k, v := range r.models {
		out[k] = v
	}
	r.mu.RUnlock()
	return out
}

// SnapshotAliases returns a copy of the alias map for dashboard reads.
func (r *CustomRegistry) SnapshotAliases() map[string]string {
	out := map[string]string{}
	if r == nil {
		return out
	}
	r.mu.RLock()
	for k, v := range r.aliases {
		out[k] = v
	}
	r.mu.RUnlock()
	return out
}

// AddModel validates + persists a custom-model entry and refreshes the cache.
// With a nil store the entry is kept only in memory (used by tests).
func (r *CustomRegistry) AddModel(id string, cm db.CustomModel) error {
	if r == nil {
		return fmt.Errorf("registry not initialised")
	}
	id = strings.TrimSpace(id)
	cm.Upstream = strings.TrimSpace(cm.Upstream)
	cm.ModelName = strings.TrimSpace(cm.ModelName)
	cm.OwnedBy = strings.TrimSpace(cm.OwnedBy)
	if err := validateName(id, "id"); err != nil {
		return err
	}
	switch cm.Upstream {
	case "codebuddy", "grok", "freebuff":
	default:
		return fmt.Errorf("upstream must be 'codebuddy', 'grok' or 'freebuff'")
	}
	if cm.ModelName == "" {
		// Default: derive from id (strip the provider prefix).
		switch cm.Upstream {
		case "codebuddy":
			cm.ModelName = strings.TrimPrefix(id, "cb/")
		case "freebuff":
			cm.ModelName = strings.TrimPrefix(id, "fb/")
		default:
			cm.ModelName = id
		}
	}
	if cm.OwnedBy == "" {
		cm.OwnedBy = cm.Upstream
	}
	if r.store != nil {
		if err := r.store.SaveCustomModel(id, cm); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.models[id] = cm
	r.mu.Unlock()
	return nil
}

// DeleteModel removes one custom-model entry from Redis + cache.
func (r *CustomRegistry) DeleteModel(id string) error {
	if r == nil {
		return fmt.Errorf("registry not initialised")
	}
	if r.store != nil {
		if err := r.store.DeleteCustomModel(id); err != nil {
			return err
		}
	}
	r.mu.Lock()
	delete(r.models, id)
	r.mu.Unlock()
	return nil
}

// AddAlias validates + persists one alias → target mapping.
func (r *CustomRegistry) AddAlias(alias, target string) error {
	if r == nil {
		return fmt.Errorf("registry not initialised")
	}
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if err := validateName(alias, "alias"); err != nil {
		return err
	}
	if err := validateName(target, "target"); err != nil {
		return err
	}
	if alias == target {
		return fmt.Errorf("alias cannot point to itself")
	}
	if r.store != nil {
		if err := r.store.SaveCustomAlias(alias, target); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.aliases[alias] = target
	r.mu.Unlock()
	return nil
}

// DeleteAlias removes one alias entry from Redis + cache.
func (r *CustomRegistry) DeleteAlias(alias string) error {
	if r == nil {
		return fmt.Errorf("registry not initialised")
	}
	if r.store != nil {
		if err := r.store.DeleteCustomAlias(alias); err != nil {
			return err
		}
	}
	r.mu.Lock()
	delete(r.aliases, alias)
	r.mu.Unlock()
	return nil
}
