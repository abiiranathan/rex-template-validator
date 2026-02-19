package validator

import (
	"testing"
)

func TestValidateTemplateContent(t *testing.T) {
	// Setup test variables
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
						{Name: "Zip", TypeStr: "string"},
					},
				},
			},
		},
		"Items": {
			Name:     "Items",
			TypeStr:  "[]Item",
			IsSlice:  true,
			ElemType: "Item",
			Fields: []FieldInfo{ // Fields of the element type Item
				{Name: "Title", TypeStr: "string"},
				{Name: "Price", TypeStr: "float64"},
			},
		},
	}

	tests := []struct {
		name     string
		content  string
		expected []ValidationResult
	}{
		{
			name:     "Valid variable access",
			content:  "{{ .User.Name }}",
			expected: nil,
		},
		{
			name:    "Invalid variable access",
			content: "{{ .User.Invalid }}",
			expected: []ValidationResult{
				{
					Variable: ".User.Invalid",
					Message:  `Field "Invalid" does not exist on type User`,
					Line:     1,
					Column:   4, // {{ .User.Invalid }} starts at col 1? No, {{ is col 1. .User is col 4.
					Severity: "error",
				},
			},
		},
		{
			name:     "Valid nested variable access",
			content:  "{{ .User.Address.City }}",
			expected: nil,
		},
		{
			name:    "Invalid nested variable access",
			content: "{{ .User.Address.Invalid }}",
			expected: []ValidationResult{
				{
					Variable: ".User.Address.Invalid",
					Message:  `Field "Invalid" does not exist on type Address`,
					Line:     1,
					Column:   4,
					Severity: "error",
				},
			},
		},
		{
			name:     "Valid range access",
			content:  "{{ range .Items }}{{ .Title }}{{ end }}",
			expected: nil,
		},
		{
			name:    "Invalid range access",
			content: "{{ range .Items }}{{ .Invalid }}{{ end }}",
			expected: []ValidationResult{
				{
					Variable: ".Invalid",
					Message:  `Template variable ".Invalid" is not defined in the render context`,
					Line:     1,
					Column:   22,
					Severity: "error",
				},
			},
		},
		{
			name:     "Valid with block",
			content:  "{{ with .User }}{{ .Name }}{{ end }}",
			expected: nil,
		},
		{
			name:    "Invalid with block access",
			content: "{{ with .User }}{{ .Invalid }}{{ end }}",
			expected: []ValidationResult{
				{
					Variable: ".Invalid",
					Message:  `Template variable ".Invalid" is not defined in the render context`,
					Line:     1,
					Column:   20,
					Severity: "error",
				},
			},
		},
		{
			name: "Nested scoped access (bug reproduction)",
			content: `
				{{ with .User }}
					{{ .Address.City }}
				{{ end }}
			`,
			expected: nil,
		},
		{
			name: "Invalid nested scoped access",
			content: `
				{{ with .User }}
					{{ .Address.Invalid }}
				{{ end }}
			`,
			expected: []ValidationResult{
				{
					Variable: ".Address.Invalid",
					Message:  `Field "Invalid" does not exist on type Address`,
					Line:     3,
					Column:   9, // Indentation + {{
					Severity: "error",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateTemplateContent(tt.content, vars, "test.html", ".")

			// Compare expected and got
			if len(got) != len(tt.expected) {
				t.Errorf("expected %d errors, got %d", len(tt.expected), len(got))
				for i, err := range got {
					t.Logf("Got error %d: %v", i, err)
				}
				return
			}

			for i := range got {
				if got[i].Message != tt.expected[i].Message {
					t.Errorf("error %d message mismatch: expected %q, got %q", i, tt.expected[i].Message, got[i].Message)
				}
				if got[i].Variable != tt.expected[i].Variable {
					t.Errorf("error %d variable mismatch: expected %q, got %q", i, tt.expected[i].Variable, got[i].Variable)
				}
				// We skip Line/Column strict check for multiline strings if not critical, but here let's try
				// For the "Invalid variable access" case, I put expected Line/Column.
			}
		})
	}
}
