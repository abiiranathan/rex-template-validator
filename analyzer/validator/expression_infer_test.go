package validator_test

import (
	"testing"

	"github.com/abiiranathan/gotpl-analyzer/ast"
	"github.com/abiiranathan/gotpl-analyzer/validator"
)

func TestInferExpressionTypeUsesFuncMapReturn(t *testing.T) {
	result := validator.InferExpressionType(
		"getAuthUser 1",
		map[string]ast.TemplateVar{},
		nil,
		nil,
		validator.BuildFuncMapRegistry([]ast.FuncMapInfo{{
			Name: "getAuthUser",
			Returns: []ast.ParamInfo{{
				TypeStr: "User",
			}},
		}}),
		map[string][]ast.FieldInfo{
			"User": {{Name: "Name", TypeStr: "string"}},
		},
	)

	if result == nil {
		t.Fatal("expected inferred type result")
	}
	if result.TypeStr != "User" {
		t.Fatalf("expected User, got %q", result.TypeStr)
	}
	if len(result.Fields) != 1 || result.Fields[0].Name != "Name" {
		t.Fatalf("expected hydrated User fields, got %#v", result.Fields)
	}
}

func TestInferExpressionTypeBuildsDictFields(t *testing.T) {
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}

	result := validator.InferExpressionType(
		`dict "user" .User "count" 1`,
		vars,
		nil,
		nil,
		nil,
		map[string][]ast.FieldInfo{
			"User": {{Name: "Name", TypeStr: "string"}},
		},
	)

	if result == nil {
		t.Fatal("expected inferred dict result")
	}
	if !result.IsMap {
		t.Fatalf("expected dict result to be map, got %#v", result)
	}
	if len(result.Fields) != 2 {
		t.Fatalf("expected two dict fields, got %#v", result.Fields)
	}
	if result.Fields[0].Name != "user" || result.Fields[1].Name != "count" {
		t.Fatalf("expected literal dict keys, got %#v", result.Fields)
	}
}

func TestInferExpressionTypeIndexesCollection(t *testing.T) {
	vars := map[string]ast.TemplateVar{
		"Items": {
			Name:     "Items",
			TypeStr:  "[]Item",
			IsSlice:  true,
			ElemType: "Item",
		},
	}

	result := validator.InferExpressionType(
		"index .Items 0",
		vars,
		[]validator.ScopeType{{
			IsRoot: true,
			Fields: []ast.FieldInfo{{
				Name:     "Items",
				TypeStr:  "[]Item",
				IsSlice:  true,
				ElemType: "Item",
			}},
		}},
		nil,
		nil,
		map[string][]ast.FieldInfo{
			"Item": {{Name: "Name", TypeStr: "string"}},
		},
	)

	if result == nil {
		t.Fatal("expected indexed result")
	}
	if result.TypeStr != "Item" {
		t.Fatalf("expected Item, got %q", result.TypeStr)
	}
	if len(result.Fields) != 1 || result.Fields[0].Name != "Name" {
		t.Fatalf("expected hydrated Item fields, got %#v", result.Fields)
	}
}
