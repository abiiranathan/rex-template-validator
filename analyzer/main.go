package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/rex-template-analyzer/validator"
)

func main() {
	dir := flag.String("dir", ".", "Go source directory to analyze")
	templateRoot := flag.String("template-root", "", "Root directory for templates (relative to -dir)")
	validate := flag.Bool("validate", false, "Validate templates against render calls")
	flag.Parse()

	// Resolve dir to an absolute path so all downstream joins are unambiguous
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		absDir = *dir
	}

	result := validator.AnalyzeDir(absDir)

	// Filter out false-positive import errors caused by missing third-party
	// dependencies that are irrelevant to template variable extraction.
	// We still surface genuine parse errors.
	result.Errors = filterImportErrors(result.Errors)

	if *validate {
		// templateRoot is relative to absDir.
		// ValidateTemplates resolves: absDir / templateRoot / template
		validationErrors := validator.ValidateTemplates(result.RenderCalls, absDir, *templateRoot)

		output := struct {
			RenderCalls      []validator.RenderCall       `json:"renderCalls"`
			ValidationErrors []validator.ValidationResult `json:"validationErrors"`
			Errors           []string                     `json:"errors,omitempty"`
		}{
			RenderCalls:      result.RenderCalls,
			ValidationErrors: validationErrors,
			Errors:           result.Errors,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(output)
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	}
}

// filterImportErrors removes type-check errors that are solely caused by
// missing third-party or unavailable imports. These are expected when the
// analyzer runs outside the module's full dependency tree and do not affect
// template variable extraction (which uses AST fallback paths).
func filterImportErrors(errs []string) []string {
	var filtered []string
	for _, e := range errs {
		// Drop errors that are purely about missing imports / packages.
		// Keep real parse errors and anything else actionable.
		if isImportError(e) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

func isImportError(e string) bool {
	importPhrases := []string{
		"could not import",
		"can't find import",
		"cannot find package",
		"no required module provides",
		"build constraints exclude all Go files",
	}
	lower := strings.ToLower(e)
	for _, phrase := range importPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
