package validator

import "testing"

func TestBlockScoping(t *testing.T) {
	content := `
		{{ block "billed-drug" .Drug }}
		    <div>{{ capitalize or (.Name) | upper }}</div>
		{{ end }}
	`
	vars := []TemplateVar{
		{
			Name: "Drug",
			TypeStr: "Drug",
			Fields: []FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}
	varMap := make(map[string]TemplateVar)
	varMap["Drug"] = vars[0]
	
	errs := validateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}
