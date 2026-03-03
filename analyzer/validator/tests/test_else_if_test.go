package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

func TestElseIfSupport(t *testing.T) {
	content := `
		{{ if eq .User.Role "admin" }}
			<p>Admin</p>
		{{ else if eq .User.Role "manager" }}
			<p>Manager</p>
		{{ else if .User.IsGuest }}
			<p>Guest</p>
		{{ else }}
			<p>User</p>
		{{ end }}
	`
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Role", TypeStr: "string"},
				{Name: "IsGuest", TypeStr: "bool"},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
		for _, e := range errs {
			t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

func TestElseIfWithParentheses(t *testing.T) {
	content := `
		{{ if(eq .User.Role "admin") }}
			<p>Admin</p>
		{{ else if(eq .User.Role "manager") }}
			<p>Manager</p>
		{{ end }}
	`
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Role", TypeStr: "string"},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
		for _, e := range errs {
			t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

func TestElseWithAndElseRange(t *testing.T) {
	content := `
		{{ with .User.Profile }}
			<p>{{ .Bio }}</p>
		{{ else with .User.BackupProfile }}
			<p>{{ .Bio }}</p>
		{{ else range .User.Tags }}
			<p>{{ . }}</p>
		{{ end }}
	`
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{
					Name:    "Profile",
					TypeStr: "Profile",
					Fields: []ast.FieldInfo{
						{Name: "Bio", TypeStr: "string"},
					},
				},
				{
					Name:    "BackupProfile",
					TypeStr: "Profile",
					Fields: []ast.FieldInfo{
						{Name: "Bio", TypeStr: "string"},
					},
				},
				{
					Name:     "Tags",
					TypeStr:  "[]string",
					IsSlice:  true,
					ElemType: "string",
				},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
		for _, e := range errs {
			t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}
