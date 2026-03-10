package ast

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStringMapTemplateLookup verifies that template names are resolved when
// they come from a package-level map[K]string variable indexed at runtime.
//
// Pattern under test:
//
//	var labforms = map[enums.ReportType]string{
//	    enums.ReportTypeGeneral: "views/lab/GENERAL.html",
//	    enums.ReportTypeCbc:     "views/lab/CBC.html",
//	}
//	view, ok := labforms[request.ReportType]
//	c.Render(view, data)
//
// Expected: one RenderCall per value in labforms, all sharing the same vars.
func TestStringMapTemplateLookup(t *testing.T) {
	tmpDir := t.TempDir()

	src := `package main

type ReportType int

const (
	ReportTypeGeneral ReportType = iota
	ReportTypeCbc
	ReportTypeLfts
)

var labforms = map[ReportType]string{
	ReportTypeGeneral: "views/lab/GENERAL.html",
	ReportTypeCbc:     "views/lab/CBC.html",
	ReportTypeLfts:    "views/lab/LFTS.html",
}

type Context struct{}
func (c *Context) Render(tpl string, data map[string]interface{}) {}

type Request struct{ ReportType ReportType }

func handler(c *Context, request *Request) {
	view, ok := labforms[request.ReportType]
	if !ok {
		return
	}
	c.Render(view, map[string]interface{}{
		"testName": "CBC Panel",
		"reportID": 42,
	})
}
`
	mod := "module example.com/test\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatal(err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)
	if len(result.Errors) > 0 {
		t.Fatalf("analysis errors: %v", result.Errors)
	}

	// We expect exactly 3 render calls — one per map value.
	expected := map[string]bool{
		"views/lab/GENERAL.html": false,
		"views/lab/CBC.html":     false,
		"views/lab/LFTS.html":    false,
	}

	for _, rc := range result.RenderCalls {
		if _, ok := expected[rc.Template]; ok {
			expected[rc.Template] = true
		} else {
			t.Errorf("unexpected render call for template %q", rc.Template)
		}
	}

	for tpl, found := range expected {
		if !found {
			t.Errorf("expected render call for template %q — not found", tpl)
		}
	}

	// Every render call must carry the data-map variables.
	for _, rc := range result.RenderCalls {
		hasTestName := false
		hasReportID := false
		for _, v := range rc.Vars {
			switch v.Name {
			case "testName":
				hasTestName = true
			case "reportID":
				hasReportID = true
			}
		}
		if !hasTestName {
			t.Errorf("render call %q missing var 'testName'", rc.Template)
		}
		if !hasReportID {
			t.Errorf("render call %q missing var 'reportID'", rc.Template)
		}
	}
}

// TestStringMapSingleResult verifies the single-assignment form: `v := m[k]`
// (no boolean second result).
func TestStringMapSingleResult(t *testing.T) {
	tmpDir := t.TempDir()

	src := `package main

var views = map[string]string{
	"home":    "templates/home.html",
	"profile": "templates/profile.html",
}

type Context struct{}
func (c *Context) Render(tpl string, data map[string]interface{}) {}

func handler(c *Context, page string) {
	tpl := views[page]
	c.Render(tpl, map[string]interface{}{
		"user": "alice",
	})
}
`
	mod := "module example.com/test\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatal(err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)
	if len(result.Errors) > 0 {
		t.Fatalf("analysis errors: %v", result.Errors)
	}

	expected := map[string]bool{
		"templates/home.html":    false,
		"templates/profile.html": false,
	}

	for _, rc := range result.RenderCalls {
		if _, ok := expected[rc.Template]; ok {
			expected[rc.Template] = true
		} else {
			t.Errorf("unexpected render call for template %q", rc.Template)
		}
	}
	for tpl, found := range expected {
		if !found {
			t.Errorf("expected render call for %q", tpl)
		}
	}
}

// TestStringMapNoFalsePositives verifies that a map[K]SomeStruct (non-string
// values) is NOT treated as a template-name map.
func TestStringMapNoFalsePositives(t *testing.T) {
	tmpDir := t.TempDir()

	src := `package main

type Config struct{ Path string }

// Values are structs, not strings — must NOT be picked up.
var configs = map[string]Config{
	"a": {Path: "a.html"},
}

type Context struct{}
func (c *Context) Render(tpl string, data map[string]interface{}) {}

func handler(c *Context) {
	name := "direct.html"
	c.Render(name, nil)
}
`
	mod := "module example.com/test\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatal(err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)
	if len(result.Errors) > 0 {
		t.Fatalf("analysis errors: %v", result.Errors)
	}

	if len(result.RenderCalls) != 1 || result.RenderCalls[0].Template != "direct.html" {
		t.Errorf("expected exactly one render call for 'direct.html', got %+v", result.RenderCalls)
	}
}
