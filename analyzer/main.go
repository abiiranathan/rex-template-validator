/*
Package main provides the command-line interface for the Rex template analyzer.

It performs static analysis on Go source code to identify template rendering
invocations, function map declarations, and validate templates against their
corresponding render calls.

The analyzer can output various forms of JSON results, including detected
render calls, function maps, validation errors, and named template blocks.
It supports gzip compression for the output.
*/
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

// ValidationOutput represents the JSON structure emitted when
// template validation is enabled.
//
// It combines static analysis results with template validation
// diagnostics.
type ValidationOutput struct {
	// RenderCalls contains all detected template render invocations.
	// Variable field trees are stripped; consumers resolve types via Types.
	RenderCalls []ast.RenderCall `json:"renderCalls"`

	// FuncMaps contains discovered template function maps.
	FuncMaps []ast.FuncMapInfo `json:"funcMaps"`

	// ValidationErrors contains template-to-render-call mismatches.
	ValidationErrors []validator.ValidationResult `json:"validationErrors"`

	// Errors contains non-fatal analysis errors (optional).
	Errors []string `json:"errors,omitempty"`

	// NamedBlocks contains all defined blocks across the project.
	NamedBlocks map[string][]validator.NamedBlockEntry `json:"namedBlocks"`

	// NamedBlockErrors contains duplicate block declarations.
	NamedBlockErrors []validator.NamedBlockDuplicateError `json:"namedBlockErrors"`

	// Types is the global type registry: each named type is stored once with
	// its direct fields. Consumers reconstruct the full type hierarchy by
	// recursively looking up each field's TypeStr in this map.
	Types map[string][]ast.FieldInfo `json:"types,omitempty"`
}

// main is the CLI entry point for the template analyzer.
func main() {
	// Command-line flags
	dir := flag.String("dir", ".", "Go source directory to analyze")
	templateRoot := flag.String("template-root", "", "Root directory for templates")
	templateBaseDir := flag.String("template-base-dir", "", "Base directory for template-root")
	validate := flag.Bool("validate", false, "Validate templates against render calls")
	contextFile := flag.String("context-file", "", "Path to JSON file with additional context variables")
	compress := flag.Bool("compress", false, "Output gzip-compressed JSON")
	daemon := flag.Bool("daemon", false, "Run as a long-lived JSON-RPC daemon over stdio")
	showNamedTemplates := flag.Bool("named-templates", false, "Return all named template as JSON")
	viewContext := flag.String("view-context", "", "Show context for a specific template")
	flag.Parse()

	if *daemon {
		if err := runDaemon(os.Stdin, os.Stdout); err != nil {
			panic("daemon failed: " + err.Error())
		}
		return
	}

	// Resolve absolute paths
	absDir := mustAbs(*dir)

	templateBase := absDir
	if *templateBaseDir != "" {
		templateBase = mustAbs(*templateBaseDir)
	}

	// Run static analysis on the source directory.
	result := ast.AnalyzeDir(absDir, *contextFile, ast.DefaultConfig)

	// view-context outputs the full variable context (including inline field
	// trees) for a single template so the editor extension can render hover
	// and autocomplete information. Do NOT flatten before this call.
	if *viewContext != "" {
		handleViewContext(result, *viewContext, *compress)
		return
	}

	// Filter out import-related noise
	result.Errors = filterImportErrors(result.Errors)

	// Prepare output payload
	var output any

	if *validate || *showNamedTemplates {
		// Validation reads inline field trees from render call variables to
		// build per-template variable maps. Flatten AFTER validation completes
		// so those trees are available throughout the validation pass.
		ve, namedBlocks, namedBlockErrors := validator.ValidateTemplates(
			result.RenderCalls,
			result.FuncMaps,
			templateBase,
			*templateRoot,
		)

		// Build the type registry and strip inline field trees before
		// serialization to keep the JSON payload small.
		result.Flatten()

		if *showNamedTemplates {
			keys := make([]string, 0, len(namedBlocks))
			for k := range namedBlocks {
				keys = append(keys, k)
			}
			output = keys
		} else {
			// Produce extended output with validation results.
			output = ValidationOutput{
				RenderCalls:      result.RenderCalls,
				FuncMaps:         result.FuncMaps,
				ValidationErrors: ve,
				Errors:           result.Errors,
				NamedBlocks:      namedBlocks,
				NamedBlockErrors: namedBlockErrors,
				Types:            result.Types,
			}
		}
	} else {
		// Raw analysis output: build the registry and flatten before encoding.
		result.Flatten()
		output = result
	}

	// Encode and write JSON output
	encodeJSON(output, *compress)
}

// encodeJSON serializes output as JSON and writes it to stdout.
//
// If compress is true, the output is gzip-compressed.
func encodeJSON(output any, compress bool) {
	if compress {
		writeGzipJSON(output)
		return
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "") // disable indent (reduces size by > 2x)

	if err := enc.Encode(output); err != nil {
		panic("failed to encode JSON: " + err.Error())
	}
}

// writeGzipJSON writes gzip-compressed JSON to stdout.
func writeGzipJSON(output any) {
	gzWriter := gzip.NewWriter(os.Stdout)
	defer gzWriter.Close()

	enc := json.NewEncoder(gzWriter)
	enc.SetIndent("", "") // disable indent (reduces size by > 2x)

	if err := enc.Encode(output); err != nil {
		panic("failed to encode JSON: " + err.Error())
	}

	if err := gzWriter.Close(); err != nil {
		panic("failed to close gzip writer: " + err.Error())
	}
}

// mustAbs resolves path to an absolute path.
//
// The program panics if resolution fails, since relative paths
// would invalidate downstream analysis.
func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic("could not resolve absolute path for " + path + ": " + err.Error())
	}
	return abs
}

// filterImportErrors removes known import-related errors
// from the analysis error list.
//
// These errors are typically environmental and not actionable
// for template validation.
func filterImportErrors(errs []string) []string {
	filtered := make([]string, 0, len(errs))
	for _, e := range errs {
		if !isImportError(e) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// isImportError determines whether an error message
// corresponds to a dependency/import failure.
func isImportError(e string) bool {
	lower := strings.ToLower(e)

	for _, phrase := range []string{
		"could not import",
		"can't find import",
		"cannot find package",
		"no required module provides",
		"build constraints exclude all go files",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// handleViewContext filters render calls for a specific template and outputs
// the full variable context including inline field trees. This endpoint is
// intentionally not flattened so the caller receives complete type information
// for hover and autocomplete features.
func handleViewContext(result ast.AnalysisResult, templateName string, compress bool) {
	type ContextInfo struct {
		File string            `json:"file"`
		Line int               `json:"line"`
		Vars []ast.TemplateVar `json:"vars"`
	}

	foundContexts := []ContextInfo{}

	for _, rc := range result.RenderCalls {
		// Check for exact match or suffix match (to handle relative paths vs absolute/partial paths)
		if rc.Template == templateName || strings.HasSuffix(rc.Template, "/"+templateName) || strings.HasSuffix(rc.Template, "\\"+templateName) {
			// Avoid NULLs
			if rc.Vars == nil {
				rc.Vars = []ast.TemplateVar{}
			}
			foundContexts = append(foundContexts, ContextInfo{
				File: rc.File,
				Line: rc.Line,
				Vars: rc.Vars,
			})
		}
	}

	if len(foundContexts) == 0 {
		// Output empty list to indicate no context found
		encodeJSON([]ContextInfo{}, compress)
		return
	}

	encodeJSON(foundContexts, compress)
}
