package ast

import (
	"fmt"
	goast "go/ast"
	"go/token"
	"go/types"
	"strings"
)

// getASTKey generates a base identifier for a named type to look up docs.
// Format: "packageName.TypeName" or just "TypeName" for built-in types.
// This intentionally ignores type arguments (generics) because the AST
// definition is the same regardless of instantiation.
func getASTKey(named *types.Named) string {
	obj := named.Obj()
	if obj.Pkg() != nil {
		return obj.Pkg().Name() + "." + obj.Name()
	}
	return obj.Name()
}

// normalizeTypeStr makes type strings more readable by removing package paths.
// It correctly handles generic instantiations.
// Example: "github.com/user/pkg.Page[github.com/user/pkg.User]" → "Page[User]"
func normalizeTypeStr(t types.Type) string {
	if t == nil {
		return ""
	}
	return types.TypeString(t, func(*types.Package) string {
		return "" // Omits package paths completely
	})
}

// getElementType extracts the element type from a slice or array type.
// Recursively unwraps pointers and named types.
func getElementType(t types.Type) types.Type {
	switch v := t.(type) {
	case *types.Slice:
		return v.Elem()
	case *types.Array:
		return v.Elem()
	case *types.Pointer:
		return getElementType(v.Elem())
	case *types.Named:
		return getElementType(v.Underlying())
	}
	return nil
}

// getMapTypes extracts key and value types from a map type.
// Recursively unwraps pointers and named types.
func getMapTypes(t types.Type) (types.Type, types.Type) {
	switch v := t.(type) {
	case *types.Map:
		return v.Key(), v.Elem()
	case *types.Pointer:
		return getMapTypes(v.Elem())
	case *types.Named:
		return getMapTypes(v.Underlying())
	}
	return nil, nil
}

// unwrapType removes pointer and map wrappers to get the underlying type.
func unwrapType(t types.Type) types.Type {
	for {
		switch v := t.(type) {
		case *types.Pointer:
			t = v.Elem()
		case *types.Map:
			t = v.Elem()
		default:
			return t
		}
	}
}

// parseTypeString strips [] and * prefixes to get base type name.
// Returns: (baseType, isSlice)
func parseTypeString(typeStr string) (string, bool) {
	base := typeStr
	isSlice := false

	for {
		if strings.HasPrefix(base, "[]") {
			isSlice = true
			base = base[2:]
		} else if strings.HasPrefix(base, "*") {
			base = base[1:]
		} else {
			break
		}
	}

	return base, isSlice
}

// findDefinitionLocation resolves the source location where an expression's
// value is defined. Prioritizes declarations over usages.
func findDefinitionLocation(expr goast.Expr, info *types.Info, fset *token.FileSet) (string, int, int) {
	var ident *goast.Ident

	// Extract identifier from expression
	switch e := expr.(type) {
	case *goast.Ident:
		ident = e
	case *goast.UnaryExpr:
		// &MyStruct{}
		if id, ok := e.X.(*goast.Ident); ok {
			ident = id
		}
	case *goast.CallExpr:
		// Function call: use call site
		pos := fset.Position(e.Pos())
		return pos.Filename, pos.Line, pos.Column
	case *goast.CompositeLit:
		// Composite literal: use literal site
		pos := fset.Position(e.Pos())
		return pos.Filename, pos.Line, pos.Column
	case *goast.SelectorExpr:
		// pkg.Name: use selector position
		pos := fset.Position(e.Sel.Pos())
		return pos.Filename, pos.Line, pos.Column
	}

	// Resolve identifier definition
	if ident != nil {
		// Prioritize definition
		if obj, ok := info.Defs[ident]; ok && obj != nil {
			pos := fset.Position(obj.Pos())
			return pos.Filename, pos.Line, pos.Column
		}
		// Fallback to usage
		if obj, ok := info.Uses[ident]; ok && obj != nil {
			pos := fset.Position(obj.Pos())
			return pos.Filename, pos.Line, pos.Column
		}
		// Fallback to identifier position
		pos := fset.Position(ident.Pos())
		return pos.Filename, pos.Line, pos.Column
	}

	// Default: expression position
	pos := fset.Position(expr.Pos())
	return pos.Filename, pos.Line, pos.Column
}

// inferTypeFromAST makes a best-effort guess at the type based on AST structure.
// Used when type information is unavailable.
func inferTypeFromAST(expr goast.Expr) string {
	switch e := expr.(type) {
	case *goast.BasicLit:
		switch e.Kind {
		case token.STRING:
			return "string"
		case token.INT:
			return "int"
		case token.FLOAT:
			return "float64"
		}
	case *goast.Ident:
		return e.Name
	case *goast.SelectorExpr:
		return fmt.Sprintf("%v.%s", e.X, e.Sel.Name)
	case *goast.CallExpr:
		if sel, ok := e.Fun.(*goast.SelectorExpr); ok {
			return fmt.Sprintf("call:%s", sel.Sel.Name)
		}
	case *goast.CompositeLit:
		if e.Type != nil {
			return fmt.Sprintf("%v", e.Type)
		}
	case *goast.UnaryExpr:
		return "unary"
	}
	return "unknown"
}
