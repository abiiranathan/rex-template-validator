package validator_test

import (
	"path/filepath"
	"testing"

	"github.com/abiiranathan/gotpl-analyzer/ast"
	"github.com/abiiranathan/gotpl-analyzer/validator"
)

func TestValidateTemplateFilePrefersOverlayEntry(t *testing.T) {
	registry := map[string][]validator.NamedBlockEntry{
		"views/partial.html": {
			{
				Name:         "views/partial.html",
				TemplatePath: "views/partial.html",
				AbsolutePath: filepath.Join("/tmp", "views", "partial.html"),
				Content:      `{{ .Missing }}`,
			},
		},
	}

	vars := []ast.TemplateVar{{
		Name:    "User",
		TypeStr: "User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
		},
	}}

	errs := validator.ValidateTemplateFile(
		filepath.Join("/tmp", "views", "partial.html"),
		vars,
		"views/partial.html",
		"/tmp",
		"views",
		registry,
	)

	if len(errs) != 1 {
		t.Fatalf("expected 1 overlay-backed validation error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != ".Missing" {
		t.Fatalf("expected overlay validation to report .Missing, got %q", errs[0].Variable)
	}
}
