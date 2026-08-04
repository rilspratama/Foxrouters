package proxy

import (
	"strings"
	"testing"
)

func TestApplyFilters(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		exclude []string // substrings that must NOT appear in output
	}{
		{"cc_entrypoint_cli", "cc_entrypoint=cli", "", nil},
		{"cc_entrypoint_gui", "cc_entrypoint=gui", "", nil},
		{"cc_entrypoint_vscode", "cc_entrypoint=vscode", "", nil},
		{"cc_entrypoint_jetbrains", "cc_entrypoint=jetbrains", "", nil},
		{"cc_version", "cc_version=2.114.45a", "", nil},
		{"cc_version_3", "cc_version=3.0.0", "", nil},
		{"cch_hash", "cch=33c97", "", nil},
		{"cch_hash_long", "cch=abcdef1234", "", nil},
		{"ch_hash", "ch=8b6e8", "", nil},
		{"claude_code_identity_1", "You are Claude Code, Anthropic's official CLI for Claude.", "", nil},
		{"claude_code_identity_2", "You are Claude Code, a powerful AI coding assistant.", "", nil},
		{"claude_code_identity_3", "You are Claude Code.", "", nil},
		{"anthropic_cli_ref", "Anthropic's official CLI tool.", "", nil},
		{"anthropic_agent_ref", "Anthropic's official agent for coding.", "", nil},
		{"claude_code_mention", "This tool is used by Claude Code to fetch URLs", "This tool is used by the assistant to fetch URLs", nil},
		{"billing_header_1", "x-billing-header: cc_version=2.114.45a; cc_entrypoint=cli; ch=33c97;", "", nil},
		{"billing_header_2", "x-anthropic-billing-header: cc_version=5.0.0; cc_entrypoint=gui; cch=abc123", "", nil},
		{"github_link", "Report at https://github.com/anthropics/claude-code/issues/123", "Report at ", nil},
		{"cursor_identity", "You are a powerful AI assistant made by Cursor.", "", nil},
		{"windsurf_identity", "You are Windsurf, an AI coding assistant.", "", nil},
		{"cline_identity", "You are Cline, a coding agent.", "", nil},
		{"powered_by_claude", "This tool is powered by Claude.", "This tool is ", nil},
		{"powered_by_anthropic", "powered by Anthropic's API.", "", nil},
		{"normal_content", "Please help me write a function that fetches data from an API.", "Please help me write a function that fetches data from an API.", nil},
		{"feedback_line", "Claude Code. To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", "", nil},
		{"advanced_ai_agent", "Advanced AI Agent", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyFilters(tt.input)
			if tt.want != "" && got != tt.want {
				t.Errorf("ApplyFilters(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if tt.exclude != nil {
				for _, e := range tt.exclude {
					if strings.Contains(got, e) {
						t.Errorf("ApplyFilters(%q) = %q, should not contain %q", tt.input, got, e)
					}
				}
			}
		})
	}
}

// TestApplyFiltersToolResult simulates the TS "handles tool result content
// with mixed patterns" test: file content with Claude Code + billing headers
// must be stripped, normal code preserved.
func TestApplyFiltersToolResult(t *testing.T) {
	toolResult := `File contents:
# README
This project uses Claude Code for development.
x-billing-header: cc_version=2.5.0; cc_entrypoint=gui; cch=12345
Some normal code here.`

	filtered := ApplyFilters(toolResult)

	for _, forbidden := range []string{"Claude Code", "x-billing-header", "cc_version"} {
		if strings.Contains(filtered, forbidden) {
			t.Errorf("output still contains %q: %q", forbidden, filtered)
		}
	}
	if !strings.Contains(filtered, "Some normal code here.") {
		t.Errorf("normal content lost: %q", filtered)
	}
}

// TestApplyFiltersCaseInsensitive verifies regex rules match case-insensitively
// (the "gi" flag behavior from the TS implementation).
func TestApplyFiltersCaseInsensitive(t *testing.T) {
	inputs := []string{
		"YOU ARE CLAUDE CODE, ANTHROPIC'S OFFICIAL CLI FOR CLAUDE.",
		"You Are Claude Code, a powerful AI coding assistant.",
		"powered by CLAUDE.",
		"CC_ENTRYPOINT=CLI",
	}
	for _, in := range inputs {
		if got := ApplyFilters(in); got != "" {
			t.Errorf("ApplyFilters(%q) = %q, want empty (case-insensitive match)", in, got)
		}
	}
}

// TestApplyFiltersRealisticClaudePrompt verifies a realistic Claude Code
// system prompt: billing header on its own line + identity sentences stripped,
// but the remaining prompt instructions preserved.
func TestApplyFiltersRealisticClaudePrompt(t *testing.T) {
	s := "x-anthropic-billing-header: cc_version=2.1.220.fea; cc_entrypoint=sdk-cli;\n" +
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.\n\n" +
		"You are an interactive agent that helps users with software engineering tasks.\n\n" +
		"IMPORTANT: Assist with authorized security testing.\n"
	out := ApplyFilters(s)

	for _, forbidden := range []string{"billing-header", "cc_version", "cc_entrypoint", "Anthropic"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output still contains %q:\n%q", forbidden, out)
		}
	}
	for _, wanted := range []string{"an AI coding assistant", "Built on AI Labs", "interactive agent that helps users", "authorized security testing"} {
		if !strings.Contains(out, wanted) {
			t.Errorf("output missing %q:\n%q", wanted, out)
		}
	}
	if strings.Contains(out, "a AI") {
		t.Errorf("grammar artifact 'a AI' present:\n%q", out)
	}
}

// TestApplyFiltersPreservesAutonomousUserContent verifies the aggressive
// autonomous/agentic pattern is disabled by default and does NOT eat
// legitimate user content.
func TestApplyFiltersPreservesAutonomousUserContent(t *testing.T) {
	s := "Build me an autonomous agent that fetches data every hour. It should be robust."
	if got := ApplyFilters(s); got != s {
		t.Errorf("ApplyFilters(%q) = %q, want unchanged (aggressive rule must be disabled)", s, got)
	}
}

// TestRewriteMessageContentString verifies plain-string message content.
func TestRewriteMessageContentString(t *testing.T) {
	m := map[string]any{
		"role":    "system",
		"content": "You are Claude Code, Anthropic's official CLI for Claude. Help with code.",
	}
	rewriteMessageContent(m)
	out := m["content"].(string)
	if strings.Contains(out, "Claude") {
		t.Errorf("system prompt still contains Claude identity: %q", out)
	}
	if !strings.Contains(out, "Help with code.") {
		t.Errorf("normal content lost: %q", out)
	}
}

// TestRewriteMessageContentArrayBlocks verifies array-of-blocks content.
func TestRewriteMessageContentArrayBlocks(t *testing.T) {
	m := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "Powered by Claude. Please analyze this file."},
			map[string]any{"type": "text", "text": "cc_version=2.1.0"},
		},
	}
	rewriteMessageContent(m)
	blocks := m["content"].([]any)
	for _, b := range blocks {
		text := b.(map[string]any)["text"].(string)
		if strings.Contains(text, "Claude") || strings.Contains(text, "cc_version") {
			t.Errorf("block still contains identity/billing: %q", text)
		}
	}
}

// TestRewriteMessageContentToolResult verifies tool_result nested blocks
// (both string and array-of-text-block content forms).
func TestRewriteMessageContentToolResult(t *testing.T) {
	m := map[string]any{
		"role": "tool",
		"content": []any{
			map[string]any{
				"type": "tool_result",
				"content": "File: x-billing-header: cc_version=1.0.0; cc_entrypoint=cli\n" +
					"Claude Code made this.",
			},
			map[string]any{
				"type": "tool_result",
				"content": []any{
					map[string]any{"type": "text", "text": "You are Windsurf, an AI coding assistant."},
				},
			},
		},
	}
	rewriteMessageContent(m)
	blocks := m["content"].([]any)
	for _, b := range blocks {
		blk := b.(map[string]any)
		switch v := blk["content"].(type) {
		case string:
			if strings.Contains(v, "Claude Code") || strings.Contains(v, "x-billing-header") {
				t.Errorf("tool_result string content not sanitized: %q", v)
			}
		case []any:
			for _, inner := range v {
				ib := inner.(map[string]any)
				if strings.Contains(ib["text"].(string), "Windsurf") {
					t.Errorf("tool_result nested text not sanitized: %q", ib["text"])
				}
			}
		}
	}
}

// TestRewriteMessageContentToolResultString verifies tool_result with plain
// string content.
func TestRewriteMessageContentToolResultString(t *testing.T) {
	m := map[string]any{
		"role":    "tool",
		"content": []any{map[string]any{"type": "tool_result", "content": "You are Cline, a coding agent."}},
	}
	rewriteMessageContent(m)
	block := m["content"].([]any)[0].(map[string]any)
	if strings.Contains(block["content"].(string), "Cline") {
		t.Errorf("tool_result string content not sanitized: %q", block["content"])
	}
}

// TestRewriteAgentIdentityEndToEnd builds a full bodyMap (messages with
// string + blocks + tool_result + tools) and verifies the whole pipeline.
func TestRewriteAgentIdentityEndToEnd(t *testing.T) {
	bm := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{
				"role":    "system",
				"content": "You are Claude Code, Anthropic's official CLI for Claude. Be concise.",
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.0; cc_entrypoint=cli"},
					map[string]any{"type": "text", "text": "Help me refactor this."},
				},
			},
			map[string]any{
				"role": "tool",
				"content": []any{
					map[string]any{"type": "tool_result", "content": "File has Claude Code mentions."},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "fetch_url",
					"description": "Fetch a URL for Claude Code. Powered by Anthropic.",
				},
			},
		},
	}

	rewriteAgentIdentity(bm)

	// System message must not contain identity
	sys := bm["messages"].([]any)[0].(map[string]any)
	if strings.Contains(sys["content"].(string), "Claude") {
		t.Errorf("system message not sanitized: %q", sys["content"])
	}
	if !strings.Contains(sys["content"].(string), "Be concise.") {
		t.Errorf("system message lost normal content: %q", sys["content"])
	}

	// User array blocks: billing line gone, normal text kept
	userBlocks := bm["messages"].([]any)[1].(map[string]any)["content"].([]any)
	userTexts := make([]string, 0, len(userBlocks))
	for _, b := range userBlocks {
		userTexts = append(userTexts, b.(map[string]any)["text"].(string))
	}
	joined := strings.Join(userTexts, " ")
	if strings.Contains(joined, "billing-header") || strings.Contains(joined, "cc_version") || strings.Contains(joined, "cc_entrypoint") {
		t.Errorf("user blocks not sanitized: %v", userTexts)
	}
	if !strings.Contains(joined, "Help me refactor this.") {
		t.Errorf("user blocks lost normal content: %v", userTexts)
	}

	// tool_result block sanitized
	toolBlock := bm["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if strings.Contains(toolBlock["content"].(string), "Claude Code") {
		t.Errorf("tool_result not sanitized: %q", toolBlock["content"])
	}

	// tool description sanitized
	toolDesc := bm["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["description"].(string)
	if strings.Contains(toolDesc, "Claude") || strings.Contains(toolDesc, "Anthropic") {
		t.Errorf("tool description not sanitized: %q", toolDesc)
	}
	if !strings.Contains(toolDesc, "Fetch a URL") {
		t.Errorf("tool description lost normal content: %q", toolDesc)
	}
}

// TestRewriteAgentIdentityNoMessages verifies safe behavior when messages
// key is missing (returns without panic).
func TestRewriteAgentIdentityNoMessages(t *testing.T) {
	rewriteAgentIdentity(map[string]any{"model": "glm-5.2"}) // must not panic
}
