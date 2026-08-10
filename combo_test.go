package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"foxrouters/internal/db"
	"foxrouters/internal/proxy"

	"github.com/gin-gonic/gin"
)

// TestResolveComboFallback: combo/<name> with fallback strategy returns
// models[0]. NextInFallback walks the chain on failure.
func TestResolveComboFallback(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	if err := reg.AddCombo(db.Combo{
		Name:     "smart-fallback",
		Strategy: "fallback",
		Models:   []string{"cb/gpt-5.5", "cb/claude-sonnet-4.6", "grok-4.5"},
	}); err != nil {
		t.Fatalf("AddCombo: %v", err)
	}
	got, ok := reg.Resolve("combo/smart-fallback")
	if !ok {
		t.Fatalf("Resolve returned isCombo=false")
	}
	if got != "cb/gpt-5.5" {
		t.Errorf("Resolve = %q, want cb/gpt-5.5", got)
	}
}

// TestResolveComboRoundRobin: successive Resolves rotate through the
// models list. With a nil store the registry falls back to an in-process
// counter which is still deterministic per-key.
func TestResolveComboRoundRobin(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	if err := reg.AddCombo(db.Combo{
		Name:     "rr-test-uniq",
		Strategy: "round_robin",
		Models:   []string{"cb/gpt-5.5", "cb/claude-sonnet-4.6"},
	}); err != nil {
		t.Fatalf("AddCombo: %v", err)
	}
	// Collect 4 rotations — expect each model twice.
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got, ok := reg.Resolve("combo/rr-test-uniq")
		if !ok {
			t.Fatalf("iter %d Resolve isCombo=false", i)
		}
		seen[got]++
	}
	if seen["cb/gpt-5.5"] != 2 || seen["cb/claude-sonnet-4.6"] != 2 {
		t.Errorf("rotation not fair: %v (want each model x2)", seen)
	}
}

// TestNextInFallback: NextInFallback returns the entry after failedModel,
// or empty when the failure was on the last entry.
func TestNextInFallback(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	_ = reg.AddCombo(db.Combo{
		Name:     "chain",
		Strategy: "fallback",
		Models:   []string{"a", "b", "c"},
	})
	if next, ok := reg.NextInFallback("chain", "a"); !ok || next != "b" {
		t.Errorf("after a: got (%q,%v), want (b,true)", next, ok)
	}
	if next, ok := reg.NextInFallback("chain", "b"); !ok || next != "c" {
		t.Errorf("after b: got (%q,%v), want (c,true)", next, ok)
	}
	if next, ok := reg.NextInFallback("chain", "c"); ok || next != "" {
		t.Errorf("after c: got (%q,%v), want empty end-of-chain", next, ok)
	}
	// Missing combo → false.
	if _, ok := reg.NextInFallback("nope", "x"); ok {
		t.Errorf("missing combo should return false")
	}
}

// TestListCombos: added combos appear in ListCombos.
func TestListCombos(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	_ = reg.AddCombo(db.Combo{Name: "a", Strategy: "fallback", Models: []string{"x"}})
	_ = reg.AddCombo(db.Combo{Name: "b", Strategy: "round_robin", Models: []string{"y", "z"}})

	entries := reg.ListCombos()
	if len(entries) != 2 {
		t.Fatalf("want 2 combos, got %d", len(entries))
	}
	names := map[string]bool{}
	for _, c := range entries {
		names[c.Name] = true
	}
	if !names["a"] || !names["b"] {
		t.Errorf("missing entries: %v", names)
	}
}

// TestAddDeleteCombo: CRUD round-trip.
func TestAddDeleteCombo(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	if err := reg.AddCombo(db.Combo{
		Name:     "crud",
		Strategy: "fallback",
		Models:   []string{"cb/gpt-5.5"},
	}); err != nil {
		t.Fatalf("AddCombo: %v", err)
	}
	if _, ok := reg.GetCombo("crud"); !ok {
		t.Fatalf("GetCombo should find combo after add")
	}
	if err := reg.DeleteCombo("crud"); err != nil {
		t.Fatalf("DeleteCombo: %v", err)
	}
	if _, ok := reg.GetCombo("crud"); ok {
		t.Fatalf("GetCombo should miss after delete")
	}
	if _, ok := reg.Resolve("combo/crud"); ok {
		t.Fatalf("Resolve should miss after delete")
	}
}

// TestResolveNotCombo: non-combo models fall through unchanged (isCombo=false).
func TestResolveNotCombo(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	_ = reg.AddCombo(db.Combo{Name: "x", Strategy: "fallback", Models: []string{"cb/gpt-5.5"}})

	if _, ok := reg.Resolve("cb/gpt-5.5"); ok {
		t.Errorf("plain model should return isCombo=false")
	}
	if _, ok := reg.Resolve("grok-4.5"); ok {
		t.Errorf("plain model should return isCombo=false")
	}
	// Unknown combo name also returns false.
	if _, ok := reg.Resolve("combo/unknown"); ok {
		t.Errorf("unknown combo should return isCombo=false")
	}
}

// TestComboInModelsList: /v1/models includes combos as "combo/<name>".
func TestComboInModelsList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	comboReg := proxy.NewComboRegistry(nil)
	_ = comboReg.AddCombo(db.Combo{
		Name:        "shown-in-list",
		Strategy:    "fallback",
		Models:      []string{"cb/gpt-5.5"},
		Description: "test",
	})

	h := proxy.ProxyRequest(nil, nil, nil, nil, nil, nil, comboReg)

	r := gin.New()
	r.GET("/v1/models", h)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, m := range resp.Data {
		if m["id"] == "combo/shown-in-list" {
			found = true
			if m["owned_by"] != "foxrouters" {
				t.Errorf("owned_by = %v, want foxrouters", m["owned_by"])
			}
			break
		}
	}
	if !found {
		t.Errorf("combo/shown-in-list not in /v1/models")
	}
}

// TestAddComboValidation: bad inputs are rejected.
func TestAddComboValidation(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	// empty name
	if err := reg.AddCombo(db.Combo{Strategy: "fallback", Models: []string{"a"}}); err == nil {
		t.Error("expected error for empty name")
	}
	// bad strategy
	if err := reg.AddCombo(db.Combo{Name: "x", Strategy: "fusion", Models: []string{"a"}}); err == nil {
		t.Error("expected error for bogus strategy")
	}
	// no models
	if err := reg.AddCombo(db.Combo{Name: "x", Strategy: "fallback"}); err == nil {
		t.Error("expected error for empty models list")
	}
	// reserved prefix
	if err := reg.AddCombo(db.Combo{Name: "combo/nested", Strategy: "fallback", Models: []string{"a"}}); err == nil {
		t.Error("expected error for name starting with combo/")
	}
}

// TestAddComboDefaults: strategy defaults to "fallback" when empty, models
// with surrounding whitespace get trimmed.
func TestAddComboDefaults(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	if err := reg.AddCombo(db.Combo{
		Name:   "defaults",
		Models: []string{"  cb/gpt-5.5  ", "", "grok-4.5"},
	}); err != nil {
		t.Fatalf("AddCombo: %v", err)
	}
	got, ok := reg.GetCombo("defaults")
	if !ok {
		t.Fatalf("GetCombo miss")
	}
	if got.Strategy != "fallback" {
		t.Errorf("Strategy = %q, want fallback (default)", got.Strategy)
	}
	if len(got.Models) != 2 || got.Models[0] != "cb/gpt-5.5" || got.Models[1] != "grok-4.5" {
		t.Errorf("Models = %v, want [cb/gpt-5.5 grok-4.5] (trimmed + empty dropped)", got.Models)
	}
}

// TestConcurrentComboResolve: concurrent Resolve + Add/Delete stays consistent.
// Race detector catches map corruption.
func TestConcurrentComboResolve(t *testing.T) {
	reg := proxy.NewComboRegistry(nil)
	_ = reg.AddCombo(db.Combo{Name: "concur", Strategy: "round_robin", Models: []string{"a", "b", "c"}})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			_, _ = reg.Resolve("combo/concur")
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 500; i++ {
			_ = reg.AddCombo(db.Combo{Name: "flap", Strategy: "fallback", Models: []string{"x"}})
			_ = reg.DeleteCombo("flap")
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}
