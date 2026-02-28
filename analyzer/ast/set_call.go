package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
)

// extractSetCallVarOptimized extracts template variable information from
// a context.Set() call. Validates the receiver type and extracts comprehensive
// type information including nested fields and documentation.
//
// Example: ctx.Set("user", user)
// Extracts: name="user", type, fields, documentation
func extractSetCallVarOptimized(
	call *goast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	seenPool *seenMapPool,
) *TemplateVar {
	// Must be method call
	sel, ok := call.Fun.(*goast.SelectorExpr)
	if !ok || sel.Sel.Name != config.SetFunctionName {
		return nil
	}

	// Verify receiver type matches configured context type
	if !isContextType(sel.X, info, config.ContextTypeName) {
		return nil
	}

	// Extract variable name (first argument)
	if len(call.Args) < 2 {
		return nil
	}

	key := extractStringFast(call.Args[0])
	if key == "" {
		return nil
	}

	// Build template variable with full type information
	tv := TemplateVar{Name: key}
	valArg := call.Args[1]

	// Extract type information if available
	if typeInfo, ok := info.Types[valArg]; ok && typeInfo.Type != nil {
		tv.TypeStr = normalizeTypeStr(typeInfo.Type)

		seen := seenPool.get()
		tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

		// Handle collection types
		tv.IsSlice, tv.ElemType = checkSliceType(typeInfo.Type, structIndex, fc, seen, fset, &tv)
		tv.IsMap, tv.KeyType = checkMapType(typeInfo.Type, structIndex, fc, seen, fset, &tv)

		seenPool.put(seen)
	} else {
		// Fallback: infer basic type from AST
		tv.TypeStr = inferTypeFromAST(valArg)
	}

	// Find definition location
	tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(valArg, info, fset)

	return &tv
}

// isContextType verifies that an expression has the configured context type.
func isContextType(expr goast.Expr, info *types.Info, contextTypeName string) bool {
	if info == nil || expr == nil {
		return false
	}

	typeAndValue, ok := info.Types[expr]
	if !ok {
		return false
	}

	t := typeAndValue.Type

	// Dereference pointer
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}

	// Check named type
	named, ok := t.(*types.Named)
	return ok && named.Obj().Name() == contextTypeName
}

// checkSliceType determines if a type is a slice and extracts element type info.
func checkSliceType(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	tv *TemplateVar,
) (isSlice bool, elemType string) {
	elem := getElementType(t)
	if elem == nil {
		return false, ""
	}

	// Clear seen map for element type extraction
	clear(seen)

	tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elem, structIndex, fc, seen, fset, tv.Doc)
	return true, normalizeTypeStr(elem)
}

// checkMapType determines if a type is a map and extracts key/value type info.
func checkMapType(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	tv *TemplateVar,
) (isMap bool, keyType string) {
	keyT, elemT := getMapTypes(t)
	if keyT == nil || elemT == nil {
		return false, ""
	}

	// Clear seen map for element type extraction
	clear(seen)

	tv.ElemType = normalizeTypeStr(elemT)
	tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemT, structIndex, fc, seen, fset, tv.Doc)
	return true, normalizeTypeStr(keyT)
}
