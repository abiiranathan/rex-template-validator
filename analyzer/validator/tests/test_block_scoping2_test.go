package validator_test

import (
	"testing"

	"analyzer/ast"
	"analyzer/validator"
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
	if len(errs) > 0 {
		for _, e := range errs {
			t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
		}
		t.Fatalf("expected no validation errors, got %d", len(errs))
	}
}
