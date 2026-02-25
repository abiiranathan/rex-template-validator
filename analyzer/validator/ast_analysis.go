package validator

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unique"

	"golang.org/x/tools/go/packages"
)

// structKeyHandle provides a unique, interned identifier for struct types.
// Using unique.Handle reduces memory overhead when the same type appears
// multiple times across the codebase.
type structKeyHandle = unique.Handle[string]

// structIndexEntry caches documentation and field metadata for a struct type.
// This prevents redundant AST traversals for the same type.
type structIndexEntry struct {
	doc    string               // Documentation comment for the struct
	fields map[string]fieldInfo // Field name → metadata mapping
}

// fieldInfo captures the source location and documentation of a struct field or method.
type fieldInfo struct {
	file string // Source file path
	line int    // Line number in source
	col  int    // Column number in source
	doc  string // Associated documentation comment
}

// makeStructKey generates a unique handle for a named type by combining
// its package name and type name. This handle serves as a cache key.
func makeStructKey(named *types.Named) structKeyHandle {
	return unique.Make(rawStructKey(named))
}

// rawStructKey constructs a fully qualified type identifier string.
// Format: "packageName.TypeName" or just "TypeName" for built-in types.
func rawStructKey(named *types.Named) string {
	obj := named.Obj()
	if obj.Pkg() != nil {
		return obj.Pkg().Name() + "." + obj.Name()
	}
	return obj.Name()
}

// ═══════════════════════════════════════════════════════════════════════════
// FIELD CACHE - Thread-safe caching of extracted field information
// ═══════════════════════════════════════════════════════════════════════════

// cachedFields stores pre-extracted field information to avoid redundant work.
// Each struct type's fields are computed once and reused throughout analysis.
type cachedFields struct {
	fields []FieldInfo // Exported fields and methods
	doc    string      // Struct-level documentation
}

// fieldCache provides concurrent-safe caching for struct field extraction.
// This is critical for performance when analyzing large codebases with
// many references to the same types.
type fieldCache struct {
	mu    sync.RWMutex                     // Protects concurrent map access
	cache map[structKeyHandle]cachedFields // Cache storage
}

// newFieldCache initializes a fieldCache with reasonable default capacity.
func newFieldCache() *fieldCache {
	return &fieldCache{
		cache: make(map[structKeyHandle]cachedFields, 256),
	}
}

// get retrieves cached field data with read lock for concurrent safety.
// Returns the cached data and a boolean indicating cache hit/miss.
func (fc *fieldCache) get(k structKeyHandle) (cachedFields, bool) {
	fc.mu.RLock()
	v, ok := fc.cache[k]
	fc.mu.RUnlock()
	return v, ok
}

// set stores field data in cache with write lock for concurrent safety.
func (fc *fieldCache) set(k structKeyHandle, v cachedFields) {
	fc.mu.Lock()
	fc.cache[k] = v
	fc.mu.Unlock()
}

// ═══════════════════════════════════════════════════════════════════════════
// SEEN MAP POOL - Optimized memory reuse for cycle detection
// ═══════════════════════════════════════════════════════════════════════════

// seenMapPool manages a pool of maps used to track visited types during
// recursive traversals. Pooling prevents excessive allocations, especially
// important when processing deeply nested type hierarchies.
type seenMapPool struct {
	pool sync.Pool
}

// newSeenMapPool creates a pool that generates fresh seen maps on demand.
func newSeenMapPool() *seenMapPool {
	return &seenMapPool{
		pool: sync.Pool{
			New: func() any {
				return make(map[structKeyHandle]bool, 16)
			},
		},
	}
}

// get retrieves a cleared seen map from the pool, ready for use.
// Maps are cleared to ensure no stale state from previous uses.
func (smp *seenMapPool) get() map[structKeyHandle]bool {
	m := smp.pool.Get().(map[structKeyHandle]bool)
	clear(m) // Go 1.21+ clear built-in
	return m
}

// put returns a seen map to the pool for later reuse.
func (smp *seenMapPool) put(m map[structKeyHandle]bool) {
	smp.pool.Put(m)
}

// ═══════════════════════════════════════════════════════════════════════════
// ANALYSIS ORCHESTRATION
// ═══════════════════════════════════════════════════════════════════════════

// AnalyzeDir performs comprehensive static analysis on Go source code to extract:
// 1. Template render calls with their associated data variables
// 2. Template function maps (custom functions available in templates)
// 3. Template variable definitions from context setters
//
// The analysis proceeds in phases:
// - Load and parse Go packages
// - Build struct field index (concurrent)
// - Collect function scopes and template operations (concurrent)
// - Aggregate and deduplicate results
// - Enrich with external context if provided
//
// Parameters:
//
//	dir: Root directory to analyze
//	contextFile: Optional JSON file with additional template context
//	config: Analysis configuration (function names, type names)
//
// Returns: AnalysisResult containing all discovered template-related information
func AnalyzeDir(dir string, contextFile string, config AnalysisConfig) AnalysisResult {
	result := AnalysisResult{}
	fset := token.NewFileSet()

	// Configure package loader to include all necessary type information
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
			packages.NeedImports,
		Dir:   dir,
		Fset:  fset,
		Tests: false, // Skip test files for cleaner analysis
	}

	// Load all packages in the directory tree
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
		return result
	}

	// Merge type information from all packages into a unified index
	info, allFiles := mergeTypeInfo(pkgs, &result)

	// Build quick lookup map: filename → AST
	filesMap := buildFileMap(allFiles, fset)

	// Initialize shared infrastructure
	fc := newFieldCache()
	seenPool := newSeenMapPool()

	// Phase 1: Build struct index (concurrent)
	structIndex := buildStructIndex(fset, filesMap)

	// Phase 2: Collect function scopes (concurrent)
	scopes := collectFuncScopesOptimized(allFiles, info, fset, structIndex, fc, config, filesMap, seenPool)

	// Phase 3: Identify global implicit variables
	// These are variables set outside any render call (global context)
	globalImplicitVars := extractGlobalImplicitVars(scopes)

	// Phase 4: Generate render calls from collected scopes
	result.RenderCalls = generateRenderCalls(scopes, globalImplicitVars, info, fset, dir, structIndex, fc, seenPool)

	// Phase 5: Aggregate and deduplicate function maps
	result.FuncMaps = aggregateFuncMaps(scopes)

	// Phase 6: Enrich with external context if provided
	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(
			result.RenderCalls, contextFile, pkgs, structIndex, fc, fset, config, seenPool,
		)
	}

	return result
}

// ═══════════════════════════════════════════════════════════════════════════
// TYPE INFORMATION MERGING
// ═══════════════════════════════════════════════════════════════════════════

// mergeTypeInfo consolidates type information from all loaded packages into
// a single unified types.Info structure. This enables cross-package type
// resolution during analysis.
//
// Also collects all AST files and non-import-related errors.
func mergeTypeInfo(pkgs []*packages.Package, result *AnalysisResult) (*types.Info, []*ast.File) {
	// Pre-calculate total sizes to avoid map growth
	totalTypes, totalDefs, totalUses := 0, 0, 0
	for _, pkg := range pkgs {
		if pkg.TypesInfo != nil {
			totalTypes += len(pkg.TypesInfo.Types)
			totalDefs += len(pkg.TypesInfo.Defs)
			totalUses += len(pkg.TypesInfo.Uses)
		}
	}

	// Create unified type info with pre-sized maps
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue, totalTypes),
		Defs:  make(map[*ast.Ident]types.Object, totalDefs),
		Uses:  make(map[*ast.Ident]types.Object, totalUses),
	}

	// Estimate file count
	allFiles := make([]*ast.File, 0, totalTypes/10+len(pkgs))

	// Merge all package data
	for _, pkg := range pkgs {
		// Collect non-import errors
		for _, e := range pkg.Errors {
			if !isImportRelatedError(e.Msg) {
				result.Errors = append(result.Errors, fmt.Sprintf("type error: %v", e.Msg))
			}
		}

		// Collect AST files
		allFiles = append(allFiles, pkg.Syntax...)

		// Merge type information
		if pkg.TypesInfo != nil {
			maps.Copy(info.Types, pkg.TypesInfo.Types)
			maps.Copy(info.Defs, pkg.TypesInfo.Defs)
			maps.Copy(info.Uses, pkg.TypesInfo.Uses)
		}
	}

	return info, allFiles
}

// buildFileMap creates a fast lookup map from filename to AST file.
// This enables quick file resolution when processing type definitions.
func buildFileMap(files []*ast.File, fset *token.FileSet) map[string]*ast.File {
	filesMap := make(map[string]*ast.File, len(files))
	for _, f := range files {
		if pos := fset.File(f.Pos()); pos != nil {
			filesMap[pos.Name()] = f
		}
	}
	return filesMap
}

// ═══════════════════════════════════════════════════════════════════════════
// GLOBAL VARIABLE EXTRACTION
// ═══════════════════════════════════════════════════════════════════════════

// extractGlobalImplicitVars identifies template variables that are set
// outside any render call context. These represent global template state
// available to all templates.
func extractGlobalImplicitVars(scopes []FuncScope) []TemplateVar {
	var globalVars []TemplateVar
	for _, scope := range scopes {
		// If scope has variables but no render calls, they're global
		if len(scope.RenderNodes) == 0 && len(scope.SetVars) > 0 {
			globalVars = append(globalVars, scope.SetVars...)
		}
	}
	return globalVars
}

// ═══════════════════════════════════════════════════════════════════════════
// RENDER CALL GENERATION
// ═══════════════════════════════════════════════════════════════════════════

// generateRenderCalls transforms collected scope information into structured
// RenderCall entries with full variable information. Each render call is
// associated with:
// - Source location (file, line, column range)
// - Template name(s)
// - Available template variables (local + scope + global)
func generateRenderCalls(
	scopes []FuncScope,
	globalImplicitVars []TemplateVar,
	info *types.Info,
	fset *token.FileSet,
	dir string,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seenPool *seenMapPool,
) []RenderCall {
	// Pre-count total render calls for efficient allocation
	totalRenders := 0
	for _, scope := range scopes {
		totalRenders += len(scope.RenderNodes)
	}

	renderCalls := make([]RenderCall, 0, totalRenders)

	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 {
			continue
		}

		for _, rr := range scope.RenderNodes {
			call := rr.Node
			templateArgIdx := rr.TemplateArgIdx

			// Skip invalid render calls
			if len(rr.TemplateNames) == 0 ||
				templateArgIdx < 0 ||
				templateArgIdx >= len(call.Args) {
				continue
			}

			templatePathExpr := call.Args[templateArgIdx]

			// Calculate precise column range for template name
			// This enables accurate editor highlighting and navigation
			tplNameStartCol, tplNameEndCol := getExprColumnRange(fset, templatePathExpr)

			// Adjust for string literal quotes
			if lit, ok := templatePathExpr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				tplNameStartCol++ // Skip opening quote
				tplNameEndCol--   // Skip closing quote
			}

			// Process each template name (usually one, but can be multiple from variables)
			for _, templatePath := range rr.TemplateNames {
				if templatePath == "" {
					continue
				}

				// Extract variables from data argument if present
				dataArgIdx := templateArgIdx + 1
				var localVars []TemplateVar
				if dataArgIdx < len(call.Args) {
					seen := seenPool.get()
					localVars = extractMapVars(call.Args[dataArgIdx], info, fset, structIndex, fc, seen)
					seenPool.put(seen)
				}

				// Combine all available variables: local + scope + global
				allVars := make([]TemplateVar, 0, len(localVars)+len(scope.SetVars)+len(globalImplicitVars))
				allVars = append(allVars, localVars...)
				allVars = append(allVars, scope.SetVars...)
				allVars = append(allVars, globalImplicitVars...)

				// Resolve file path relative to analysis root
				pos := fset.Position(call.Pos())
				relFile := resolveRelativePath(pos.Filename, dir)

				renderCalls = append(renderCalls, RenderCall{
					File:                 relFile,
					Line:                 pos.Line,
					Template:             templatePath,
					TemplateNameStartCol: tplNameStartCol,
					TemplateNameEndCol:   tplNameEndCol,
					Vars:                 allVars,
				})
			}
		}
	}

	return renderCalls
}

// resolveRelativePath attempts to convert an absolute path to a path
// relative to the specified directory. Falls back to the original path
// if conversion fails.
func resolveRelativePath(absPath, baseDir string) string {
	if abs, err := filepath.Abs(absPath); err == nil {
		if rel, err := filepath.Rel(baseDir, abs); err == nil {
			return rel
		}
	}
	return absPath
}

// ═══════════════════════════════════════════════════════════════════════════
// FUNCTION MAP AGGREGATION
// ═══════════════════════════════════════════════════════════════════════════

// aggregateFuncMaps collects all function map definitions from scopes
// and deduplicates them by name. This provides a complete catalog of
// template functions available across the codebase.
func aggregateFuncMaps(scopes []FuncScope) []FuncMapInfo {
	// Pre-count for efficient allocation
	totalFuncMaps := 0
	for _, scope := range scopes {
		totalFuncMaps += len(scope.FuncMaps)
	}

	allFuncMaps := make([]FuncMapInfo, 0, totalFuncMaps)
	for _, scope := range scopes {
		allFuncMaps = append(allFuncMaps, scope.FuncMaps...)
	}

	// Deduplicate by name
	seen := make(map[string]bool, len(allFuncMaps))
	unique := make([]FuncMapInfo, 0, len(allFuncMaps))

	for _, fm := range allFuncMaps {
		if !seen[fm.Name] {
			seen[fm.Name] = true
			unique = append(unique, fm)
		}
	}

	return unique
}

// ═══════════════════════════════════════════════════════════════════════════
// UTILITY: EXPRESSION COLUMN RANGE
// ═══════════════════════════════════════════════════════════════════════════

// getExprColumnRange calculates the precise column span of an AST expression.
// This is used for accurate editor highlighting and navigation features.
func getExprColumnRange(fset *token.FileSet, expr ast.Expr) (startCol, endCol int) {
	pos := fset.Position(expr.Pos())
	endPos := fset.Position(expr.End())
	return pos.Column, endPos.Column
}

// ═══════════════════════════════════════════════════════════════════════════
// SCOPE TYPES
// ═══════════════════════════════════════════════════════════════════════════

// FuncScope encapsulates all template-related operations within a single
// function or code block scope.
type FuncScope struct {
	SetVars     []TemplateVar    // Template variables set via context.Set()
	RenderNodes []ResolvedRender // Template render calls found
	FuncMaps    []FuncMapInfo    // Function map definitions
}

// ResolvedRender represents a template render call with resolved template
// names and argument positions.
type ResolvedRender struct {
	Node           *ast.CallExpr // The actual call expression
	TemplateNames  []string      // Resolved template name(s)
	TemplateArgIdx int           // Index of template name argument
}

// funcWorkUnit wraps an AST node for concurrent processing.
type funcWorkUnit struct {
	node ast.Node
}

// ═══════════════════════════════════════════════════════════════════════════
// FUNCTION SCOPE COLLECTION (CONCURRENT)
// ═══════════════════════════════════════════════════════════════════════════

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
	files []*ast.File,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
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
func identifyFuncNodes(files []*ast.File) []funcWorkUnit {
	// Estimate capacity: ~8 functions per file is typical
	funcNodes := make([]funcWorkUnit, 0, len(files)*8)

	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				// Function declarations (func Foo()) and literals (func() {})
				funcNodes = append(funcNodes, funcWorkUnit{node: node})

			case *ast.GenDecl:
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
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
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
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
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

// ═══════════════════════════════════════════════════════════════════════════
// FUNCTION PROCESSING
// ═══════════════════════════════════════════════════════════════════════════

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
	n ast.Node,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
	seenPool *seenMapPool,
) FuncScope {
	var scope FuncScope

	// Local symbol tables for name resolution
	stringAssignments := make(map[string][]string, 8)
	funcMapAssignments := make(map[string]*ast.CompositeLit, 4)

	// Pass 1: Collect assignments
	collectAssignments(n, info, fset, filesMap, &scope, stringAssignments, funcMapAssignments)

	// Pass 2: Find template operations
	findTemplateOperations(n, info, fset, structIndex, fc, config, filesMap, seenPool, &scope, stringAssignments)

	return scope
}

// collectAssignments walks the AST to build local symbol tables.
// This enables template name resolution when names are passed via variables.
func collectAssignments(
	n ast.Node,
	info *types.Info,
	fset *token.FileSet,
	filesMap map[string]*ast.File,
	scope *FuncScope,
	stringAssignments map[string][]string,
	funcMapAssignments map[string]*ast.CompositeLit,
) {
	ast.Inspect(n, func(child ast.Node) bool {
		// Stop at nested function literals to maintain scope boundaries
		if child != n {
			if _, isFunc := child.(*ast.FuncLit); isFunc {
				return false
			}
		}

		switch node := child.(type) {
		case *ast.AssignStmt:
			processAssignStmt(node, info, fset, filesMap, scope, stringAssignments, funcMapAssignments)

		case *ast.GenDecl:
			processGenDecl(node, info, fset, filesMap, scope, stringAssignments, funcMapAssignments)
		}

		return true
	})
}

// processAssignStmt handles assignment statements, extracting:
// - String literals assigned to variables
// - FuncMap composite literals
// - Map index assignments to FuncMap[key]
func processAssignStmt(
	assign *ast.AssignStmt,
	info *types.Info,
	fset *token.FileSet,
	filesMap map[string]*ast.File,
	scope *FuncScope,
	stringAssignments map[string][]string,
	funcMapAssignments map[string]*ast.CompositeLit,
) {
	for i, lhs := range assign.Lhs {
		if i >= len(assign.Rhs) {
			continue
		}
		rhs := assign.Rhs[i]

		// Handle map index assignments: funcMap["key"] = value
		if indexExpr, ok := lhs.(*ast.IndexExpr); ok {
			if processFuncMapIndexAssign(indexExpr, rhs, info, fset, i, assign, scope) {
				continue
			}
		}

		// Handle regular variable assignments
		ident, ok := lhs.(*ast.Ident)
		if !ok {
			continue
		}

		// Collect string literal assignments
		if s := extractStringFast(rhs); s != "" {
			stringAssignments[ident.Name] = append(stringAssignments[ident.Name], s)
		}

		// Collect FuncMap composite literals
		if comp, ok := rhs.(*ast.CompositeLit); ok {
			funcMapAssignments[ident.Name] = comp
			if isFuncMapType(ident, info) {
				scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
			}
		}
	}
}

// processFuncMapIndexAssign handles assignments to FuncMap via index expression.
// Example: myFuncMap["add"] = addFunc
func processFuncMapIndexAssign(
	indexExpr *ast.IndexExpr,
	rhs ast.Expr,
	info *types.Info,
	fset *token.FileSet,
	rhsIdx int,
	assign *ast.AssignStmt,
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
	keyLit, ok := indexExpr.Index.(*ast.BasicLit)
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

// processGenDecl handles general declarations (var, const, type).
// Extracts string and FuncMap literals from var/const declarations.
func processGenDecl(
	decl *ast.GenDecl,
	info *types.Info,
	fset *token.FileSet,
	filesMap map[string]*ast.File,
	scope *FuncScope,
	stringAssignments map[string][]string,
	funcMapAssignments map[string]*ast.CompositeLit,
) {
	if decl.Tok != token.VAR && decl.Tok != token.CONST {
		return
	}

	for _, spec := range decl.Specs {
		vspec, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}

		for i, name := range vspec.Names {
			if i >= len(vspec.Values) {
				continue
			}
			rhs := vspec.Values[i]

			// Collect string literals
			if s := extractStringFast(rhs); s != "" {
				stringAssignments[name.Name] = append(stringAssignments[name.Name], s)
			}

			// Collect FuncMap literals
			if comp, ok := rhs.(*ast.CompositeLit); ok {
				funcMapAssignments[name.Name] = comp

				if info != nil {
					if tv, ok := info.Defs[name]; ok && tv.Type() != nil {
						if strings.HasSuffix(tv.Type().String(), "template.FuncMap") {
							scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
						}
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
	n ast.Node,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
	seenPool *seenMapPool,
	scope *FuncScope,
	stringAssignments map[string][]string,
) {
	ast.Inspect(n, func(child ast.Node) bool {
		// Stop at nested function literals
		if child != n {
			if _, isFunc := child.(*ast.FuncLit); isFunc {
				return false
			}
		}

		switch node := child.(type) {
		case *ast.CompositeLit:
			// Inline FuncMap literals
			if isFuncMapCompositeLit(node, info) {
				scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(node, info, fset, filesMap)...)
			}

		case *ast.CallExpr:
			processCallExpr(node, info, fset, structIndex, fc, config, seenPool, scope, stringAssignments)
		}

		return true
	})
}

// processCallExpr handles function calls, identifying:
// - Template render calls
// - Context Set calls
func processCallExpr(
	call *ast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
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

// ═══════════════════════════════════════════════════════════════════════════
// RENDER CALL RESOLUTION
// ═══════════════════════════════════════════════════════════════════════════

// resolveRenderCall analyzes a render call expression to extract:
// - Template name(s) being rendered
// - Index of the template name argument
//
// Template names can come from:
// 1. String literals: c.Render("template.html", data)
// 2. Constants: c.Render(TemplateName, data)
// 3. Variables: c.Render(tplName, data)
func resolveRenderCall(
	call *ast.CallExpr,
	info *types.Info,
	stringAssignments map[string][]string,
) *ResolvedRender {
	resolved := &ResolvedRender{
		Node:           call,
		TemplateArgIdx: -1,
	}

	// Determine expected position of template argument
	templateArgIdx := inferTemplateArgIdx(call)

	// Find actual template argument position
	templateArgIdx = findTemplateArg(call, templateArgIdx, stringAssignments)

	if templateArgIdx < 0 || templateArgIdx >= len(call.Args) {
		return nil
	}

	resolved.TemplateArgIdx = templateArgIdx
	arg := call.Args[templateArgIdx]

	// Resolve template name(s)
	resolved.TemplateNames = resolveTemplateName(arg, info, stringAssignments)

	if len(resolved.TemplateNames) == 0 {
		return nil
	}

	return resolved
}

// inferTemplateArgIdx determines the likely index of the template argument
// based on the function call syntax.
func inferTemplateArgIdx(call *ast.CallExpr) int {
	switch call.Fun.(type) {
	case *ast.SelectorExpr:
		// Method call: obj.Render(template, ...)
		return 0
	case *ast.Ident:
		// Function call: Render(obj, template, ...)
		return -1
	default:
		return -1
	}
}

// findTemplateArg locates the template name argument in the call.
// If initial index is -1, searches for first string-like argument.
func findTemplateArg(
	call *ast.CallExpr,
	initialIdx int,
	stringAssignments map[string][]string,
) int {
	if initialIdx >= 0 {
		return initialIdx
	}

	// Search for first string argument or known string variable
	for i, arg := range call.Args {
		// String literal
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return i
		}

		// Variable with known string value
		if ident, ok := arg.(*ast.Ident); ok {
			if _, ok := stringAssignments[ident.Name]; ok {
				return i
			}
		}
	}

	return -1
}

// resolveTemplateName extracts template name(s) from an argument expression.
// Handles string literals, constants, and variables.
func resolveTemplateName(
	arg ast.Expr,
	info *types.Info,
	stringAssignments map[string][]string,
) []string {
	// Try direct string extraction
	if s := extractStringFast(arg); s != "" {
		return []string{s}
	}

	// Try identifier resolution
	ident, ok := arg.(*ast.Ident)
	if !ok {
		return nil
	}

	// Try constant resolution
	if info != nil {
		if obj := info.ObjectOf(ident); obj != nil {
			if c, ok := obj.(*types.Const); ok {
				val := c.Val()
				if val.Kind() == constant.String {
					return []string{constant.StringVal(val)}
				}
			}
		}
	}

	// Try variable resolution
	if vals, ok := stringAssignments[ident.Name]; ok {
		return vals
	}

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// SET CALL VARIABLE EXTRACTION
// ═══════════════════════════════════════════════════════════════════════════

// extractSetCallVarOptimized extracts template variable information from
// a context.Set() call. Validates the receiver type and extracts comprehensive
// type information including nested fields and documentation.
//
// Example: ctx.Set("user", user)
// Extracts: name="user", type, fields, documentation
func extractSetCallVarOptimized(
	call *ast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	seenPool *seenMapPool,
) *TemplateVar {
	// Must be method call
	sel, ok := call.Fun.(*ast.SelectorExpr)
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
		tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())

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
func isContextType(expr ast.Expr, info *types.Info, contextTypeName string) bool {
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
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
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
	return true, normalizeTypeStr(elem.String())
}

// checkMapType determines if a type is a map and extracts key/value type info.
func checkMapType(
	t types.Type,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
	tv *TemplateVar,
) (isMap bool, keyType string) {
	keyT, elemT := getMapTypes(t)
	if keyT == nil || elemT == nil {
		return false, ""
	}

	// Clear seen map for element type extraction
	clear(seen)

	tv.ElemType = normalizeTypeStr(elemT.String())
	tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemT, structIndex, fc, seen, fset, tv.Doc)
	return true, normalizeTypeStr(keyT.String())
}

// ═══════════════════════════════════════════════════════════════════════════
// STRUCT INDEX BUILDING (CONCURRENT)
// ═══════════════════════════════════════════════════════════════════════════

// buildStructIndex performs concurrent extraction of struct metadata across
// all AST files. The index maps each struct type to its documentation and
// field/method information.
//
// Two-pass algorithm:
// Pass 1: Extract struct fields and documentation (concurrent)
// Pass 2: Attach method documentation (sequential, typically small)
//
// Concurrency: Workers write directly to sync.Map to avoid coordination overhead.
func buildStructIndex(fset *token.FileSet, files map[string]*ast.File) map[structKeyHandle]structIndexEntry {
	numWorkers := max(runtime.NumCPU(), 1)
	fileChan := make(chan *ast.File, len(files))

	var sharedIndex sync.Map // Concurrent-safe map for worker writes
	var wg sync.WaitGroup

	// Pass 1: Extract struct fields concurrently
	for range numWorkers {
		wg.Add(1)
		go extractStructFieldsWorker(fileChan, fset, &sharedIndex, &wg)
	}

	// Feed files to workers
	for _, f := range files {
		fileChan <- f
	}
	close(fileChan)
	wg.Wait()

	// Convert sync.Map to regular map for fast O(1) reads
	finalIndex := convertSyncMapToMap(&sharedIndex, len(files))

	// Pass 2: Attach method documentation
	attachMethodDocs(files, fset, finalIndex)

	return finalIndex
}

// extractStructFieldsWorker is a worker function that processes files to extract
// struct type declarations and their field metadata.
func extractStructFieldsWorker(
	fileChan <-chan *ast.File,
	fset *token.FileSet,
	sharedIndex *sync.Map,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for f := range fileChan {
		pkgName := f.Name.Name

		ast.Inspect(f, func(n ast.Node) bool {
			genDecl, ok := n.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				return true
			}

			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}

				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}

				// Build struct index entry
				entry := structIndexEntry{
					doc:    extractTypeDoc(genDecl, typeSpec),
					fields: make(map[string]fieldInfo, len(structType.Fields.List)),
				}

				// Extract field metadata
				for _, field := range structType.Fields.List {
					pos := fset.Position(field.Pos())
					doc := extractFieldDoc(field)

					for _, name := range field.Names {
						entry.fields[name.Name] = fieldInfo{
							file: pos.Filename,
							line: pos.Line,
							col:  pos.Column,
							doc:  doc,
						}
					}
				}

				// Store in shared index
				key := unique.Make(pkgName + "." + typeSpec.Name.Name)
				sharedIndex.Store(key, entry)
			}

			return true
		})
	}
}

// convertSyncMapToMap converts sync.Map to regular map for optimized reads.
func convertSyncMapToMap(sharedIndex *sync.Map, estimatedSize int) map[structKeyHandle]structIndexEntry {
	finalIndex := make(map[structKeyHandle]structIndexEntry, estimatedSize*4)

	sharedIndex.Range(func(k, v any) bool {
		finalIndex[k.(structKeyHandle)] = v.(structIndexEntry)
		return true
	})

	return finalIndex
}

// attachMethodDocs walks all files to find method declarations and attach
// their documentation to the corresponding struct entries.
func attachMethodDocs(files map[string]*ast.File, fset *token.FileSet, index map[structKeyHandle]structIndexEntry) {
	for _, f := range files {
		pkgName := f.Name.Name

		ast.Inspect(f, func(n ast.Node) bool {
			funcDecl, ok := n.(*ast.FuncDecl)
			if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
				return true
			}

			// Extract receiver type name
			recvType := funcDecl.Recv.List[0].Type
			if starExpr, ok := recvType.(*ast.StarExpr); ok {
				recvType = starExpr.X
			}

			ident, ok := recvType.(*ast.Ident)
			if !ok {
				return true
			}

			// Find corresponding struct entry
			key := unique.Make(pkgName + "." + ident.Name)
			entry, exists := index[key]
			if !exists {
				return true
			}

			// Extract method documentation
			doc := ""
			if funcDecl.Doc != nil {
				doc = funcDecl.Doc.Text()
			}

			// Only update if we have documentation to add
			if doc != "" {
				pos := fset.Position(funcDecl.Pos())
				entry.fields[funcDecl.Name.Name] = fieldInfo{
					file: pos.Filename,
					line: pos.Line,
					col:  pos.Column,
					doc:  doc,
				}
			}

			return true
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// FIELD EXTRACTION WITH CACHING
// ═══════════════════════════════════════════════════════════════════════════

// extractFieldsWithDocs recursively extracts exported fields and methods from
// a type, leveraging caching to avoid redundant work. The seen map prevents
// infinite recursion on self-referential types.
//
// Caching strategy:
// - Each unique struct type is processed exactly once
// - Results are cached by structKeyHandle
// - Subsequent requests hit the cache
//
// Recursion handling:
// - seen map tracks types in current path
// - Prevents infinite loops on cyclic types
// - Copies for independent branches (slice elements)
func extractFieldsWithDocs(
	t types.Type,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
) ([]FieldInfo, string) {
	// Unwrap pointers and maps
	t = unwrapType(t)
	if t == nil {
		return nil, ""
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil, ""
	}

	handle := makeStructKey(named)

	// Cycle detection
	if seen[handle] {
		return nil, ""
	}
	seen[handle] = true

	// Check cache
	if cached, ok := fc.get(handle); ok {
		return cached.fields, cached.doc
	}

	// Extract fields (cache miss)
	fields, doc := extractFieldsUncached(named, handle, structIndex, fc, seen, fset)

	// Store in cache
	fc.set(handle, cachedFields{fields: fields, doc: doc})

	return fields, doc
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

// extractFieldsUncached performs the actual field extraction without cache lookup.
// Handles both struct types and interface types differently.
func extractFieldsUncached(
	named *types.Named,
	handle structKeyHandle,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
) ([]FieldInfo, string) {
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		// Interface or other named type: expose methods only
		return extractMethodFields(named, fset), ""
	}

	// Struct type: extract fields and methods
	entry := structIndex[handle]
	fields := extractStructFields(strct, entry, structIndex, fc, seen, fset)

	// Append methods
	fields = append(fields, extractMethodFields(named, fset)...)

	// Add method docs from struct index
	addMethodDocs(fields, entry)

	return fields, entry.doc
}

// extractStructFields processes all fields in a struct type.
func extractStructFields(
	strct *types.Struct,
	entry structIndexEntry,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
) []FieldInfo {
	fields := make([]FieldInfo, 0, strct.NumFields())

	for field := range strct.Fields() {
		if !field.Exported() {
			continue
		}

		fi := buildFieldInfo(field, entry, structIndex, fc, seen, fset)
		fields = append(fields, fi)
	}

	return fields
}

// buildFieldInfo constructs a FieldInfo for a single struct field.
func buildFieldInfo(
	field *types.Var,
	entry structIndexEntry,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
) FieldInfo {
	fi := FieldInfo{
		Name:    field.Name(),
		TypeStr: normalizeTypeStr(field.Type().String()),
	}

	// Set definition location
	if pos := field.Pos(); pos.IsValid() && fset != nil {
		position := fset.Position(pos)
		fi.DefFile = position.Filename
		fi.DefLine = position.Line
		fi.DefCol = position.Column
	}

	// Unwrap pointer
	ft := field.Type()
	if ptr, ok := ft.(*types.Pointer); ok {
		ft = ptr.Elem()
	}

	// Handle collection types
	if slice, ok := ft.(*types.Slice); ok {
		fi.IsSlice = true
		// Independent recursion branch for slice elements
		elemSeen := copySeenMap(seen)
		fi.Fields, _ = extractFieldsWithDocs(slice.Elem(), structIndex, fc, elemSeen, fset)
	} else if keyType, elemType := getMapTypes(ft); keyType != nil && elemType != nil {
		fi.IsMap = true
		fi.KeyType = normalizeTypeStr(keyType.String())
		fi.ElemType = normalizeTypeStr(elemType.String())
		// Independent recursion branch for map values
		elemSeen := copySeenMap(seen)
		fi.Fields, _ = extractFieldsWithDocs(elemType, structIndex, fc, elemSeen, fset)
	} else {
		// Regular field: continue with shared seen map
		fi.Fields, _ = extractFieldsWithDocs(ft, structIndex, fc, seen, fset)
	}

	// Add field documentation from index
	if pos, ok := entry.fields[field.Name()]; ok {
		if fi.DefFile == "" {
			fi.DefFile = pos.file
			fi.DefLine = pos.line
			fi.DefCol = pos.col
		}
		fi.Doc = pos.doc
	}

	return fi
}

// extractMethodFields extracts exported methods as FieldInfo entries.
func extractMethodFields(named *types.Named, fset *token.FileSet) []FieldInfo {
	fields := make([]FieldInfo, 0, named.NumMethods())

	for method := range named.Methods() {
		if !method.Exported() {
			continue
		}

		fi := FieldInfo{
			Name:    method.Name(),
			TypeStr: "method",
		}

		// Extract method signature
		if sig, ok := method.Type().(*types.Signature); ok {
			fi.Params, fi.Returns, _ = extractSignatureInfo(sig)
		}

		// Set definition location
		if pos := method.Pos(); pos.IsValid() && fset != nil {
			position := fset.Position(pos)
			fi.DefFile = position.Filename
			fi.DefLine = position.Line
			fi.DefCol = position.Column
		}

		fields = append(fields, fi)
	}

	return fields
}

// addMethodDocs enriches method FieldInfo entries with documentation from the index.
func addMethodDocs(fields []FieldInfo, entry structIndexEntry) {
	for i := range fields {
		fi := &fields[i]
		if fi.TypeStr != "method" {
			continue
		}

		if pos, ok := entry.fields[fi.Name]; ok {
			if fi.DefFile == "" {
				fi.DefFile = pos.file
				fi.DefLine = pos.line
				fi.DefCol = pos.col
			}
			fi.Doc = pos.doc
		}
	}
}

// copySeenMap creates an independent copy for separate recursion branches.
func copySeenMap(src map[structKeyHandle]bool) map[structKeyHandle]bool {
	dst := make(map[structKeyHandle]bool, len(src))
	maps.Copy(dst, src)
	return dst
}

// ═══════════════════════════════════════════════════════════════════════════
// MAP VARIABLE EXTRACTION
// ═══════════════════════════════════════════════════════════════════════════

// extractMapVars extracts template variables from a map composite literal.
// Example: map[string]interface{}{"user": user, "posts": posts}
// Returns: [TemplateVar{Name:"user",...}, TemplateVar{Name:"posts",...}]
func extractMapVars(
	expr ast.Expr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
) []TemplateVar {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}

	vars := make([]TemplateVar, 0, len(comp.Elts))

	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		keyLit, ok := kv.Key.(*ast.BasicLit)
		if !ok {
			continue
		}

		name := strings.Trim(keyLit.Value, `"`)
		tv := TemplateVar{Name: name}

		// Extract type information
		if typeInfo, ok := info.Types[kv.Value]; ok {
			// Clear seen map for this variable
			clear(seen)

			tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

			// Handle collection types
			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
				tv.IsMap = true
				tv.KeyType = normalizeTypeStr(keyType.String())
				tv.ElemType = normalizeTypeStr(elemType.String())
				tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			}
		} else {
			// Fallback: infer from AST
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		// Find definition location
		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)

		vars = append(vars, tv)
	}

	return vars
}

// ═══════════════════════════════════════════════════════════════════════════
// CONTEXT FILE ENRICHMENT
// ═══════════════════════════════════════════════════════════════════════════

// enrichRenderCallsWithContext augments RenderCall entries with variables
// defined in an external JSON context file. Also creates synthetic entries
// for templates defined in context but not found in code.
//
// Context file format:
//
//	{
//	  "template1.html": {"user": "User", "posts": "[]Post"},
//	  "template2.html": {"config": "Config"}
//	}
func enrichRenderCallsWithContext(
	calls []RenderCall,
	contextFile string,
	pkgs []*packages.Package,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	config AnalysisConfig,
	seenPool *seenMapPool,
) []RenderCall {
	// Load context file
	data, err := os.ReadFile(contextFile)
	if err != nil {
		return calls
	}

	var contextConfig map[string]map[string]string
	if err := json.Unmarshal(data, &contextConfig); err != nil {
		return calls
	}

	// Build type map from all packages
	typeMap := buildTypeMap(pkgs)

	// Build global variables
	globalVars := buildTemplateVarsOptimized(
		contextConfig[config.GlobalTemplateName],
		typeMap,
		structIndex,
		fc,
		fset,
		seenPool,
	)

	// Enrich existing calls
	seenTpls := make(map[string]bool, len(calls))
	calls = enrichExistingCalls(calls, contextConfig, globalVars, typeMap, structIndex, fc, fset, seenPool, seenTpls)

	// Add synthetic calls for templates in context but not in code
	calls = addSyntheticCalls(calls, contextConfig, globalVars, typeMap, structIndex, fc, fset, config, seenPool, seenTpls)

	return calls
}

// buildTypeMap creates a lookup map from type names to TypeName objects
// by traversing the package import graph via BFS.
func buildTypeMap(pkgs []*packages.Package) map[string]*types.TypeName {
	typeMap := make(map[string]*types.TypeName, len(pkgs)*32)
	visited := make(map[string]bool, len(pkgs)*32)
	queue := make([]*packages.Package, 0, len(pkgs)*8)

	// Initialize with root packages
	for _, pkg := range pkgs {
		if !visited[pkg.ID] {
			visited[pkg.ID] = true
			queue = append(queue, pkg)
		}
	}

	// BFS traversal
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]

		// Extract types from package
		if p.Types != nil {
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				if typeName, ok := scope.Lookup(name).(*types.TypeName); ok {
					typeMap[p.Types.Name()+"."+name] = typeName
				}
			}
		}

		// Add imports to queue
		for _, imp := range p.Imports {
			if !visited[imp.ID] {
				visited[imp.ID] = true
				queue = append(queue, imp)
			}
		}
	}

	return typeMap
}

// enrichExistingCalls adds context-defined variables to existing render calls.
func enrichExistingCalls(
	calls []RenderCall,
	contextConfig map[string]map[string]string,
	globalVars []TemplateVar,
	typeMap map[string]*types.TypeName,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	seenPool *seenMapPool,
	seenTpls map[string]bool,
) []RenderCall {
	for i, call := range calls {
		seenTpls[call.Template] = true

		// Combine global + template-specific + code-extracted variables
		base := make([]TemplateVar, 0, len(globalVars)+len(call.Vars)+8)
		base = append(base, globalVars...)

		if tplVars, ok := contextConfig[call.Template]; ok {
			base = append(base, buildTemplateVarsOptimized(tplVars, typeMap, structIndex, fc, fset, seenPool)...)
		}

		base = append(base, call.Vars...)
		calls[i].Vars = base
	}

	return calls
}

// addSyntheticCalls creates RenderCall entries for templates defined in
// context but not found in the codebase.
func addSyntheticCalls(
	calls []RenderCall,
	contextConfig map[string]map[string]string,
	globalVars []TemplateVar,
	typeMap map[string]*types.TypeName,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	config AnalysisConfig,
	seenPool *seenMapPool,
	seenTpls map[string]bool,
) []RenderCall {
	for tplName, tplVars := range contextConfig {
		// Skip global template and already-seen templates
		if tplName == config.GlobalTemplateName || seenTpls[tplName] {
			continue
		}

		// Build combined variables
		newVars := make([]TemplateVar, 0, len(globalVars)+len(tplVars))
		newVars = append(newVars, globalVars...)
		newVars = append(newVars, buildTemplateVarsOptimized(tplVars, typeMap, structIndex, fc, fset, seenPool)...)

		// Create synthetic entry
		calls = append(calls, RenderCall{
			File:     "context-file",
			Line:     1,
			Template: tplName,
			Vars:     newVars,
		})
	}

	return calls
}

// ═══════════════════════════════════════════════════════════════════════════
// TEMPLATE VARIABLE BUILDING
// ═══════════════════════════════════════════════════════════════════════════

// buildTemplateVarsOptimized constructs TemplateVar entries from type string
// definitions in the context file. Resolves types via typeMap and extracts
// full field information.
func buildTemplateVarsOptimized(
	varDefs map[string]string,
	typeMap map[string]*types.TypeName,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	seenPool *seenMapPool,
) []TemplateVar {
	vars := make([]TemplateVar, 0, len(varDefs))

	for name, typeStr := range varDefs {
		tv := TemplateVar{Name: name, TypeStr: typeStr}

		// Parse type string to identify base type
		baseTypeStr, isSlice := parseTypeString(typeStr)

		// Handle map types
		if strings.HasPrefix(baseTypeStr, "map[") {
			if idx := strings.IndexByte(baseTypeStr, ']'); idx != -1 {
				tv.IsMap = true
				tv.KeyType = strings.TrimSpace(baseTypeStr[4:idx])
				tv.ElemType = strings.TrimSpace(baseTypeStr[idx+1:])

				// Resolve element type fields
				valLookup := strings.TrimLeft(tv.ElemType, "*")
				if typeNameObj, ok := typeMap[valLookup]; ok {
					seen := seenPool.get()
					tv.Fields, tv.Doc = extractFieldsWithDocs(typeNameObj.Type(), structIndex, fc, seen, fset)
					seenPool.put(seen)
				}

				vars = append(vars, tv)
				continue
			}
		}

		// Resolve named type
		if typeNameObj, ok := typeMap[baseTypeStr]; ok {
			t := typeNameObj.Type()
			seen := seenPool.get()
			tv.Fields, tv.Doc = extractFieldsWithDocs(t, structIndex, fc, seen, fset)
			seenPool.put(seen)

			// Set definition location
			if pos := typeNameObj.Pos(); pos.IsValid() && fset != nil {
				position := fset.Position(pos)
				tv.DefFile = position.Filename
				tv.DefLine = position.Line
				tv.DefCol = position.Column
			}

			// Mark as slice if original type string indicated it
			if isSlice {
				tv.IsSlice = true
				tv.ElemType = baseTypeStr
			}
		} else if isSlice {
			// Unknown slice type
			tv.IsSlice = true
			tv.ElemType = baseTypeStr
		}

		vars = append(vars, tv)
	}

	return vars
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

// ═══════════════════════════════════════════════════════════════════════════
// UTILITY FUNCTIONS
// ═══════════════════════════════════════════════════════════════════════════

// findDefinitionLocation resolves the source location where an expression's
// value is defined. Prioritizes declarations over usages.
func findDefinitionLocation(expr ast.Expr, info *types.Info, fset *token.FileSet) (string, int, int) {
	var ident *ast.Ident

	// Extract identifier from expression
	switch e := expr.(type) {
	case *ast.Ident:
		ident = e
	case *ast.UnaryExpr:
		// &MyStruct{}
		if id, ok := e.X.(*ast.Ident); ok {
			ident = id
		}
	case *ast.CallExpr:
		// Function call: use call site
		pos := fset.Position(e.Pos())
		return pos.Filename, pos.Line, pos.Column
	case *ast.CompositeLit:
		// Composite literal: use literal site
		pos := fset.Position(e.Pos())
		return pos.Filename, pos.Line, pos.Column
	case *ast.SelectorExpr:
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

// normalizeTypeStr makes type strings more readable by removing package paths.
// Preserves [] and * prefixes.
// Example: "*github.com/user/pkg.MyType" → "*MyType"
func normalizeTypeStr(s string) string {
	var prefix strings.Builder
	base := s

	// Strip prefixes while preserving them
	for {
		switch {
		case strings.HasPrefix(base, "[]"):
			prefix.WriteString("[]")
			base = base[2:]
		case strings.HasPrefix(base, "*"):
			prefix.WriteString("*")
			base = base[1:]
		default:
			// Remove package path
			if idx := strings.LastIndex(base, "/"); idx >= 0 {
				base = base[idx+1:]
			}
			return prefix.String() + base
		}
	}
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

// extractTypeDoc retrieves documentation from type declaration.
// Checks genDecl.Doc, typeSpec.Doc, and typeSpec.Comment in order.
func extractTypeDoc(genDecl *ast.GenDecl, typeSpec *ast.TypeSpec) string {
	if genDecl.Doc != nil {
		return genDecl.Doc.Text()
	}
	if typeSpec.Doc != nil {
		return typeSpec.Doc.Text()
	}
	if typeSpec.Comment != nil {
		return typeSpec.Comment.Text()
	}
	return ""
}

// extractFieldDoc retrieves documentation from field declaration.
// Checks field.Doc and field.Comment in order.
func extractFieldDoc(field *ast.Field) string {
	if field.Doc != nil {
		return field.Doc.Text()
	}
	if field.Comment != nil {
		return field.Comment.Text()
	}
	return ""
}

// inferTypeFromAST makes a best-effort guess at the type based on AST structure.
// Used when type information is unavailable.
func inferTypeFromAST(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		switch e.Kind {
		case token.STRING:
			return "string"
		case token.INT:
			return "int"
		case token.FLOAT:
			return "float64"
		}
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return fmt.Sprintf("%v.%s", e.X, e.Sel.Name)
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			return fmt.Sprintf("call:%s", sel.Sel.Name)
		}
	case *ast.CompositeLit:
		if e.Type != nil {
			return fmt.Sprintf("%v", e.Type)
		}
	case *ast.UnaryExpr:
		return "unary"
	}
	return "unknown"
}

// isImportRelatedError checks if an error message is about import resolution.
// These errors are typically noise and not relevant to template analysis.
func isImportRelatedError(msg string) bool {
	lower := strings.ToLower(msg)
	importPhrases := []string{
		"could not import",
		"can't find import",
		"cannot find package",
		"no required module provides",
	}

	for _, phrase := range importPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// resolveFuncDefLocation finds the definition location of a function value.
// For named functions, resolves to declaration site.
// For literals, returns literal position.
func resolveFuncDefLocation(expr ast.Expr, info *types.Info, fset *token.FileSet) (file string, line, col int) {
	if fset == nil {
		return
	}

	switch e := expr.(type) {
	case *ast.Ident:
		if info != nil {
			if obj := info.ObjectOf(e); obj != nil && obj.Pos().IsValid() {
				pos := fset.Position(obj.Pos())
				return pos.Filename, pos.Line, pos.Column
			}
		}
	case *ast.SelectorExpr:
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
func resolveFuncDoc(expr ast.Expr, info *types.Info, filesMap map[string]*ast.File) string {
	if info == nil {
		return ""
	}

	var obj types.Object
	switch e := expr.(type) {
	case *ast.Ident:
		obj = info.ObjectOf(e)
	case *ast.SelectorExpr:
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
			fd, ok := decl.(*ast.FuncDecl)
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

// extractFuncMaps extracts function definitions from a FuncMap composite literal.
// Example: template.FuncMap{"add": addFunc, "multiply": multiplyFunc}
func extractFuncMaps(comp *ast.CompositeLit, info *types.Info, fset *token.FileSet, filesMap map[string]*ast.File) []FuncMapInfo {
	var result []FuncMapInfo

	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		key, ok := kv.Key.(*ast.BasicLit)
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

	for i := range sig.Params().Len() {
		p := sig.Params().At(i)
		ts := normalizeTypeStr(p.Type().String())
		params[i] = ParamInfo{Name: p.Name(), TypeStr: ts}
		args[i] = ts
	}

	// Extract return types
	returns = make([]ParamInfo, sig.Results().Len())

	for i := range sig.Results().Len() {
		r := sig.Results().At(i)
		ts := normalizeTypeStr(r.Type().String())
		returns[i] = ParamInfo{Name: r.Name(), TypeStr: ts}
	}

	return
}

// isRenderCall checks if a call expression is a template render call
// based on configured function names.
func isRenderCall(call *ast.CallExpr, config AnalysisConfig) bool {
	funcName := ""

	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		funcName = fn.Sel.Name
	case *ast.Ident:
		funcName = fn.Name
	}

	return (funcName == config.RenderFunctionName || funcName == config.ExecuteTemplateFunctionName) &&
		len(call.Args) >= 2
}

// isFuncMapType checks if an identifier has type template.FuncMap.
func isFuncMapType(ident *ast.Ident, info *types.Info) bool {
	if info == nil {
		return false
	}

	if tv, ok := info.Types[ident]; ok && tv.Type != nil {
		return strings.HasSuffix(tv.Type.String(), "template.FuncMap")
	}

	return false
}

// isFuncMapCompositeLit checks if a composite literal is of type template.FuncMap.
func isFuncMapCompositeLit(comp *ast.CompositeLit, info *types.Info) bool {
	if info == nil {
		return false
	}

	if tv, ok := info.Types[comp]; ok && tv.Type != nil {
		return strings.HasSuffix(tv.Type.String(), "template.FuncMap")
	}

	return false
}

// extractFieldsWithDocsPreservingDoc extracts fields while preserving
// existing documentation if new extraction returns empty doc.
func extractFieldsWithDocsPreservingDoc(
	t types.Type,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
	existingDoc string,
) ([]FieldInfo, string) {
	fields, doc := extractFieldsWithDocs(t, structIndex, fc, seen, fset)
	if doc == "" {
		doc = existingDoc
	}
	return fields, doc
}

// extractStringFast efficiently extracts string value from a BasicLit.
// Optimized to avoid allocations by direct slicing.
func extractStringFast(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}

	// Valid string literal must have at least 2 chars (quotes)
	if len(lit.Value) < 2 {
		return ""
	}

	// Slice to remove surrounding quotes
	return lit.Value[1 : len(lit.Value)-1]
}

// FindGoFiles recursively finds all .go files in a directory tree.
func FindGoFiles(root string) ([]string, error) {
	var files []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, don't fail entire walk
		}

		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}

		return nil
	})
	return files, err
}
