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
	// Phase 1: Identify all nodes that represent distinct scopes
	funcNodes := identifyFuncNodes(files)

	if len(funcNodes) == 0 {
		return nil
	}

	// Phase 2: Process concurrently with optimal work distribution
	return processNodesConcurrently(funcNodes, info, fset, structIndex, fc, config, filesMap, seenPool)
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
) []FuncScope {
	// Calculate optimal worker count and chunk size
	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (len(funcNodes) + numWorkers - 1) / numWorkers

	// Channel for collecting results from workers
	resultChan := make(chan []FuncScope, numWorkers)
	var wg sync.WaitGroup

	// Spawn workers
	for w := range numWorkers {
		start := w * chunkSize
		if start >= len(funcNodes) {
			break
		}
		end := min(start+chunkSize, len(funcNodes))
		chunk := funcNodes[start:end]

		wg.Add(1)
		go processChunk(chunk, info, fset, structIndex, fc, config, filesMap, seenPool, resultChan, &wg)
	}

	// Close result channel when all workers complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Aggregate results from all workers
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
	resultChan chan<- []FuncScope,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	// Process each work unit in this chunk
	localScopes := make([]FuncScope, 0, len(chunk)/2)

	for _, unit := range chunk {
		scope := processFunc(unit.node, info, fset, structIndex, fc, config, filesMap, seenPool)

		// Only keep scopes that found something useful
		if len(scope.RenderNodes) > 0 || len(scope.SetVars) > 0 || len(scope.FuncMaps) > 0 {
			localScopes = append(localScopes, scope)
		}
	}

	resultChan <- localScopes
}
