// Package validator
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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

var validTemplateName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ═══════════════════════════════════════════════════════════════════════════
// NAMED TEMPLATE PARSING (CONCURRENT)
// ═══════════════════════════════════════════════════════════════════════════

// parseAllNamedTemplates extracts all {{define}} and {{block}} declarations
// from template files in the specified directory tree.
//
// This function performs concurrent file processing for improved performance
// on large template directories. Each template file is parsed independently
// to extract named block definitions.
//
// Named blocks can be declared with either:
//   - {{define "blockName"}}...{{end}}
//   - {{block "blockName" .}}...{{end}}
//
// The function detects duplicate declarations of the same block name and
// returns them as errors.
//
// Parameters:
//   - baseDir: Root directory of the project
//   - templateRoot: Subdirectory containing template files (relative to baseDir)
//
// Returns:
//   - registry: Map of block names to their definitions (may contain duplicates)
//   - errors: Slice of duplicate block errors
//
// Concurrency: File processing is done concurrently using a worker pool.
// Thread-safety: Uses sync.Map for concurrent writes, then converts to regular map.
func parseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	root := filepath.Join(baseDir, templateRoot)

	// Phase 1: Collect all template file paths
	var templateFiles []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if isFileBasedPartial(path) {
			templateFiles = append(templateFiles, path)
		}
		return nil
	})

	// Phase 2: Process files concurrently
	registry := processTemplateFilesConcurrently(templateFiles, root)

	// Phase 3: Detect duplicates
	errors := detectDuplicateBlocks(registry)

	return registry, errors
}

// processTemplateFilesConcurrently processes template files using a worker pool
// to extract named block definitions concurrently.
//
// Concurrency model:
//   - One worker per CPU core (optimal for I/O + CPU mixed workload)
//   - Workers read from shared file channel
//   - Results collected via sync.Map (concurrent-safe)
//   - Converted to regular map after all workers complete
//
// Thread-safety: Uses sync.Map for concurrent writes during processing.
func processTemplateFilesConcurrently(templateFiles []string, root string) map[string][]NamedBlockEntry {
	if len(templateFiles) == 0 {
		return make(map[string][]NamedBlockEntry)
	}

	// Shared data structures
	var sharedRegistry sync.Map // map[string][]NamedBlockEntry
	numWorkers := max(runtime.NumCPU(), 1)
	fileChan := make(chan string, len(templateFiles))

	// Start workers
	var wg sync.WaitGroup
	for range numWorkers {
		wg.Go(func() {
			processTemplateFileWorker(fileChan, root, &sharedRegistry)
		})
	}

	// Feed work to workers
	for _, path := range templateFiles {
		fileChan <- path
	}
	close(fileChan)

	// Wait for completion
	wg.Wait()

	// Convert sync.Map to regular map
	return convertRegistryToMap(&sharedRegistry)
}

// processTemplateFileWorker is a worker function that processes template files
// from a channel and extracts named block definitions.
//
// Each worker:
//  1. Reads template file content
//  2. Computes relative path for error reporting
//  3. Extracts named blocks from content
//  4. Stores results in shared registry (thread-safe)
//
// Thread-safety: All writes to sharedRegistry use sync.Map's thread-safe operations.
func processTemplateFileWorker(
	fileChan <-chan string,
	root string,
	sharedRegistry *sync.Map,
) {
	for path := range fileChan {
		// Calculate relative path for consistent error reporting
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		// Normalize to forward slashes for cross-platform consistency
		rel = filepath.ToSlash(rel)

		// Read file content
		content, err := os.ReadFile(path)
		if err != nil {
			continue // Skip files we can't read
		}

		// Extract named templates and store in shared registry
		extractNamedTemplatesFromContent(string(content), path, rel, sharedRegistry)
	}
}

// convertRegistryToMap converts a sync.Map registry to a regular map.
// This is done after all concurrent writes are complete for better read performance.
func convertRegistryToMap(sharedRegistry *sync.Map) map[string][]NamedBlockEntry {
	registry := make(map[string][]NamedBlockEntry)

	sharedRegistry.Range(func(key, value any) bool {
		registry[key.(string)] = value.([]NamedBlockEntry)
		return true
	})

	return registry
}

// detectDuplicateBlocks identifies block names that are defined multiple times.
// This is an error condition as each block name should be unique.
//
// Returns a slice of NamedBlockDuplicateError, one for each duplicated block name.
func detectDuplicateBlocks(registry map[string][]NamedBlockEntry) []NamedBlockDuplicateError {
	var errors []NamedBlockDuplicateError
	for name, entries := range registry {
		if len(entries) > 1 {
			msg := fmt.Sprintf(`Duplicate named block "%s" found`, name)
			errors = append(errors, NamedBlockDuplicateError{
				Name:    name,
				Entries: entries,
				Message: msg,
			})
		}
	}

	return errors
}

// ═══════════════════════════════════════════════════════════════════════════
// NAMED TEMPLATE CONTENT EXTRACTION
// ═══════════════════════════════════════════════════════════════════════════

// extractNamedTemplatesFromContent parses template content to find {{define}}
// and {{block}} declarations and extracts their content.
//
// Algorithm:
//  1. Scans content line-by-line for template actions
//  2. Tracks nesting depth to handle nested define/block declarations
//  3. Extracts content between declaration and matching {{end}}
//  4. Records source location (file, line, column) for error reporting
//
// Supported syntax:
//   - {{define "name"}}...{{end}}
//   - {{block "name" .}}...{{end}}
//
// Nesting behavior:
//   - Tracks depth to handle nested control structures (if/with/range/block)
//   - Only extracts top-level define/block declarations
//   - Inner define blocks are included in outer block's content
//
// Parameters:
//   - content: Template file content as string
//   - absolutePath: Full filesystem path to template file
//   - templatePath: Relative path for display in errors
//   - registry: Target registry (sync.Map or regular map via interface)
//
// Thread-safety: When used with sync.Map, concurrent calls are safe.
func extractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry any) {
	var activeName string
	var startIndex int
	var startLine int
	var startCol int
	depth := 0

	cur := 0
	lineNum := 1 // 1-based for root files

	for cur < len(content) {
		openRel := strings.Index(content[cur:], "{{")
		if openRel == -1 {
			break
		}
		openIdx := cur + openRel

		// Add newlines between cur and openIdx
		lineNum += strings.Count(content[cur:openIdx], "\n")

		closeRel := strings.Index(content[openIdx:], "}}")
		if closeRel == -1 {
			break // Unclosed tag
		}
		closeIdx := openIdx + closeRel

		// Calculate column
		lastNewline := strings.LastIndexByte(content[:openIdx], '\n')
		col := openIdx - lastNewline

		// Extract action content (trim whitespace and `-`)
		contentStart := openIdx + 2
		if contentStart < closeIdx && content[contentStart] == '-' {
			contentStart++
		}
		for contentStart < closeIdx && isWhitespace(content[contentStart]) {
			contentStart++
		}

		contentEnd := closeIdx
		if contentEnd > contentStart && content[contentEnd-1] == '-' {
			contentEnd--
		}
		for contentEnd > contentStart && isWhitespace(content[contentEnd-1]) {
			contentEnd--
		}

		var action string
		if contentStart < contentEnd {
			action = content[contentStart:contentEnd]
		}

		// Update cur and lineNum for next iteration
		lineNumInside := strings.Count(content[openIdx:closeIdx+2], "\n")
		cur = closeIdx + 2

		// Skip comments
		if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
			lineNum += lineNumInside
			continue
		}

		// Parse action into words (strings.Fields handles \n automatically)
		words := strings.Fields(action)
		if len(words) == 0 {
			lineNum += lineNumInside
			continue
		}

		first := words[0]

		switch first {
		case "if", "with", "range", "block":
			if activeName != "" {
				depth++
			} else if first == "block" && len(words) >= 2 {
				activeName = strings.Trim(words[1], `"`)
				startIndex = cur
				startLine = lineNum
				startCol = col
				depth = 1
			}

		case "define":
			if activeName != "" {
				depth++
			} else if len(words) >= 2 {
				activeName = strings.Trim(words[1], `"`)
				startIndex = cur
				startLine = lineNum
				startCol = col
				depth = 1
			}

		case "end":
			if activeName != "" {
				depth--
				if depth == 0 {
					entry := NamedBlockEntry{
						Name:         activeName,
						Content:      content[startIndex:openIdx],
						AbsolutePath: absolutePath,
						TemplatePath: templatePath,
						Line:         startLine,
						Col:          startCol,
					}
					storeNamedBlock(registry, activeName, entry)
					activeName = ""
				}
			}
		}

		lineNum += lineNumInside
	}
}

// storeNamedBlock stores a named block entry in the registry.
// Handles both sync.Map (concurrent) and regular map (sequential) registries.
//
// Thread-safety: When registry is *sync.Map, operations are thread-safe.
func storeNamedBlock(registry any, name string, entry NamedBlockEntry) {
	switch r := registry.(type) {
	case *sync.Map:
		// Concurrent registry: use sync.Map operations
		for {
			val, loaded := r.LoadOrStore(name, []NamedBlockEntry{entry})
			if !loaded {
				// Successfully stored new entry
				return
			}

			// Entry exists: append to existing slice
			existing := val.([]NamedBlockEntry)
			updated := append(existing, entry)

			// Attempt to update (may fail if another goroutine updated concurrently)
			if r.CompareAndSwap(name, existing, updated) {
				return
			}
			// CAS failed: retry
		}

	case map[string][]NamedBlockEntry:
		// Sequential registry: direct map access
		r[name] = append(r[name], entry)
	}
}

// isWhitespace checks if a byte is whitespace (space, tab, newline, carriage return).
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// ═══════════════════════════════════════════════════════════════════════════
// TEMPLATE VALIDATION (CONCURRENT)
// ═══════════════════════════════════════════════════════════════════════════

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
func ValidateTemplates(renderCalls []RenderCall, baseDir string, templateRoot string) ([]ValidationResult, map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
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
	renderCalls []RenderCall,
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
	chunk []RenderCall,
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
	resultChan chan<- []ValidationResult,
) {
	var errors []ValidationResult

	for _, rc := range chunk {
		templatePath := filepath.Join(baseDir, templateRoot, rc.Template)

		// Validate this render call
		rcErrors := validateTemplateFile(templatePath, rc.Vars, rc.Template, baseDir, templateRoot, namedBlocks)

		// Annotate errors with source location (Go file/line where render call occurs)
		for i := range rcErrors {
			rcErrors[i].GoFile = rc.File
			rcErrors[i].GoLine = rc.Line
		}

		errors = append(errors, rcErrors...)
	}

	resultChan <- errors
}

// ═══════════════════════════════════════════════════════════════════════════
// SINGLE TEMPLATE VALIDATION
// ═══════════════════════════════════════════════════════════════════════════

// validateTemplateFile validates a single template file against its expected
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
func validateTemplateFile(
	templatePath string,
	vars []TemplateVar,
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
			return validateTemplateContent(
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
	return validateTemplateContent(
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
func buildVarMap(vars []TemplateVar) map[string]TemplateVar {
	varMap := make(map[string]TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return varMap
}

// ═══════════════════════════════════════════════════════════════════════════
// TEMPLATE CONTENT VALIDATION WITH SCOPE TRACKING
// ═══════════════════════════════════════════════════════════════════════════

// validateTemplateContent performs comprehensive validation of template content
// with full scope tracking and nested template support.
//
// This is the core validation function that:
//   - Parses all template actions ({{ ... }})
//   - Tracks scope changes (with, range, if blocks)
//   - Validates all variable references
//   - Validates field access paths
//   - Handles nested template calls recursively
//   - Skips validation inside {{define}} blocks (separate scope)
//
// Scope tracking:
//   - Root scope: All top-level variables from varMap
//   - with scope: Changes dot context to specific variable
//   - range scope: Creates iteration scope with collection elements
//   - if scope: Maintains current scope (just for depth tracking)
//
// Parameters:
//   - content: Template content string to validate
//   - varMap: Available variables (map for O(1) lookup)
//   - templateName: Template name for error reporting
//   - baseDir: Project root directory
//   - templateRoot: Template subdirectory
//   - lineOffset: Starting line number (for nested blocks)
//   - registry: Named block registry
//
// Returns: Slice of validation errors found in this content
//
// Thread-safety: Read-only operations on shared data (varMap, registry).
// This function can be called concurrently for different templates.
func validateTemplateContent(
	content string,
	varMap map[string]TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	lineOffset int,
	registry map[string][]NamedBlockEntry,
) []ValidationResult {
	var errors []ValidationResult

	// Initialize scope stack with root scope
	var scopeStack []ScopeType
	rootScope := buildRootScope(varMap)
	scopeStack = append(scopeStack, rootScope)

	// Track depth inside {{define}} blocks (validation is skipped)
	defineSkipDepth := 0

	cur := 0
	lineNum := 0 // 0-based offset from start of this content block

	for cur < len(content) {
		openRel := strings.Index(content[cur:], "{{")
		if openRel == -1 {
			break
		}
		openIdx := cur + openRel

		// Add newlines between cur and openIdx
		lineNum += strings.Count(content[cur:openIdx], "\n")
		actualLineNum := lineNum + lineOffset

		closeRel := strings.Index(content[openIdx:], "}}")
		if closeRel == -1 {
			break // Unclosed tag
		}
		closeIdx := openIdx + closeRel

		// Extract and trim action content
		contentStart := openIdx + 2
		if contentStart < closeIdx && content[contentStart] == '-' {
			contentStart++
		}
		for contentStart < closeIdx && isWhitespace(content[contentStart]) {
			contentStart++
		}

		contentEnd := closeIdx
		if contentEnd > contentStart && content[contentEnd-1] == '-' {
			contentEnd--
		}
		for contentEnd > contentStart && isWhitespace(content[contentEnd-1]) {
			contentEnd--
		}

		// Calculate column based on contentStart to match expected test behavior
		lastNewline := strings.LastIndexByte(content[:openIdx], '\n')
		col := contentStart - lastNewline

		var action string
		if contentStart < contentEnd {
			action = content[contentStart:contentEnd]
		}

		// Update cur and lineNum for next iteration
		lineNumInside := strings.Count(content[openIdx:closeIdx+2], "\n")
		cur = closeIdx + 2

		// Skip comments
		if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
			lineNum += lineNumInside
			continue
		}

		// Parse action into words
		words := strings.Fields(action)
		first := ""
		if len(words) > 0 {
			first = words[0]
		}

		// ── Handle {{define}} blocks ────────────────────────────────────
		// Define blocks create separate scopes and should not be validated
		// in the context of the parent template.
		if first == "define" {
			defineSkipDepth++
			lineNum += lineNumInside
			continue
		}

		// Track nesting depth inside define blocks
		if defineSkipDepth > 0 {
			switch first {
			case "if", "with", "range", "block":
				defineSkipDepth++
			case "end":
				defineSkipDepth--
			}
			lineNum += lineNumInside
			continue
		}

		// ── Handle scope popping (else, end) BEFORE validation ──────────
		isElse := first == "else"
		var elseAction string
		if isElse {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			} else {
				panic(fmt.Sprintf("Template validation error in %s:%d: unexpected {{else}} without matching {{if/with/range}}", templateName, actualLineNum))
			}
			if len(words) > 1 {
				elseAction = words[1] // "if", "with", "range"
			}
		} else if first == "end" {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			} else {
				panic(fmt.Sprintf("Template validation error in %s:%d: unexpected {{end}} without matching {{if/with/range}}", templateName, actualLineNum))
			}
			lineNum += lineNumInside
			continue
		}

		// ── Validate variables in action ────────────────────────────────
		// Extract and validate all variable references in this action.
		extractVariablesFromAction(action, func(v string) {
			if err := validateVariableInScope(
				v,
				scopeStack,
				varMap,
				actualLineNum,
				col,
				templateName,
			); err != nil {
				errors = append(errors, *err)
			}
		})

		// ── Handle {{block}} blocks ─────────────────────────────────────
		// Block is similar to define but with inline content
		if first == "block" {
			defineSkipDepth++
			lineNum += lineNumInside
			continue
		}

		// ── Handle scope pushing (if, with, range, else) AFTER validation ─────
		actionToPush := first
		exprToParse := action

		if isElse {
			if elseAction != "" {
				actionToPush = elseAction
				idx := strings.Index(action, elseAction)
				if idx != -1 {
					exprToParse = action[idx:]
				}
			} else {
				// Plain else
				if len(scopeStack) > 0 {
					scopeStack = append(scopeStack, scopeStack[len(scopeStack)-1])
				} else {
					scopeStack = append(scopeStack, ScopeType{})
				}
				lineNum += lineNumInside
				continue
			}
		}

		if actionToPush == "range" {
			rangeExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "range"))
			newScope := createScopeFromRange(rangeExpr, scopeStack, varMap)
			scopeStack = append(scopeStack, newScope)
			lineNum += lineNumInside
			continue
		}

		if actionToPush == "with" {
			withExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "with"))
			newScope := createScopeFromWith(withExpr, scopeStack, varMap)
			scopeStack = append(scopeStack, newScope)
			lineNum += lineNumInside
			continue
		}

		if actionToPush == "if" {
			if len(scopeStack) > 0 {
				scopeStack = append(scopeStack, scopeStack[len(scopeStack)-1])
			} else {
				scopeStack = append(scopeStack, ScopeType{})
			}
			lineNum += lineNumInside
			continue
		}

		if isElse {
			lineNum += lineNumInside
			continue
		}

		// ── Handle {{template}} calls ───────────────────────────────────
		// Template calls invoke other templates/named blocks with a context.
		if first == "template" {
			parts := parseTemplateAction(action)

			if len(parts) >= 1 {
				tmplName := parts[0]
				var contextArg string
				if len(parts) >= 2 {
					contextArg = parts[1]
				}

				// Validate context argument exists
				if contextArg != "" && contextArg != "." {
					if !validateContextArg(contextArg, scopeStack, varMap) {
						// Validation failed, skip recursive check to prevent cascading errors
						lineNum += lineNumInside
						continue
					}
				}

				// Validate nested template - check if it's a named block
				if entries, ok := registry[tmplName]; ok && len(entries) > 0 {
					nt := entries[0]

					// Skip deep validation for untracked local vars ($var)
					// to prevent false positives
					if contextArg != "" && contextArg != "." && !strings.HasPrefix(contextArg, ".") {
						lineNum += lineNumInside
						continue
					}

					// Build scope for nested template
					partialScope := resolvePartialScope(contextArg, scopeStack, varMap)
					partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

					// Recursively validate nested template
					partialErrors := validateTemplateContent(
						nt.Content,
						partialVarMap,
						nt.TemplatePath,
						baseDir,
						templateRoot,
						nt.Line,
						registry,
					)
					errors = append(errors, partialErrors...)

				} else if isFileBasedPartial(tmplName) {
					// Check if it's a file-based partial
					fullPath := filepath.Join(baseDir, templateRoot, tmplName)
					if _, err := os.Stat(fullPath); os.IsNotExist(err) {
						errors = append(errors, ValidationResult{
							Template: templateName,
							Line:     actualLineNum,
							Column:   col,
							Variable: tmplName,
							Message:  fmt.Sprintf(`Partial template "%s" could not be found at %s`, tmplName, fullPath),
							Severity: "error",
						})
						lineNum += lineNumInside
						continue
					}

					// Skip deep validation for untracked local vars
					if contextArg != "" && contextArg != "." && !strings.HasPrefix(contextArg, ".") {
						lineNum += lineNumInside
						continue
					}

					// Build scope for file-based partial
					partialScope := resolvePartialScope(contextArg, scopeStack, varMap)
					partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

					// Recursively validate file-based partial
					partialErrors := validateTemplateFile(
						fullPath,
						scopeVarsToTemplateVars(partialVarMap),
						tmplName,
						baseDir,
						templateRoot,
						registry,
					)
					errors = append(errors, partialErrors...)
				}
			}
		}

		lineNum += lineNumInside
	}

	return errors
}

// buildRootScope creates the root scope from the available variables.
// The root scope contains all top-level variables accessible via $.VarName.
func buildRootScope(varMap map[string]TemplateVar) ScopeType {
	rootScope := ScopeType{
		IsRoot: true,
		Fields: make([]FieldInfo, 0, len(varMap)),
	}

	for name, v := range varMap {
		rootScope.Fields = append(rootScope.Fields, FieldInfo{
			Name:     name,
			TypeStr:  v.TypeStr,
			IsSlice:  v.IsSlice,
			IsMap:    v.IsMap,
			KeyType:  v.KeyType,
			ElemType: v.ElemType,
			Fields:   v.Fields,
		})
	}

	return rootScope
}

// ═══════════════════════════════════════════════════════════════════════════
// SCOPE MANAGEMENT
// ═══════════════════════════════════════════════════════════════════════════

// resolvePartialScope determines what scope/type the context argument refers to
// for a nested template call.
//
// Context argument types:
//   - "." : Current scope (dot context)
//   - ".VarName" : Specific variable from current scope
//   - "$var" : Local pipeline variable (returns empty scope)
//
// Returns: ScopeType representing the context that will be passed to the nested template
func resolvePartialScope(
	contextArg string,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
) ScopeType {
	if contextArg == "." {
		// Pass current scope
		if len(scopeStack) > 0 {
			return scopeStack[len(scopeStack)-1]
		}
		return ScopeType{IsRoot: true}
	}

	if strings.HasPrefix(contextArg, ".") {
		// Specific variable access
		return createScopeFromExpression(contextArg, scopeStack, varMap)
	}

	// Untracked local variable or other expression
	return ScopeType{Fields: []FieldInfo{}}
}

// buildPartialVarMap constructs the variable map available to a nested template
// based on the context argument.
//
// The resulting map represents what will be available as the dot (.) context
// in the nested template.
//
// Context semantics:
//   - "." : All variables from current scope
//   - ".VarName" : Fields of VarName become top-level variables
//
// Returns: Map of variables available in the nested template
func buildPartialVarMap(
	contextArg string,
	partialScope ScopeType,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
) map[string]TemplateVar {
	result := make(map[string]TemplateVar)

	if contextArg == "." {
		// Pass entire current scope
		if len(scopeStack) > 0 {
			currentScope := scopeStack[len(scopeStack)-1]
			if currentScope.IsRoot {
				// Root scope: copy all variables
				maps.Copy(result, varMap)
			} else {
				// Non-root scope: convert fields to variables
				for _, f := range currentScope.Fields {
					result[f.Name] = TemplateVar{
						Name:     f.Name,
						TypeStr:  f.TypeStr,
						Fields:   f.Fields,
						IsSlice:  f.IsSlice,
						IsMap:    f.IsMap,
						KeyType:  f.KeyType,
						ElemType: f.ElemType,
					}
				}
			}
		}
		return result
	}

	// Specific variable: its fields become top-level in nested template
	for _, f := range partialScope.Fields {
		result[f.Name] = TemplateVar{
			Name:     f.Name,
			TypeStr:  f.TypeStr,
			Fields:   f.Fields,
			IsSlice:  f.IsSlice,
			IsMap:    f.IsMap,
			KeyType:  f.KeyType,
			ElemType: f.ElemType,
		}
	}

	return result
}

// scopeVarsToTemplateVars converts a variable map back to a TemplateVar slice.
// This is used when recursively validating file-based partials.
func scopeVarsToTemplateVars(varMap map[string]TemplateVar) []TemplateVar {
	vars := make([]TemplateVar, 0, len(varMap))
	for _, v := range varMap {
		vars = append(vars, v)
	}
	return vars
}

// createScopeFromRange creates a new scope for a {{range}} block.
//
// Range syntax:
//   - {{range .Collection}} : Iterate over collection, dot becomes element
//   - {{range $val := .Collection}} : Iterate with named value
//   - {{range $key, $val := .Collection}} : Iterate with named key and value
//
// The new scope represents the type of elements being iterated over.
//
// Returns: ScopeType for the range block body
func createScopeFromRange(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
) ScopeType {
	expr = strings.TrimSpace(expr)

	var collectionScope ScopeType

	// Handle assignment syntax: $var := expr
	if strings.Contains(expr, ":=") {
		parts := strings.SplitN(expr, ":=", 2)
		if len(parts) == 2 {
			varExpr := strings.TrimSpace(parts[1])
			collectionScope = createScopeFromExpression(varExpr, scopeStack, varMap)
		} else {
			// Fallback for malformed assignment
			return ScopeType{Fields: []FieldInfo{}}
		}
	} else {
		// Simple range: {{range .Collection}}
		collectionScope = createScopeFromExpression(expr, scopeStack, varMap)
	}

	// If we are iterating over a map or slice, the scope inside the range
	// corresponds to the element type, not the collection type.
	// We need to unwrap the IsMap/IsSlice properties based on the element type.
	if collectionScope.IsMap || collectionScope.IsSlice {
		baseType := collectionScope.ElemType
		// Unwrap pointer types
		for strings.HasPrefix(baseType, "*") {
			baseType = baseType[1:]
		}

		newIsMap := false
		newIsSlice := false
		newElemType := ""
		// KeyType logic omitted for now as it's not critical for IsMap determination

		if strings.HasPrefix(baseType, "map[") {
			// Logic to parse map[Key]Value
			depth := 0
			splitIdx := -1

			// Start after "map[" (index 3)
			for i := 3; i < len(baseType); i++ {
				if baseType[i] == '[' {
					depth++
				} else if baseType[i] == ']' {
					depth--
					if depth == 0 {
						splitIdx = i
						break
					}
				}
			}

			if splitIdx != -1 {
				valType := baseType[splitIdx+1:]
				newIsMap = true
				newElemType = strings.TrimSpace(valType)
			}
		} else if strings.HasPrefix(baseType, "[]") {
			newIsSlice = true
			newElemType = baseType[2:]
		}

		// Return updated scope representing the element
		return ScopeType{
			IsRoot:   false,
			VarName:  expr, // or original varExpr
			TypeStr:  collectionScope.ElemType,
			Fields:   collectionScope.Fields,
			IsSlice:  newIsSlice,
			IsMap:    newIsMap,
			KeyType:  "",          // Not currently parsed
			ElemType: newElemType, // Derived from ElemType string
		}
	}

	return collectionScope
}

// createScopeFromWith creates a new scope for a {{with}} block.
//
// With syntax: {{with .Variable}}
// Changes the dot context to the specified variable.
//
// Returns: ScopeType for the with block body
func createScopeFromWith(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
) ScopeType {
	return createScopeFromExpression(expr, scopeStack, varMap)
}

// createScopeFromExpression creates a scope by resolving a variable expression.
// Supports arbitrary nesting depth (e.g., .User.Profile.Address.City).
//
// Expression types:
//   - "." : Current scope
//   - ".VarName" : Top-level variable
//   - ".Var.Field" : Nested field access
//   - ".Var.Field.SubField" : Deep nested access
//
// Algorithm:
//  1. Split expression into path segments
//  2. Resolve first segment in current scope or varMap
//  3. Traverse remaining segments through field hierarchy
//  4. Return scope representing the final type
//
// Returns: ScopeType representing the resolved expression's type
func createScopeFromExpression(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
) ScopeType {
	expr = strings.TrimSpace(expr)

	// Handle dot (current scope)
	if expr == "." {
		if len(scopeStack) > 0 {
			return scopeStack[len(scopeStack)-1]
		}
		return ScopeType{IsRoot: true}
	}

	// Must start with dot for variable access
	if !strings.HasPrefix(expr, ".") {
		return ScopeType{Fields: []FieldInfo{}}
	}

	// Split into path segments
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return ScopeType{Fields: []FieldInfo{}}
	}

	// Resolve first segment (parts[0] is empty due to leading dot)
	var currentField *FieldInfo
	firstPart := parts[1]

	// Look in current scope first
	if len(scopeStack) > 0 {
		currentScope := scopeStack[len(scopeStack)-1]
		for _, f := range currentScope.Fields {
			if f.Name == firstPart {
				fCopy := f
				currentField = &fCopy
				break
			}
		}
	}

	// Fall back to varMap if not in current scope
	if currentField == nil {
		if v, ok := varMap[firstPart]; ok {
			currentField = &FieldInfo{
				Name:     v.Name,
				TypeStr:  v.TypeStr,
				Fields:   v.Fields,
				IsSlice:  v.IsSlice,
				IsMap:    v.IsMap,
				KeyType:  v.KeyType,
				ElemType: v.ElemType,
			}
		}
	}

	// Variable not found
	if currentField == nil {
		return ScopeType{Fields: []FieldInfo{}}
	}

	// Traverse remaining path segments
	for _, part := range parts[2:] {
		found := false
		for _, f := range currentField.Fields {
			if f.Name == part {
				fCopy := f
				currentField = &fCopy
				found = true
				break
			}
		}
		if !found {
			// Path segment not found
			return ScopeType{Fields: []FieldInfo{}}
		}
	}

	// Return scope representing the resolved type
	return ScopeType{
		IsRoot:   false,
		VarName:  expr,
		TypeStr:  currentField.TypeStr,
		Fields:   currentField.Fields,
		IsSlice:  currentField.IsSlice,
		IsMap:    currentField.IsMap,
		KeyType:  currentField.KeyType,
		ElemType: currentField.ElemType,
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// VARIABLE VALIDATION
// ═══════════════════════════════════════════════════════════════════════════

// validateVariableInScope validates a variable access expression in the
// current scope context.
//
// This function handles:
//   - Root variable access: $.VarName
//   - Current scope access: .VarName
//   - Nested field access: .Var.Field.SubField
//   - Map access: .MapVar.key
//   - Unlimited nesting depth
//
// Validation logic:
//  1. Parse expression into path segments
//  2. Determine if root ($) or scoped (.) access
//  3. Validate first segment exists in appropriate scope
//  4. Validate remaining segments exist in field hierarchy
//
// Parameters:
//   - varExpr: Variable expression to validate (e.g., ".User.Name")
//   - scopeStack: Current scope stack
//   - varMap: Root variable map
//   - line, col: Source location for error reporting
//   - templateName: Template name for error reporting
//
// Returns: ValidationResult pointer if error found, nil if valid
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateVariableInScope(
	varExpr string,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
	line, col int,
	templateName string,
) *ValidationResult {
	varExpr = strings.TrimSpace(varExpr)

	// Skip special variables
	if varExpr == "." || varExpr == "$" {
		return nil
	}

	// Normalize expression (remove trailing dots)
	varExpr = strings.TrimRight(varExpr, ".")

	// Split into path segments
	parts := strings.Split(varExpr, ".")
	if len(parts) < 2 {
		return nil
	}

	isRootAccess := parts[0] == "$"

	// ── Scoped access in nested block ──────────────────────────────────────
	// When inside a with/range block, check current scope first
	if !isRootAccess && len(scopeStack) > 1 {
		currentScope := scopeStack[len(scopeStack)-1]
		fieldName := parts[1]

		// Handle map access
		if currentScope.IsMap {
			// Map key access is always valid
			// Validate nested access if present
			if len(parts) > 2 {
				return validateNestedFields(
					parts[2:],
					nil,
					currentScope.ElemType,
					false,
					"",
					varExpr,
					line,
					col,
					templateName,
				)
			}
			return nil
		}

		// Look for field in current scope
		var foundField *FieldInfo
		for _, f := range currentScope.Fields {
			if f.Name == fieldName {
				fCopy := f
				foundField = &fCopy
				break
			}
		}

		// Found in current scope
		if foundField != nil {
			// Validate nested access if present
			if len(parts) > 2 {
				return validateNestedFields(
					parts[2:],
					foundField.Fields,
					foundField.TypeStr,
					foundField.IsMap,
					foundField.ElemType,
					varExpr,
					line,
					col,
					templateName,
				)
			}
			return nil
		}
	}

	// ── Root variable access ───────────────────────────────────────────────
	// Access to top-level variables (either $ or . at root)

	if len(parts) == 2 {
		// Simple access: .VarName or $.VarName
		rootVar := parts[1]

		// Check root scope
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				return nil
			}
		}

		// Check varMap
		if _, ok := varMap[rootVar]; ok {
			return nil
		}

		// Variable not found
		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	// ── Nested access: .Var.Field.SubField ─────────────────────────────────
	rootVar := parts[1]

	// Look up root variable
	var rootVarInfo *TemplateVar
	if v, ok := varMap[rootVar]; ok {
		rootVarInfo = &v
	} else {
		// Try root scope fields
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				// Handle map with single key access
				if f.IsMap && len(parts) == 3 {
					return nil
				}
				// Validate nested fields
				return validateNestedFields(
					parts[2:],
					f.Fields,
					f.TypeStr,
					f.IsMap,
					f.ElemType,
					varExpr,
					line,
					col,
					templateName,
				)
			}
		}

		// Root variable not found
		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	// Handle map with single key access
	if rootVarInfo.IsMap && len(parts) == 3 {
		return nil
	}

	// Validate nested fields
	return validateNestedFields(
		parts[2:],
		rootVarInfo.Fields,
		rootVarInfo.TypeStr,
		rootVarInfo.IsMap,
		rootVarInfo.ElemType,
		varExpr,
		line,
		col,
		templateName,
	)
}

// validateNestedFields validates a field access path through a type hierarchy.
// Supports unlimited nesting depth and handles maps, slices, and structs.
//
// This function recursively traverses the field path, validating each segment
// exists on the parent type.
//
// Special handling:
//   - Map types: Any key is valid, validates the value type for further nesting
//   - Slice types: Element type is used for validation
//   - Struct types: Field must exist in Fields slice
//
// Parameters:
//   - fieldParts: Remaining field path segments to validate
//   - fields: Available fields at current level
//   - parentTypeName: Type name for error messages
//   - isMap: Whether current type is a map
//   - elemType: Element/value type for maps/slices
//   - fullExpr: Complete original expression for error messages
//   - line, col: Source location for error reporting
//   - templateName: Template name for error reporting
//
// Returns: ValidationResult pointer if error found, nil if valid
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateNestedFields(
	fieldParts []string,
	fields []FieldInfo,
	parentTypeName string,
	isMap bool,
	elemType string,
	fullExpr string,
	line, col int,
	templateName string,
) *ValidationResult {
	currentFields := fields
	parentType := parentTypeName
	currentIsMap := isMap
	currentElemType := elemType

	// Traverse each field in the path
	for _, fieldName := range fieldParts {
		if currentIsMap {
			// ── Map key access ─────────────────────────────────────────────
			// Any key is valid for map access
			// Parse element type to determine if further nesting is valid

			baseType := currentElemType
			// Unwrap pointer types
			for strings.HasPrefix(baseType, "*") {
				baseType = baseType[1:]
			}

			newIsMap := false
			newElemType := ""

			if strings.HasPrefix(baseType, "map[") {
				// Nested map: parse map[Key]Value
				// Use bracket counting to handle complex key types like map[string]
				depth := 0
				splitIdx := -1

				// Start after "map[" (index 3)
				for i := 3; i < len(baseType); i++ {
					if baseType[i] == '[' {
						depth++
					} else if baseType[i] == ']' {
						depth--
						if depth == 0 {
							splitIdx = i
							break
						}
					}
				}

				if splitIdx != -1 {
					valType := baseType[splitIdx+1:]
					newIsMap = true
					newElemType = strings.TrimSpace(valType)
				}
			} else if strings.HasPrefix(baseType, "[]") {
				// Slice: element type is after []
				newElemType = baseType[2:]
			}

			currentIsMap = newIsMap
			if newElemType != "" {
				currentElemType = newElemType
			} else {
				// Basic type or struct: use element type as parent type
				parentType = currentElemType
			}

			continue
		}

		// ── Struct field access ────────────────────────────────────────────
		// Field must exist in Fields slice

		found := false
		var nextFields []FieldInfo
		var nextIsMap bool
		var nextElemType string

		for _, f := range currentFields {
			if f.Name == fieldName {
				found = true
				nextFields = f.Fields
				parentType = f.TypeStr
				nextIsMap = f.IsMap
				nextElemType = f.ElemType
				break
			}
		}

		if !found {
			// Field doesn't exist on this type
			if parentType == "" {
				parentType = "unknown"
			}
			return &ValidationResult{
				Template: templateName,
				Line:     line,
				Column:   col,
				Variable: fullExpr,
				Message:  fmt.Sprintf(`Field %q does not exist on type %s`, fieldName, parentType),
				Severity: "error",
			}
		}

		// Move to next level in hierarchy
		currentFields = nextFields
		currentIsMap = nextIsMap
		currentElemType = nextElemType
	}

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// ACTION PARSING
// ═══════════════════════════════════════════════════════════════════════════

// parseTemplateAction parses a {{template}} action to extract its arguments.
//
// Template action syntax:
//   - {{template "name"}}
//   - {{template "name" .}}
//   - {{template "name" .Context}}
//
// Returns slice of parsed arguments:
//   - [0]: template name (without quotes)
//   - [1]: context argument (if present)
//
// Handles both quoted strings ("name", `name`) and unquoted identifiers.
//
// Thread-safety: No shared state, safe for concurrent calls.
func parseTemplateAction(action string) []string {
	rest := strings.TrimPrefix(action, "template ")
	rest = strings.TrimSpace(rest)

	var parts []string
	var current strings.Builder
	inString := false
	stringChar := rune(0)

	for _, r := range rest {
		switch {
		case !inString && (r == '"' || r == '`'):
			// Start of string literal
			inString = true
			stringChar = r
			if current.Len() > 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
			}

		case inString && r == stringChar:
			// End of string literal
			inString = false
			parts = append(parts, current.String())
			current.Reset()

		case !inString && (r == ' ' || r == '\n' || r == '\r' || r == '\t'):
			// Whitespace separator (outside string)
			if current.Len() > 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
			}

		default:
			// Regular character
			current.WriteRune(r)
		}
	}

	// Add any remaining content
	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}

	return parts
}

// extractVariablesFromAction extracts all variable references from a template
// action string.
//
// Variable references are identified by:
//   - Starting with . (current scope) or $. (root scope)
//   - Not being . or $ alone (special variables)
//   - Not starting with .. (invalid syntax)
//
// The function parses the action, skipping:
//   - String literals (quoted content)
//   - Operators and delimiters
//   - Keywords
//
// Calls onVar callback for each valid variable found.
//
// Thread-safety: No shared state, safe for concurrent calls.
func extractVariablesFromAction(action string, onVar func(string)) {
	start := -1
	inString := false
	stringChar := rune(0)

	for i, r := range action {
		if inString {
			// Inside string literal: skip until closing quote
			if r == stringChar {
				inString = false
			}
			continue
		}

		switch r {
		case '"', '`':
			// Start of string literal
			if start != -1 {
				emitVar(action[start:i], onVar)
				start = -1
			}
			inString = true
			stringChar = r

		case ' ', '\n', '\r', '\t', '(', ')', '|', '=', ',', '+', '-', '*', '/', '!', '<', '>', '%', '&':
			// Delimiter: emit pending variable
			if start != -1 {
				emitVar(action[start:i], onVar)
				start = -1
			}

		default:
			// Regular character: mark start of potential variable
			if start == -1 {
				start = i
			}
		}
	}

	// Emit any remaining variable
	if start != -1 {
		emitVar(action[start:], onVar)
	}
}

// emitVar checks if a token is a valid variable reference and calls the callback.
//
// Valid variable references:
//   - Start with . or $.
//   - Not exactly . or $ (these are special variables)
//   - Not starting with .. (invalid)
func emitVar(v string, onVar func(string)) {
	v = strings.TrimSpace(v)
	if (strings.HasPrefix(v, ".") || strings.HasPrefix(v, "$.")) &&
		v != "." && v != "$" && !strings.HasPrefix(v, "..") {
		onVar(v)
	}
}

// validateContextArg checks whether a template call context expression
// resolves in the current scope.
//
// Used to validate that context arguments in {{template "name" .Context}}
// actually exist before recursively validating the nested template.
//
// Returns true if valid, false if undefined.
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateContextArg(
	contextArg string,
	scopeStack []ScopeType,
	varMap map[string]TemplateVar,
) bool {
	// Special cases always valid
	if contextArg == "" || contextArg == "." || contextArg == "$" {
		return true
	}

	// Validate using standard validation logic
	result := validateVariableInScope(contextArg, scopeStack, varMap, 0, 0, "")
	return result == nil
}

// ═══════════════════════════════════════════════════════════════════════════
// FILE TYPE DETECTION
// ═══════════════════════════════════════════════════════════════════════════

// isFileBasedPartial determines if a template name refers to a file path
// rather than a named block.
//
// Detection criteria:
//   - Contains path separators (/ or \)
//   - Has a recognized template file extension
//
// Recognized extensions:
//   - .html, .htm
//   - .tmpl, .tpl
//   - .gohtml
//
// Thread-safety: Pure function, safe for concurrent calls.
func isFileBasedPartial(name string) bool {
	// Check for path separators
	if strings.ContainsAny(name, "/\\") {
		return true
	}

	// Check for template file extensions
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".tmpl", ".gohtml", ".tpl", ".htm":
		return true
	}

	return false
}

// ═══════════════════════════════════════════════════════════════════════════
// PUBLIC API FOR TESTING
// ═══════════════════════════════════════════════════════════════════════════

// ValidateTemplateFileStr exposes internal validation for testing.
// Validates template content string with provided variables.
func ValidateTemplateFileStr(
	content string,
	vars []TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	registry map[string][]NamedBlockEntry,
) []ValidationResult {
	varMap := buildVarMap(vars)
	return validateTemplateContent(content, varMap, templateName, baseDir, templateRoot, 1, registry)
}

// ParseAllNamedTemplates exposes named template parsing for testing.
func ParseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	return parseAllNamedTemplates(baseDir, templateRoot)
}

// ExtractNamedTemplatesFromContent exposes content extraction for testing.
func ExtractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry map[string][]NamedBlockEntry) {
	extractNamedTemplatesFromContent(content, absolutePath, templatePath, registry)
}
