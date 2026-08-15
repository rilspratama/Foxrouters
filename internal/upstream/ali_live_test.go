package upstream

import (
	"os"
	"testing"
)

// TestRefreshAliModelsKey fetches the real DashScope model list using an
// explicit key passed via ALI_TEST_KEY env (no Redis dependency).
func TestRefreshAliModelsKey(t *testing.T) {
	key := os.Getenv("ALI_TEST_KEY")
	if key == "" {
		t.Skip("ALI_TEST_KEY not set")
	}
	am := NewAlibabaKeyManager(nil)
	am.AddAccount(key, "")
	if err := RefreshAliModels(am); err != nil {
		t.Fatalf("RefreshAliModels: %v", err)
	}
	models := GetAliModels()
	t.Logf("fetched %d ali models", len(models))
	if len(models) < 20 {
		t.Fatalf("expected >=20 models, got %d", len(models))
	}
	for _, m := range models[:10] {
		t.Logf("  %s | %s | reasoning=%v", m.Gateway, m.Name, m.Reasoning)
	}
}
