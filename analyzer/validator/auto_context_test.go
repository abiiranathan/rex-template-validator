package validator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAutoContextDiscovery(t *testing.T) {
	// Path to the repro directory
	reproDir, _ := filepath.Abs("testdata/repro")

	// Run analysis
	result := AnalyzeDir(reproDir, "")

	if len(result.Errors) > 0 {
		t.Fatalf("Analysis failed with errors: %v", result.Errors)
	}

	if len(result.RenderCalls) != 1 {
		t.Fatalf("Expected 1 render call, got %d", len(result.RenderCalls))
	}

	rc := result.RenderCalls[0]
	if rc.Template != "index.html" {
		t.Errorf("Expected template 'index.html', got '%s'", rc.Template)
	}

	// Check for 'title' (explicit) and 'currentUser' (implicit via Set)
	foundTitle := false
	foundCurrentUser := false

	for _, v := range rc.Vars {
		if v.Name == "title" {
			foundTitle = true
		}
		if v.Name == "currentUser" {
			foundCurrentUser = true
		}
	}

	if !foundTitle {
		t.Error("Variable 'title' not found in RenderCall")
	}
	if !foundCurrentUser {
		t.Error("Variable 'currentUser' (from c.Set) not found in RenderCall")
	}
}

func TestGlobalVsLocalContext(t *testing.T) {
	// Create a temporary directory for this test
	tmpDir := t.TempDir()

	src := `package main

type Context struct {}
func (c *Context) Set(key string, val interface{}) {}
func (c *Context) Render(tpl string, data map[string]interface{}) {}

// Middleware: has Set but no Render -> Global
func middleware(c *Context) {
	c.Set("globalVar", "global")
}

// Handler A: has Set "localVarA" and Render "viewA.html" -> Local A
func handlerA(c *Context) {
	c.Set("localVarA", "A")
	c.Render("viewA.html", nil)
}

// Handler B: has Set "localVarB" and Render "viewB.html" -> Local B
func handlerB(c *Context) {
	c.Set("localVarB", "B")
	c.Render("viewB.html", nil)
}
`
	// Write main.go
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	// Write go.mod
	mod := `module test
go 1.20
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatal(err)
	}

	// Run analysis on the temp dir
	result := AnalyzeDir(tmpDir, "")

	// We expect 2 render calls (viewA and viewB)
	if len(result.RenderCalls) != 2 {
		t.Logf("Analyzer errors: %v", result.Errors)
		t.Fatalf("Expected 2 render calls, got %d", len(result.RenderCalls))
	}

	// Find the render calls
	var callA, callB *RenderCall
	for i := range result.RenderCalls {
		rc := &result.RenderCalls[i]
		switch rc.Template {
		case "viewA.html":
			callA = rc
		case "viewB.html":
			callB = rc
		}
	}

	if callA == nil {
		t.Fatal("viewA.html render call not found")
	}
	if callB == nil {
		t.Fatal("viewB.html render call not found")
	}

	// Helper to check for variable existence
	hasVar := func(rc *RenderCall, name string) bool {
		for _, v := range rc.Vars {
			if v.Name == name {
				return true
			}
		}
		return false
	}

	// Check View A
	if !hasVar(callA, "globalVar") {
		t.Error("viewA should have globalVar (from middleware)")
	}
	if !hasVar(callA, "localVarA") {
		t.Error("viewA should have localVarA (from handlerA)")
	}
	if hasVar(callA, "localVarB") {
		t.Error("viewA should NOT have localVarB (from handlerB)")
	}

	// Check View B
	if !hasVar(callB, "globalVar") {
		t.Error("viewB should have globalVar (from middleware)")
	}
	if !hasVar(callB, "localVarB") {
		t.Error("viewB should have localVarB (from handlerB)")
	}
	if hasVar(callB, "localVarA") {
		t.Error("viewB should NOT have localVarA (from handlerA)")
	}
}
