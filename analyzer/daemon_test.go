package main

import (
	"path/filepath"
	"testing"

	"analyzer/ast"
)

func TestFindRenderVarsForTemplateMatchesBySuffix(t *testing.T) {
	baseDir := filepath.Join(string(filepath.Separator), "workspace")
	templateRoot := "templates"
	absPath := filepath.Join(baseDir, templateRoot, "views", "dashboard.html")

	renderVarsByTemplate := map[string][]ast.TemplateVar{
		"dashboard.html": {
			{Name: "Patient"},
		},
	}

	matchedKey, vars, ok := findRenderVarsForTemplate(renderVarsByTemplate, absPath, baseDir, templateRoot)
	if !ok {
		t.Fatal("expected template context to be resolved")
	}
	if matchedKey != "dashboard.html" {
		t.Fatalf("expected matched key dashboard.html, got %q", matchedKey)
	}
	if len(vars) != 1 || vars[0].Name != "Patient" {
		t.Fatalf("expected Patient vars, got %#v", vars)
	}
}

func TestFindRenderVarsForTemplateMatchesByAbsolutePath(t *testing.T) {
	baseDir := filepath.Join(string(filepath.Separator), "workspace")
	templateRoot := "templates"
	absPath := filepath.Join(baseDir, templateRoot, "views", "dashboard.html")

	renderVarsByTemplate := map[string][]ast.TemplateVar{
		"./views/dashboard.html": {
			{Name: "Patient"},
		},
	}

	matchedKey, _, ok := findRenderVarsForTemplate(renderVarsByTemplate, absPath, baseDir, templateRoot)
	if !ok {
		t.Fatal("expected template context to be resolved")
	}
	if matchedKey != "./views/dashboard.html" {
		t.Fatalf("expected original key to be preserved, got %q", matchedKey)
	}
}
