package validator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

func hoverVarMap() map[string]ast.TemplateVar {
	return map[string]ast.TemplateVar{
		"Title": {Name: "Title", TypeStr: "string"},
		"prescriptions": {
			Name: "prescriptions", TypeStr: "[]handlers.Prescription",
			IsSlice: true, ElemType: "handlers.Prescription",
			Fields: []ast.FieldInfo{
				{Name: "DrugName", TypeStr: "string"},
				{Name: "Quantity", TypeStr: "int"},
				{Name: "Dosage", TypeStr: "string"},
				{Name: "Drug", TypeStr: "handlers.Drug", Fields: []ast.FieldInfo{
					{Name: "Name", TypeStr: "string"},
					{Name: "Quantity", TypeStr: "int"},
					{Name: "Price", TypeStr: "float64"},
				}},
			},
		},
		"billedDrugs": {
			Name: "billedDrugs", TypeStr: "[]handlers.Drug",
			IsSlice: true, ElemType: "handlers.Drug",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{Name: "Quantity", TypeStr: "int"},
				{Name: "Price", TypeStr: "float64"},
			},
		},
		"visit": {
			Name: "visit", TypeStr: "*handlers.Visit",
			Fields: []ast.FieldInfo{
				{Name: "Patient", TypeStr: "handlers.Patient", Fields: []ast.FieldInfo{
					{Name: "Name", TypeStr: "string"},
				}},
			},
		},
		"roles":       {Name: "roles", TypeStr: "map[string]string", IsMap: true, KeyType: "string", ElemType: "string"},
		"currentUser": {Name: "currentUser", TypeStr: "*handlers.User", Fields: []ast.FieldInfo{{Name: "Name", TypeStr: "string"}}},
	}
}

func hoverTypeRegistry() map[string][]ast.FieldInfo {
	return map[string][]ast.FieldInfo{
		"handlers.Prescription": {
			{Name: "DrugName", TypeStr: "string"},
			{Name: "Quantity", TypeStr: "int"},
			{Name: "Dosage", TypeStr: "string"},
			{Name: "Drug", TypeStr: "handlers.Drug"},
		},
		"handlers.Drug": {
			{Name: "Name", TypeStr: "string"},
			{Name: "Quantity", TypeStr: "int"},
			{Name: "Price", TypeStr: "float64"},
		},
		"handlers.Visit": {
			{Name: "Patient", TypeStr: "handlers.Patient"},
		},
		"handlers.Patient": {
			{Name: "Name", TypeStr: "string"},
		},
		"handlers.User": {
			{Name: "Name", TypeStr: "string"},
		},
	}
}

func TestGetHoverResult_InsideBlock_RangeVar(t *testing.T) {
	content := "{{ block \"prescription-summary\" . }}\n{{ range .prescriptions }}\n{{ $rx := . }}\n{{ .DrugName }}\n{{ end }}\n{{ end }}"

	result := validator.GetHoverResult(
		content, hoverVarMap(), "test.html", "", "",
		0, 4, 5,
		nil, nil, hoverTypeRegistry(),
	)
	if result == nil {
		t.Fatal("expected hover result, got nil")
	}
	t.Logf("Expression: %q -> type: %q", result.Expression, result.TypeStr)
	if result.TypeStr != "string" {
		t.Errorf("expected type 'string', got %q", result.TypeStr)
	}
}

func TestGetHoverResult_InsideBlock_WithScope(t *testing.T) {
	content := "{{ block \"prescription-summary\" . }}\n{{ range .prescriptions }}\n{{ with .Drug }}\n{{ .Name }}\n{{ end }}\n{{ end }}\n{{ end }}"

	result := validator.GetHoverResult(
		content, hoverVarMap(), "test.html", "", "",
		0, 4, 5,
		nil, nil, hoverTypeRegistry(),
	)
	if result == nil {
		t.Fatal("expected hover result, got nil")
	}
	t.Logf("Expression: %q -> type: %q", result.Expression, result.TypeStr)
	if result.TypeStr != "string" {
		t.Errorf("expected type 'string', got %q", result.TypeStr)
	}
}

func TestGetHoverResult_InsideBlock_LocalVar(t *testing.T) {
	content := "{{ block \"prescription-summary\" . }}\n{{ range .prescriptions }}\n{{ $rx := . }}\n{{ $rx.DrugName }}\n{{ end }}\n{{ end }}"

	result := validator.GetHoverResult(
		content, hoverVarMap(), "test.html", "", "",
		0, 4, 10, // col 10 = on "DrugName" in "$rx.DrugName"
		nil, nil, hoverTypeRegistry(),
	)
	if result == nil {
		t.Fatal("expected hover result, got nil")
	}
	t.Logf("Expression: %q -> type: %q", result.Expression, result.TypeStr)
	if result.TypeStr != "string" {
		t.Errorf("expected type 'string', got %q", result.TypeStr)
	}
}

func TestGetHoverResult_InsideBlock_DollarAccess(t *testing.T) {
	content := "{{ block \"prescription-summary\" . }}\n{{ range .prescriptions }}\n{{ $.Title }}\n{{ end }}\n{{ end }}"

	result := validator.GetHoverResult(
		content, hoverVarMap(), "test.html", "", "",
		0, 3, 5,
		nil, nil, hoverTypeRegistry(),
	)
	if result == nil {
		t.Fatal("expected hover result, got nil")
	}
	t.Logf("Expression: %q -> type: %q", result.Expression, result.TypeStr)
	if result.TypeStr != "string" {
		t.Errorf("expected type 'string', got %q", result.TypeStr)
	}
}

func TestGetHoverResult_InsideBlock_NestedRange(t *testing.T) {
	content := "{{ block \"prescription-summary\" . }}\n{{ range .prescriptions }}\n{{ $rx := . }}\n{{ range $.billedDrugs }}\n{{ .Price }}\n{{ end }}\n{{ end }}\n{{ end }}"

	result := validator.GetHoverResult(
		content, hoverVarMap(), "test.html", "", "",
		0, 5, 5,
		nil, nil, hoverTypeRegistry(),
	)
	if result == nil {
		t.Fatal("expected hover result, got nil")
	}
	t.Logf("Expression: %q -> type: %q", result.Expression, result.TypeStr)
	if result.TypeStr != "float64" {
		t.Errorf("expected type 'float64', got %q", result.TypeStr)
	}
}

func TestGetHoverResult_SubExpressionInIfClause(t *testing.T) {
	// Hover on ".Name" inside {{ if eq .Name $rx.DrugName }}
	// Line 5: {{ if eq .Name $rx.DrugName }}
	//          1234567890123
	//                   ^col 12 = on "N" of ".Name"
	content := "{{ block \"prescription-summary\" . }}\n{{ range .prescriptions }}\n{{ $rx := . }}\n{{ range $.billedDrugs }}\n{{ if eq .Name $rx.DrugName }}\nmatched\n{{ end }}\n{{ end }}\n{{ end }}\n{{ end }}"

	result := validator.GetHoverResult(
		content, hoverVarMap(), "test.html", "", "",
		0, 5, 12,
		nil, nil, hoverTypeRegistry(),
	)
	if result == nil {
		t.Fatal("expected hover result, got nil")
	}
	t.Logf("Expression: %q -> type: %q", result.Expression, result.TypeStr)
	// Should resolve .Name (sub-expression) to string, not the whole "eq .Name $rx.DrugName" to bool
	if result.TypeStr != "string" {
		t.Errorf("expected type 'string' for sub-expression .Name, got %q", result.TypeStr)
	}
}

func TestGetHoverResult_RealTreatmentChart(t *testing.T) {
	sampleDir := filepath.Join("..", "sample")
	htmlPath := filepath.Join(sampleDir, "templates", "views", "inpatient", "treatment-chart.html")
	contentBytes, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Skipf("sample file not found: %v", err)
	}

	content := string(contentBytes)
	varMap := hoverVarMap()
	typeReg := hoverTypeRegistry()

	// Line 86: <h3>{{ .Name }}</h3> inside range .prescriptions > with .Drug > if .Name
	result := validator.GetHoverResult(
		content, varMap, "treatment-chart.html", "", "",
		0, 86, 23,
		nil, nil, typeReg,
	)
	if result == nil {
		t.Fatal("expected hover result at line 86, got nil")
	}
	t.Logf("Line 86: expression=%q type=%q", result.Expression, result.TypeStr)
	if result.TypeStr != "string" {
		t.Errorf("line 86: expected type 'string', got %q", result.TypeStr)
	}

	// Line 87: <p>Quantity: {{ $rx.Quantity }}</p>
	result = validator.GetHoverResult(
		content, varMap, "treatment-chart.html", "", "",
		0, 87, 31,
		nil, nil, typeReg,
	)
	if result == nil {
		t.Fatal("expected hover result at line 87, got nil")
	}
	t.Logf("Line 87: expression=%q type=%q", result.Expression, result.TypeStr)
	if result.TypeStr != "int" {
		t.Errorf("line 87: expected type 'int', got %q", result.TypeStr)
	}

	// Line 76: <h2>{{ $.Title }} inside block "prescription-summary"
	result = validator.GetHoverResult(
		content, varMap, "treatment-chart.html", "", "",
		0, 76, 18,
		nil, nil, typeReg,
	)
	if result == nil {
		t.Fatal("expected hover result at line 76, got nil")
	}
	t.Logf("Line 76: expression=%q type=%q", result.Expression, result.TypeStr)
	if result.TypeStr != "string" {
		t.Errorf("line 76: expected type 'string', got %q", result.TypeStr)
	}
}
