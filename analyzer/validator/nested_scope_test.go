package validator

import (
	"strings"
	"testing"
)

// TestNestedRangeLoops validates field access in double-nested range loops
func TestNestedRangeLoops(t *testing.T) {
	content := `
        {{ range .Orders }}
            {{ range .Items }}
                {{ .Product.Name }}
                {{ .Product.Price }}
                {{ $.User.Email }}           {{/* root access */}}
                {{ $.Shop.Name }}            {{/* root access */}}
            {{ end }}
            {{ .OrderID }}
        {{ end }}
    `

	vars := map[string]TemplateVar{
		"Orders": {
			Name:     "Orders",
			TypeStr:  "[]Order",
			IsSlice:  true,
			ElemType: "Order",
			Fields: []FieldInfo{
				{Name: "OrderID", TypeStr: "string"},
				{
					Name:    "Items",
					TypeStr: "[]OrderItem",
					IsSlice: true,
					Fields: []FieldInfo{
						{
							Name:    "Product",
							TypeStr: "Product",
							Fields: []FieldInfo{
								{Name: "Name", TypeStr: "string"},
								{Name: "Price", TypeStr: "float64"},
							},
						},
					},
				},
			},
		},
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields:  []FieldInfo{{Name: "Email", TypeStr: "string"}},
		},
		"Shop": {
			Name:    "Shop",
			TypeStr: "Shop",
			Fields:  []FieldInfo{{Name: "Name", TypeStr: "string"}},
		},
	}

	errs := validateTemplateContent(content, vars, "nested-range.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error in nested ranges: %s (var: %s)", e.Message, e.Variable)
		}
	}
}

// TestNestedRangeWithInvalidAccess checks error reporting in nested ranges
func TestNestedRangeWithInvalidAccess(t *testing.T) {
	content := `
        {{ range .Department }}
            {{ range .Employees }}
                {{ .Name }}
                {{ .Salary }}
                {{ .DepartmentHead.Name }}     {{/* wrong - . is Employee, no DepartmentHead */}}
                {{ .InvalidField }}
                {{ $.Company.Name }}
            {{ end }}
        {{ end }}
    `

	vars := map[string]TemplateVar{
		"Department": {
			Name:     "Department",
			TypeStr:  "[]Dept",
			IsSlice:  true,
			ElemType: "Dept",
			Fields: []FieldInfo{
				{
					Name:    "Employees",
					TypeStr: "[]Employee",
					IsSlice: true,
					Fields: []FieldInfo{
						{Name: "Name", TypeStr: "string"},
						{Name: "Salary", TypeStr: "int"},
					},
				},
			},
		},
		"Company": {
			Name:    "Company",
			TypeStr: "Company",
			Fields:  []FieldInfo{{Name: "Name", TypeStr: "string"}},
		},
	}

	errs := validateTemplateContent(content, vars, "invalid-nested.html", ".", ".", 1, nil)

	expectedInvalid := []string{"DepartmentHead", "InvalidField"}

	if len(errs) != len(expectedInvalid) {
		t.Errorf("Expected %d errors, got %d", len(expectedInvalid), len(errs))
		t.Logf("Got errors:")
		for _, e := range errs {
			t.Logf("  - %s (variable: %s)", e.Message, e.Variable)
		}
		return
	}

	for _, e := range errs {
		found := false
		for _, want := range expectedInvalid {
			if strings.Contains(e.Message, want) || e.Variable == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Unexpected error reported: %s (var: %s)", e.Message, e.Variable)
		}
	}
}

// TestRangeInsideWithAndRootAccess
func TestRangeInsideWithAndRootAccess(t *testing.T) {
	content := `
        {{ with .CurrentUser }}
            {{ .FullName }}
            {{ .Role }}
            {{ range .Permissions }}
                {{ .Name }}
                {{ .Level }}
                {{ $.CurrentUser.Email }}       {{/* still accessible */}}
                {{ $.Settings.Theme }}          {{/* root */}}
            {{ end }}
            {{ .Invalid }}                  {{/* should error */}}
        {{ end }}
    `

	vars := map[string]TemplateVar{
		"CurrentUser": {
			Name:    "CurrentUser",
			TypeStr: "User",
			Fields: []FieldInfo{
				{Name: "FullName", TypeStr: "string"},
				{Name: "Role", TypeStr: "string"},
				{Name: "Email", TypeStr: "string"},
				{
					Name:    "Permissions",
					TypeStr: "[]Permission",
					IsSlice: true,
					Fields: []FieldInfo{
						{Name: "Name", TypeStr: "string"},
						{Name: "Level", TypeStr: "int"},
					},
				},
			},
		},
		"Settings": {
			Name:    "Settings",
			TypeStr: "Settings",
			Fields:  []FieldInfo{{Name: "Theme", TypeStr: "string"}},
		},
	}

	errs := validateTemplateContent(content, vars, "with-range.html", ".", ".", 1, nil)

	if len(errs) != 1 {
		t.Errorf("Expected exactly 1 error (Invalid), got %d", len(errs))
		for _, e := range errs {
			t.Logf("Error: %s (var: %s)", e.Message, e.Variable)
		}
	}
}

// TestTripleNestedRangeWithMixedDotAndDollar
func TestTripleNestedRangeWithMixedDotAndDollar(t *testing.T) {
	content := `
        {{ range .Regions }}
            {{ range .Countries }}
                {{ range .Cities }}
                    {{ .Name }}
                    {{ .Population }}
                    {{ $.Company.Name }}               {{/* root */}}
                    {{ $.Regions[0].Name }}            {{/* invalid - no index access yet */}}
                    {{ .Country.Name }}                {{/* wrong scope - . is City */}}
                {{ end }}
            {{ end }}
        {{ end }}
    `

	vars := map[string]TemplateVar{
		"Regions": {
			Name:     "Regions",
			TypeStr:  "[]Region",
			IsSlice:  true,
			ElemType: "Region",
			Fields: []FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{
					Name:    "Countries",
					TypeStr: "[]Country",
					IsSlice: true,
					Fields: []FieldInfo{
						{Name: "Name", TypeStr: "string"},
						{
							Name:    "Cities",
							TypeStr: "[]City",
							IsSlice: true,
							Fields: []FieldInfo{
								{Name: "Name", TypeStr: "string"},
								{Name: "Population", TypeStr: "int"},
							},
						},
					},
				},
			},
		},
		"Company": {
			Name:    "Company",
			TypeStr: "Company",
			Fields:  []FieldInfo{{Name: "Name", TypeStr: "string"}},
		},
	}

	errs := validateTemplateContent(content, vars, "triple-nest.html", ".", ".", 1, nil)

	if len(errs) < 1 {
		t.Error("Expected at least one scope-related error")
	}

	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
}
