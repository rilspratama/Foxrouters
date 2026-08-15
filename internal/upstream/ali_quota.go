package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// ALIBABA FREE-TIER QUOTA REFERENCE (static, from Model Studio console)
// ============================================================================
// Source: zeldaEasy.bailian-commerce.freeTrial.queryFreeTierQuotaAsyn —
// freeTierQuotas[].quotaInitTotal per model (LLM category). These are the
// PUBLISHED free-tier limits (1M tokens per model unless noted). They are
// the "patokan" (reference) for tracking: gateway accumulates actual usage
// per model from response usage fields (RecordUsageModel) and the dashboard
// shows limit vs used vs remaining.
//
// Models NOT in this map (or with quota 0) have no free tier — they are
// excluded from the dynamic registry (/v1/models) so clients never get a
// request routed to an unpurchasable model.

// aliQuotaLimit returns the free-tier token limit for an upstream model,
// or 0 if the model has no free tier (or is unknown).
func aliQuotaLimit(model string) int64 {
	model = strings.ToLower(model)
	if aliZeroQuotaModel(model) {
		return 0
	}
	if v, ok := aliFreeTierLimits[model]; ok {
		return v
	}
	// Upstream may return dated variants (e.g. qwen3.7-flash-2026-07-15).
	// Fall back to the base name when the exact id is not listed.
	base := stripDateSuffix(model)
	if aliZeroQuotaModel(base) {
		return 0
	}
	if v, ok := aliFreeTierLimits[base]; ok {
		return v
	}
	return 0
}

// stripDateSuffix removes a trailing -YYYY-MM-DD (or -YYYY-MM) variant
// suffix, e.g. qwen3.7-flash-2026-07-15 → qwen3.7-flash. Regex-anchored so
// a "-20" appearing mid-name never truncates the wrong way.
var aliDateSuffixRe = regexp.MustCompile(`-\d{4}(-\d{2}(-\d{2})?)?$`)

func stripDateSuffix(model string) string {
	loc := aliDateSuffixRe.FindStringIndex(model)
	if loc == nil || loc[0] <= 1 {
		return model
	}
	return model[:loc[0]]
}

// aliZeroQuotaModel reports whether upstream lists the model but its free
// tier is 0 (exclude from registry).
func aliZeroQuotaModel(model string) bool {
	return aliZeroQuota[strings.ToLower(model)]
}

// aliFreeTierLimits: model -> free-tier token quota (quotaInitTotal).
// Auto-generated from Model Studio console data (2026-08-14).
var aliFreeTierLimits = map[string]int64{
	"qwen-vl-ocr-2025-11-20":         1000000,
	"qwen3.5-122b-a10b":              1000000,
	"qwen3.7-plus":                   1000000,
	"qwen3-vl-235b-a22b-thinking":    1000000,
	"qwen3-vl-32b-thinking":          1000000,
	"qwen-plus-2025-07-28":           1000000,
	"qwen3-max":                      1000000,
	"qwen3.5-plus-2026-02-15":        1000000,
	"qwen-max":                       1000000,
	"qwen-mt-flash":                  1000000,
	"qwen3-vl-30b-a3b-thinking":      1000000,
	"qwen3-235b-a22b-thinking-2507":  1000000,
	"glm-5.1":                        1000000,
	"qwen3.7-max-2026-06-08":         1000000,
	"qwen3.6-plus":                   1000000,
	"qwen3.7-max-preview":            1000000,
	"qwen3.6-max-preview":            1000000,
	"qwen3-32b":                      1000000,
	"glm-5.2":                        1000000,
	"kimi-k2.7-code":                 1000000,
	"qwen3.5-397b-a17b":              1000000,
	"qwen3.6-flash":                  1000000,
	"qwen3-vl-plus-2025-09-23":       1000000,
	"deepseek-v3.2":                  1000000,
	"qwen-vl-plus":                   1000000,
	"qwen3-coder-next":               1000000,
	"qwen3.7-flash-2026-07-15":       1000000,
	"qwen3.5-flash":                  1000000,
	"qwen3-vl-32b-instruct":          1000000,
	"deepseek-v4-flash":              1000000,
	"qwen3.5-35b-a3b":                1000000,
	"qwen3-30b-a3b-thinking-2507":    1000000,
	"qwen3-coder-plus-2025-09-23":    1000000,
	"qwen-plus-latest":               1000000,
	"qwen3-max-2026-01-23":           1000000,
	"qwen3-coder-480b-a35b-instruct": 1000000,
	"qwen3-vl-8b-thinking":           1000000,
	"qwen3-coder-plus":               1000000,
	"qwen-plus-2025-09-11":           1000000,
	"qwen3-vl-flash-2026-01-22":      1000000,
	"deepseek-v4-flash-0731":         1000000,
	"qwen3.5-flash-2026-02-23":       1000000,
	"qwen3-vl-flash-2025-10-15":      1000000,
	"qwen3-max-preview":              1000000,
	"qwen-vl-max":                    1000000,
	"qwen3.7-max-2026-05-20":         1000000,
	"qwen3-vl-30b-a3b-instruct":      1000000,
	"qwen3.7-plus-2026-05-26":        1000000,
	"qwen3.8-2.4t-a95b":              1000000,
	"qwen3-8b":                       1000000,
	"qwen3-coder-30b-a3b-instruct":   1000000,
	"qwen3-vl-235b-a22b-instruct":    1000000,
	"qwen3.6-27b":                    1000000,
	"qwen3-235b-a22b":                1000000,
	"qwen-plus":                      1000000,
	"qwen-turbo":                     1000000,
	"qwen-mt-lite":                   1000000,
	"qwen3.6-flash-2026-04-16":       1000000,
	"qvq-max":                        1000000,
	"qwen3-coder-flash":              1000000,
	"qwen3-vl-plus":                  1000000,
	"qwen3-next-80b-a3b-thinking":    1000000,
	"qwen3.5-27b":                    1000000,
	"qwen3.7-max-2026-05-17":         1000000,
	"qwen3-30b-a3b":                  1000000,
	"qwen-mt-plus":                   1000000,
	"qwen3-vl-flash":                 1000000,
	"qwen3-14b":                      1000000,
	"qwen3-vl-8b-instruct":           1000000,
	"qwen3-max-2025-09-23":           1000000,
	"qwen-plus-character":            1000000,
	"deepseek-v4-pro":                1000000,
	"qwen3-coder-flash-2025-07-28":   1000000,
	"qwen-flash-character":           1000000,
	"qwen3-vl-plus-2025-12-19":       1000000,
	"qwen-plus-2025-04-28":           1000000,
	"qwen-mt-turbo":                  1000000,
	"qwen3-30b-a3b-instruct-2507":    1000000,
	"qwen3.5-plus":                   1000000,
	"qwen-flash":                     1000000,
	"qwen-flash-2025-07-28":          1000000,
	"qwen3.7-flash":                  1000000,
	"qwen3.6-35b-a3b":                1000000,
	"qwen-plus-2025-07-14":           1000000,
	"qwen3-235b-a22b-instruct-2507":  1000000,
	"qwq-plus":                       1000000,
	"qwen3.6-plus-2026-04-02":        1000000,
	"qwen3-coder-plus-2025-07-22":    1000000,
	"qwen3.5-plus-2026-04-20":        1000000,
	"qwen3.7-max":                    1000000,
	"qwen-vl-ocr":                    1000000,
	"qwen3.8-max":                    1000000,
	"qwen3-next-80b-a3b-instruct":    1000000,
}

// aliZeroQuota: upstream model ids with 0 free-tier quota → excluded.
var aliZeroQuota = map[string]bool{
	"qwen-plus-character-ja": true,
	"qwen-plus-2025-01-25":   true,
	"glm-5.2-fast-preview":   true,
}

// ============================================================================
// PER-MODEL USAGE ACCOUNTING (gateway-side, from response usage fields)
// ============================================================================
// Redis: ali:model_usage:<model> hash {tokens_in, tokens_out, requests}.
// Incremented on every successful chat completion (stream + non-stream).

// aliUsageKey builds the Redis key for per-model usage counters. Sanitized
// to [a-z0-9._-] so a model name can never create arbitrary Redis keys.
func aliUsageKey(model string) string {
	model = strings.ToLower(strings.TrimPrefix(model, "ali/"))
	var b strings.Builder
	b.Grow(len(model))
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "ali:model_usage:" + b.String()
}

// RecordUsageModel accumulates per-model token counters in Redis. Fire-and-forget.
func (am *AlibabaKeyManager) RecordUsageModel(model string, tokIn, tokOut int64) {
	if am == nil || am.db == nil {
		return
	}
	rdb := am.db.Redis()
	if rdb == nil {
		return
	}
	key := aliUsageKey(model)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pipe := rdb.Pipeline()
	pipe.HIncrBy(ctx, key, "tokens_in", tokIn)
	pipe.HIncrBy(ctx, key, "tokens_out", tokOut)
	pipe.HIncrBy(ctx, key, "requests", 1)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Debug("ali usage persist failed", "module", "alibaba", "model", model, "error", err)
	}
}

// AliModelUsage is one model's limit + accumulated usage (dashboard row).
type AliModelUsage struct {
	Model     string  `json:"model"`
	Gateway   string  `json:"gateway"`
	Limit     int64   `json:"limit"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Requests  int64   `json:"requests"`
	Remaining int64   `json:"remaining"`
	UsedPct   float64 `json:"used_pct"`
	Reasoning bool    `json:"reasoning"`
}

// AliModelUsageList joins the static free-tier limits with accumulated
// per-model usage from Redis, sorted by used % descending.
func (am *AlibabaKeyManager) AliModelUsageList() []AliModelUsage {
	if am == nil || am.db == nil {
		return nil
	}
	rdb := am.db.Redis()
	if rdb == nil {
		return nil
	}
	models := GetAliModels()
	if len(models) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := make([]AliModelUsage, 0, len(models))
	// Quota is 1M PER KEY — pool of N active keys = N × limit per model.
	keyFactor := int64(am.ActiveKeyCount())
	if keyFactor < 1 {
		keyFactor = 1
	}
	for _, m := range models {
		limit := aliQuotaLimit(m.Upstream)
		if limit <= 0 {
			continue // no free tier — skip from quota view too
		}
		limit *= keyFactor
		key := aliUsageKey(m.Upstream)
		vals, err := rdb.HGetAll(ctx, key).Result()
		if err != nil {
			// Redis down — surface instead of silently hiding rows.
			slog.Warn("ali usage read failed", "module", "alibaba", "model", m.Upstream, "error", err)
			continue
		}
		mu := AliModelUsage{
			Model:     m.Upstream,
			Gateway:   m.Gateway,
			Limit:     limit,
			Reasoning: m.Reasoning,
		}
		mu.TokensIn = atoi64(vals["tokens_in"])
		mu.TokensOut = atoi64(vals["tokens_out"])
		mu.Requests = atoi64(vals["requests"])
		used := mu.TokensIn + mu.TokensOut
		mu.Remaining = limit - used
		if mu.Remaining < 0 {
			mu.Remaining = 0 // clamp — never show negative
		}
		if limit > 0 {
			mu.UsedPct = float64(used) * 100 / float64(limit)
		}
		out = append(out, mu)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UsedPct > out[j].UsedPct })
	return out
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// AliQuotaCount returns the number of chat models with a free-tier limit.
func AliQuotaCount() int { return len(aliFreeTierLimits) }

// Validate aliQuotaLimit fallback (dated variant lookup) — keep compiler happy.
var _ = fmt.Sprintf
