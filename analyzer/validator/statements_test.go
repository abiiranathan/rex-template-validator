package validator

import (
	"testing"
)

func TestStatementsAndFunctions(t *testing.T) {
	content := `
		{{ if (eq .User.Status "active") }}
			{{ and (eq .User.Role "admin") (.Feature.Enabled) }}
			{{ .User.Name | upper }}
			{{ $.User.Age }}
		{{ end }}
		{{ range $i, $v := .User.Items }}
		    {{ $.User.Age }}
            {{ .Name }}
		{{ end }}
	`
	vars := []TemplateVar{
		{
			Name:    "User",
			TypeStr: "User",
			Fields: []FieldInfo{
				{Name: "Status", TypeStr: "string"},
				{Name: "Role", TypeStr: "string"},
				{Name: "Name", TypeStr: "string"},
				{Name: "Age", TypeStr: "int"},
				{
					Name: "Items", TypeStr: "[]Item",
					Fields: []FieldInfo{
						{Name: "Name", TypeStr: "string"},
					},
				},
			},
		},
		{
			Name:    "Feature",
			TypeStr: "Feature",
			Fields: []FieldInfo{
				{Name: "Enabled", TypeStr: "bool"},
			},
		},
	}
	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	errs := validateTemplateContent(content, varMap, "test.html", ".", ".")
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

func TestStatementsAndFunctions_Errors(t *testing.T) {
	content := `
		{{ if (eq .User.Invalid1 "active") }}
		{{ end }}
		{{ range .User.Items }}
		    {{ $.User.Invalid2 }}
            {{ .Invalid3 }}
		{{ end }}
	`
	vars := []TemplateVar{
		{
			Name:    "User",
			TypeStr: "User",
			Fields: []FieldInfo{
				{
					Name: "Items", TypeStr: "[]Item",
					Fields: []FieldInfo{
						{Name: "Name", TypeStr: "string"},
					},
				},
			},
		},
	}
	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	errs := validateTemplateContent(content, varMap, "test.html", ".", ".")

	expectedErrors := []string{
		"Invalid1", "Invalid2", "Invalid3",
	}

	if len(errs) != len(expectedErrors) {
		t.Errorf("Expected %d errors, got %d", len(expectedErrors), len(errs))
	}

	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
}
