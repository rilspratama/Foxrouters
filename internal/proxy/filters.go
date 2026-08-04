package proxy

import (
	"log"
	"regexp"
	"strings"
)

// FilterRule is a single content-sanitization rule. Rules are ordered:
// broad regex patterns first, then exact string fallbacks (mirrors the
// pudidil filter template from enowxai, as implemented in etteum-pool).
type FilterRule struct {
	ID          string
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
	{ID: "replace_sdk_attribution", Pattern: `Built on Anthropic's Claude Agent SDK`, Replacement: "Built on AI Labs", IsActive: true, IsRegex: true},
	// "a Claude agent" → "an AI coding assistant" (grammar) — must precede the
	// generic rule below so "a Claude" doesn't become "a AI".
	{ID: "replace_a_claude_agent", Pattern: `a Claude agent`, Replacement: "an AI coding assistant", IsActive: true, IsRegex: true},
	// Replace remaining "Claude agent" mentions (case-insensitive)
	{ID: "replace_claude_agent", Pattern: `Claude agent`, Replacement: "AI coding assistant", IsActive: true, IsRegex: true},
	// Catch full billing header lines (any version, any entrypoint)
	{ID: "remove_billing_header_regex", Pattern: `x-(?:anthropic-)?billing-header:?\s*[^\n]*`, Replacement: "", IsActive: true, IsRegex: true},
	// Catch any cc_entrypoint variation (cli, gui, vscode, jetbrains, etc.)
	{ID: "remove_cc_entrypoint_any", Pattern: `cc_entrypoint=\w+`, Replacement: "", IsActive: true, IsRegex: true},
	// Catch cc_version=X.Y.Z patterns (any version)
	{ID: "remove_cc_version_any", Pattern: `cc_version=[\w.]+`, Replacement: "", IsActive: true, IsRegex: true},
	// Catch cch= and ch= hash patterns
	{ID: "remove_cch_hash", Pattern: `c?ch=[a-f0-9]+`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove claude-code GitHub references (full URL with path)
	{ID: "remove_claude_code_github", Pattern: `https?://github\.com/anthropics/claude-code[^\s]*`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Claude Code identity variations (case-insensitive)
	{ID: "remove_claude_code_identity_variations", Pattern: `You are Claude Code[^.]*\.`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Anthropic CLI references
	{ID: "remove_anthropic_cli_ref", Pattern: `Anthropic'?s official (?:CLI|tool|agent)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove "Anxthxropic" obfuscated references
	{ID: "remove_anxthxropic_ref", Pattern: `Anxthxropic'?s official[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Cursor agent identity
	{ID: "remove_cursor_identity", Pattern: `You are (?:a )?(?:powerful )?(?:AI )?(?:assistant|agent) (?:made|built|created) by (?:Cursor|Anysphere)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Windsurf/Codeium agent identity
	{ID: "remove_windsurf_identity", Pattern: `You are (?:Windsurf|Cascade|Codeium)[^.]*\.`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove Cline agent identity
	{ID: "remove_cline_identity", Pattern: `You are Cline[^.]*\.`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove generic "AI coding agent" patterns that may trigger moderation.
	// NOTE: DISABLED by default — the pattern is too aggressive and also matches
	// legitimate user content (e.g. "Build me an autonomous agent that ...").
	// Enable only if upstream moderation flags this class of text.
	{ID: "remove_ai_coding_agent_pattern", Pattern: `(?:autonomous|agentic) (?:AI |coding )?(?:agent|assistant)[^.]*\.`, Replacement: "", IsActive: false, IsRegex: true},
	// Remove tool use framework identifiers (MCP, tool_use markers)
	{ID: "remove_mcp_server_ref", Pattern: `MCP (?:server|client|protocol)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},
	// Remove "powered by Claude" / "powered by Anthropic" patterns
	{ID: "remove_powered_by_anthropic", Pattern: `powered by (?:Claude|Anthropic|Anxthxropic)[^.]*\.?`, Replacement: "", IsActive: true, IsRegex: true},

	// ═════════════════════════════════════════════════════════════════════
	// PHASE 2: exact string rules
	// ═════════════════════════════════════════════════════════════════════

	{ID: "remove_feedback_line", Pattern: "Claude Code. To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", Replacement: "", IsActive: true, IsRegex: false},
	{ID: "remove_powerful_ai_agent", Pattern: "Advanced AI Agent", Replacement: "", IsActive: true, IsRegex: false},
	{ID: "remove_claude_code_identity", Pattern: "You are Claude Code, Anxthxropic's official CLI for Claude.", Replacement: "", IsActive: true, IsRegex: false},
	// Replace remaining "Claude Code" mentions with neutral text
	{ID: "remove_claude_code_mention", Pattern: "Claude Code", Replacement: "the assistant", IsActive: true, IsRegex: false},
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

// compileFilters pre-compiles active rules. Invalid regexes are logged and
// skipped (never panics).
func compileFilters(rules []FilterRule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		if !r.IsActive {
			continue
		}
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

// ApplyFilters applies the default sanitization rules to a string.
// Mirrors applyPudidilFilters in etteum-pool: regex rules with "gi" flags,
// exact rules with a Contains + ReplaceAll loop (handles repeated matches).
func ApplyFilters(content string) string {
	filtered := content
	for _, cr := range compiledFilters {
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
