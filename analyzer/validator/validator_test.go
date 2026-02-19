package validator

import (
	"os"
	"path/filepath"
	"testing"
)

// sharedVars is the common variable set used across tests
var sharedVars = map[string]TemplateVar{
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
		Fields: []FieldInfo{
			{Name: "Title", TypeStr: "string"},
			{Name: "Price", TypeStr: "float64"},
		},
	},
}

func TestValidateTemplateContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []ValidationResult
	}{
		// --- Basic variable access ---
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
					Column:   4,
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

		// --- Range scope ---
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
			expected: []ValidationResult{
				{
					Variable: ".Address.Invalid",
					Message:  `Field "Invalid" does not exist on type Address`,
					Line:     3,
					Column:   9,
					Severity: "error",
				},
			},
		},

		// --- FIX 1: Named block vs file partial discrimination ---
		{
			name: "Named block template call is not resolved as a file",
			// "content" is a named block (no extension, no path sep) — must not trigger file-not-found
			content:  `{{ template "content" . }}`,
			expected: nil, // no error: named blocks are skipped for file resolution
		},
		{
			name:     "Named block with dot-only context is not resolved as a file",
			content:  `{{ template "sidebar" . }}`,
			expected: nil,
		},
		{
			name:     "Named block with variable context skips file resolution but validates var",
			content:  `{{ template "header" .User }}`,
			expected: nil, // .User exists, no file resolution attempted
		},
		{
			name:    "Named block with invalid variable context still validates the variable",
			content: `{{ template "header" .NonExistent }}`,
			expected: []ValidationResult{
				{
					Variable: ".NonExistent",
					Message:  `Template variable ".NonExistent" is not defined in the render context`,
					Severity: "error",
				},
			},
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
			name:    "Invalid variable inside if",
			content: "{{ if .User.Name }}{{ .User.Invalid }}{{ end }}",
			expected: []ValidationResult{
				{
					Variable: ".User.Invalid",
					Message:  `Field "Invalid" does not exist on type User`,
					Severity: "error",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateTemplateContent(tt.content, sharedVars, "test.html", ".", "")

			if len(got) != len(tt.expected) {
				t.Errorf("expected %d errors, got %d", len(tt.expected), len(got))
				for i, err := range got {
					t.Logf("Got[%d]: variable=%q message=%q line=%d col=%d", i, err.Variable, err.Message, err.Line, err.Column)
				}
				return
			}

			for i := range got {
				if got[i].Message != tt.expected[i].Message {
					t.Errorf("[%d] message mismatch:\n  want: %q\n   got: %q", i, tt.expected[i].Message, got[i].Message)
				}
				if got[i].Variable != tt.expected[i].Variable {
					t.Errorf("[%d] variable mismatch:\n  want: %q\n   got: %q", i, tt.expected[i].Variable, got[i].Variable)
				}
				if tt.expected[i].Severity != "" && got[i].Severity != tt.expected[i].Severity {
					t.Errorf("[%d] severity mismatch:\n  want: %q\n   got: %q", i, tt.expected[i].Severity, got[i].Severity)
				}
				if tt.expected[i].Line != 0 && got[i].Line != tt.expected[i].Line {
					t.Errorf("[%d] line mismatch:\n  want: %d\n   got: %d", i, tt.expected[i].Line, got[i].Line)
				}
				if tt.expected[i].Column != 0 && got[i].Column != tt.expected[i].Column {
					t.Errorf("[%d] column mismatch:\n  want: %d\n   got: %d", i, tt.expected[i].Column, got[i].Column)
				}
			}
		})
	}
}

// TestIsFileBasedPartial directly tests the block vs file discrimination (Fix 1)
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
			got := isFileBasedPartial(tc.input)
			if got != tc.expected {
				t.Errorf("isFileBasedPartial(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

// TestPartialTemplateResolution tests Fix 2 (templateName in diagnostics) and
// Fix 3 (recursive partial validation with scope propagation).
func TestPartialTemplateResolution(t *testing.T) {
	// Create a temp directory that mimics a template root
	tmpDir := t.TempDir()
	templateRoot := "views"
	viewsDir := filepath.Join(tmpDir, templateRoot)
	if err := os.MkdirAll(viewsDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("Fix2: diagnostic templateName is relative not absolute", func(t *testing.T) {
		// Write a partial that accesses a non-existent field
		partialPath := filepath.Join(viewsDir, "partials", "user_card.html")
		if err := os.MkdirAll(filepath.Dir(partialPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(partialPath, []byte(`{{ .Name }}{{ .NonExistent }}`), 0644); err != nil {
			t.Fatal(err)
		}

		// Write parent template that includes the partial with .User scope
		parentPath := filepath.Join(viewsDir, "index.html")
		if err := os.WriteFile(parentPath, []byte(`{{ template "partials/user_card.html" .User }}`), 0644); err != nil {
			t.Fatal(err)
		}

		vars := []TemplateVar{sharedVars["User"]}
		errs := validateTemplateFile(parentPath, vars, "index.html", tmpDir, templateRoot)

		// We expect exactly 1 error from the partial (NonExistent field)
		if len(errs) != 1 {
			t.Errorf("expected 1 error, got %d", len(errs))
			for _, e := range errs {
				t.Logf("  error: template=%q variable=%q message=%q", e.Template, e.Variable, e.Message)
			}
			return
		}

		// Fix 2: Template name in error should be the relative partial name, not the OS path
		wantTemplate := "partials/user_card.html"
		if errs[0].Template != wantTemplate {
			t.Errorf("Fix2: template name in diagnostic = %q, want %q", errs[0].Template, wantTemplate)
		}
	})

	t.Run("Fix3: partial receives correct scope when called with .User", func(t *testing.T) {
		// Partial accesses fields of User directly (Name, Age, Address.City)
		partialPath := filepath.Join(viewsDir, "user_detail.html")
		if err := os.WriteFile(partialPath, []byte(`{{ .Name }} {{ .Age }} {{ .Address.City }}`), 0644); err != nil {
			t.Fatal(err)
		}

		parentPath := filepath.Join(viewsDir, "parent.html")
		if err := os.WriteFile(parentPath, []byte(`{{ template "user_detail.html" .User }}`), 0644); err != nil {
			t.Fatal(err)
		}

		vars := []TemplateVar{sharedVars["User"]}
		errs := validateTemplateFile(parentPath, vars, "parent.html", tmpDir, templateRoot)

		if len(errs) != 0 {
			t.Errorf("Fix3: expected no errors for valid partial scope, got %d", len(errs))
			for _, e := range errs {
				t.Logf("  error: template=%q variable=%q message=%q", e.Template, e.Variable, e.Message)
			}
		}
	})

	t.Run("Fix3: partial with . receives full root scope", func(t *testing.T) {
		// Partial accessed with . should see all root-level vars
		partialPath := filepath.Join(viewsDir, "full_ctx.html")
		if err := os.WriteFile(partialPath, []byte(`{{ .User.Name }} {{ .Items }}`), 0644); err != nil {
			t.Fatal(err)
		}

		parentPath := filepath.Join(viewsDir, "root_parent.html")
		if err := os.WriteFile(parentPath, []byte(`{{ template "full_ctx.html" . }}`), 0644); err != nil {
			t.Fatal(err)
		}

		vars := []TemplateVar{sharedVars["User"], sharedVars["Items"]}
		errs := validateTemplateFile(parentPath, vars, "root_parent.html", tmpDir, templateRoot)

		if len(errs) != 0 {
			t.Errorf("Fix3: expected no errors when partial receives full root scope, got %d", len(errs))
			for _, e := range errs {
				t.Logf("  error: template=%q variable=%q message=%q", e.Template, e.Variable, e.Message)
			}
		}
	})

	t.Run("Fix3: partial with invalid field access is caught", func(t *testing.T) {
		partialPath := filepath.Join(viewsDir, "bad_partial.html")
		if err := os.WriteFile(partialPath, []byte(`{{ .Name }} {{ .DoesNotExist }}`), 0644); err != nil {
			t.Fatal(err)
		}

		parentPath := filepath.Join(viewsDir, "bad_parent.html")
		if err := os.WriteFile(parentPath, []byte(`{{ template "bad_partial.html" .User }}`), 0644); err != nil {
			t.Fatal(err)
		}

		vars := []TemplateVar{sharedVars["User"]}
		errs := validateTemplateFile(parentPath, vars, "bad_parent.html", tmpDir, templateRoot)

		if len(errs) != 1 {
			t.Errorf("Fix3: expected 1 error for invalid field in partial, got %d", len(errs))
			for _, e := range errs {
				t.Logf("  error: template=%q variable=%q message=%q", e.Template, e.Variable, e.Message)
			}
			return
		}
		if errs[0].Template != "bad_partial.html" {
			t.Errorf("Fix2+3: error template should be %q, got %q", "bad_partial.html", errs[0].Template)
		}
	})

	t.Run("Fix1+Fix2: missing file partial reports error with relative name", func(t *testing.T) {
		parentPath := filepath.Join(viewsDir, "missing_parent.html")
		if err := os.WriteFile(parentPath, []byte(`{{ template "does_not_exist.html" . }}`), 0644); err != nil {
			t.Fatal(err)
		}

		vars := []TemplateVar{sharedVars["User"]}
		errs := validateTemplateFile(parentPath, vars, "missing_parent.html", tmpDir, templateRoot)

		if len(errs) != 1 {
			t.Errorf("expected 1 error for missing partial, got %d", len(errs))
			return
		}
		// Fix 2: The error Template should be the caller's name, not an absolute path
		if errs[0].Template != "missing_parent.html" {
			t.Errorf("Fix2: error.Template = %q, want %q", errs[0].Template, "missing_parent.html")
		}
		if errs[0].Variable != "does_not_exist.html" {
			t.Errorf("error.Variable should be the missing partial name, got %q", errs[0].Variable)
		}
	})
}
