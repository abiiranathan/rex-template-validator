package validator

import "testing"

func TestUserSnippetElseBug(t *testing.T) {
	// This reproduces the exact issue from the user's snippet
	content := `
		{{ if $.account.IsSuperuser }}
            <p class="p-2 font-bold text-green-600 bg-green-100 border rounded-sm">Superuser has all permissions</p>
        {{ else }}
            {{ range .account.Permission.Slice }}
                <li class="w-full px-4 py-2 bg-white border border-gray-300">
                    {{ . }}
                </li>
            {{ else }}
                <div class="badge badge-danger">NO PERMISSIONS ASSIGNED!</div>
            {{ end }}
        {{ end }}
	`

	vars := map[string]TemplateVar{
		"account": {
			Name:    "account",
			TypeStr: "Account",
			Fields: []FieldInfo{
				{Name: "IsSuperuser", TypeStr: "bool"},
				{
					Name:    "Permission",
					TypeStr: "Permission",
					Fields: []FieldInfo{
						{
							Name:     "Slice",
							TypeStr:  "[]string",
							IsSlice:  true,
							ElemType: "string",
							Fields:   []FieldInfo{}, // string has no fields
						},
					},
				},
			},
		},
	}

	errs := validateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d:", len(errs))
		for _, e := range errs {
			t.Logf("  Error: %s (variable: %s, line: %d, col: %d)", e.Message, e.Variable, e.Line, e.Column)
		}
	}
}

func TestSimpleElseClause(t *testing.T) {
	// Simpler test case to isolate the else clause issue
	content := `
		{{ if .User.Name }}
			{{ .User.Age }}
		{{ else }}
			{{ .User.Address.City }}
		{{ end }}
	`

	vars := map[string]TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{Name: "Age", TypeStr: "int"},
				{
					Name:    "Address",
					TypeStr: "Address",
					Fields: []FieldInfo{
						{Name: "City", TypeStr: "string"},
					},
				},
			},
		},
	}

	errs := validateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d:", len(errs))
		for _, e := range errs {
			t.Logf("  Error: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}

func TestRangeElseClause(t *testing.T) {
	// Test range with else clause
	content := `
		{{ range .Items }}
			{{ .Name }}
		{{ else }}
			<p>No items</p>
		{{ end }}
	`

	vars := map[string]TemplateVar{
		"Items": {
			Name:     "Items",
			TypeStr:  "[]Item",
			IsSlice:  true,
			ElemType: "Item",
			Fields: []FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}

	errs := validateTemplateContent(content, vars, "test.html", ".", ".", 1, nil)
	if len(errs) > 0 {
		t.Errorf("Expected 0 errors, got %d:", len(errs))
		for _, e := range errs {
			t.Logf("  Error: %s (variable: %s)", e.Message, e.Variable)
		}
	}
}
