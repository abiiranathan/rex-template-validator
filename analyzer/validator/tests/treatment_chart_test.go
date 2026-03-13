package validator_test

import (
	"testing"

	"analyzer/ast"
	"analyzer/validator"
)

// treatmentChartVars mirrors the variables passed from handler.go's RenderTreatmentChart
// to the treatment-chart.html template.
var treatmentChartVars = map[string]ast.TemplateVar{
	"management": {
		Name:     "management",
		TypeStr:  "[]handlers.Management",
		IsSlice:  true,
		ElemType: "handlers.Management",
		Fields: []ast.FieldInfo{
			{
				Name:    "Prescription",
				TypeStr: "handlers.Prescription",
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
			{
				Name:    "AsPresc",
				TypeStr: "method",
				Returns: []ast.ParamInfo{
					{
						TypeStr: "handlers.Prescription",
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
				},
			},
		},
	},
	"visit": {
		Name:    "visit",
		TypeStr: "*handlers.Visit",
		Fields: []ast.FieldInfo{
			{Name: "ID", TypeStr: "uint"},
			{Name: "PatientID", TypeStr: "uint"},
			{
				Name:    "Patient",
				TypeStr: "handlers.Patient",
				Fields: []ast.FieldInfo{
					{Name: "Name", TypeStr: "string"},
					{Name: "ID", TypeStr: "uint"},
				},
			},
			{
				Name:    "Doctor",
				TypeStr: "handlers.Doctor",
				Fields: []ast.FieldInfo{
					{Name: "DisplayName", TypeStr: "string"},
					{Name: "ID", TypeStr: "uint"},
				},
			},
			{Name: "CreatedAt", TypeStr: "time.Time"},
		},
	},
	"Title": {
		Name:    "Title",
		TypeStr: "string",
	},
	"newuser": {
		Name:    "newuser",
		TypeStr: "*handlers.User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{
				Name:    "Permission",
				TypeStr: "handlers.Permission",
			},
		},
	},
	"data": {
		Name:     "data",
		TypeStr:  "map[uint][]*handlers.User",
		IsMap:    true,
		KeyType:  "uint",
		ElemType: "[]*handlers.User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{
				Name:    "Permission",
				TypeStr: "handlers.Permission",
			},
		},
	},
	"PathPrefix": {
		Name:    "PathPrefix",
		TypeStr: "string",
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
	"doctor": {
		Name:    "doctor",
		TypeStr: "string",
	},
	"roles": {
		Name:     "roles",
		TypeStr:  "map[string]string",
		IsMap:    true,
		KeyType:  "string",
		ElemType: "string",
	},
	"breadcrumbs": {
		Name:     "breadcrumbs",
		TypeStr:  "handlers.Breadcrumbs",
		IsSlice:  true,
		ElemType: "handlers.Breadcrumb",
		Fields: []ast.FieldInfo{
			{Name: "Label", TypeStr: "string"},
			{Name: "URL", TypeStr: "string"},
			{Name: "IsLast", TypeStr: "bool"},
		},
	},
	"currentUser": {
		Name:    "currentUser",
		TypeStr: "*handlers.User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{
				Name:    "Permission",
				TypeStr: "handlers.Permission",
			},
		},
	},
	"user": {
		Name:    "user",
		TypeStr: "*handlers.User",
		Fields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
			{
				Name:    "Permission",
				TypeStr: "handlers.Permission",
				Fields: []ast.FieldInfo{
					{
						Name:    "String",
						TypeStr: "method",
						Returns: []ast.ParamInfo{{TypeStr: "string"}},
					},
				},
			},
		},
	},
	"appVersion": {
		Name:    "appVersion",
		TypeStr: "string",
	},
}

var treatmentChartFuncMaps = validator.FuncMapRegistry{
	"getAuthUser": ast.FuncMapInfo{
		Name: "getAuthUser",
		Args: []string{"int"},
		Returns: []ast.ParamInfo{
			{
				TypeStr: "*handlers.User",
				Fields: []ast.FieldInfo{
					{Name: "Name", TypeStr: "string"},
					{
						Name:    "Permission",
						TypeStr: "handlers.Permission",
					},
				},
			},
		},
	},
	"dict": ast.FuncMapInfo{
		Name:    "dict",
		Args:    []string{"[]any"},
		Returns: []ast.ParamInfo{{TypeStr: "map[string]any"}, {TypeStr: "error"}},
	},
	"upper": ast.FuncMapInfo{
		Name:    "upper",
		Args:    []string{"string"},
		Returns: []ast.ParamInfo{{TypeStr: "string"}},
	},
}

// TestTreatmentChartBreadcrumbs tests the breadcrumbs range loop
func TestTreatmentChartBreadcrumbs(t *testing.T) {
	content := `
	{{ range .breadcrumbs }}
	<a href="{{ .URL }}">{{ .Label }}</a>
	{{ if not .IsLast }} / {{ end }}
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartManagementRange tests range over management with $.Title access
func TestTreatmentChartManagementRange(t *testing.T) {
	content := `
	{{ range .management }}
	<div>{{ .Prescription.Drug.Name }}</div>
	{{ $.Title }}
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartPrescriptionsRange tests range with local variable assignment
func TestTreatmentChartPrescriptionsRange(t *testing.T) {
	content := `
	{{ range .prescriptions }}
	<div class="prescription">
		{{ $name := .DrugName }}
		<span>{{ $name }}</span>
		<span>{{ .Quantity }}</span>
		<span>{{ .Dosage }}</span>
	</div>
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartBilledDrugsBlock tests the billed-drug named block called from range
func TestTreatmentChartBilledDrugsBlock(t *testing.T) {
	content := `
	{{ range .billedDrugs }}
	{{ $title := $.Title }}
	{{ template "billed-drug" . }}
	{{ end }}
	<a href="{{ .PathPrefix }}/new">New Entry</a>
	{{ block "billed-drug" . }}
	<div>{{ .Name }}</div>
	<div>{{ .Price }}</div>
	<div>{{ .Quantity }}</div>
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartUserMsgBlock tests the user-msg block with .currentUser context
func TestTreatmentChartUserMsgBlock(t *testing.T) {
	content := `
	{{ block "user-msg" .currentUser }}
	<h1>Hello: {{ .Name }}</h1>
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartIndexExpression tests index expression and field access
func TestTreatmentChartIndexExpression(t *testing.T) {
	content := `
	{{ $large := index $.billedDrugs 0 }}
	{{ $drugName := $large.Name }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartFuncMapCall tests getAuthUser function call
func TestTreatmentChartFuncMapCall(t *testing.T) {
	content := `
	{{ $authUser := getAuthUser 1 }}
	{{ $authUser.Name }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartPrescriptionSummaryBlock tests the complex nested prescription-summary block
func TestTreatmentChartPrescriptionSummaryBlock(t *testing.T) {
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
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartElseIfElseWith tests else if / else with chains
func TestTreatmentChartElseIfElseWith(t *testing.T) {
	content := `
	{{ if .Title}}
	<p>Title: {{ .Title }}</p>
	{{ else if .appVersion }}
	<p>App version:{{ .appVersion }}</p>
	{{ else with .billedDrugs }}
	{{ range . }}
	<p>Drug: {{ .Name }}</p>
	{{ end }}
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartMethodCall tests method call: .management.AsPresc and .user.Permission.String
func TestTreatmentChartMethodCall(t *testing.T) {
	content := `
	{{ $pres := .management.AsPresc }}
	{{ $pres.DrugName }}
	{{ .user.Permission.String }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartNestedIndexInRange tests index with nested access inside range
func TestTreatmentChartNestedIndexInRange(t *testing.T) {
	content := `
	{{ range .billedDrugs }}
	{{ $userData := index $.data 10 }}
	{{ $firstRecord := index $userData 0 }}
	{{ $firstRecord.Name }}
	{{ range $userData }}
	<p>{{ .Name }}</p>
	{{ end }}
	{{ end }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}

// TestTreatmentChartContextFileVars tests context-file variables ($.newuser.Name, $.currentUser)
func TestTreatmentChartContextFileVars(t *testing.T) {
	content := `
	{{ $.newuser.Name }}
	{{ $.currentUser }}
	{{ .user.Permission.String }}
	`
	errs := validator.ValidateTemplateContent(content, treatmentChartVars, "treatment-chart.html", ".", ".", 1, nil, treatmentChartFuncMaps)
	for _, e := range errs {
		t.Logf("Error: %s (variable: %s)", e.Message, e.Variable)
	}
	if len(errs) != 0 {
		t.Errorf("Expected 0 errors, got %d", len(errs))
	}
}
