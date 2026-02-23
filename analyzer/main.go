package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/rex-template-analyzer/validator"
)

// ValidationOutput represents the JSON structure emitted when
// template validation is enabled.
//
// It combines static analysis results with template validation
// diagnostics.
type ValidationOutput struct {
	// RenderCalls contains all detected template render invocations.
	RenderCalls []validator.RenderCall `json:"renderCalls"`

	// FuncMaps contains discovered template function maps.
	FuncMaps []validator.FuncMapInfo `json:"funcMaps"`

	// ValidationErrors contains template-to-render-call mismatches.
	ValidationErrors []validator.ValidationResult `json:"validationErrors"`

	// Errors contains non-fatal analysis errors (optional).
	Errors []string `json:"errors,omitempty"`
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
	showNamedTemplates := flag.Bool("named-templates", false, "Return all named template as JSON")
	flag.Parse()

	// Resolve absolute paths
	absDir := mustAbs(*dir)

	templateBase := absDir
	if *templateBaseDir != "" {
		templateBase = mustAbs(*templateBaseDir)
	}

	// Run static analysis on the source directory
	result := validator.AnalyzeDir(absDir, *contextFile, validator.DefaultConfig)

	// Filter out import-related noise
	result.Errors = filterImportErrors(result.Errors)

	// Prepare output payload
	var output any

	if *validate || *showNamedTemplates {
		ve, namedTemplates := validator.ValidateTemplates(
			result.RenderCalls,
			templateBase,
			*templateRoot,
		)
		if *showNamedTemplates {
			keys := make([]string, 0, len(namedTemplates))
			for k := range namedTemplates {
				keys = append(keys, k)
			}
			output = keys
		} else {
			// Produce extended output with validation results
			output = ValidationOutput{
				RenderCalls:      result.RenderCalls,
				FuncMaps:         result.FuncMaps,
				ValidationErrors: ve,
				Errors:           result.Errors,
			}
		}
	} else {
		// Emit raw analysis result
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
