package validator_test

import (
	"testing"

	"analyzer/ast"
	"analyzer/validator"
)

// nestedBlockVars mirrors the context for the "prescription-summary" block test.
// Contains: Title, prescriptions (slice of Prescription), billedDrugs (slice of Drug),
// roles (map[string]string), visit (with Patient.Name).
var nestedBlockVars = map[string]ast.TemplateVar{
	"Title": {
		Name:    "Title",
		TypeStr: "string",
	},
	"prescriptions": {
		Name:     "prescriptions",
		TypeStr:  "[]handlers.Prescription",
		IsSlice:  true,
		ElemType: "handlers.Prescription",
		Fields: []ast.FieldInfo{
			{
				Name:    "Drug",
				TypeStr: "handlers.Drug",
				Fields: []ast.FieldInfo{
					{Name: "Name", TypeStr: "string"},
					{Name: "Quantity", TypeStr: "int"},
					{Name: "Price", TypeStr: "float64"},
				},
			},
			{Name: "DrugName", TypeStr: "string"},
			{Name: "Quantity", TypeStr: "int"},
			{Name: "Dosage", TypeStr: "string"},
		},
	},
	"billedDrugs": {
		Name:     "billedDrugs",
		TypeStr:  "[]handlers.Drug",
		IsSlice:  true,
		ElemType: "handlers.Drug",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{Name: "Quantity", TypeStr: "int"},
			{Name: "Price", TypeStr: "float64"},
		},
	},
	"roles": {
		Name:     "roles",
		TypeStr:  "map[string]string",
		IsMap:    true,
		KeyType:  "string",
		ElemType: "string",
	},
	"visit": {
		Name:    "visit",
		TypeStr: "*handlers.Visit",
		Fields: []ast.FieldInfo{
			{Name: "ID", TypeStr: "uint"},
			{
				Name:    "Patient",
				TypeStr: "handlers.Patient",
				Fields: []ast.FieldInfo{
					{Name: "Name", TypeStr: "string"},
					{Name: "ID", TypeStr: "uint"},
				},
			},
		},
	},
}

var nestedBlockFuncMaps = validator.FuncMapRegistry{
	"dict": ast.FuncMapInfo{
		Name:    "dict",
		Args:    []string{"[]any"},
		Returns: []ast.ParamInfo{{TypeStr: "map[string]any"}, {TypeStr: "error"}},
	},
}

// ---------------------------------------------------------------------------
// Tests for the full complex nested block template
// ---------------------------------------------------------------------------

// TestNestedBlockFullPrescriptionSummary validates the complete prescription-summary
// block with all nested scopes: range > with > if > else, $-access, local vars,
// reassignment, nested range, and else-range fallback.
func TestNestedBlockFullPrescriptionSummary(t *testing.T) {
	content := `
    {{ block "prescription-summary" . }}
    <section class="prescription-summary">
        <h2>{{ $.Title }} — Prescription Summary</h2>

        {{ range .prescriptions }}
        {{ $rx := . }}

        {{ with .Drug }}
        {{ if .Name }}
        <div class="rx-card">
            <h3>{{ .Name }}</h3>
            <p>Quantity: {{ $rx.Quantity }}</p>
            <p>Dosage: {{ $rx.Dosage }}</p>

            {{ $matched := false }}
            {{ range $.billedDrugs }}
            {{ if eq .Name $rx.DrugName }}
            {{ $matched = true }}
            <p class="billed">
                Billed: {{ .Price }} x {{ .Quantity }}
            </p>
            {{ end }}
            {{ end }}

            {{ if $matched }}
            <span class="badge billed">Billed</span>
            {{ else }}
            <span class="badge unbilled">Not Yet Billed</span>
            {{ end }}

            {{ with $.roles }}
            {{ $admin := index . "admin" }}
            {{ if $admin }}
            <p class="admin-only">
                Unit price visible to admins only: {{ $rx.Drug.Price }}
            </p>
            {{ else }}
            <p class="restricted">Price restricted.</p>
            {{ end }}
            {{ end }}
        </div>
        {{ else }}
        <div class="rx-card unnamed">
            <p>Unnamed drug — Qty: {{ $rx.Quantity }}</p>
        </div>
        {{ end }}
        {{ end }}
        {{ else }}
        <p class="empty">No prescriptions recorded for {{ $.visit.Patient.Name }}.</p>
        {{ end }}
    </section>
    {{ end }}
`

	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestLocalVarVisibleInNestedScope verifies that $var declared in an outer scope
// is accessible inside a nested range/with/if scope.
func TestLocalVarVisibleInNestedScope(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ $rx := . }}
{{ with .Drug }}
<span>{{ $rx.Quantity }}</span>
<span>{{ $rx.Dosage }}</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestLocalVarVisibleInNestedIf verifies $var is visible inside nested if blocks.
func TestLocalVarVisibleInNestedIf(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ $rx := . }}
{{ if .DrugName }}
<span>{{ $rx.Quantity }}</span>
{{ else }}
<span>{{ $rx.Dosage }}</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestLocalVarReassignment verifies that $var = expr (reassignment without :=) works.
func TestLocalVarReassignment(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ $matched := false }}
{{ range $.billedDrugs }}
{{ if eq .Name "aspirin" }}
{{ $matched = true }}
{{ end }}
{{ end }}
{{ if $matched }}
<span>Billed</span>
{{ else }}
<span>Not Billed</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestDollarAccessInNestedRange verifies $.field works inside nested range.
func TestDollarAccessInNestedRange(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ range $.billedDrugs }}
<span>{{ $.Title }}</span>
<span>{{ .Name }}</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestElseRangeFallback verifies the {{ else }} branch of a {{ range }} works
// and that $.visit.Patient.Name resolves correctly in that branch.
func TestElseRangeFallback(t *testing.T) {
	content := `
{{ range .prescriptions }}
<span>{{ .DrugName }}</span>
{{ else }}
<p>No prescriptions for {{ $.visit.Patient.Name }}.</p>
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestElseIfChain validates else-if chains work correctly.
func TestElseIfChain(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ $rx := . }}
{{ if .DrugName }}
<span>{{ .DrugName }}</span>
{{ else if $rx.Dosage }}
<span>{{ $rx.Dosage }}</span>
{{ else }}
<span>Unknown</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestLocalVarNestedFieldAccess validates $rx.Drug.Price (multi-level field access on local var).
func TestLocalVarNestedFieldAccess(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ $rx := . }}
{{ with $.roles }}
<span>{{ $rx.Drug.Price }}</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestLocalVarInElseBranch validates that $rx is accessible in an else branch.
func TestLocalVarInElseBranch(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ $rx := . }}
{{ with .Drug }}
{{ if .Name }}
<span>{{ .Name }}</span>
{{ else }}
<span>{{ $rx.Quantity }}</span>
{{ end }}
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestIndexOnMapInNestedBlock validates {{ $admin := index . "admin" }} inside with.
func TestIndexOnMapInNestedBlock(t *testing.T) {
	content := `
{{ with .roles }}
{{ $admin := index . "admin" }}
{{ if $admin }}
<span>Admin</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	for _, e := range errs {
		t.Logf("Error: line %d col %d: %s (variable: %s)", e.Line, e.Column, e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestInvalidVarInNestedScope ensures we still catch truly undefined vars.
func TestInvalidVarInNestedScope(t *testing.T) {
	content := `
{{ range .prescriptions }}
{{ with .Drug }}
<span>{{ $nonexistent.Foo }}</span>
{{ end }}
{{ end }}
`
	errs := validator.ValidateTemplateContent(content, nestedBlockVars, "test.html", ".", ".", 1, nil, nestedBlockFuncMaps)
	if len(errs) == 0 {
		t.Errorf("Expected at least 1 error for $nonexistent, got 0")
	}
}
