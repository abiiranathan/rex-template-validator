/*
Package validator performs static analysis on Go templates to identify potential issues.

It analyzes template render calls, discovers template function maps, and validates
template usages against their defined contexts. This package also includes
functionality to extract and validate named template blocks (`define` and `block` actions).

The validator provides:
  - Detection of undefined template variables
  - Validation of field access paths
  - Detection of missing template files and named blocks
  - Duplicate named block detection
  - Scope-aware validation (handles with, range, if blocks)
  - Support for nested templates and partials
*/
package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/rex-template-analyzer/ast"
)

// ValidateTemplates validates all templates against their render calls.
// This is the main entry point for template validation.
//
// Validation process:
//  1. Parse all named blocks from template directory (concurrent)
//  2. Detect duplicate block definitions
//  3. Validate each render call against its template (concurrent)
//  4. Aggregate all errors
//
// For each render call, the validator:
//   - Locates the template file or named block
//   - Checks all variable references in template actions
//   - Validates field access paths
//   - Validates nested template calls (recursively)
//   - Tracks scope changes (with, range, if blocks)
//
// Parameters:
//   - renderCalls: Slice of render calls discovered by static analysis
//   - baseDir: Root directory of the project
//   - templateRoot: Subdirectory containing template files
//
// Returns:
//   - allErrors: All validation errors found across all templates
//   - namedBlocks: Registry of all named block definitions
//   - namedBlockErrors: Errors related to duplicate block definitions
//
// Concurrency: Render calls are validated concurrently for better performance.
// Thread-safety: Results are aggregated safely using channels.
func ValidateTemplates(renderCalls []ast.RenderCall, baseDir string, templateRoot string) ([]ValidationResult, map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	// Phase 1: Parse all named blocks (concurrent)
	namedBlocks, namedBlockErrors := parseAllNamedTemplates(baseDir, templateRoot)

	// Phase 2: Validate all render calls (concurrent)
	allErrors := validateRenderCallsConcurrently(renderCalls, baseDir, templateRoot, namedBlocks)

	return allErrors, namedBlocks, namedBlockErrors
}

// validateRenderCallsConcurrently validates multiple render calls concurrently
// using a worker pool pattern.
//
// Concurrency model:
//   - One worker per CPU core
//   - Each worker validates a subset of render calls
//   - Results are collected via channels and aggregated
//   - No shared mutable state between workers
//
// Thread-safety: Each worker operates on independent data. Results are
// aggregated sequentially from the result channel.
func validateRenderCallsConcurrently(
	renderCalls []ast.RenderCall,
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
) []ValidationResult {
	if len(renderCalls) == 0 {
		return nil
	}

	// Setup worker pool
	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (len(renderCalls) + numWorkers - 1) / numWorkers
	resultChan := make(chan []ValidationResult, numWorkers)
	var wg sync.WaitGroup

	// Spawn workers
	for w := range numWorkers {
		start := w * chunkSize
		if start >= len(renderCalls) {
			break
		}
		end := min(start+chunkSize, len(renderCalls))
		chunk := renderCalls[start:end]

		wg.Go(func() {
			validateRenderCallsWorker(chunk, baseDir, templateRoot, namedBlocks, resultChan)
		})
	}

	// Close result channel when all workers complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Aggregate results
	var allErrors []ValidationResult
	for errors := range resultChan {
		allErrors = append(allErrors, errors...)
	}

	return allErrors
}

// validateRenderCallsWorker validates a chunk of render calls.
// This is the worker function used in the concurrent validation pool.
//
// Each worker independently validates its assigned render calls without
// accessing shared mutable state, ensuring thread-safety.
func validateRenderCallsWorker(
	chunk []ast.RenderCall,
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
	resultChan chan<- []ValidationResult,
) {
	var errors []ValidationResult

	for _, rc := range chunk {
		templatePath := filepath.Join(baseDir, templateRoot, rc.Template)

		// Validate this render call
		rcErrors := ValidateTemplateFile(templatePath, rc.Vars, rc.Template, baseDir, templateRoot, namedBlocks)

		// Annotate errors with source location (Go file/line where render call occurs)
		for i := range rcErrors {
			rcErrors[i].GoFile = rc.File
			rcErrors[i].GoLine = rc.Line
		}

		errors = append(errors, rcErrors...)
	}

	resultChan <- errors
}

// ValidateTemplateFile validates a single template file against its expected
// variable context.
//
// Validation logic:
//  1. Attempt to read template file
//  2. If file not found, check if it's a named block
//  3. If found, validate content against provided variables
//
// Parameters:
//   - templatePath: Full path to template file
//   - vars: Variables expected to be available in template
//   - templateName: Display name for error messages
//   - baseDir: Root directory of project
//   - templateRoot: Template subdirectory
//   - registry: Named block registry
//
// Returns: Slice of validation errors found in this template
//
// Thread-safety: Read-only operations on shared data structures (registry, vars).
func ValidateTemplateFile(
	templatePath string,
	vars []ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	registry map[string][]NamedBlockEntry,
) []ValidationResult {
	// Attempt to read template file
	content, err := os.ReadFile(templatePath)
	if err != nil {
		// File not found - check if it's a named block
		if entries, ok := registry[templateName]; ok && len(entries) > 0 {
			// Template is a named block - validate its content
			varMap := buildVarMap(vars)

			entry := entries[0]
			return ValidateTemplateContent(
				entry.Content,
				varMap,
				entry.TemplatePath,
				baseDir,
				templateRoot,
				entry.Line,
				registry,
			)
		}

		// may be inline HTML.
		if !validTemplateName.MatchString(templateName) {
			return []ValidationResult{}
		}

		// Neither file nor named block found
		return []ValidationResult{{
			Template: templateName,
			Line:     1,
			Column:   1,
			Variable: "",
			Message:  fmt.Sprintf("Template or named block not found: %s", templateName),
			Severity: "error",
		}}
	}

	// File found - validate content
	varMap := buildVarMap(vars)
	return ValidateTemplateContent(
		string(content),
		varMap,
		templateName,
		baseDir,
		templateRoot,
		1,
		registry,
	)
}

// buildVarMap converts a slice of TemplateVar to a map for O(1) lookup.
func buildVarMap(vars []ast.TemplateVar) map[string]ast.TemplateVar {
	varMap := make(map[string]ast.TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return varMap
}
