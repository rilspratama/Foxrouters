package main

import (
	"strings"
	"testing"
)

func TestDashboardAssembly(t *testing.T) {
	// Byte-critical structure: both script blocks present, INIT last, no double-newline
	if strings.Count(dashboardHTML, "<script>\n") != 2 {
		t.Fatalf("expected 2 script blocks, got %d", strings.Count(dashboardHTML, "<script>\n"))
	}
	if !strings.Contains(dashboardHTML, "router();") {
		t.Fatal("missing router() init")
	}
	if !strings.Contains(dashboardHTML, "function mediaInit") {
		t.Fatal("missing mediaInit (block 2)")
	}
	if !strings.Contains(dashboardHTML, "function renderProviders") {
		t.Fatal("missing renderProviders (block 1)")
	}
	// INIT must be in the LAST script block (after mediaInit etc)
	lastScript := strings.LastIndex(dashboardHTML, "<script>\n")
	if !strings.Contains(dashboardHTML[lastScript:], "router();") {
		t.Fatal("INIT (router()) not in last script block")
	}
	// no empty <script></script>
	if strings.Contains(dashboardHTML, "<script>\n</script>") {
		t.Fatal("empty script block")
	}
	t.Logf("dashboard assembled: %d bytes, %d lines", len(dashboardHTML), strings.Count(dashboardHTML, "\n"))
}
