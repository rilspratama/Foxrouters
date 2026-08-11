package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// DYNAMIC MODEL REGISTRY
// ============================================================================
//
// Refreshes Freebuff + Grok model lists from upstream sources so new models
// appear WITHOUT code changes / rebuilds. Falls back to the static lists
// (FreebuffModels / hardcoded grok aliases) on any fetch/parse failure —
// zero downtime. CodeBuddy has NO models endpoint (verified) — stays static.
//
// Sources:
//   - Freebuff: pre-parsed JSON generated daily by the freebuff2api project
//     (mirrors CodebuffAI/freebuff official source). Contains the full model
//     table {id, session, agent, upstream} + pools {premium, glm, standard}.
//   - Grok: upstream GET /v1/models (authenticated with a live account) —
//     returns grok-4.5 + reasoning_efforts[] + context_window.

const (
	fbModelsReleaseURL      = "https://github.com/pingmike2/freebuff2api-wokers/releases/latest/download/freebuff-models.json"
	modelRegistryRefresh    = 6 * time.Hour
	modelRegistryFetchTo    = 15 * time.Second
	modelRegistryMaxBody    = 1 << 20 // 1MB
)

// grokModelEntry is one fetched Grok model entry from upstream /v1/models.
type grokModelEntry struct {
	ID               string   `json:"id"`
	ContextWindow    int      `json:"context_window"`
	ReasoningEfforts []string `json:"-"`
}

// modelRegistry holds the runtime model lists (mutex-protected).
type modelRegistry struct {
	sync.RWMutex
	fb         []FreebuffModelConfig
	fbSource   string
	fbSyncedAt time.Time
	grok       []grokModelEntry
	grokSource string
	grokSyncedAt time.Time
}

var modelReg modelRegistry

// GetFBModels returns the dynamic Freebuff model list, falling back to the
// static FreebuffModels when the registry is empty (not yet refreshed / failed).
func GetFBModels() []FreebuffModelConfig {
	modelReg.RLock()
	defer modelReg.RUnlock()
	if len(modelReg.fb) > 0 {
		return modelReg.fb
	}
	return FreebuffModels
}

// FBModelsInfo returns the registry source label + last sync time (for dashboard).
func FBModelsInfo() (source string, syncedAt time.Time, count int) {
	modelReg.RLock()
	defer modelReg.RUnlock()
	return modelReg.fbSource, modelReg.fbSyncedAt, len(modelReg.fb)
}

// GetGrokModels returns the fetched Grok model entries (empty when not fetched).
func GetGrokModels() []grokModelEntry {
	modelReg.RLock()
	defer modelReg.RUnlock()
	return modelReg.grok
}

// GrokModelsInfo returns the Grok registry source label + last sync time.
func GrokModelsInfo() (source string, syncedAt time.Time, count int) {
	modelReg.RLock()
	defer modelReg.RUnlock()
	return modelReg.grokSource, modelReg.grokSyncedAt, len(modelReg.grok)
}

// RefreshFBModels fetches + parses the Freebuff model JSON and updates the
// registry. Returns an error (registry unchanged) on any failure.
func RefreshFBModels() error {
	client := &http.Client{Timeout: modelRegistryFetchTo}
	resp, err := client.Get(fbModelsReleaseURL)
	if err != nil {
		return fmt.Errorf("fb models fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fb models fetch: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, modelRegistryMaxBody))

	cfg, generatedAt, source, err := parseFBModelsJSON(body)
	if err != nil {
		return err
	}

	modelReg.Lock()
	modelReg.fb = cfg
	modelReg.fbSource = fmt.Sprintf("freebuff-models.json@%s (%s)", generatedAt, source)
	modelReg.fbSyncedAt = time.Now()
	modelReg.Unlock()

	slog.Info("fb models refreshed", "module", "model-registry", "count", len(cfg), "source", modelReg.fbSource)
	return nil
}

// parseFBModelsJSON maps the freebuff-models.json body to FreebuffModelConfig
// entries. Exposed as a helper for unit testing.
func parseFBModelsJSON(body []byte) ([]FreebuffModelConfig, string, string, error) {
	var data struct {
		GeneratedAt string `json:"generatedAt"`
		Source      string `json:"source"`
		Models      []struct {
			ID       string `json:"id"`
			Session  string `json:"session"`
			Agent    string `json:"agent"`
			Upstream string `json:"upstream"`
		} `json:"models"`
		Pools struct {
			Premium  []string `json:"premium"`
			Glm      []string `json:"glm"`
			Standard []string `json:"standard"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", "", fmt.Errorf("fb models parse: %w", err)
	}
	if len(data.Models) == 0 {
		return nil, "", "", fmt.Errorf("fb models: empty model list")
	}

	premium := make(map[string]bool, len(data.Pools.Premium)+len(data.Pools.Glm))
	for _, m := range data.Pools.Premium {
		premium[m] = true
	}
	for _, m := range data.Pools.Glm {
		premium[m] = true
	}

	var cfg []FreebuffModelConfig
	for _, m := range data.Models {
		id := strings.TrimPrefix(m.ID, "fb/")
		// GatewayID convention (matches static list): "fb/<short-model-name>"
		// — strip the provider/ prefix (e.g. "deepseek/deepseek-v4-flash" →
		// "fb/deepseek-v4-flash", "openai/gpt-5.6-luna" → "fb/gpt-5.6-luna").
		short := id
		if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
			short = parts[1]
		}
		gwID := "fb/" + short
		// Reasoning heuristic (JSON has no reasoning field): deepseek* + GLM
		reasoning := strings.HasPrefix(id, "deepseek/") || id == "z-ai/glm-5.2"
		cfg = append(cfg, FreebuffModelConfig{
			GatewayID: gwID,
			Upstream:  id,
			Agent:     m.Agent,
			Reasoning: reasoning,
			FullMode:  premium[id], // premium + glm pools → full-mode only
		})
	}
	return cfg, data.GeneratedAt, data.Source, nil
}

// RefreshGrokModels fetches upstream GET /v1/models using a live account.
// A 401/403 (account dead) just returns an error — registry keeps its state.
func RefreshGrokModels(grokAM *GrokAccountManager) error {
	if grokAM == nil {
		return fmt.Errorf("grok models: manager nil")
	}
	acc, err := grokAM.Next()
	if err != nil {
		return fmt.Errorf("grok models: no account: %w", err)
	}
	token := acc.GetAccessToken()
	if token == "" {
		return fmt.Errorf("grok models: empty access token")
	}

	req, _ := http.NewRequest("GET", "https://cli-chat-proxy.grok.com/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-grok-client-version", "0.2.93")
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("User-Agent", "grok-shell/0.2.93")

	client := &http.Client{Timeout: modelRegistryFetchTo}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("grok models fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("grok models fetch: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, modelRegistryMaxBody))

	var data struct {
		Data []struct {
			ID              string `json:"id"`
			ContextWindow   int    `json:"context_window"`
			ReasoningEfforts []struct {
				ID string `json:"id"`
			} `json:"reasoning_efforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("grok models parse: %w", err)
	}
	if len(data.Data) == 0 {
		return fmt.Errorf("grok models: empty model list")
	}

	var entries []grokModelEntry
	for _, m := range data.Data {
		e := grokModelEntry{ID: m.ID, ContextWindow: m.ContextWindow}
		for _, r := range m.ReasoningEfforts {
			if r.ID != "" {
				e.ReasoningEfforts = append(e.ReasoningEfforts, r.ID)
			}
		}
		entries = append(entries, e)
	}

	modelReg.Lock()
	modelReg.grok = entries
	modelReg.grokSource = "cli-chat-proxy.grok.com/v1/models"
	modelReg.grokSyncedAt = time.Now()
	modelReg.Unlock()

	slog.Info("grok models refreshed", "module", "model-registry", "count", len(entries), "account", acc.Email)
	return nil
}

// FbModelsWorker refreshes the Freebuff model list every 6h. Runs an initial
// refresh at startup (best-effort — failure keeps the static list).
func FbModelsWorker(ctx context.Context) {
	if err := RefreshFBModels(); err != nil {
		slog.Warn("fb models initial refresh failed", "module", "model-registry", "error", err)
	}
	ticker := time.NewTicker(modelRegistryRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := RefreshFBModels(); err != nil {
				slog.Warn("fb models refresh failed", "module", "model-registry", "error", err)
			}
		}
	}
}

// GrokModelsWorker refreshes the Grok model list every 6h. Uses a live account
// from the pool; on failure (no valid account / upstream error) keeps the
// current list and retries next tick.
func GrokModelsWorker(ctx context.Context, grokAM *GrokAccountManager) {
	if err := RefreshGrokModels(grokAM); err != nil {
		slog.Warn("grok models initial refresh failed", "module", "model-registry", "error", err)
	}
	ticker := time.NewTicker(modelRegistryRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := RefreshGrokModels(grokAM); err != nil {
				slog.Warn("grok models refresh failed", "module", "model-registry", "error", err)
			}
		}
	}
}
