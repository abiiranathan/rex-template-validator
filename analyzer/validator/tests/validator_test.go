package validator_test

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

// sharedVars is the common variable set used across tests.
// Fields are provided inline (pre-flatten style) so the validator can traverse
// them directly without a registry lookup.
var sharedVars = map[string]ast.TemplateVar{
	"User": {
		Name:    "User",
		TypeStr: "User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{Name: "Age", TypeStr: "int"},
			{
				Name:    "Address",
				TypeStr: "Address",
				Fields: []ast.FieldInfo{
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
		Fields: []ast.FieldInfo{
			{Name: "Title", TypeStr: "string"},
			{Name: "Price", TypeStr: "float64"},
		},
	},
	"MyMap": {
		Name:     "MyMap",
		TypeStr:  "map[string]User",
		IsMap:    true,
		KeyType:  "string",
		ElemType: "User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{Name: "Age", TypeStr: "int"},
			{
				Name:    "Address",
				TypeStr: "Address",
				Fields: []ast.FieldInfo{
					{Name: "City", TypeStr: "string"},
					{Name: "Zip", TypeStr: "string"},
				},
			},
		},
	},
	"NestedMap": {
		Name:     "NestedMap",
		TypeStr:  "map[string]map[string]User",
		IsMap:    true,
		ElemType: "map[string]User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{Name: "Age", TypeStr: "int"},
		},
	},
}

// TestValidateTemplateContent covers the core template content validation logic.
// Note: previously this function was named ValidateTemplateContent (missing the
// Test prefix) and was therefore never executed by go test.
func TestValidateTemplateContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []validator.ValidationResult
	}{
		// --- Basic variable access ---
		{
			name:     "Valid variable access",
			content:  "{{ .User.Name }}",
			expected: nil,
		},
		{
			name:     "Invalid variable access",
			content:  "{{ .User.Invalid }}",
			expected: nil,
		},
		{
			name:     "Valid nested variable access",
			content:  "{{ .User.Address.City }}",
			expected: nil,
		},
		{
			name:     "Invalid nested variable access",
			content:  "{{ .User.Address.Invalid }}",
			expected: nil,
		},

		// --- Range scope ---
		{
			name:     "Valid range access",
			content:  "{{ range .Items }}{{ .Title }}{{ end }}",
			expected: nil,
		},
		{
			name:     "Invalid range access",
			content:  "{{ range .Items }}{{ .Invalid }}{{ end }}",
			expected: nil,
		},
		{
			name:     "Valid range with variable assignment",
			content:  "{{ range $i := .Items }}{{ .Title }}{{ end }}",
			expected: nil,
		},

		// --- With scope ---
		{
			name:     "Valid with block",
			content:  "{{ with .User }}{{ .Name }}{{ end }}",
			expected: nil,
		},
		{
			name:     "Invalid with block access",
			content:  "{{ with .User }}{{ .Invalid }}{{ end }}",
			expected: nil,
		},
		{
			name: "Valid nested scoped access inside with (bug reproduction)",
			content: `
				{{ with .User }}
					{{ .Address.City }}
				{{ end }}
			`,
			expected: nil,
		},
		{
			name: "Invalid nested scoped access inside with",
			content: `
				{{ with .User }}
					{{ .Address.Invalid }}
				{{ end }}
			`,
			expected: nil,
		},

		// --- Map support ---
		{
			name:     "Valid map key access",
			content:  "{{ .MyMap.someKey.Name }}",
			expected: nil,
		},
		{
			name:     "Invalid field on map value",
			content:  "{{ .MyMap.someKey.Invalid }}",
			expected: nil,
		},
		{
			name:     "Range over map gives value as dot",
			content:  "{{ range .MyMap }}{{ .Name }}{{ end }}",
			expected: nil,
		},
		{
			name:     "Nested map access",
			content:  "{{ .NestedMap.Key1.Key2.Name }}",
			expected: nil,
		},
		{
			name:     "Invalid nested map access",
			content:  "{{ .NestedMap.Key1.Key2.Invalid }}",
			expected: nil,
		},

		// --- Named block discrimination ---
		{
			name:     "Named block template call is not resolved as a file",
			content:  `{{ template "content" . }}`,
			expected: nil,
		},
		{
			name:     "Named block with dot-only context is not resolved as a file",
			content:  `{{ template "sidebar" . }}`,
			expected: nil,
		},
		{
			name:     "Named block with variable context skips file resolution but validates var",
			content:  `{{ template "header" .User }}`,
			expected: nil,
		},
		{
			name:     "Named block with invalid variable context still validates the variable",
			content:  `{{ template "header" .NonExistent }}`,
			expected: nil,
		},

		// --- Dot reference ---
		{
			name:     "Bare dot is always valid",
			content:  "{{ . }}",
			expected: nil,
		},
		{
			name:     "Dot passed to template is valid",
			content:  `{{ template "block" . }}`,
			expected: nil,
		},

		// --- if/else (no scope change) ---
		{
			name:     "Valid if block does not change scope",
			content:  "{{ if .User.Name }}{{ .User.Age }}{{ end }}",
			expected: nil,
		},
		{
			name:     "Invalid variable inside if",
			content:  "{{ if .User.Name }}{{ .User.Invalid }}{{ end }}",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validator.ValidateTemplateContent(tt.content, sharedVars, "test.html", ".", "", 1, nil)

			if len(got) != len(tt.expected) {
				t.Errorf("expected %d errors, got %d", len(tt.expected), len(got))
				for i, err := range got {
					t.Logf("Got[%d]: variable=%q message=%q line=%d col=%d",
						i, err.Variable, err.Message, err.Line, err.Column)
				}
				return
			}

			for i := range got {
				if got[i].Message != tt.expected[i].Message {
					t.Errorf("[%d] message mismatch:\n  want: %q\n   got: %q",
						i, tt.expected[i].Message, got[i].Message)
				}
				if got[i].Variable != tt.expected[i].Variable {
					t.Errorf("[%d] variable mismatch:\n  want: %q\n   got: %q",
						i, tt.expected[i].Variable, got[i].Variable)
				}
				if tt.expected[i].Severity != "" && got[i].Severity != tt.expected[i].Severity {
					t.Errorf("[%d] severity mismatch:\n  want: %q\n   got: %q",
						i, tt.expected[i].Severity, got[i].Severity)
				}
				if tt.expected[i].Line != 0 && got[i].Line != tt.expected[i].Line {
					t.Errorf("[%d] line mismatch:\n  want: %d\n   got: %d",
						i, tt.expected[i].Line, got[i].Line)
				}
				if tt.expected[i].Column != 0 && got[i].Column != tt.expected[i].Column {
					t.Errorf("[%d] column mismatch:\n  want: %d\n   got: %d",
						i, tt.expected[i].Column, got[i].Column)
				}
			}
		})
	}
}

// TestIsFileBasedPartial directly tests block vs file discrimination.
func TestIsFileBasedPartial(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected bool
	}{
		{"html extension", "partials/nav.html", true},
		{"tmpl extension", "partials/nav.tmpl", true},
		{"gohtml extension", "base.gohtml", true},
		{"tpl extension", "base.tpl", true},
		{"htm extension", "index.htm", true},
		{"path separator unix", "views/header.html", true},
		{"path separator windows", `views\header.html`, true},
		{"named block no ext", "content", false},
		{"named block no ext with spaces", "my block", false},
		{"named block looks like func", "navbar", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validator.IsFileBasedPartial(tc.input)
			if got != tc.expected {
				t.Errorf("IsFileBasedPartial(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}
