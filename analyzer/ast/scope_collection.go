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
// Replace the collectFuncScopesOptimized signature and body to accept + build the mutator index.

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
	return processNodesConcurrently(funcNodes, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex)
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
		go processChunk(chunk, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, resultChan, &wg)
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
	resultChan chan<- []FuncScope,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	localScopes := make([]FuncScope, 0, len(chunk)/2)
	for _, unit := range chunk {
		scope := processFunc(unit.node, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex)
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
