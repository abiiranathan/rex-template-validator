package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rex-template-analyzer/ast"
)

// validateTemplateCall processes a {{template}} action, validating its context
// argument and recursively validating the nested template or named block.
func validateTemplateCall(
	action string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	actualLineNum int,
	col int,
	templateName string,
	baseDir string,
	templateRoot string,
	registry map[string][]NamedBlockEntry,
) []ValidationResult {
	var errors []ValidationResult
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
				return errors
			}
		}

		// Validate nested template - check if it's a named block
		if entries, ok := registry[tmplName]; ok && len(entries) > 0 {
			nt := entries[0]

			// Skip deep validation for untracked local vars ($var)
			// to prevent false positives
			if contextArg != "" && contextArg != "." && !strings.HasPrefix(contextArg, ".") {
				return errors
			}

			// Build scope for nested template
			partialScope := resolvePartialScope(contextArg, scopeStack, varMap)
			partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

			// Recursively validate nested template
			partialErrors := ValidateTemplateContent(
				nt.Content,
				partialVarMap,
				nt.TemplatePath,
				baseDir,
				templateRoot,
				nt.Line,
				registry,
			)
			errors = append(errors, partialErrors...)

		} else if IsFileBasedPartial(tmplName) {
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
				return errors
			}

			// Skip deep validation for untracked local vars
			if contextArg != "" && contextArg != "." && !strings.HasPrefix(contextArg, ".") {
				return errors
			}

			// Build scope for file-based partial
			partialScope := resolvePartialScope(contextArg, scopeStack, varMap)
			partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

			// Recursively validate file-based partial
			partialErrors := ValidateTemplateFile(
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

	return errors
}
