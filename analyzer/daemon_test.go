package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
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

func TestValidateTemplateRecoversFromPanic(t *testing.T) {
	daemon := &analyzerDaemon{
		initialized:  true,
		baseDir:      "/tmp",
		templateRoot: "templates",
		validate:     true,
		renderVarsByTemplate: map[string][]ast.TemplateVar{
			"views/test.html": {{Name: "User"}},
		},
		namedBlocks:      map[string][]validator.NamedBlockEntry{},
		templateOverlays: map[string]string{},
	}
	_, err := daemon.validateTemplate(daemonValidateTemplateParams{
		AbsolutePath: "/tmp/templates/views/test.html",
		Content:      "{{",
	})
	if err == nil {
		t.Fatal("expected panic to be converted into error")
	}
	if !strings.Contains(err.Error(), "validateTemplate panic") {
		t.Fatalf("expected recovered panic error, got %v", err)
	}
}
