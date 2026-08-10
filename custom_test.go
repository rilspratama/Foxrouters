package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxrouters/internal/db"
	"foxrouters/internal/proxy"

	"github.com/gin-gonic/gin"
)

// TestResolveAlias verifies alias replaces the incoming model with the target.
func TestResolveAlias(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	if err := reg.AddAlias("my-claude", "cb/claude-sonnet-4.6"); err != nil {
		t.Fatalf("AddAlias failed: %v", err)
	}
	resolved, upstream, mn := reg.Resolve("my-claude")
	if resolved != "cb/claude-sonnet-4.6" {
		t.Errorf("resolved = %q, want cb/claude-sonnet-4.6", resolved)
	}
	// No custom model registered for the target — upstream + modelName empty.
	if upstream != "" || mn != "" {
		t.Errorf("expected empty upstream/modelName for pure alias, got %q %q", upstream, mn)
	}
}

// TestResolveCustomModel verifies a custom model routes to the declared upstream.
func TestResolveCustomModel(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	if err := reg.AddModel("cb/kimi-k3", db.CustomModel{
		Upstream:  "codebuddy",
		ModelName: "kimi-k3",
		OwnedBy:   "codebuddy",
	}); err != nil {
		t.Fatalf("AddModel failed: %v", err)
	}
	resolved, upstream, mn := reg.Resolve("cb/kimi-k3")
	if resolved != "cb/kimi-k3" {
		t.Errorf("resolved = %q, want cb/kimi-k3", resolved)
	}
	if upstream != "codebuddy" {
		t.Errorf("upstream = %q, want codebuddy", upstream)
	}
	if mn != "kimi-k3" {
		t.Errorf("modelName = %q, want kimi-k3", mn)
	}
}

// TestResolveAliasToCustomModel: alias → custom model should still surface upstream.
func TestResolveAliasToCustomModel(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	_ = reg.AddModel("cb/kimi-k3", db.CustomModel{Upstream: "codebuddy", ModelName: "kimi-k3", OwnedBy: "codebuddy"})
	_ = reg.AddAlias("kimi", "cb/kimi-k3")
	resolved, upstream, mn := reg.Resolve("kimi")
	if resolved != "cb/kimi-k3" || upstream != "codebuddy" || mn != "kimi-k3" {
		t.Errorf("got (%q,%q,%q), want (cb/kimi-k3,codebuddy,kimi-k3)", resolved, upstream, mn)
	}
}

// TestResolveFallthrough: unknown model falls through unchanged with empty upstream.
func TestResolveFallthrough(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	resolved, upstream, mn := reg.Resolve("grok-4.5")
	if resolved != "grok-4.5" || upstream != "" || mn != "" {
		t.Errorf("fallthrough got (%q,%q,%q), want (grok-4.5,,)", resolved, upstream, mn)
	}
}

// TestListModels: custom models appear in the flat list with correct owned_by.
func TestListModels(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	_ = reg.AddModel("cb/kimi-k3", db.CustomModel{Upstream: "codebuddy", ModelName: "kimi-k3"})
	_ = reg.AddModel("grok-secret", db.CustomModel{Upstream: "grok", ModelName: "grok-secret", OwnedBy: "xai"})

	entries := reg.ListModels()
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	found := map[string]string{}
	for _, e := range entries {
		found[e.ID] = e.OwnedBy
	}
	if found["cb/kimi-k3"] != "codebuddy" {
		t.Errorf("cb/kimi-k3 owned_by = %q, want codebuddy (defaulted from upstream)", found["cb/kimi-k3"])
	}
	if found["grok-secret"] != "xai" {
		t.Errorf("grok-secret owned_by = %q, want xai", found["grok-secret"])
	}
}

// TestAddDeleteModel: CRUD round-trip.
func TestAddDeleteModel(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	if err := reg.AddModel("cb/foo", db.CustomModel{Upstream: "codebuddy", ModelName: "foo"}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	if _, up, _ := reg.Resolve("cb/foo"); up != "codebuddy" {
		t.Fatalf("expected codebuddy after add, got %q", up)
	}
	if err := reg.DeleteModel("cb/foo"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, up, _ := reg.Resolve("cb/foo"); up != "" {
		t.Fatalf("expected empty upstream after delete, got %q", up)
	}
}

// TestAddDeleteAlias: CRUD round-trip.
func TestAddDeleteAlias(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	if err := reg.AddAlias("shortcut", "cb/gpt-5"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	if r, _, _ := reg.Resolve("shortcut"); r != "cb/gpt-5" {
		t.Fatalf("expected cb/gpt-5, got %q", r)
	}
	if err := reg.DeleteAlias("shortcut"); err != nil {
		t.Fatalf("DeleteAlias: %v", err)
	}
	if r, _, _ := reg.Resolve("shortcut"); r != "shortcut" {
		t.Fatalf("expected unchanged after delete, got %q", r)
	}
}

// TestAddModelValidation: bad inputs are rejected.
func TestAddModelValidation(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	// missing id
	if err := reg.AddModel("", db.CustomModel{Upstream: "codebuddy"}); err == nil {
		t.Error("expected error for empty id")
	}
	// bad upstream
	if err := reg.AddModel("cb/x", db.CustomModel{Upstream: "openai"}); err == nil {
		t.Error("expected error for bogus upstream")
	}
}

// TestAddModelDefaults: defaults for ModelName + OwnedBy applied.
func TestAddModelDefaults(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	if err := reg.AddModel("cb/foo", db.CustomModel{Upstream: "codebuddy"}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	snap := reg.SnapshotModels()["cb/foo"]
	if snap.ModelName != "foo" {
		t.Errorf("default ModelName = %q, want foo (stripped cb/)", snap.ModelName)
	}
	if snap.OwnedBy != "codebuddy" {
		t.Errorf("default OwnedBy = %q, want codebuddy", snap.OwnedBy)
	}
}

// TestAddAliasSelfLoop rejects alias pointing at itself.
func TestAddAliasSelfLoop(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	if err := reg.AddAlias("loop", "loop"); err == nil {
		t.Error("expected error for self-referential alias")
	}
}

// TestModelsListIncludesCustom: /v1/models returns hardcoded + custom entries.
func TestModelsListIncludesCustom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := proxy.NewCustomRegistry(nil)
	_ = reg.AddModel("cb/kimi-k3", db.CustomModel{Upstream: "codebuddy", ModelName: "kimi-k3", OwnedBy: "codebuddy"})

	// The handler needs typed nils for the upstream managers — the /v1/models
	// branch never touches them, so this is safe.
	h := proxy.ProxyRequest(nil, nil, nil, nil, nil, reg, nil)

	r := gin.New()
	r.GET("/v1/models", h)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q", resp.Object)
	}
	found := false
	for _, m := range resp.Data {
		if m["id"] == "cb/kimi-k3" {
			found = true
			if m["owned_by"] != "codebuddy" {
				t.Errorf("owned_by = %v, want codebuddy", m["owned_by"])
			}
			break
		}
	}
	if !found {
		t.Errorf("cb/kimi-k3 not in /v1/models output; got %d entries", len(resp.Data))
	}
}

// TestModelsListNilRegistry keeps the hardcoded list working when registry is nil.
func TestModelsListNilRegistry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := proxy.ProxyRequest(nil, nil, nil, nil, nil, nil, nil)
	r := gin.New()
	r.GET("/v1/models", h)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	// Sanity: at least one hardcoded id present.
	if !strings.Contains(w.Body.String(), "grok-4.5") {
		t.Errorf("expected grok-4.5 in output, got %s", w.Body.String())
	}
}

// TestConcurrentResolve: many concurrent Resolve/Add calls don't race and
// the state stays consistent. Race detector catches map corruption.
func TestConcurrentResolve(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	_ = reg.AddAlias("a", "cb/gpt-5")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			_, _, _ = reg.Resolve("a")
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 500; i++ {
			_ = reg.AddAlias("b", "cb/gpt-5.5")
			_ = reg.DeleteAlias("b")
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// TestAliasNonRecursive: alias chains only resolve one hop deep (documented).
func TestAliasNonRecursive(t *testing.T) {
	reg := proxy.NewCustomRegistry(nil)
	_ = reg.AddAlias("a", "b")
	_ = reg.AddAlias("b", "cb/gpt-5")
	// One hop: "a" resolves to "b", NOT to "cb/gpt-5".
	if r, _, _ := reg.Resolve("a"); r != "b" {
		t.Errorf("resolve(a) = %q, want b (single-hop resolution)", r)
	}
}

// _ import guard: prevent unused-import lint if we shrink the file.
var _ = http.StatusOK
