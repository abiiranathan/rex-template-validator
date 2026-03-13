package validator_test

import (
	"strings"
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
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
		name            string
		content         string
		wantErr         bool
		wantVariable    string
		wantMessagePart string
	}{
		// --- Basic variable access ---
		{
			name:    "Valid variable access",
			content: "{{ .User.Name }}",
		},
		{
			name:            "Invalid variable access",
			content:         "{{ .User.Invalid }}",
			wantErr:         true,
			wantVariable:    ".User.Invalid",
			wantMessagePart: "not defined",
		},
		{
			name:    "Valid nested variable access",
			content: "{{ .User.Address.City }}",
		},
		{
			name:            "Invalid nested variable access",
			content:         "{{ .User.Address.Invalid }}",
			wantErr:         true,
			wantVariable:    ".User.Address.Invalid",
			wantMessagePart: "not defined",
		},

		// --- Range scope ---
		{
			name:    "Valid range access",
			content: "{{ range .Items }}{{ .Title }}{{ end }}",
		},
		{
			name:            "Invalid range access",
			content:         "{{ range .Items }}{{ .Invalid }}{{ end }}",
			wantErr:         true,
			wantVariable:    ".Invalid",
			wantMessagePart: "not defined",
		},
		{
			name:    "Valid range with variable assignment",
			content: "{{ range $i := .Items }}{{ .Title }}{{ end }}",
		},

		// --- With scope ---
		{
			name:    "Valid with block",
			content: "{{ with .User }}{{ .Name }}{{ end }}",
		},
		{
			name:            "Invalid with block access",
			content:         "{{ with .User }}{{ .Invalid }}{{ end }}",
			wantErr:         true,
			wantVariable:    ".Invalid",
			wantMessagePart: "not defined",
		},
		{
			name: "Valid nested scoped access inside with (bug reproduction)",
			content: `
				{{ with .User }}
					{{ .Address.City }}
				{{ end }}
			`,
		},
		{
			name: "Invalid nested scoped access inside with",
			content: `
				{{ with .User }}
					{{ .Address.Invalid }}
				{{ end }}
			`,
			wantErr:         true,
			wantVariable:    ".Address.Invalid",
			wantMessagePart: "not defined",
		},

		// --- Map support ---
		{
			name:    "Valid map key access",
			content: "{{ .MyMap.someKey.Name }}",
		},
		{
			name:            "Invalid field on map value",
			content:         "{{ .MyMap.someKey.Invalid }}",
			wantErr:         true,
			wantVariable:    ".MyMap.someKey.Invalid",
			wantMessagePart: "not defined",
		},
		{
			name:    "Range over map gives value as dot",
			content: "{{ range .MyMap }}{{ .Name }}{{ end }}",
		},
		{
			name:    "Nested map access",
			content: "{{ .NestedMap.Key1.Key2.Name }}",
		},
		{
			name:            "Invalid nested map access",
			content:         "{{ .NestedMap.Key1.Key2.Invalid }}",
			wantErr:         true,
			wantVariable:    ".NestedMap.Key1.Key2.Invalid",
			wantMessagePart: "not defined",
		},

		// --- Named block discrimination ---
		{
			name:    "Named block template call is not resolved as a file",
			content: `{{ template "content" . }}`,
		},
		{
			name:    "Named block with dot-only context is not resolved as a file",
			content: `{{ template "sidebar" . }}`,
		},
		{
			name:    "Named block with variable context skips file resolution but validates var",
			content: `{{ template "header" .User }}`,
		},
		{
			name:            "Named block with invalid variable context still validates the variable",
			content:         `{{ template "header" .NonExistent }}`,
			wantErr:         true,
			wantVariable:    ".NonExistent",
			wantMessagePart: "not defined",
		},

		// --- Dot reference ---
		{
			name:    "Bare dot is always valid",
			content: "{{ . }}",
		},
		{
			name:    "Dot passed to template is valid",
			content: `{{ template "block" . }}`,
		},

		// --- if/else (no scope change) ---
		{
			name:    "Valid if block does not change scope",
			content: "{{ if .User.Name }}{{ .User.Age }}{{ end }}",
		},
		{
			name:            "Invalid variable inside if",
			content:         "{{ if .User.Name }}{{ .User.Invalid }}{{ end }}",
			wantErr:         true,
			wantVariable:    ".User.Invalid",
			wantMessagePart: "not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validator.ValidateTemplateContent(tt.content, sharedVars, "test.html", ".", "", 1, nil)

			if tt.wantErr {
				if len(got) == 0 {
					t.Fatalf("expected validation error, got none")
				}
				if tt.wantVariable != "" && got[0].Variable != tt.wantVariable {
					t.Fatalf("expected variable %q, got %q", tt.wantVariable, got[0].Variable)
				}
				if tt.wantMessagePart != "" && !contains(got[0].Message, tt.wantMessagePart) {
					t.Fatalf("expected message %q to contain %q", got[0].Message, tt.wantMessagePart)
				}
				return
			}

			if len(got) != 0 {
				t.Fatalf("expected no validation errors, got %#v", got)
			}
		})
	}
}

func TestValidateTemplateContentValidatesLocalNamedTemplateBodies(t *testing.T) {
	content := `
		{{ template "header" .User }}
		{{ define "header" }}
			{{ .Missing }}
		{{ end }}
	`

	got := validator.ValidateTemplateContent(content, sharedVars, "test.html", ".", "", 1, nil)
	if len(got) == 0 {
		t.Fatal("expected validation error for missing variable inside local define body")
	}
	if got[0].Variable != ".Missing" {
		t.Fatalf("expected missing variable .Missing, got %q", got[0].Variable)
	}
	if !contains(got[0].Message, "named template \"header\"") {
		t.Fatalf("expected pinned named template message, got %q", got[0].Message)
	}
}

func TestValidateTemplateContentValidatesBlockBodiesViaTemplateCallContext(t *testing.T) {
	content := `
		{{ template "header" .User }}
		{{ block "header" .User }}
			{{ .Missing }}
		{{ end }}
	`

	got := validator.ValidateTemplateContent(content, sharedVars, "test.html", ".", "", 1, nil)
	if len(got) == 0 {
		t.Fatal("expected validation error for missing variable inside block body reached through template call")
	}
	if got[0].Variable != ".Missing" {
		t.Fatalf("expected missing variable .Missing, got %q", got[0].Variable)
	}
	if !contains(got[0].Message, "named template \"header\"") {
		t.Fatalf("expected pinned named template message, got %q", got[0].Message)
	}
}

func contains(value, fragment string) bool {
	return strings.Contains(value, fragment)
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
