package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/analyzer/validator"
)

func TestBlockBodyWithNonDotContextIsValidated(t *testing.T) {
	content := `
		{{ block "user-msg" .currentUser }}
			<h1>Hello: {{ .Missing }}</h1>
		{{ end }}
	`

	varMap := map[string]ast.TemplateVar{
		"currentUser": {
			Name:    "currentUser",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)
	if len(errs) == 0 {
		t.Fatal("expected validation error inside block body for non-dot context")
	}
	if errs[0].Variable != ".Missing" {
		t.Fatalf("expected missing variable .Missing, got %q", errs[0].Variable)
	}
	if errs[0].Line != 3 {
		t.Fatalf("expected error line 3 inside block body, got %d", errs[0].Line)
	}
}
