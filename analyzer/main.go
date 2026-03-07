/*
Package main provides the command-line interface for the Rex template analyzer.

It can operate in two modes:

# CLI mode (default)

Performs a one-shot static analysis on a Go source directory and writes the
results as JSON to stdout. Useful for scripting, CI, and editor integrations
that prefer process-per-request over a long-lived daemon.

# LSP daemon mode (--lsp)

Starts a long-lived JSON-RPC 2.0 server on stdin/stdout that implements the
LSP wire protocol (Content-Length framing). The extension communicates with
this daemon to fetch analysis data on demand, avoiding the cost of a full
upfront analysis and the memory pressure of keeping a giant knowledge graph
alive in the extension process.

Supported custom methods:

	rex/getTemplateContext   — variable list for a single template (fast path)
	rex/validate             — diagnostics for a single template file
	rex/getFuncMaps          — all template.FuncMap entries in the workspace
	rex/getNamedBlocks       — all {{define}} / {{block}} declarations
	rex/getRenderCalls       — full render-call list (heavier; prefer getTemplateContext)
	rex/invalidateCache      — evict cached analysis (send after Go file changes)

Standard LSP methods handled:

	initialize               — returns server capabilities
	initialized              — no-op acknowledgement
	shutdown / exit          — graceful termination
	workspace/didChangeWatchedFiles — invalidates cache for modified directories
	$/cancelRequest          — no-op (requests run to completion)
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
	"github.com/rex-template-analyzer/lsp"
	"github.com/rex-template-analyzer/validator"
)

// ValidationOutput represents the JSON structure emitted when
// template validation is enabled.
//
// It combines static analysis results with template validation
// diagnostics.
type ValidationOutput struct {
	// RenderCalls contains all detected template render invocations.
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
}

// main is the CLI / LSP daemon entry point.
func main() {
	// ── Flags ─────────────────────────────────────────────────────────────

	lspMode := flag.Bool("lsp", false,
		"Run as a JSON-RPC 2.0 LSP daemon on stdin/stdout instead of performing a one-shot analysis")

	dir := flag.String("dir", ".", "Go source directory to analyze")
	templateRoot := flag.String("template-root", "", "Root directory for templates")
	templateBaseDir := flag.String("template-base-dir", "", "Base directory for template-root")
	validate := flag.Bool("validate", false, "Validate templates against render calls")
	contextFile := flag.String("context-file", "", "Path to JSON file with additional context variables")
	compress := flag.Bool("compress", false, "Output gzip-compressed JSON")
	showNamedTemplates := flag.Bool("named-templates", false, "Return all named template as JSON")
	viewContext := flag.String("view-context", "", "Show context for a specific template")

	flag.Parse()

	// ── LSP daemon mode ───────────────────────────────────────────────────

	if *lspMode {
		server := lsp.NewServer()
		server.Serve()
		return
	}

	// ── One-shot CLI mode ─────────────────────────────────────────────────

	absDir := mustAbs(*dir)

	templateBase := absDir
	if *templateBaseDir != "" {
		templateBase = mustAbs(*templateBaseDir)
	}

	result := ast.AnalyzeDir(absDir, *contextFile, ast.DefaultConfig)

	if *viewContext != "" {
		handleViewContext(result, *viewContext, *compress)
		return
	}

	result.Errors = filterImportErrors(result.Errors)

	var output any

	if *validate || *showNamedTemplates {
		ve, namedBlocks, namedBlockErrors := validator.ValidateTemplates(
			result.RenderCalls,
			templateBase,
			*templateRoot,
		)
		if *showNamedTemplates {
			keys := make([]string, 0, len(namedBlocks))
			for k := range namedBlocks {
				keys = append(keys, k)
			}
			output = keys
		} else {
			output = ValidationOutput{
				RenderCalls:      result.RenderCalls,
				FuncMaps:         result.FuncMaps,
				ValidationErrors: ve,
				Errors:           result.Errors,
				NamedBlocks:      namedBlocks,
				NamedBlockErrors: namedBlockErrors,
			}
		}
	} else {
		output = result
	}

	encodeJSON(output, *compress)
}

// encodeJSON serializes output as JSON and writes it to stdout.
func encodeJSON(output any, compress bool) {
	if compress {
		writeGzipJSON(output)
		return
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "")
	if err := enc.Encode(output); err != nil {
		panic("failed to encode JSON: " + err.Error())
	}
}

// writeGzipJSON writes gzip-compressed JSON to stdout.
func writeGzipJSON(output any) {
	gzWriter := gzip.NewWriter(os.Stdout)
	defer gzWriter.Close()

	enc := json.NewEncoder(gzWriter)
	enc.SetIndent("", "")
	if err := enc.Encode(output); err != nil {
		panic("failed to encode JSON: " + err.Error())
	}
	if err := gzWriter.Close(); err != nil {
		panic("failed to close gzip writer: " + err.Error())
	}
}

// mustAbs resolves path to an absolute path, panicking on failure.
func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic("could not resolve absolute path for " + path + ": " + err.Error())
	}
	return abs
}

// filterImportErrors removes known import-related errors from the list.
func filterImportErrors(errs []string) []string {
	filtered := make([]string, 0, len(errs))
	for _, e := range errs {
		if !isImportError(e) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// isImportError determines whether an error message corresponds to a
// dependency or import resolution failure.
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
// the context variables.
func handleViewContext(result ast.AnalysisResult, templateName string, compress bool) {
	type ContextInfo struct {
		File string            `json:"file"`
		Line int               `json:"line"`
		Vars []ast.TemplateVar `json:"vars"`
	}

	var foundContexts []ContextInfo

	for _, rc := range result.RenderCalls {
		if rc.Template == templateName ||
			strings.HasSuffix(rc.Template, "/"+templateName) ||
			strings.HasSuffix(rc.Template, "\\"+templateName) {
			vars := rc.Vars
			if vars == nil {
				vars = []ast.TemplateVar{}
			}
			foundContexts = append(foundContexts, ContextInfo{
				File: rc.File,
				Line: rc.Line,
				Vars: vars,
			})
		}
	}

	if len(foundContexts) == 0 {
		encodeJSON([]ContextInfo{}, compress)
		return
	}

	encodeJSON(foundContexts, compress)
}
