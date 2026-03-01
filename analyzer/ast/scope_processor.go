package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"strings"
)

// MaxAssignmentsPerVar is the maximum number of string assignments to track per variable
// This prevents excessive memory usage on variables assigned in loops
const MaxAssignmentsPerVar = 10

// processFunc analyzes a single function or declaration to extract:
// 1. String literal assignments (for template name resolution)
// 2. FuncMap assignments (template function definitions)
// 3. Template render calls
// 4. Context variable Set calls
//
// The analysis proceeds in two passes:
// Pass 1: Collect assignments to build a local symbol table
// Pass 2: Identify and process template-related calls
func processFunc(
	n goast.Node,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
) FuncScope {
	scope := FuncScope{
		MapAssignments: make(map[string]*goast.CompositeLit, 4),
	}

	// Local symbol tables for name resolution
	stringAssignments := make(map[string][]string, 8)
	funcMapAssignments := make(map[string]*goast.CompositeLit, 4)

	// Pass 1: Collect assignments
	collectAssignments(n, info, fset, filesMap, &scope, stringAssignments, funcMapAssignments)

	// Pass 2: Find template operations
	findTemplateOperations(n, info, fset, structIndex, fc, config, filesMap, seenPool, &scope, stringAssignments)

	return scope
}

// collectAssignments walks the AST to build local symbol tables.
// This enables template name resolution when names are passed via variables.
func collectAssignments(
	n goast.Node,
	info *types.Info,
	fset *token.FileSet,
	filesMap map[string]*goast.File,
	scope *FuncScope,
	stringAssignments map[string][]string,
	funcMapAssignments map[string]*goast.CompositeLit,
) {
	goast.Inspect(n, func(child goast.Node) bool {
		// Stop at nested function literals to maintain scope boundaries
		if child != n {
			if _, isFunc := child.(*goast.FuncLit); isFunc {
				return false
			}
		}

		switch node := child.(type) {
		case *goast.AssignStmt:
			processAssignStmt(node, info, fset, filesMap, scope, stringAssignments, funcMapAssignments)

		case *goast.GenDecl:
			processGenDecl(node, info, fset, filesMap, scope, stringAssignments, funcMapAssignments)
		}

		return true
	})
}

// processAssignStmt handles assignment statements, extracting:
// - String literals assigned to variables (with limit)
// - FuncMap composite literals
// - Map index assignments to FuncMap[key]
// - Map variable assignments for data argument resolution
func processAssignStmt(
	assign *goast.AssignStmt,
	info *types.Info,
	fset *token.FileSet,
	filesMap map[string]*goast.File,
	scope *FuncScope,
	stringAssignments map[string][]string,
	funcMapAssignments map[string]*goast.CompositeLit,
) {
	for i, lhs := range assign.Lhs {
		if i >= len(assign.Rhs) {
			continue
		}
		rhs := assign.Rhs[i]

		// Handle map index assignments: funcMap["key"] = value
		if indexExpr, ok := lhs.(*goast.IndexExpr); ok {
			if processFuncMapIndexAssign(indexExpr, rhs, info, fset, i, assign, scope) {
				continue
			}
			// Also track map[string]interface{} index mutations so we can
			// resolve data variables built up via ctx["key"] = val.
			trackMapIndexAssign(indexExpr, rhs, scope)
			continue
		}

		// Handle regular variable assignments
		ident, ok := lhs.(*goast.Ident)
		if !ok {
			continue
		}

		// Collect string literal assignments with limit
		if s := extractStringFast(rhs); s != "" {
			if len(stringAssignments[ident.Name]) < MaxAssignmentsPerVar {
				stringAssignments[ident.Name] = append(stringAssignments[ident.Name], s)
			}
		}

		// Collect composite literal assignments.
		// This covers both template.FuncMap and map[string]interface{} data maps.
		if comp, ok := rhs.(*goast.CompositeLit); ok {
			funcMapAssignments[ident.Name] = comp

			if isFuncMapType(ident, info) {
				scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
			} else if isDataMapType(ident, info) {
				// Track as a potential render data variable map.
				scope.MapAssignments[ident.Name] = comp
			}
		}
	}
}

// trackMapIndexAssign records an index-assignment mutation on a map variable
// that is already tracked in scope.MapAssignments. This handles the pattern:
//
//	ctx := rex.Map{"key": val}
//	ctx["extra"] = val2
//
// We synthesise a new composite literal that merges the original entries with
// the new key so that downstream var extraction sees the full picture.
func trackMapIndexAssign(indexExpr *goast.IndexExpr, rhs goast.Expr, scope *FuncScope) {
	ident, ok := indexExpr.X.(*goast.Ident)
	if !ok {
		return
	}

	existing, tracked := scope.MapAssignments[ident.Name]
	if !tracked {
		return
	}

	key, ok := indexExpr.Index.(*goast.BasicLit)
	if !ok || key.Kind != token.STRING {
		return
	}

	// Append the new key-value pair to a shallow copy of the composite literal
	// so the original AST node is not mutated.
	updated := &goast.CompositeLit{
		Type:   existing.Type,
		Lbrace: existing.Lbrace,
		Rbrace: existing.Rbrace,
		Elts:   make([]goast.Expr, len(existing.Elts), len(existing.Elts)+1),
	}
	copy(updated.Elts, existing.Elts)
	updated.Elts = append(updated.Elts, &goast.KeyValueExpr{
		Key:   key,
		Value: rhs,
	})

	scope.MapAssignments[ident.Name] = updated
}

// isDataMapType returns true when ident has a map type whose key is string and
// whose value is interface{} / any. This matches rex.Map, gin.H, echo.Map, and
// raw map[string]interface{} / map[string]any literals.
func isDataMapType(ident *goast.Ident, info *types.Info) bool {
	if info == nil {
		return false
	}

	tv, ok := info.Defs[ident]
	if !ok || tv == nil || tv.Type() == nil {
		return false
	}

	t := tv.Type()
	// Unwrap named types (rex.Map, gin.H, etc.)
	if named, ok := t.(*types.Named); ok {
		t = named.Underlying()
	}

	m, ok := t.(*types.Map)
	if !ok {
		return false
	}

	// Key must be string
	if basic, ok := m.Key().(*types.Basic); !ok || basic.Kind() != types.String {
		return false
	}

	// Value must be interface{} / any
	_, isIface := m.Elem().Underlying().(*types.Interface)
	return isIface
}

// processGenDecl handles general declarations (var, const, type).
// Extracts string and FuncMap literals from var/const declarations.
func processGenDecl(
	decl *goast.GenDecl,
	info *types.Info,
	fset *token.FileSet,
	filesMap map[string]*goast.File,
	scope *FuncScope,
	stringAssignments map[string][]string,
	funcMapAssignments map[string]*goast.CompositeLit,
) {
	if decl.Tok != token.VAR && decl.Tok != token.CONST {
		return
	}

	for _, spec := range decl.Specs {
		vspec, ok := spec.(*goast.ValueSpec)
		if !ok {
			continue
		}

		for i, name := range vspec.Names {
			if i >= len(vspec.Values) {
				continue
			}
			rhs := vspec.Values[i]

			// Collect string literals with limit
			if s := extractStringFast(rhs); s != "" {
				if len(stringAssignments[name.Name]) < MaxAssignmentsPerVar {
					stringAssignments[name.Name] = append(stringAssignments[name.Name], s)
				}
			}

			// Collect composite literal assignments
			if comp, ok := rhs.(*goast.CompositeLit); ok {
				funcMapAssignments[name.Name] = comp

				if info != nil {
					if tv, ok := info.Defs[name]; ok && tv.Type() != nil {
						typeStr := tv.Type().String()
						if strings.HasSuffix(typeStr, "template.FuncMap") {
							scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
						}
					}
				}

				// Also track data map literals declared via var/const
				if info != nil {
					if isDataMapType(name, info) {
						scope.MapAssignments[name.Name] = comp
					}
				}
			}
		}
	}
}

// findTemplateOperations walks the AST to identify:
// - Template render calls
// - Context variable Set calls
// - Inline FuncMap composite literals
func findTemplateOperations(
	n goast.Node,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
	scope *FuncScope,
	stringAssignments map[string][]string,
) {
	goast.Inspect(n, func(child goast.Node) bool {
		// Stop at nested function literals
		if child != n {
			if _, isFunc := child.(*goast.FuncLit); isFunc {
				return false
			}
		}

		switch node := child.(type) {
		case *goast.CompositeLit:
			// Inline FuncMap literals
			if isFuncMapCompositeLit(node, info) {
				scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(node, info, fset, filesMap)...)
			}

		case *goast.CallExpr:
			processCallExpr(node, info, fset, structIndex, fc, config, seenPool, scope, stringAssignments)
		}

		return true
	})
}

// processCallExpr handles function calls, identifying:
// - Template render calls
// - Context Set calls
func processCallExpr(
	call *goast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	seenPool *seenMapPool,
	scope *FuncScope,
	stringAssignments map[string][]string,
) {
	// Check for render calls
	if isRenderCall(call, config) {
		if resolved := resolveRenderCall(call, info, stringAssignments); resolved != nil {
			scope.RenderNodes = append(scope.RenderNodes, *resolved)
		}
		return
	}

	// Check for Set calls
	if setVar := extractSetCallVarOptimized(call, info, fset, structIndex, fc, config, seenPool); setVar != nil {
		scope.SetVars = append(scope.SetVars, *setVar)
	}
}
