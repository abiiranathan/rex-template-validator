package validator

import (
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
			if v.TypeStr != "repro.User" && v.TypeStr != "User" { // Exact string depends on how it's normalized
				t.Errorf("Warning: currentUser type is '%s'", v.TypeStr)
			}
		}
	}

	if !foundTitle {
		t.Error("Variable 'title' not found in RenderCall")
	}
	if !foundCurrentUser {
		t.Error("Variable 'currentUser' (from c.Set) not found in RenderCall")
	}
}
