package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// templateActionRe matches any {{ ... }} template action, including multi-line.
// (?s) makes . match \n; non-greedy .*? stops at the FIRST }}.
// Groups: 1 = leading dash, 2 = inner content, 3 = trailing dash.
var templateActionRe = regexp.MustCompile(`(?s)\{\{(-?)\s*(.*?)\s*(-?)\}\}`)

// defineOrBlockNameRe extracts the quoted name from a define or block action.
var defineOrBlockNameRe = regexp.MustCompile(`^(?:define|block)\s+"([^"]+)"`)

// parseAllNamedTemplates extracts all {{define}} and {{block}} declarations
// from template files in the specified directory tree.
func parseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	root := filepath.Join(baseDir, templateRoot)

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

	registry := processTemplateFilesConcurrently(templateFiles, root)
	errors := detectDuplicateBlocks(registry)
	return registry, errors
}

// processTemplateFilesConcurrently processes template files using a worker pool.
// Uses a mutex-protected regular map — avoids sync.Map's CAS incompatibility
// with non-comparable slice types.
func processTemplateFilesConcurrently(templateFiles []string, root string) map[string][]NamedBlockEntry {
	if len(templateFiles) == 0 {
		return make(map[string][]NamedBlockEntry)
	}

	var (
		mu       sync.Mutex
		registry = make(map[string][]NamedBlockEntry)
	)

	numWorkers := max(runtime.NumCPU(), 1)
	fileChan := make(chan string, len(templateFiles))

	// Feed work before starting workers so the buffered channel never blocks.
	for _, p := range templateFiles {
		fileChan <- p
	}
	close(fileChan)

	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			processTemplateFileWorker(fileChan, root, &mu, registry)
		}()
	}
	wg.Wait()

	return registry
}

// processTemplateFileWorker reads files from fileChan, parses named blocks,
// and merges results into the shared registry under the provided mutex.
func processTemplateFileWorker(
	fileChan <-chan string,
	root string,
	mu *sync.Mutex,
	registry map[string][]NamedBlockEntry,
) {
	for path := range fileChan {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		// Parse into a goroutine-local map — no locking needed during parse.
		local := make(map[string][]NamedBlockEntry)
		extractNamedTemplatesFromContent(string(content), path, rel, local)

		if len(local) == 0 {
			continue
		}

		// Merge local results into the shared registry under lock.
		mu.Lock()
		for name, entries := range local {
			registry[name] = append(registry[name], entries...)
		}
		mu.Unlock()
	}
}

// detectDuplicateBlocks identifies block names declared more than once.
func detectDuplicateBlocks(registry map[string][]NamedBlockEntry) []NamedBlockDuplicateError {
	var errors []NamedBlockDuplicateError
	for name, entries := range registry {
		if len(entries) > 1 {
			errors = append(errors, NamedBlockDuplicateError{
				Name:    name,
				Entries: entries,
				Message: fmt.Sprintf(`Duplicate named block "%s" found`, name),
			})
		}
	}
	return errors
}

// extractNamedTemplatesFromContent uses compiled regexes to find all
// {{define "name"}} and {{block "name" ...}} declarations in content,
// tracking nesting depth to locate the matching {{end}} for each.
//
// registry must be a map[string][]NamedBlockEntry; it is written without
// any locking (callers are responsible for serialisation when needed).
func extractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry any) {
	reg, ok := registry.(map[string][]NamedBlockEntry)
	if !ok {
		return
	}

	var (
		activeName  string
		startOffset int // byte index in content immediately after the opening tag
		startLine   int
		startCol    int
		depth       int
	)

	// FindAllStringSubmatchIndex returns indices for every non-overlapping match.
	// For our regex the groups are: 0=full, 1=lead-dash, 2=inner, 3=trail-dash.
	for _, loc := range templateActionRe.FindAllStringSubmatchIndex(content, -1) {
		fullStart, fullEnd := loc[0], loc[1]
		innerStart, innerEnd := loc[4], loc[5] // group 2 — inner content
		if innerStart < 0 || innerEnd < 0 {
			continue
		}

		action := content[innerStart:innerEnd]
		if action == "" {
			continue
		}

		// Skip template comments: {{/* ... */}} and {{// ...}}
		if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
			continue
		}

		fields := strings.Fields(action)
		if len(fields) == 0 {
			continue
		}
		keyword := fields[0]

		// Compute 1-based line and column of the opening {{ for diagnostics.
		before := content[:fullStart]
		lineNum := 1 + strings.Count(before, "\n")
		lastNL := strings.LastIndexByte(before, '\n')
		col := fullStart - lastNL // 1-based: distance past the last newline

		switch keyword {
		case "define", "block":
			if activeName != "" {
				// Nested opener inside an active named block.
				depth++
			} else {
				// New top-level named block declaration.
				m := defineOrBlockNameRe.FindStringSubmatch(action)
				if m == nil {
					// Malformed or unquoted name — skip.
					continue
				}
				activeName = m[1]
				startOffset = fullEnd
				startLine = lineNum
				startCol = col
				depth = 1
			}

		case "if", "with", "range":
			if activeName != "" {
				depth++
			}

		case "end":
			if activeName != "" {
				depth--
				if depth == 0 {
					reg[activeName] = append(reg[activeName], NamedBlockEntry{
						Name:         activeName,
						Content:      content[startOffset:fullStart],
						AbsolutePath: absolutePath,
						TemplatePath: templatePath,
						Line:         startLine,
						Col:          startCol,
					})
					activeName = ""
				}
			}
		}
	}
}
