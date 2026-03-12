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
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/abiiranathan/gotpl-analyzer/ast"
)

// ValidateTemplates validates all templates against their render calls AND
// independently validates every template file and named block discovered by
// walking the full template directory tree.
//
// Previously the validator only checked templates that appeared in Go render
// calls.  That left two large blind spots:
//
//  1. Template files never directly targeted by Render() (layouts, partials,
//     base files) were silently skipped.
//  2. {{define}} / {{block}} blocks embedded anywhere in the tree are globally
//     registered by Go's engine and reachable without any explicit render call.
//
// Validation process:
//  1. Parse all named blocks from the template directory tree (concurrent).
//  2. Detect duplicate block definitions.
//  3. Build a lookup: template-name → merged variable context from render calls.
//  4. Find all templates used as partials to avoid validating them with empty context.
//  5. Validate each render call against its template (concurrent).
//  6. Validate every file in the template tree NOT already covered by a render
//     call and NOT used as a partial (concurrent).
//  7. Validate every named block NOT already covered by a render call and NOT
//     used as a partial (concurrent).
func ValidateTemplates(
	renderCalls []ast.RenderCall,
	funcMaps []ast.FuncMapInfo,
	baseDir string,
	templateRoot string,
) ([]ValidationResult, map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	funcMapRegistry := BuildFuncMapRegistry(funcMaps)
	// Parse all named blocks from the entire template tree.
	namedBlocks, namedBlockErrors := parseAllNamedTemplates(baseDir, templateRoot)

	// Build template-name → merged var list from all render calls.
	renderVarsByTemplate := buildRenderVarIndex(renderCalls)

	// Find all templates used as partials to avoid validating them with empty context.
	partialTargets := FindPartialTargets(baseDir, templateRoot)

	// Validate render-call targets (existing behaviour).
	renderErrors := validateRenderCallsConcurrently(renderCalls, baseDir, templateRoot, namedBlocks, partialTargets, funcMapRegistry)

	// Validate all files in the tree not already covered.
	treeErrors := validateTemplateTree(baseDir, templateRoot, namedBlocks, renderVarsByTemplate, partialTargets, funcMapRegistry)

	// Validate named blocks not already covered by a render call.
	blockErrors := validateOrphanedNamedBlocks(namedBlocks, renderVarsByTemplate, baseDir, templateRoot, partialTargets, funcMapRegistry)

	allErrors := append(renderErrors, treeErrors...)
	allErrors = append(allErrors, blockErrors...)

	return allErrors, namedBlocks, namedBlockErrors
}

func BuildFuncMapRegistry(funcMaps []ast.FuncMapInfo) FuncMapRegistry {
	registry := make(FuncMapRegistry, len(funcMaps))
	for _, funcMap := range funcMaps {
		registry[funcMap.Name] = funcMap
	}
	return registry
}

var templateRegex = regexp.MustCompile(`\{\{-?\s*(?:template|block|define)\s+["'\x60]([^"'\x60]+)["'\x60]`)

// FindPartialTargets scans all template files to find targets of {{template "..."}} or {{block "..."}} calls.
func FindPartialTargets(baseDir, templateRoot string) map[string]bool {
	targets := make(map[string]bool)

	root := filepath.Join(baseDir, templateRoot)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !IsFileBasedPartial(path) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err == nil {
			matches := templateRegex.FindAllStringSubmatch(string(content), -1)
			for _, m := range matches {
				targets[m[1]] = true
			}
		}
		return nil
	})

	return targets
}

// buildRenderVarIndex creates a lookup: template-name → merged TemplateVar list.
// When multiple render calls target the same template the variable sets are
// unioned so validation gets the broadest possible context.
func buildRenderVarIndex(renderCalls []ast.RenderCall) map[string][]ast.TemplateVar {
	idx := make(map[string][]ast.TemplateVar, len(renderCalls))
	seen := make(map[string]map[string]bool, len(renderCalls))

	for _, rc := range renderCalls {
		if _, ok := idx[rc.Template]; !ok {
			idx[rc.Template] = nil
			seen[rc.Template] = make(map[string]bool)
		}
		for _, v := range rc.Vars {
			if !seen[rc.Template][v.Name] {
				seen[rc.Template][v.Name] = true
				idx[rc.Template] = append(idx[rc.Template], v)
			}
		}
	}

	return idx
}

// validateTemplateTree walks every template file under baseDir/templateRoot and
// validates files whose relative name was NOT already directly targeted by a
// render call AND is NOT used as a partial. Already-validated files are skipped.
func validateTemplateTree(
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
	renderVarsByTemplate map[string][]ast.TemplateVar,
	partialTargets map[string]bool,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	root := filepath.Join(baseDir, templateRoot)

	type workItem struct {
		absPath string
		relName string
		vars    []ast.TemplateVar
	}

	var items []workItem
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !IsFileBasedPartial(path) {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		// Skip files that are direct render-call targets — already validated.
		if isCoveredByRenderCall(rel, renderVarsByTemplate) {
			return nil
		}

		// Skip files that are used as partials — they will be validated via their callers.
		if partialTargets[rel] {
			return nil
		}

		items = append(items, workItem{
			absPath: path,
			relName: rel,
			vars:    renderVarsByTemplate[rel], // nil → empty context (valid)
		})
		return nil
	})

	if len(items) == 0 {
		return nil
	}

	return runWorkers(len(items), func(chunk []int) []ValidationResult {
		var errs []ValidationResult
		for _, i := range chunk {
			item := items[i]
			errs = append(errs, ValidateTemplateFile(
				item.absPath,
				item.vars,
				item.relName,
				baseDir,
				templateRoot,
				namedBlocks,
				funcMaps,
			)...)
		}
		return errs
	})
}

func isCoveredByRenderCall(rel string, renderVarsByTemplate map[string][]ast.TemplateVar) bool {
	if _, ok := renderVarsByTemplate[rel]; ok {
		return true
	}
	// Normalize and try suffix/prefix matching
	normalizedRel := filepath.ToSlash(filepath.Clean(rel))
	normalizedRel = strings.TrimPrefix(normalizedRel, "./")
	for key := range renderVarsByTemplate {
		normalizedKey := filepath.ToSlash(filepath.Clean(key))
		normalizedKey = strings.TrimPrefix(normalizedKey, "./")
		if normalizedRel == normalizedKey {
			return true
		}
		if strings.HasSuffix(normalizedRel, normalizedKey) || strings.HasSuffix(normalizedKey, normalizedRel) {
			return true
		}
	}
	return false
}

// validateOrphanedNamedBlocks validates every {{define}} / {{block}} entry in
// the registry that does NOT have a corresponding render call target AND is NOT
// used as a partial.
func validateOrphanedNamedBlocks(
	namedBlocks map[string][]NamedBlockEntry,
	renderVarsByTemplate map[string][]ast.TemplateVar,
	baseDir string,
	templateRoot string,
	partialTargets map[string]bool,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	type workItem struct {
		entry NamedBlockEntry
		vars  []ast.TemplateVar
	}

	var items []workItem
	for name, entries := range namedBlocks {
		if _, covered := renderVarsByTemplate[name]; covered {
			continue
		}

		// Skip blocks that are used as partials — they will be validated via their callers.
		if partialTargets[name] {
			continue
		}

		for _, entry := range entries {
			items = append(items, workItem{
				entry: entry,
				vars:  renderVarsByTemplate[name],
			})
		}
	}

	if len(items) == 0 {
		return nil
	}

	return runWorkers(len(items), func(chunk []int) []ValidationResult {
		var errs []ValidationResult
		for _, i := range chunk {
			item := items[i]
			varMap := buildVarMap(item.vars)
			errs = append(errs, ValidateTemplateContent(
				item.entry.Content,
				varMap,
				item.entry.TemplatePath,
				baseDir,
				templateRoot,
				item.entry.Line,
				namedBlocks,
				funcMaps,
			)...)
		}
		return errs
	})
}

// runWorkers fans out index-based work to one goroutine per CPU core and
// aggregates the results.  fn receives a slice of item indices to process.
func runWorkers(total int, fn func([]int) []ValidationResult) []ValidationResult {
	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (total + numWorkers - 1) / numWorkers

	resultChan := make(chan []ValidationResult, numWorkers)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		start := w * chunkSize
		if start >= total {
			break
		}
		end := min(start+chunkSize, total)

		indices := make([]int, end-start)
		for j := range indices {
			indices[j] = start + j
		}

		wg.Add(1)
		go func(idx []int) {
			defer wg.Done()
			resultChan <- fn(idx)
		}(indices)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var all []ValidationResult
	for errs := range resultChan {
		all = append(all, errs...)
	}
	return all
}

// validateRenderCallsConcurrently validates multiple render calls concurrently.
func validateRenderCallsConcurrently(
	renderCalls []ast.RenderCall,
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
	partialTargets map[string]bool,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	if len(renderCalls) == 0 {
		return nil
	}

	// Build the union var index FIRST — same as what the daemon uses for live validation.
	renderVarsByTemplate := buildRenderVarIndex(renderCalls)

	// Deduplicate: only validate each unique template once, with unioned vars.
	type workItem struct {
		template string
		vars     []ast.TemplateVar
		rc       ast.RenderCall // for GoFile/GoLine metadata — use first call
	}

	seen := make(map[string]bool)
	var items []workItem
	for _, rc := range renderCalls {
		if seen[rc.Template] {
			continue
		}
		seen[rc.Template] = true
		if _, isNamedBlock := namedBlocks[rc.Template]; isNamedBlock && partialTargets[rc.Template] {
			continue
		}
		items = append(items, workItem{
			template: rc.Template,
			vars:     renderVarsByTemplate[rc.Template],
			rc:       rc,
		})
	}

	return runWorkers(len(items), func(chunk []int) []ValidationResult {
		var errors []ValidationResult
		for _, i := range chunk {
			item := items[i]
			templatePath := filepath.Join(baseDir, templateRoot, item.template)
			rcErrors := ValidateTemplateFile(
				templatePath, item.vars, item.template, baseDir, templateRoot, namedBlocks, funcMaps,
			)
			for j := range rcErrors {
				rcErrors[j].GoFile = item.rc.File
				rcErrors[j].GoLine = item.rc.Line
				rcErrors[j].TemplateNameStartCol = item.rc.TemplateNameStartCol
				rcErrors[j].TemplateNameEndCol = item.rc.TemplateNameEndCol
			}
			errors = append(errors, rcErrors...)
		}
		return errors
	})
}

var validTemplateName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateTemplateFile — accept the pre-built registry so the internal
// ValidateTemplateContent call uses validateTemplateContentWithRegistry.
// Public signature gains an optional variadic registry parameter so existing
// callers (tests, external packages) need no changes.
//
// NOTE: The existing variadic `funcMaps ...FuncMapRegistry` parameter means we
// cannot add a second variadic. Instead, thread the registry through the
// existing non-variadic path and add an internal helper.
func ValidateTemplateFile(
	templatePath string,
	vars []ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	registry map[string][]NamedBlockEntry,
	funcMaps ...FuncMapRegistry,
) []ValidationResult {
	effectiveFuncMaps := optionalFuncMapRegistry(funcMaps...)

	if entry, ok := findOverlayTemplateEntry(registry, templateName); ok {
		varMap := buildVarMap(vars)
		// Overlay content: merge once then use internal path.
		effectiveRegistry := mergeNamedBlockRegistry(registry, entry.Content, entry.TemplatePath)
		return validateTemplateContentWithRegistry(
			entry.Content, varMap, entry.TemplatePath,
			baseDir, templateRoot, 1, effectiveRegistry, effectiveFuncMaps,
		)
	}

	content, err := os.ReadFile(templatePath)
	if err != nil {
		if entries, ok := registry[templateName]; ok && len(entries) > 0 {
			varMap := buildVarMap(vars)
			entry := entries[0]
			effectiveRegistry := mergeNamedBlockRegistry(registry, entry.Content, entry.TemplatePath)
			return validateTemplateContentWithRegistry(
				entry.Content, varMap, entry.TemplatePath,
				baseDir, templateRoot, entry.Line, effectiveRegistry, effectiveFuncMaps,
			)
		}

		if !validTemplateName.MatchString(templateName) {
			return []ValidationResult{}
		}

		return []ValidationResult{{
			Template: templateName, Line: 1, Column: 1,
			Message:  fmt.Sprintf("Template or named block not found: %s", templateName),
			Severity: "error",
		}}
	}

	varMap := buildVarMap(vars)
	// Merge once here; all recursive calls through validateTemplateContentWithRegistry
	// will use this registry without re-merging.
	effectiveRegistry := mergeNamedBlockRegistry(registry, string(content), templateName)
	return validateTemplateContentWithRegistry(
		string(content), varMap, templateName,
		baseDir, templateRoot, 1, effectiveRegistry, effectiveFuncMaps,
	)
}

func findOverlayTemplateEntry(registry map[string][]NamedBlockEntry, templateName string) (NamedBlockEntry, bool) {
	entries, ok := registry[templateName]
	if !ok {
		return NamedBlockEntry{}, false
	}

	for _, entry := range entries {
		if entry.Name == templateName && entry.TemplatePath == templateName {
			return entry, true
		}
	}

	return NamedBlockEntry{}, false
}

// buildVarMap converts a slice of TemplateVar to a map for O(1) lookup.
func buildVarMap(vars []ast.TemplateVar) map[string]ast.TemplateVar {
	varMap := make(map[string]ast.TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return varMap
}
