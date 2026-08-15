package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
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
	fbModelsReleaseURL   = "https://github.com/pingmike2/freebuff2api-wokers/releases/latest/download/freebuff-models.json"
	modelRegistryRefresh = 6 * time.Hour
	modelRegistryFetchTo = 15 * time.Second
	modelRegistryMaxBody = 1 << 20 // 1MB
)

// grokModelEntry is one fetched Grok model entry from upstream /v1/models.
type grokModelEntry struct {
	ID               string   `json:"id"`
	ContextWindow    int      `json:"context_window"`
	ReasoningEfforts []string `json:"-"`
}

// AlibabaModelInfo is one fetched DashScope model entry (from /api/v1/models).
type AlibabaModelInfo struct {
	Gateway   string `json:"gateway"`  // ali/<upstream>
	Upstream  string `json:"upstream"` // bare model id (e.g. qwen3.7-flash)
	Name      string `json:"name"`     // brand label (e.g. Qwen3.7-Flash)
	Reasoning bool   `json:"reasoning"`
	Quota     int64  `json:"quota"` // free-tier token limit (0 = none)
}

// modelRegistry holds the runtime model lists (mutex-protected).
type modelRegistry struct {
	sync.RWMutex
	fb           []FreebuffModelConfig
	fbSource     string
	fbSyncedAt   time.Time
	grok         []grokModelEntry
	grokSource   string
	grokSyncedAt time.Time
	ali          []AlibabaModelInfo
	aliSource    string
	aliSyncedAt  time.Time
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
		if !validModelID(id) || !validModelID(short) {
			// Upstream-controlled id from a third-party GitHub release —
			// drop anything outside a safe charset (fail-closed).
			slog.Warn("fb model id rejected", "module", "fb-registry", "model", id)
			continue
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
			ID               string `json:"id"`
			ContextWindow    int    `json:"context_window"`
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

// ============================================================================
// ALIBABA (DashScope) MODEL REGISTRY
// ============================================================================
// Source: GET /api/v1/models (authenticated with a live sk-ws-* key) — returns
// the full model table {model, name, description} paginated 100/page (~241).
// We keep only LLM/chat models (qwen/deepseek/glm/kimi families), skipping
// non-chat products (video/image/audio/embedding/realtime). Reasoning is a
// best-effort heuristic on the model id. Falls back to the static
// alibabaModelConfigs list when the registry is empty.

const (
	aliModelsURL      = "https://dashscope-intl.aliyuncs.com/api/v1/models"
	aliModelsPageSize = 100
)

// GetAliModels returns the dynamic Alibaba model list, falling back to the
// static alibabaModelConfigs when the registry is empty.
func GetAliModels() []AlibabaModelInfo {
	modelReg.RLock()
	defer modelReg.RUnlock()
	if len(modelReg.ali) > 0 {
		return modelReg.ali
	}
	var out []AlibabaModelInfo
	for _, c := range alibabaModelConfigs {
		out = append(out, AlibabaModelInfo{
			Gateway:   c.Gateway,
			Upstream:  c.Upstream,
			Name:      c.Upstream,
			Reasoning: c.Reasoning,
			Quota:     aliQuotaLimit(c.Upstream),
		})
	}
	return out
}

// AliModelsInfo returns the registry source label + last sync time (dashboard).
func AliModelsInfo() (source string, syncedAt time.Time, count int) {
	modelReg.RLock()
	defer modelReg.RUnlock()
	return modelReg.aliSource, modelReg.aliSyncedAt, len(modelReg.ali)
}

// RefreshAliModels fetches + filters the DashScope model list via a live key
// from the pool. Returns an error (registry unchanged) on any failure.
func RefreshAliModels(am *AlibabaKeyManager) error {
	if am == nil || am.Len() == 0 {
		return fmt.Errorf("ali models: no keys available")
	}
	acc, err := am.Next()
	if err != nil {
		return fmt.Errorf("ali models: %w", err)
	}

	client := &http.Client{Timeout: modelRegistryFetchTo}
	var entries []AlibabaModelInfo
	for page := 1; page <= 4; page++ {
		url := fmt.Sprintf("%s?page_no=%d&page_size=%d", aliModelsURL, page, aliModelsPageSize)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+acc.Key)
		req.Header.Set("User-Agent", "foxrouters/1.6.13")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("ali models fetch p%d: %w", page, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("ali models fetch p%d: status %d", page, resp.StatusCode)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, modelRegistryMaxBody))
		resp.Body.Close()

		var data struct {
			Output struct {
				Total  int `json:"total"`
				Models []struct {
					Model       string `json:"model"`
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"models"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return fmt.Errorf("ali models parse p%d: %w", page, err)
		}
		for _, m := range data.Output.Models {
			if !aliIsChatModel(m.Model) {
				continue
			}
			if aliZeroQuotaModel(m.Model) {
				continue // no free tier — don't expose (would 403 at runtime)
			}
			if !validModelID(m.Model) {
				// Upstream-controlled string flowing into /v1/models +
				// dashboard — drop anything that isn't a safe charset.
				slog.Warn("ali model id rejected", "module", "ali-registry", "model", m.Model)
				continue
			}
			entries = append(entries, AlibabaModelInfo{
				Gateway:   "ali/" + m.Model,
				Upstream:  m.Model,
				Name:      m.Name,
				Reasoning: aliModelReasoning(m.Model),
				Quota:     aliQuotaLimit(m.Model),
			})
		}
		if page*aliModelsPageSize >= data.Output.Total {
			break
		}
	}
	if len(entries) == 0 {
		return fmt.Errorf("ali models: no chat models after filter")
	}

	modelReg.Lock()
	modelReg.ali = entries
	modelReg.aliSource = "dashscope-intl /api/v1/models"
	modelReg.aliSyncedAt = time.Now()
	modelReg.Unlock()

	slog.Info("ali models refreshed", "module", "model-registry", "count", len(entries))
	return nil
}

// validModelID enforces a strict charset for model ids sourced from
// third-party registries (GitHub releases, DashScope) before they flow into
// /v1/models and the dashboard. Anything else is dropped (fail-closed).
var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9._/\-]{1,80}$`)

func validModelID(id string) bool {
	return modelIDRe.MatchString(id)
}

// aliIsChatModel reports whether a DashScope model id is an LLM/chat model
// we want to expose (vs video/image/audio/embedding/realtime products).
func aliIsChatModel(id string) bool {
	if !strings.HasPrefix(id, "qwen") && !strings.HasPrefix(id, "deepseek") &&
		!strings.HasPrefix(id, "glm") && !strings.HasPrefix(id, "kimi") {
		return false
	}
	low := strings.ToLower(id)
	for _, skip := range []string{"realtime", "embedding", "t2v", "i2v", "r2v", "video",
		"image", "audio", "asr", "tts", "captioner", "wan", "happyhorse"} {
		if strings.Contains(low, skip) {
			return false
		}
	}
	return true
}

// aliModelReasoning is a best-effort heuristic: reasoning-capable families
// (max/plus/deepseek/glm/kimi/thinking/omni/preview) → true.
func aliModelReasoning(id string) bool {
	low := strings.ToLower(id)
	for _, yes := range []string{"max", "plus", "deepseek", "glm", "kimi", "thinking", "omni", "preview", "coder-32b", "long"} {
		if strings.Contains(low, yes) {
			return true
		}
	}
	return false
}

// AliModelsWorker refreshes the DashScope model list every 6h. Uses a live key
// from the pool; on failure keeps the current list and retries next tick.
func AliModelsWorker(ctx context.Context, aliAM *AlibabaKeyManager) {
	if err := RefreshAliModels(aliAM); err != nil {
		slog.Warn("ali models initial refresh failed", "module", "model-registry", "error", err)
	}
	ticker := time.NewTicker(modelRegistryRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := RefreshAliModels(aliAM); err != nil {
				slog.Warn("ali models refresh failed", "module", "model-registry", "error", err)
			}
		}
	}
}
