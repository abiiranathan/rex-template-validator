// Package ast performs comprehensive static analysis on Go source code to extract:
// 1. Template render calls with their associated data variables
// 2. Template function maps (custom functions available in templates)
// 3. Template variable definitions from context setters
package ast

import (
	"fmt"
	goast "go/ast"
	"go/token"

	"golang.org/x/tools/go/packages"
)

// AnalyzeDir performs comprehensive static analysis on Go source code and returns
// an AnalysisResult containing all discovered template-related information.
//
// Performance strategy (fastest first):
//  1. Disk cache hit  → deserialise gzip-JSON (~150 ms), skip packages.Load entirely.
//  2. In-process cache hit → skip struct-index rebuild, still calls packages.Load once.
//  3. Cold path → single packages.Load, build indexes, write disk cache for next run.
//
// Previously the function called packages.Load 2–3 times per invocation (main
// analysis + optional context-enrichment reload).  It now loads exactly once and
// passes the pkgs slice to every downstream step, eliminating the redundant
// packages.Load that previously happened inside the context-enrichment branch.
func AnalyzeDir(dir string, contextFile string, config AnalysisConfig) AnalysisResult {
	// ── 1. Disk cache ────────────────────────────────────────────────────────
	if diskCached, ok := ReadDiskCache(dir, contextFile); ok {
		return *diskCached
	}

	result := AnalysisResult{}

	// ── 2. Load packages – exactly once ──────────────────────────────────────
	fset := token.NewFileSet()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Dir:   dir,
		Fset:  fset,
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
		return result
	}

	info, allFiles := mergeTypeInfo(pkgs, &result)

	var filesMap map[string]*goast.File
	var structIndex map[string]structIndexEntry

	filesMap = buildFileMap(allFiles, fset)
	structIndex = buildStructIndex(fset, filesMap)

	// ── 4. Shared analysis infrastructure ────────────────────────────────────
	fc := newFieldCache()
	seenPool := newSeenMapPool()

	// ── 5. Collect function scopes (concurrent) ───────────────────────────────
	scopes := collectFuncScopesOptimized(allFiles, info, fset, structIndex, fc, config, filesMap, seenPool)

	// ── 6. Extract global implicit variables ──────────────────────────────────
	globalImplicitVars := extractGlobalImplicitVars(scopes)

	// ── 7. Generate render calls ──────────────────────────────────────────────
	result.RenderCalls = generateRenderCalls(scopes, globalImplicitVars, info, fset, dir, structIndex, fc, seenPool)

	// ── 8. Aggregate function maps ────────────────────────────────────────────
	result.FuncMaps = aggregateFuncMaps(scopes)

	// ── 9. Context enrichment – reuse already-loaded pkgs, no second Load! ───
	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(
			result.RenderCalls, contextFile, pkgs, structIndex, fc, fset, config, seenPool,
		)
	}

	// ── 10. Persist to disk cache for future cold starts ─────────────────────
	// Synchronous write (inside AnalyzeDir before return) prevents data races
	// with callers that modify the returned result (e.g. Flatten).
	WriteDiskCache(dir, contextFile, result)

	return result
}



// extractGlobalImplicitVars identifies template variables that are set outside
// any render call context (e.g. in middleware functions).  These are available
// to every template.
func extractGlobalImplicitVars(scopes []FuncScope) []TemplateVar {
	var globalVars []TemplateVar
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 && len(scope.SetVars) > 0 {
			globalVars = append(globalVars, scope.SetVars...)
		}
	}
	return globalVars
}

// aggregateFuncMaps collects all function-map definitions from scopes and
// deduplicates by name.
func aggregateFuncMaps(scopes []FuncScope) []FuncMapInfo {
	total := 0
	for _, scope := range scopes {
		total += len(scope.FuncMaps)
	}

	all := make([]FuncMapInfo, 0, total)
	for _, scope := range scopes {
		all = append(all, scope.FuncMaps...)
	}

	seen := make(map[string]bool, len(all))
	unique := make([]FuncMapInfo, 0, len(all))
	for _, fm := range all {
		if !seen[fm.Name] {
			seen[fm.Name] = true
			unique = append(unique, fm)
		}
	}
	return unique
}
