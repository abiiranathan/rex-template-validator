package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

func TestBlockScoping(t *testing.T) {
	content := `
		{{ block "billed-drug" .Drug }}
		    <div>{{ capitalize or (.Name) | upper }}</div>
		{{ end }}
	`
	vars := []ast.TemplateVar{
		{
			Name:    "Drug",
			TypeStr: "Drug",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}
	varMap := make(map[string]ast.TemplateVar)
	varMap["Drug"] = vars[0]

	errs := validator.ValidateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}
