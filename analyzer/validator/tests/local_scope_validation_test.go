package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

func TestLocalVariableFieldValidation(t *testing.T) {
	content := `
		{{ $user := .User }}
		{{ $user.Name }}
		{{ $user.Missing }}
	`

	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "locals.html", ".", ".", 1, nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != "$user.Missing" {
		t.Fatalf("expected missing local field error, got %q", errs[0].Variable)
	}
}

func TestNestedScopesResolveLocalVariables(t *testing.T) {
	content := `
		{{ $user := .User }}
		{{ with $user.Address }}
			{{ .City }}
		{{ end }}
		{{ range $idx, $item := .Items }}
			{{ $item.Name }}
			{{ $item.Missing }}
		{{ end }}
	`

	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{
					Name:    "Address",
					TypeStr: "Address",
					Fields: []ast.FieldInfo{
						{Name: "City", TypeStr: "string"},
					},
				},
			},
		},
		"Items": {
			Name:     "Items",
			TypeStr:  "[]Item",
			IsSlice:  true,
			ElemType: "Item",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "nested-locals.html", ".", ".", 1, nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != "$item.Missing" {
		t.Fatalf("expected nested local field error, got %q", errs[0].Variable)
	}
}

func TestUndefinedLocalVariableIsReported(t *testing.T) {
	content := `{{ $missing.Name }}`
	errs := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "missing-local.html", ".", ".", 1, nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != "$missing.Name" {
		t.Fatalf("expected undefined local variable error, got %q", errs[0].Variable)
	}
}
