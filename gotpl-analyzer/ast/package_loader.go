package ast

import (
	"fmt"
	goast "go/ast"
	"go/token"
	"go/types"
	"maps"
	"strings"

	"golang.org/x/tools/go/packages"
)

// mergeTypeInfo consolidates type information from all loaded packages into
// a single unified types.Info structure. This enables cross-package type
// resolution during analysis.
//
// Also collects all AST files and non-import-related errors.
//
// Performance: Skips vendor and generated code directories to reduce processing time.
func mergeTypeInfo(pkgs []*packages.Package, result *AnalysisResult) (*types.Info, []*goast.File) {
	// Pre-calculate total sizes to avoid map growth
	totalTypes, totalDefs, totalUses := 0, 0, 0
	for _, pkg := range pkgs {
		// Skip vendor and generated code for performance
		if shouldSkipPackage(pkg.PkgPath) {
			continue
		}

		if pkg.TypesInfo != nil {
			totalTypes += len(pkg.TypesInfo.Types)
			totalDefs += len(pkg.TypesInfo.Defs)
			totalUses += len(pkg.TypesInfo.Uses)
		}
	}

	// Create unified type info with pre-sized maps
	info := &types.Info{
		Types: make(map[goast.Expr]types.TypeAndValue, totalTypes),
		Defs:  make(map[*goast.Ident]types.Object, totalDefs),
		Uses:  make(map[*goast.Ident]types.Object, totalUses),
	}

	// Estimate file count
	allFiles := make([]*goast.File, 0, totalTypes/10+len(pkgs))

	// Merge all package data
	for _, pkg := range pkgs {
		// Skip vendor and generated code
		if shouldSkipPackage(pkg.PkgPath) {
			continue
		}

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

// shouldSkipPackage determines if a package should be skipped for performance reasons.
// Skips vendor directories and common generated code patterns.
func shouldSkipPackage(pkgPath string) bool {
	lower := strings.ToLower(pkgPath)

	// Skip vendor directories
	if strings.Contains(lower, "/vendor/") || strings.Contains(lower, "\\vendor\\") {
		return true
	}

	// Skip generated code directories
	if strings.Contains(lower, "/generated/") || strings.Contains(lower, "\\generated\\") {
		return true
	}

	// Skip common generated package suffixes
	if strings.HasSuffix(lower, "_generated") || strings.HasSuffix(lower, ".pb") {
		return true
	}

	// Skip test packages (already handled by Tests: false in config)
	if strings.HasSuffix(lower, "_test") {
		return true
	}

	return false
}

// buildFileMap creates a fast lookup map from filename to AST file.
// This enables quick file resolution when processing type definitions.
func buildFileMap(files []*goast.File, fset *token.FileSet) map[string]*goast.File {
	filesMap := make(map[string]*goast.File, len(files))
	for _, f := range files {
		if pos := fset.File(f.Pos()); pos != nil {
			filesMap[pos.Name()] = f
		}
	}
	return filesMap
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
