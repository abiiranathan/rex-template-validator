package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"runtime"
	"sync"
)

// collectFuncScopesOptimized efficiently collects template operations from
// all function and variable declaration scopes using concurrent processing.
//
// Algorithm:
// 1. Phase 1: Identify all relevant AST nodes (functions, variables)
// 2. Phase 2: Process nodes concurrently using worker pool
// 3. Each worker processes a chunk of nodes independently
// 4. Results are aggregated from all workers
//
// Concurrency model:
// - One worker per CPU core (maximum parallelism)
// - Work distribution via chunk-based partitioning
// - No shared state between workers (thread-safe by design)
func collectFuncScopesOptimized(
	files []*goast.File,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
) []FuncScope {
	funcNodes := identifyFuncNodes(files)
	if len(funcNodes) == 0 {
		return nil
	}

	// Build the cross-function map-mutator index before spawning workers.
	// This is cheap (one AST walk, no type lookups beyond what is already loaded).
	mutatorIndex := buildMapMutatorIndex(files, info)

	// Build the string-map index: package-level map[K]string variables whose
	// values are string literals. Used to resolve template names that come
	// from a map lookup (e.g. view, ok := labforms[request.ReportType]).
	stringMapIndex := buildStringMapIndex(files, info)

	return processNodesConcurrently(funcNodes, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, stringMapIndex)
}

// identifyFuncNodes walks all AST files to identify nodes representing
// distinct scopes: function declarations, function literals, and top-level
// variable/constant declarations.
func identifyFuncNodes(files []*goast.File) []funcWorkUnit {
	// Estimate capacity: ~8 functions per file is typical
	funcNodes := make([]funcWorkUnit, 0, len(files)*8)

	for _, f := range files {
		goast.Inspect(f, func(n goast.Node) bool {
			switch node := n.(type) {
			case *goast.FuncDecl, *goast.FuncLit:
				// Function declarations (func Foo()) and literals (func() {})
				funcNodes = append(funcNodes, funcWorkUnit{node: node})

			case *goast.GenDecl:
				// Top-level variables and constants can contain template operations
				if node.Tok == token.VAR || node.Tok == token.CONST {
					funcNodes = append(funcNodes, funcWorkUnit{node: node})
				}
			}
			return true
		})
	}

	return funcNodes
}

// processNodesConcurrently distributes work units across multiple workers
// and aggregates their results.
func processNodesConcurrently(
	funcNodes []funcWorkUnit,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
	mutatorIndex map[string][]*goast.KeyValueExpr,
	stringMapIndex map[string][]string,
) []FuncScope {
	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (len(funcNodes) + numWorkers - 1) / numWorkers
	resultChan := make(chan []FuncScope, numWorkers)
	var wg sync.WaitGroup

	for w := range numWorkers {
		start := w * chunkSize
		if start >= len(funcNodes) {
			break
		}
		end := min(start+chunkSize, len(funcNodes))
		chunk := funcNodes[start:end]

		wg.Add(1)
		go processChunk(chunk, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, stringMapIndex, resultChan, &wg)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var allScopes []FuncScope
	for scopes := range resultChan {
		allScopes = append(allScopes, scopes...)
	}
	return allScopes
}

// processChunk is the worker function that processes a chunk of AST nodes.
// Each worker operates independently with no shared mutable state.
func processChunk(
	chunk []funcWorkUnit,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
	mutatorIndex map[string][]*goast.KeyValueExpr,
	stringMapIndex map[string][]string,
	resultChan chan<- []FuncScope,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	localScopes := make([]FuncScope, 0, len(chunk)/2)
	for _, unit := range chunk {
		scope := processFunc(unit.node, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, stringMapIndex)
		if len(scope.RenderNodes) > 0 || len(scope.SetVars) > 0 || len(scope.FuncMaps) > 0 {
			localScopes = append(localScopes, scope)
		}
	}
	resultChan <- localScopes
}

// buildMapMutatorIndex scans all files for functions whose first parameter is
// a map[string]any / map[string]interface{} (including named aliases like rex.Map)
// and records every string-keyed index assignment made to that parameter.
//
// Example: func SetTriageContext(ctx rex.Map, ...) { ctx["visit"] = visit }
// produces an entry: "SetTriageContext" → [KeyValueExpr{Key:"visit", Value:visit}, ...]
//
// The index is consumed by applyMapMutatorCall to propagate mutations from helper
// functions back into the caller's tracked map variable.
func buildMapMutatorIndex(files []*goast.File, info *types.Info) map[string][]*goast.KeyValueExpr {
	index := make(map[string][]*goast.KeyValueExpr)

	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*goast.FuncDecl)
			if !ok || fd.Body == nil || fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
				continue
			}

			firstParam := fd.Type.Params.List[0]
			if !isMapStringAnyParam(firstParam, info) {
				continue
			}

			// Collect the parameter names (usually one, but a, b rex.Map is valid Go).
			paramNames := make(map[string]bool, len(firstParam.Names))
			for _, n := range firstParam.Names {
				paramNames[n.Name] = true
			}

			var kvs []*goast.KeyValueExpr
			goast.Inspect(fd.Body, func(n goast.Node) bool {
				assign, ok := n.(*goast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range assign.Lhs {
					idx, ok := lhs.(*goast.IndexExpr)
					if !ok {
						continue
					}
					recv, ok := idx.X.(*goast.Ident)
					if !ok || !paramNames[recv.Name] {
						continue
					}
					keyLit, ok := idx.Index.(*goast.BasicLit)
					if !ok || keyLit.Kind != token.STRING {
						continue
					}
					if i < len(assign.Rhs) {
						kvs = append(kvs, &goast.KeyValueExpr{
							Key:   keyLit,
							Value: assign.Rhs[i],
						})
					}
				}
				return true
			})

			if len(kvs) > 0 {
				index[fd.Name.Name] = kvs
			}
		}
	}
	return index
}

// buildStringMapIndex scans all files for package-level variable declarations
// of map types whose *value* type is string (e.g. map[SomeEnum]string,
// map[string]string). It records every string-literal value found in the
// composite literal, keyed by the variable name.
//
// This powers dynamic template-name resolution for the common pattern:
//
//	var labforms = map[enums.ReportType]string{
//	    enums.ReportTypeGeneral: "views/lab/labforms/GENERAL.html",
//	    enums.ReportTypeCbc:     "views/lab/labforms/CBC-3Part.html",
//	    ...
//	}
//	view, ok := labforms[request.ReportType]
//	c.Render(view, data)  // → generates a RenderCall per value in labforms
//
// The returned map is: varName → []string{all literal string values}.
func buildStringMapIndex(files []*goast.File, info *types.Info) map[string][]string {
	index := make(map[string][]string)

	for _, f := range files {
		for _, decl := range f.Decls {
			genDecl, ok := decl.(*goast.GenDecl)
			if !ok || genDecl.Tok != token.VAR {
				continue
			}

			for _, spec := range genDecl.Specs {
				vspec, ok := spec.(*goast.ValueSpec)
				if !ok {
					continue
				}

				for i, name := range vspec.Names {
					if i >= len(vspec.Values) {
						continue
					}

					comp, ok := vspec.Values[i].(*goast.CompositeLit)
					if !ok {
						continue
					}

					// Confirm the variable's type resolves to map[K]string.
					// Prefer type-checker info; fall back to AST inspection.
					if info != nil {
						if obj, ok := info.Defs[name]; ok && obj != nil {
							if !isMapToStringType(obj.Type()) {
								continue
							}
						} else {
							// Defs entry missing — fall through to AST check.
							if !isMapToStringLitType(comp) {
								continue
							}
						}
					} else {
						if !isMapToStringLitType(comp) {
							continue
						}
					}

					// Collect all string-literal values from the map literal.
					var vals []string
					for _, elt := range comp.Elts {
						kv, ok := elt.(*goast.KeyValueExpr)
						if !ok {
							continue
						}
						if s := extractStringFast(kv.Value); s != "" {
							vals = append(vals, s)
						}
					}

					if len(vals) > 0 {
						index[name.Name] = vals
					}
				}
			}
		}
	}

	return index
}

// isMapToStringType reports whether t is (or unwraps to) a map whose value
// type is the built-in string kind.
func isMapToStringType(t types.Type) bool {
	if t == nil {
		return false
	}
	if named, ok := t.(*types.Named); ok {
		t = named.Underlying()
	}
	m, ok := t.(*types.Map)
	if !ok {
		return false
	}
	basic, ok := m.Elem().Underlying().(*types.Basic)
	return ok && basic.Kind() == types.String
}

// isMapToStringLitType is a best-effort AST-only check: it looks at the
// composite literal's type node for the pattern map[…]string.
func isMapToStringLitType(comp *goast.CompositeLit) bool {
	if comp.Type == nil {
		return false
	}
	mt, ok := comp.Type.(*goast.MapType)
	if !ok {
		return false
	}
	ident, ok := mt.Value.(*goast.Ident)
	return ok && ident.Name == "string"
}

// isMapStringAnyParam reports whether a function parameter's type resolves to
// map[string]interface{} / map[string]any, including named aliases (rex.Map, gin.H, etc.).
func isMapStringAnyParam(field *goast.Field, info *types.Info) bool {
	if info == nil || len(field.Names) == 0 {
		return false
	}
	tv, ok := info.Defs[field.Names[0]]
	if !ok || tv == nil || tv.Type() == nil {
		return false
	}
	t := tv.Type()
	if named, ok := t.(*types.Named); ok {
		t = named.Underlying()
	}
	m, ok := t.(*types.Map)
	if !ok {
		return false
	}
	basic, ok := m.Key().(*types.Basic)
	if !ok || basic.Kind() != types.String {
		return false
	}
	_, isIface := m.Elem().Underlying().(*types.Interface)
	return isIface
}

// applyMapMutatorCall checks whether a call expression invokes a known
// map-mutating helper (present in mutatorIndex) and, if so, merges its
// recorded key/value mutations into the caller's tracked map variable.
//
// Example: given  ctx := rex.Map{"a": 1}  followed by  SetTriageContext(ctx, ...)
// and mutatorIndex["SetTriageContext"] = [{"visit":visit}, {"triage":triage}, ...]
// the function appends those pairs to ctx's composite literal so that downstream
// extractMapVars sees the full set of keys.
func applyMapMutatorCall(
	call *goast.CallExpr,
	scope *FuncScope,
	mutatorIndex map[string][]*goast.KeyValueExpr,
) {
	if len(mutatorIndex) == 0 || len(call.Args) == 0 {
		return
	}

	// Resolve callee name — handles both plain calls and method calls.
	var calleeName string
	switch fn := call.Fun.(type) {
	case *goast.Ident:
		calleeName = fn.Name
	case *goast.SelectorExpr:
		calleeName = fn.Sel.Name
	default:
		return
	}

	kvs, known := mutatorIndex[calleeName]
	if !known {
		return
	}

	// The first argument must be a map variable already tracked in scope.
	firstArg, ok := call.Args[0].(*goast.Ident)
	if !ok {
		return
	}

	existing, tracked := scope.MapAssignments[firstArg.Name]
	if !tracked {
		return
	}

	// Produce a shallow copy of the composite literal with the extra entries
	// appended so that the original AST node is never mutated.
	updated := &goast.CompositeLit{
		Type:   existing.Type,
		Lbrace: existing.Lbrace,
		Rbrace: existing.Rbrace,
		Elts:   make([]goast.Expr, len(existing.Elts), len(existing.Elts)+len(kvs)),
	}
	copy(updated.Elts, existing.Elts)
	for _, kv := range kvs {
		updated.Elts = append(updated.Elts, kv)
	}

	scope.MapAssignments[firstArg.Name] = updated
}
