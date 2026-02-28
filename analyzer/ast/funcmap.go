package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"strings"
)

// processFuncMapIndexAssign handles assignments to FuncMap via index expression.
// Example: myFuncMap["add"] = addFunc
func processFuncMapIndexAssign(
	indexExpr *goast.IndexExpr,
	rhs goast.Expr,
	info *types.Info,
	fset *token.FileSet,
	rhsIdx int,
	assign *goast.AssignStmt,
	scope *FuncScope,
) bool {
	if info == nil {
		return false
	}

	// Check if the indexed object is a FuncMap
	tv, ok := info.Types[indexExpr.X]
	if !ok || tv.Type == nil || !strings.HasSuffix(tv.Type.String(), "template.FuncMap") {
		return false
	}

	// Extract the key (function name)
	keyLit, ok := indexExpr.Index.(*goast.BasicLit)
	if !ok || keyLit.Kind != token.STRING {
		return false
	}

	name := strings.Trim(keyLit.Value, "\"")
	fInfo := FuncMapInfo{Name: name}

	// Extract function definition location and signature
	if rhsIdx < len(assign.Rhs) {
		fInfo.DefFile, fInfo.DefLine, fInfo.DefCol = resolveFuncDefLocation(rhs, info, fset)

		if rtv, ok := info.Types[rhs]; ok && rtv.Type != nil {
			fInfo.Params, fInfo.Returns, fInfo.Args = extractSignatureFromType(rtv.Type)
		}
	}

	scope.FuncMaps = append(scope.FuncMaps, fInfo)
	return true
}

// extractFuncMaps extracts function definitions from a FuncMap composite literal.
// Example: template.FuncMap{"add": addFunc, "multiply": multiplyFunc}
func extractFuncMaps(comp *goast.CompositeLit, info *types.Info, fset *token.FileSet, filesMap map[string]*goast.File) []FuncMapInfo {
	var result []FuncMapInfo

	for _, elt := range comp.Elts {
		kv, ok := elt.(*goast.KeyValueExpr)
		if !ok {
			continue
		}

		key, ok := kv.Key.(*goast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}

		name := strings.Trim(key.Value, "\"")
		fInfo := FuncMapInfo{Name: name}

		// Extract function metadata
		fInfo.DefFile, fInfo.DefLine, fInfo.DefCol = resolveFuncDefLocation(kv.Value, info, fset)
		fInfo.Doc = resolveFuncDoc(kv.Value, info, filesMap)

		// Extract signature if available
		if info != nil {
			if tv, ok := info.Types[kv.Value]; ok && tv.Type != nil {
				fInfo.Params, fInfo.Returns, fInfo.Args = extractSignatureFromType(tv.Type)
			}
		}

		result = append(result, fInfo)
	}

	return result
}

// isFuncMapType checks if an identifier has type template.FuncMap.
func isFuncMapType(ident *goast.Ident, info *types.Info) bool {
	if info == nil {
		return false
	}

	if tv, ok := info.Types[ident]; ok && tv.Type != nil {
		return strings.HasSuffix(tv.Type.String(), "template.FuncMap")
	}

	return false
}

// isFuncMapCompositeLit checks if a composite literal is of type template.FuncMap.
func isFuncMapCompositeLit(comp *goast.CompositeLit, info *types.Info) bool {
	if info == nil {
		return false
	}

	if tv, ok := info.Types[comp]; ok && tv.Type != nil {
		return strings.HasSuffix(tv.Type.String(), "template.FuncMap")
	}

	return false
}

// resolveFuncDefLocation finds the definition location of a function value.
// For named functions, resolves to declaration site.
// For literals, returns literal position.
func resolveFuncDefLocation(expr goast.Expr, info *types.Info, fset *token.FileSet) (file string, line, col int) {
	if fset == nil {
		return
	}

	switch e := expr.(type) {
	case *goast.Ident:
		if info != nil {
			if obj := info.ObjectOf(e); obj != nil && obj.Pos().IsValid() {
				pos := fset.Position(obj.Pos())
				return pos.Filename, pos.Line, pos.Column
			}
		}
	case *goast.SelectorExpr:
		if info != nil {
			if obj := info.ObjectOf(e.Sel); obj != nil && obj.Pos().IsValid() {
				pos := fset.Position(obj.Pos())
				return pos.Filename, pos.Line, pos.Column
			}
		}
	}

	// Fallback: expression position
	pos := fset.Position(expr.Pos())
	return pos.Filename, pos.Line, pos.Column
}

// resolveFuncDoc attempts to extract documentation for a function value.
// Only works for named functions, not anonymous literals.
func resolveFuncDoc(expr goast.Expr, info *types.Info, filesMap map[string]*goast.File) string {
	if info == nil {
		return ""
	}

	var obj types.Object
	switch e := expr.(type) {
	case *goast.Ident:
		obj = info.ObjectOf(e)
	case *goast.SelectorExpr:
		obj = info.ObjectOf(e.Sel)
	default:
		return ""
	}

	if obj == nil || !obj.Pos().IsValid() {
		return ""
	}

	// Search for function declaration in AST
	for _, file := range filesMap {
		for _, decl := range file.Decls {
			fd, ok := decl.(*goast.FuncDecl)
			if !ok {
				continue
			}

			if fd.Name.Obj != nil && fd.Name.Obj.Pos() == obj.Pos() {
				if fd.Doc != nil {
					return strings.TrimSpace(fd.Doc.Text())
				}
				return ""
			}
		}
	}

	return ""
}

// extractSignatureFromType extracts signature info from a type.
// Handles both direct signatures and pointer-to-signature.
func extractSignatureFromType(t types.Type) (params, returns []ParamInfo, args []string) {
	// Unwrap pointer
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}

	sig, ok := t.(*types.Signature)
	if !ok {
		return nil, nil, nil
	}

	return extractSignatureInfo(sig)
}

// extractSignatureInfo extracts detailed parameter and return type information
// from a function signature.
func extractSignatureInfo(sig *types.Signature) (params, returns []ParamInfo, args []string) {
	// Extract parameters
	params = make([]ParamInfo, sig.Params().Len())
	args = make([]string, sig.Params().Len())

	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		ts := normalizeTypeStr(p.Type())
		params[i] = ParamInfo{Name: p.Name(), TypeStr: ts}
		args[i] = ts
	}

	// Extract return types
	returns = make([]ParamInfo, sig.Results().Len())

	for i := 0; i < sig.Results().Len(); i++ {
		r := sig.Results().At(i)
		ts := normalizeTypeStr(r.Type())
		returns[i] = ParamInfo{Name: r.Name(), TypeStr: ts}
	}

	return
}
