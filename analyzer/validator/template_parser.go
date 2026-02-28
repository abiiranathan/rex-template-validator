package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

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
		if IsFileBasedPartial(path) {
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
