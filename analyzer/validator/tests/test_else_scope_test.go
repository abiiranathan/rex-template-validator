package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

func TestElseScopePop(t *testing.T) {
	content := `
		{{ with .User }}
			{{ if .Age }}
				<p>Age is set</p>
			{{ else if .Name }}
			    <p>Name is set</p>
			{{ else }}
			    <p>Nothing is set</p>
			{{ end }}
			{{ .Name }}
		{{ end }}
	`
	vars := []ast.TemplateVar{
		{
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{Name: "Age", TypeStr: "int"},
			},
		},
	}
	varMap := make(map[string]ast.TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	errs := validator.ValidateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}
