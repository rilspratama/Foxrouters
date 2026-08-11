package upstream

import (
	"testing"
)

// fixture mirrors the real freebuff-models.json shape (from
// pingmike2/freebuff2api-wokers releases).
const fbModelsFixture = `{
  "generatedAt": "2026-08-11T00:05:39.815Z",
  "source": "CodebuffAI/freebuff main",
  "models": [
    {"id": "mimo/mimo-v2.5", "session": "mimo/mimo-v2.5", "agent": "base2-free-mimo", "upstream": "mimo/mimo-v2.5"},
    {"id": "deepseek/deepseek-v4-flash", "session": "deepseek/deepseek-v4-flash", "agent": "base2-free-deepseek-flash", "upstream": "deepseek/deepseek-v4-flash"},
    {"id": "deepseek/deepseek-v4-pro", "session": "deepseek/deepseek-v4-pro", "agent": "base2-free-deepseek", "upstream": "deepseek/deepseek-v4-pro"},
    {"id": "z-ai/glm-5.2", "session": "z-ai/glm-5.2", "agent": "base2-free-glm", "upstream": "z-ai/glm-5.2"},
    {"id": "poolside/laguna-s-2.1", "session": "poolside/laguna-s-2.1", "agent": "base2-free-laguna-s-2-1", "upstream": "poolside/laguna-s-2.1"}
  ],
  "pools": {
    "premium": ["deepseek/deepseek-v4-pro", "poolside/laguna-s-2.1"],
    "glm": ["z-ai/glm-5.2"],
    "standard": ["mimo/mimo-v2.5", "deepseek/deepseek-v4-flash"]
  }
}`

func TestParseFBModelsJSON(t *testing.T) {
	cfg, genAt, src, err := parseFBModelsJSON([]byte(fbModelsFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg) != 5 {
		t.Fatalf("expected 5 models, got %d", len(cfg))
	}
	if genAt == "" || src == "" {
		t.Fatalf("expected generatedAt/source, got %q %q", genAt, src)
	}
	byID := make(map[string]FreebuffModelConfig, len(cfg))
	for _, m := range cfg {
		byID[m.GatewayID] = m
	}
	// standard model: not premium
	if m := byID["fb/deepseek-v4-flash"]; m.FullMode || !m.Reasoning || m.Upstream != "deepseek/deepseek-v4-flash" {
		t.Fatalf("flash config wrong: %+v", m)
	}
	if m := byID["fb/mimo-v2.5"]; m.FullMode || m.Reasoning {
		t.Fatalf("mimo config wrong: %+v", m)
	}
	// premium model: FullMode
	if m := byID["fb/deepseek-v4-pro"]; !m.FullMode || !m.Reasoning {
		t.Fatalf("pro config wrong: %+v", m)
	}
	// new model picked up from dynamic source (provider prefix stripped in GatewayID)
	if m := byID["fb/laguna-s-2.1"]; !m.FullMode || m.Agent != "base2-free-laguna-s-2-1" || m.Upstream != "poolside/laguna-s-2.1" {
		t.Fatalf("laguna config wrong: %+v", m)
	}
	// glm: premium pool → FullMode + reasoning (GatewayID strips provider prefix)
	if m := byID["fb/glm-5.2"]; !m.FullMode || !m.Reasoning || m.Upstream != "z-ai/glm-5.2" {
		t.Fatalf("glm config wrong: %+v", m)
	}
}

func TestParseFBModelsJSONEmpty(t *testing.T) {
	if _, _, _, err := parseFBModelsJSON([]byte(`{"models": []}`)); err == nil {
		t.Fatalf("expected error for empty model list")
	}
	if _, _, _, err := parseFBModelsJSON([]byte(`not json`)); err == nil {
		t.Fatalf("expected parse error")
	}
}

// TestGetFBModelsFallback verifies the registry falls back to the static list
// when never refreshed.
func TestGetFBModelsFallback(t *testing.T) {
	// Ensure registry is empty for this test
	modelReg.Lock()
	modelReg.fb = nil
	modelReg.Unlock()

	got := GetFBModels()
	if len(got) == 0 {
		t.Fatalf("expected static fallback, got empty")
	}
	if got[0].GatewayID != FreebuffModels[0].GatewayID {
		t.Fatalf("fallback mismatch: %s vs %s", got[0].GatewayID, FreebuffModels[0].GatewayID)
	}
}
