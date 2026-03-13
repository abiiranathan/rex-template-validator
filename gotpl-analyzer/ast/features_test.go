package ast

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDynamicTemplateName verifies that template names passed as variables are resolved.
func TestDynamicTemplateName(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import "net/http"

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	// Case 1: Simple variable assignment
	tplName := "index.html"
	Render(nil, tplName, nil)

	// Case 2: Variable with type inference
	var otherTpl = "other.html"
	Render(nil, otherTpl, nil)

	// Case 3: Constant
	const constTpl = "const.html"
	Render(nil, constTpl, nil)
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)

	expected := map[string]bool{
		"index.html": false,
		"other.html": false,
		"const.html": false,
	}

	for _, call := range result.RenderCalls {
		if _, ok := expected[call.Template]; ok {
			expected[call.Template] = true
		}
	}

	for tpl, found := range expected {
		if !found {
			t.Errorf("expected to find render call for template %q", tpl)
		}
	}
}

// TestFuncMapDiscovery verifies that FuncMap calls are discovered.
func TestFuncMapDiscovery(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import (
	"strings"
	"text/template"
)

var GlobalFuncMap = template.FuncMap{
	"globalFunc": strings.ToLower,
}

func GetFuncMap() template.FuncMap {
	fm := template.FuncMap{
		"toUpper": strings.ToUpper,
		"add": func(a, b int) int {
			return a + b
		},
	}
	fm["sub"] = func(a, b int) int { return a - b }
	
	// Edge case: implicit conversion
	var other template.FuncMap = map[string]any{
		"implicitMap": strings.TrimSpace,
	}
	_ = other
	return fm
}

func main() {
	_ = GetFuncMap()
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)

	expected := map[string]bool{
		"toUpper":     false,
		"add":         false,
		"sub":         false,
		"globalFunc":  false,
		"implicitMap": false,
	}

	for _, fm := range result.FuncMaps {
		if _, ok := expected[fm.Name]; ok {
			expected[fm.Name] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("expected to find FuncMap entry for %q", name)
		}
	}
}
