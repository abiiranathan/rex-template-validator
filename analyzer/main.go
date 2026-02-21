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
	templateRoot := flag.String("template-root", "", "Root directory for templates (relative to template-base-dir)")
	templateBaseDir := flag.String("template-base-dir", "", "Base directory for template-root (defaults to -dir if not set)")
	validate := flag.Bool("validate", false, "Validate templates against render calls")
	contextFile := flag.String("context-file", "", "Path to JSON file with additional context variables")
	flag.Parse()

	absDir := mustAbs(*dir)

	// If no explicit template base dir is given, fall back to the source dir.
	templateBase := absDir
	if *templateBaseDir != "" {
		templateBase = mustAbs(*templateBaseDir)
	}

	result := validator.AnalyzeDir(absDir, *contextFile, validator.DefaultConfig)
	result.Errors = filterImportErrors(result.Errors)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if *validate {
		enc.Encode(struct {
			RenderCalls      []validator.RenderCall       `json:"renderCalls"`
			ValidationErrors []validator.ValidationResult `json:"validationErrors"`
			Errors           []string                     `json:"errors,omitempty"`
		}{
			RenderCalls:      result.RenderCalls,
			ValidationErrors: validator.ValidateTemplates(result.RenderCalls, templateBase, *templateRoot),
			Errors:           result.Errors,
		})
	} else {
		enc.Encode(result)
	}
}

// mustAbs returns the absolute form of path, panicking if it cannot be resolved.
// filepath.Abs only fails when os.Getwd fails, which indicates a broken
// working directory — not a condition worth recovering from gracefully.
func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic("could not resolve absolute path for " + path + ": " + err.Error())
	}
	return abs
}

// filterImportErrors removes type-check errors caused by missing third-party
// dependencies. These are expected when the analyzer runs outside the module's
// full dependency tree and do not affect template variable extraction, which
// uses AST fallback paths.
func filterImportErrors(errs []string) []string {
	filtered := make([]string, 0, len(errs))
	for _, e := range errs {
		if !isImportError(e) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func isImportError(e string) bool {
	lower := strings.ToLower(e)
	for _, phrase := range []string{
		"could not import",
		"can't find import",
		"cannot find package",
		"no required module provides",
		"build constraints exclude all Go files",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
