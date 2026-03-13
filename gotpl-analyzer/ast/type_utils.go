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

// normalizeTypeStr makes type strings readable by replacing full import paths
// with their package names, while preserving the package qualifier.
// Example: "github.com/user/pkg.PatientPayments" → "views.PatientPayments"
// Example: "github.com/user/pkg.Page[github.com/user/pkg.User]" → "Page[User]"
func normalizeTypeStr(t types.Type) string {
	if t == nil {
		return ""
	}
	return types.TypeString(t, func(pkg *types.Package) string {
		return pkg.Name() // keep short package name, drop import path
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

// registryTypeKey derives the canonical registry key from a type string by
// stripping pointer (*) and slice ([]) prefixes, and for map types returning
// the key for the value type. Generic type parameters are preserved so that
// Page[User] and Page[Order] remain distinct registry entries.
//
// Examples:
//
//	"User"              → "User"
//	"*User"             → "User"
//	"[]User"            → "User"
//	"[]*User"           → "User"
//	"Page[User]"        → "Page[User]"
//	"[]Page[User]"      → "Page[User]"
//	"map[string]User"   → "User"
//	"map[uint]*Drug"    → "Drug"
//	"string"            → "string"   (filtered by isPrimitiveType)
func registryTypeKey(typeStr string) string {
	s := strings.TrimSpace(typeStr)

	// Strip leading pointer and slice qualifiers.
	for strings.HasPrefix(s, "*") || strings.HasPrefix(s, "[]") {
		if strings.HasPrefix(s, "*") {
			s = s[1:]
		} else {
			s = s[2:]
		}
	}

	// For map types use the value type, handling nested brackets correctly.
	if strings.HasPrefix(s, "map[") {
		depth := 0
		// Scan from index 4 (past "map[") to find the matching ']'.
		for i := 4; i < len(s); i++ {
			switch s[i] {
			case '[':
				depth++
			case ']':
				if depth == 0 {
					// Everything after the closing ] is the value type.
					return registryTypeKey(s[i+1:])
				}
				depth--
			}
		}
	}

	return s
}

// isPrimitiveType reports whether a type string names a built-in Go type,
// an empty value, or the synthetic "method" marker used by the field extractor.
// Such types are not recorded in the global type registry.
func isPrimitiveType(t string) bool {
	switch t {
	case "string", "bool", "byte", "rune", "error", "any", "interface{}", "method", "":
		return true
	case "int", "int8", "int16", "int32", "int64":
		return true
	case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return true
	case "float32", "float64", "complex64", "complex128":
		return true
	}
	return false
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
