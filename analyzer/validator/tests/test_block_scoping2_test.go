package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

func TestBlockScoping2(t *testing.T) {
	content := `
	{{ range .billedDrugs }}
        {{ template "billed-drug" . }}
    {{ end }}
    {{ block "billed-drug" . }}
        <div>{{ capitalize or (.Name) | upper }}</div>
    {{ end }}
	`
	vars := []ast.TemplateVar{
		{
			Name:    "billedDrugs",
			TypeStr: "[]Drug",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}
	varMap := make(map[string]ast.TemplateVar)
	varMap["billedDrugs"] = vars[0]
	// intentionally DO NOT add .Name to root scope, because . is the root, not the Drug!

	errs := validator.ValidateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) > 0 {
		t.Errorf("Expected some errors or no errors? Let's see")
	}
}
