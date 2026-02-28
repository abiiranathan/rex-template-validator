package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
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
	vars := []ast.TemplateVar{
		{
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Status", TypeStr: "string"},
				{Name: "Role", TypeStr: "string"},
				{Name: "Name", TypeStr: "string"},
				{Name: "Age", TypeStr: "int"},
				{
					Name: "Items", TypeStr: "[]Item",
					Fields: []ast.FieldInfo{
						{Name: "Name", TypeStr: "string"},
					},
				},
			},
		},
		{
			Name:    "Feature",
			TypeStr: "Feature",
			Fields: []ast.FieldInfo{
				{Name: "Enabled", TypeStr: "bool"},
			},
		},
	}
	varMap := make(map[string]ast.TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	errs := validator.ValidateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)
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
	vars := []ast.TemplateVar{
		{
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{
					Name: "Items", TypeStr: "[]Item",
					Fields: []ast.FieldInfo{
						{Name: "Name", TypeStr: "string"},
					},
				},
			},
		},
	}
	varMap := make(map[string]ast.TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	errs := validator.ValidateTemplateContent(content, varMap, "test.html", ".", ".", 1, nil)

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

// TestDeeplyNestedFieldAccess validates that .A.B.C.D paths (3+ levels deep)
// are correctly validated when the full field tree is present.
func TestDeeplyNestedFieldAccess(t *testing.T) {
	content := `
		{{ .User.Profile.Address.City }}
		{{ .User.Profile.Bio }}
		{{ .User.Name }}
	`
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{
					Name:    "Profile",
					TypeStr: "Profile",
					Fields: []ast.FieldInfo{
						{Name: "Bio", TypeStr: "string"},
						{
							Name:    "Address",
							TypeStr: "Address",
							Fields: []ast.FieldInfo{
								{Name: "Street", TypeStr: "string"},
								{Name: "City", TypeStr: "string"},
								{Name: "Zip", TypeStr: "string"},
							},
						},
					},
				},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error for valid deep path: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

// TestDeeplyNestedFieldAccess_Errors ensures that invalid deep paths are caught.
func TestDeeplyNestedFieldAccess_Errors(t *testing.T) {
	content := `
		{{ .User.Profile.Address.InvalidField }}
		{{ .User.Profile.InvalidNested }}
		{{ .User.InvalidTop.Whatever }}
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
						{
							Name:    "Address",
							TypeStr: "Address",
							Fields: []ast.FieldInfo{
								{Name: "Street", TypeStr: "string"},
								{Name: "City", TypeStr: "string"},
							},
						},
					},
				},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) != 3 {
		t.Errorf("Expected 3 errors for invalid deep paths, got %d", len(errs))
	}
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
}

// TestFourLevelDeepPath validates 4-level deep field access in templates.
func TestFourLevelDeepPath(t *testing.T) {
	content := `{{ .User.Profile.Address.City.Name }}`
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{
					Name:    "Profile",
					TypeStr: "Profile",
					Fields: []ast.FieldInfo{
						{
							Name:    "Address",
							TypeStr: "Address",
							Fields: []ast.FieldInfo{
								{
									Name:    "City",
									TypeStr: "City",
									Fields: []ast.FieldInfo{
										{Name: "Name", TypeStr: "string"},
										{Name: "ZipCode", TypeStr: "string"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error for 4-level path: %s", e.Message)
		}
	}
}

// TestDeepPathInsideRangeScope validates deep field access inside a range block.
func TestDeepPathInsideRangeScope(t *testing.T) {
	content := `
		{{ range .Items }}
			{{ .Product.Manufacturer.Name }}
			{{ .Product.Manufacturer.Country }}
		{{ end }}
	`
	vars := map[string]ast.TemplateVar{
		"Items": {
			Name:     "Items",
			TypeStr:  "[]OrderItem",
			IsSlice:  true,
			ElemType: "OrderItem",
			Fields: []ast.FieldInfo{
				{
					Name:    "Product",
					TypeStr: "Product",
					Fields: []ast.FieldInfo{
						{Name: "Name", TypeStr: "string"},
						{
							Name:    "Manufacturer",
							TypeStr: "Manufacturer",
							Fields: []ast.FieldInfo{
								{Name: "Name", TypeStr: "string"},
								{Name: "Country", TypeStr: "string"},
							},
						},
					},
				},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error inside range scope: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

// TestDeepPathInsideWithScope validates deep field access inside a with block.
func TestDeepPathInsideWithScope(t *testing.T) {
	content := `
		{{ with .User.Profile }}
			{{ .Address.City }}
			{{ .Bio }}
		{{ end }}
		{{ .User.Name }}
	`
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{
					Name:    "Profile",
					TypeStr: "Profile",
					Fields: []ast.FieldInfo{
						{Name: "Bio", TypeStr: "string"},
						{
							Name:    "Address",
							TypeStr: "Address",
							Fields: []ast.FieldInfo{
								{Name: "City", TypeStr: "string"},
								{Name: "Street", TypeStr: "string"},
							},
						},
					},
				},
			},
		},
	}

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error inside with scope: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

// TestRootAccessInNestedScope validates that $.VarName.Field works correctly
// inside range/with blocks to access root-level variables.
func TestRootAccessInNestedScope(t *testing.T) {
	content := `
		{{ range .Items }}
			{{ $.User.Profile.Address.City }}
			{{ .Name }}
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
						{
							Name:    "Address",
							TypeStr: "Address",
							Fields: []ast.FieldInfo{
								{Name: "City", TypeStr: "string"},
							},
						},
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

	errs := validator.ValidateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error for root access in nested scope: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}
