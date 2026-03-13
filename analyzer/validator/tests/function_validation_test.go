package validator_test

import (
	"testing"

	"analyzer/ast"
	"analyzer/validator"
)

func TestUnknownTemplateFunctionIsReported(t *testing.T) {
	content := `{{ $authUser := getAuthUsers 1 }}`
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "funcs.html", ".", ".", 1, nil, validator.BuildFuncMapRegistry(nil))
	if len(errList) == 0 {
		t.Fatal("expected validation error for unknown template function")
	}
	if errList[0].Variable != "getAuthUsers" {
		t.Fatalf("expected unknown function error for getAuthUsers, got %q", errList[0].Variable)
	}
}

func TestFunctionReturnCreatesLocalScope(t *testing.T) {
	content := `
		{{ $authUser := getAuthUser 1 }}
		{{ $authUser.Name }}
		{{ $authUser.Names }}
	`

	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildAuthUserFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "funcs.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 validation error, got %d: %#v", len(errList), errList)
	}
	if errList[0].Variable != "$authUser.Names" {
		t.Fatalf("expected invalid field error for $authUser.Names, got %q", errList[0].Variable)
	}
}

func TestCallBuiltinUsesFuncMapReturnScope(t *testing.T) {
	content := `
		{{ $authUser := call getAuthUser 1 }}
		{{ $authUser.Names }}
	`

	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildAuthUserFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "funcs.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 validation error, got %d: %#v", len(errList), errList)
	}
	if errList[0].Variable != "$authUser.Names" {
		t.Fatalf("expected invalid field error for $authUser.Names, got %q", errList[0].Variable)
	}
}

func TestExpressionParserBuiltinsAreNotReportedAsUnknownFunctions(t *testing.T) {
	content := `
		{{ $value := add 1 2 }}
		{{ dict "count" $value "total" (mul 2 3) }}
		{{ sub 3 1 }}
		{{ div 12 3 }}
		{{ mod 7 3 }}
	`

	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "funcs.html", ".", ".", 1, nil, validator.BuildFuncMapRegistry(nil))
	if len(errList) != 0 {
		t.Fatalf("expected builtins to be accepted without FuncMap entries, got %#v", errList)
	}
}

func buildAuthUserFuncMap() ast.FuncMapInfo {
	return ast.FuncMapInfo{
		Name: "getAuthUser",
		Returns: []ast.ParamInfo{{
			TypeStr: "*User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		}},
		ReturnTypeFields: []ast.FieldInfo{
			{Name: "Name", TypeStr: "string"},
		},
	}
}
