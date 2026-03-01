// Package ast performs comprehensive static analysis on Go source code to extract:
// 1. Template render calls with their associated data variables
// 2. Template function maps (custom functions available in templates)
// 3. Template variable definitions from context setters
package ast

import (
	"fmt"
	goast "go/ast"
	"go/token"
	"go/types"
	"sync"

	"golang.org/x/tools/go/packages"
)

// Package-level cache for type information to speed up repeated analysis
var (
	packageCacheMu sync.RWMutex
	packageCache   = make(map[string]*cacheEntry)
)

// cacheEntry stores cached package analysis results
type cacheEntry struct {
	filesMap    map[string]*goast.File
	structIndex map[string]structIndexEntry
}

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

	// Check cache first for performance
	cacheKey := dir
	packageCacheMu.RLock()
	cached, hasCached := packageCache[cacheKey]
	packageCacheMu.RUnlock()

	var filesMap map[string]*goast.File
	var structIndex map[string]structIndexEntry
	var info *types.Info
	var allFiles []*goast.File

	if hasCached {
		// Use cached data (filesMap and structIndex are immutable after creation)
		filesMap = cached.filesMap
		structIndex = cached.structIndex

		// Still need to load packages for type info (but we skip some processing)
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
				packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
				packages.NeedImports,
			Dir:   dir,
			Fset:  fset,
			Tests: false,
		}

		pkgs, err := packages.Load(cfg, "./...")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
			return result
		}

		info, allFiles = mergeTypeInfo(pkgs, &result)
	} else {
		// Full analysis path
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
				packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
				packages.NeedImports,
			Dir:   dir,
			Fset:  fset,
			Tests: false,
		}

		pkgs, err := packages.Load(cfg, "./...")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
			return result
		}

		info, allFiles = mergeTypeInfo(pkgs, &result)
		filesMap = buildFileMap(allFiles, fset)
		structIndex = buildStructIndex(fset, filesMap)

		// Cache the immutable data structures
		packageCacheMu.Lock()
		packageCache[cacheKey] = &cacheEntry{
			filesMap:    filesMap,
			structIndex: structIndex,
		}
		packageCacheMu.Unlock()
	}

	// Initialize shared infrastructure
	fc := newFieldCache()
	seenPool := newSeenMapPool()

	// Phase 2: Collect function scopes (concurrent)
	scopes := collectFuncScopesOptimized(allFiles, info, fset, structIndex, fc, config, filesMap, seenPool)

	// Phase 3: Identify global implicit variables
	globalImplicitVars := extractGlobalImplicitVars(scopes)

	// Phase 4: Generate render calls from collected scopes
	result.RenderCalls = generateRenderCalls(scopes, globalImplicitVars, info, fset, dir, structIndex, fc, seenPool)

	// Phase 5: Aggregate and deduplicate function maps
	result.FuncMaps = aggregateFuncMaps(scopes)

	// Phase 6: Enrich with external context if provided
	if contextFile != "" {
		// Need to reload packages for context enrichment
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
				packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
				packages.NeedImports,
			Dir:   dir,
			Fset:  fset,
			Tests: false,
		}

		pkgs, err := packages.Load(cfg, "./...")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("context load error: %v", err))
			return result
		}

		result.RenderCalls = enrichRenderCallsWithContext(
			result.RenderCalls, contextFile, pkgs, structIndex, fc, fset, config, seenPool,
		)
	}

	return result
}

// ClearCache clears the package cache. Useful for testing or when analyzing
// the same directory multiple times with different configurations.
func ClearCache() {
	packageCacheMu.Lock()
	packageCache = make(map[string]*cacheEntry)
	packageCacheMu.Unlock()
}

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
