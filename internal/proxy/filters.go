package proxy

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"sync"

	"foxrouters/internal/upstream"
)

// FilterRule is a single content-sanitization rule. Rules are ordered:
// broad regex patterns first, then exact string fallbacks (mirrors the
// pudidil filter template from enowxai, as implemented in etteum-pool).
type FilterRule struct {
	ID          string
	Label       string
	Pattern     string
	Replacement string
	IsActive    bool
	IsRegex     bool
}

// DefaultPudidilFilters is the default rule set. Phase 1 = broad regex
// rules (catch all variations before exact strings can partially match
// and leave fragments behind). Phase 2 = exact string fallbacks.
var DefaultPudidilFilters = []FilterRule{
	// ═════════════════════════════════════════════════════════════════════
	// PHASE 1: broad regex rules
	// ═════════════════════════════════════════════════════════════════════

	// Replace the full SDK attribution phrase FIRST — it contains "Claude Agent"
	// which would be partially replaced by the generic rule below if reversed.
	{ID: "replace_sdk_attribution", Label: "SDK attribution -> neutral", Pattern: `Built on Anthropic's Claude Agent SDK`, Replacement: "Built on AI Labs", IsActive: true, IsRegex: true},
	// "a Claude agent" → "an AI coding assistant" (grammar) — must precede the
	// generic rule below so "a Claude" doesn't become "a AI".
	{ID: "replace_a_claude_agent", Label: "'an AI coding assistant' (grammar)", Pattern: `a Claude agent`, Replacement: "an AI coding assistant", IsActive: true, IsRegex: true},
	// Replace remaining "Claude agent" mentions (case-insensitive)
	{ID: "replace_claude_agent", Label: "AI coding assistant -> neutral", Pattern: `Claude agent`, Replacement: "AI coding assistant", IsActive: true, IsRegex: true},
	// Catch full billing header lines (any version, any entrypoint)
	{ID: "remove_billing_header_regex", Label: "Billing headers", Pattern: `x-(?:anthropic-)?billing-header:?\s*[^\n]*`, Replacement: "", IsActive: true, IsRegex: true},
	// Catch any cc_entrypoint variation (cli, gui, vscode, jetbrains, etc.)
	{ID: "remove_cc_entrypoint_any", Label: "cc_entrypoint leaks", Pattern: `cc_entrypoint=\w+`, Replacement: "", IsActive: true, IsRegex: true},
	// Catch cc_version=X.Y.Z patterns (any version)
	{ID: "remove_cc_version_any", Label: "cc_version leaks", Pattern: `cc_version=[\w.]+`, Replacement: "", IsActive: true, IsRegex: true},
	// Catch cch= and ch= hash patterns
	{ID: "remove_cch_hash", Label: "cch/ch hash leaks", Pattern: `c?ch=[a-f0-9]+`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove claude-code GitHub references (full URL with path)
	{ID: "remove_claude_code_github", Label: "claude-code GitHub URLs", Pattern: `https?://github\.com/anthropics/claude-code[^\s]*`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Claude Code identity variations (case-insensitive)
	{ID: "remove_claude_code_identity_variations", Label: "CLI identity variations", Pattern: `You are Claude Code[^.]*\.`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Anthropic CLI references
	{ID: "remove_anthropic_cli_ref", Label: "official CLI references", Pattern: `Anthropic'?s official (?:CLI|tool|agent)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove "Anxthxropic" obfuscated references
	{ID: "remove_anxthxropic_ref", Label: "obfuscated brand refs", Pattern: `Anxthxropic'?s official[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Cursor agent identity
	{ID: "remove_cursor_identity", Label: "Cursor/Anysphere identity", Pattern: `You are (?:a )?(?:powerful )?(?:AI )?(?:assistant|agent) (?:made|built|created) by (?:Cursor|Anysphere)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Windsurf/Codeium agent identity
	{ID: "remove_windsurf_identity", Label: "Windsurf/Cascade/Codeium identity", Pattern: `You are (?:Windsurf|Cascade|Codeium)[^.]*\.`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Cline agent identity
	{ID: "remove_cline_identity", Label: "Cline identity", Pattern: `You are Cline[^.]*\.`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove generic "AI coding agent" patterns that may trigger moderation.
	// NOTE: DISABLED by default — the pattern is too aggressive and also matches
	// legitimate user content (e.g. "Build me an autonomous agent that ...").
	// Enable only if upstream moderation flags this class of text.
	{ID: "remove_ai_coding_agent_pattern", Label: "generic 'AI coding agent' phrases (too aggressive)", Pattern: `(?:autonomous|agentic) (?:AI |coding )?(?:agent|assistant)[^.]*\.`, Replacement: "", IsActive: false, IsRegex: true},
	// Remove tool use framework identifiers (MCP, tool_use markers)
	{ID: "remove_mcp_server_ref", Label: "MCP server/client refs", Pattern: `MCP (?:server|client|protocol)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove "powered by Claude" / "powered by Anthropic" patterns
	{ID: "remove_powered_by_anthropic", Pattern: `powered by (?:Claude|Anthropic|Anxthxropic)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},

	// ═════════════════════════════════════════════════════════════════════
	// PHASE 2: exact string rules
	// ═════════════════════════════════════════════════════════════════════

	{ID: "remove_feedback_line", Label: "feedback line", Pattern: "Claude Code. To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", Replacement: "", IsActive: true, IsRegex: false},
	{ID: "remove_powerful_ai_agent", Label: "'powerful AI agent' phrase", Pattern: "Advanced AI Agent", Replacement: "", IsActive: true, IsRegex: false},
	{ID: "remove_claude_code_identity", Label: "CLI identity (exact)", Pattern: "You are Claude Code, Anxthxropic's official CLI for Claude.", Replacement: "", IsActive: true, IsRegex: false},
	// Replace remaining "Claude Code" mentions with neutral text
	{ID: "remove_claude_code_mention", Label: "the assistant -> neutral", Pattern: "Claude Code", Replacement: "the assistant", IsActive: true, IsRegex: false},
}

// compiledRule is a pre-compiled filter rule ready for fast application.
type compiledRule struct {
	rule    FilterRule
	regex   *regexp.Regexp
	isRegex bool
}

var compiledFilters []compiledRule

func init() {
	compiledFilters = compileFilters(DefaultPudidilFilters)
}

// compileFilters pre-compiles ALL rules (active or not) — runtime gating
// happens in ApplyFilters so the dashboard Settings can flip any rule live.
// Invalid regexes are logged and skipped (never panics).
func compileFilters(rules []FilterRule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		if r.IsRegex {
			re, err := regexp.Compile("(?i)" + r.Pattern)
			if err != nil {
				log.Printf("[filters] invalid regex rule %q: %v (skipped)", r.ID, err)
				continue
			}
			out = append(out, compiledRule{rule: r, regex: re, isRegex: true})
		} else {
			if r.Pattern == "" {
				continue
			}
			out = append(out, compiledRule{rule: r})
		}
	}
	return out
}

// ── Runtime filter config (dashboard Settings → Redis gw:config) ─────────
// FiltersEnabled gates the whole pipeline; per-rule overrides live in
// filterActive. An id absent from the map falls back to its default IsActive.
var (
	filterMu       sync.RWMutex
	filterEnabled  = true
	filterActive   = map[string]bool{}
)

// FiltersEnabled reports whether the sanitization pipeline is on.
func FiltersEnabled() bool {
	filterMu.RLock()
	defer filterMu.RUnlock()
	return filterEnabled
}

// FilterActive reports the effective active state for a rule id.
func FilterActive(id string) bool {
	filterMu.RLock()
	v, ok := filterActive[id]
	filterMu.RUnlock()
	if ok {
		return v
	}
	for _, r := range DefaultPudidilFilters {
		if r.ID == id {
			return r.IsActive
		}
	}
	return false
}

// FiltersList returns the full rule set (for the Settings UI).
func FiltersList() []FilterRule {
	out := make([]FilterRule, len(DefaultPudidilFilters))
	copy(out, DefaultPudidilFilters)
	return out
}

// SetFilterConfig swaps runtime state (enabled + per-rule overrides).
func SetFilterConfig(enabled bool, active map[string]bool) {
	filterMu.Lock()
	filterEnabled = enabled
	filterActive = active
	filterMu.Unlock()
}

// LoadFilterConfig applies Redis-persisted values (gw:config hash) so the
// dashboard Settings survive restarts. store may be nil (defaults kept).
func LoadFilterConfig(store interface {
	GetGWConfig(field string) (string, error)
}) {
	if store == nil {
		return
	}
	enabled := filterEnabled
	if v, err := store.GetGWConfig("filters_enabled"); err == nil && v != "" {
		enabled = v == "1" || v == "true"
	}
	active := filterActive
	if v, err := store.GetGWConfig("filters_active_rules"); err == nil && v != "" {
		var m map[string]bool
		if json.Unmarshal([]byte(v), &m) == nil && m != nil {
			active = m
		}
	}
	SetFilterConfig(enabled, active)
}

// ApplyFilters applies the sanitization rules to a string (respecting the
// runtime master switch + per-rule overrides). Mirrors applyPudidilFilters
// in etteum-pool: regex rules with "gi" flags, exact rules with a Contains +
// ReplaceAll loop (handles repeated matches).
func ApplyFilters(content string) string {
	if !FiltersEnabled() {
		return content
	}
	filtered := content
	for _, cr := range compiledFilters {
		if !FilterActive(cr.rule.ID) {
			continue
		}
		if cr.isRegex {
			filtered = cr.regex.ReplaceAllString(filtered, cr.rule.Replacement)
		} else {
			if !strings.Contains(filtered, cr.rule.Pattern) {
				continue
			}
			filtered = strings.ReplaceAll(filtered, cr.rule.Pattern, cr.rule.Replacement)
		}
	}
	return filtered
}

// init wires the agent-identity sanitizer into the upstream package. It is
// registered here (proxy package) because upstream cannot import proxy
// (proxy → upstream import cycle). This makes sanitization CodeBuddy-only:
// Grok / Freebuff / Alibaba forward request content untouched.
func init() {
	upstream.AgentIdentitySanitizer = rewriteAgentIdentity
}
