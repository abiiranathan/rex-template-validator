package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abiiranathan/go-template-lsp/analyzer/ast"
)

// validateTemplateCallWithRegistry is the hot-path implementation. It accepts
// an already-merged registry and passes it directly into recursive
// ValidateTemplateContent calls, breaking the re-merge cycle.
//
// The critical difference from the old validateTemplateCall: named block
// validation calls validateTemplateContentWithRegistry (the internal variant)
// instead of ValidateTemplateContent (the public variant), bypassing the
// mergeNamedBlockRegistry call entirely since the registry is already current.
func validateTemplateCallWithRegistry(
	action string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	actualLineNum int,
	col int,
	templateName string,
	baseDir string,
	templateRoot string,
	registry map[string][]NamedBlockEntry,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	var errors []ValidationResult
	parts := parseTemplateAction(action)

	if len(parts) < 1 {
		return errors
	}

	tmplName := parts[0]
	var contextArg string
	if len(parts) >= 2 {
		contextArg = parts[1]
	}

	if contextArg != "" && contextArg != "." {
		if err := validateContextArg(contextArg, scopeStack, varMap, funcMaps); err != nil {
			err.Template = templateName
			err.Line = actualLineNum
			err.Column = max(col+strings.Index(action, contextArg), col)
			errors = append(errors, *err)
			return errors
		}
	}

	pinCallSite := func(inner []ValidationResult) []ValidationResult {
		for i := range inner {
			e := &inner[i]
			e.Message = fmt.Sprintf(
				`[in named template %q @ %s] %s`,
				tmplName, e.Template, e.Message,
			)
			if e.Template != templateName {
				e.Template = templateName
				e.Line = actualLineNum
				e.Column = col
			}
		}
		return inner
	}

	if entries, ok := registry[tmplName]; ok && len(entries) > 0 {
		anyValid := false
		allErrors := make([]ValidationResult, 0)
		for _, nt := range entries {
			partialScope := resolvePartialScope(contextArg, scopeStack, varMap, funcMaps)
			partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)
			// Use the internal variant — registry is already merged, skip re-merge.
			partialErrors := validateTemplateContentWithRegistry(
				nt.Content,
				partialVarMap,
				nt.TemplatePath,
				baseDir,
				templateRoot,
				nt.Line,
				registry, // pass through unchanged
				funcMaps,
			)
			if len(partialErrors) == 0 {
				anyValid = true
			}
			allErrors = append(allErrors, pinCallSite(partialErrors)...)
		}
		if !anyValid {
			errors = append(errors, allErrors...)
		}

	} else if IsFileBasedPartial(tmplName) {
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

		partialScope := resolvePartialScope(contextArg, scopeStack, varMap, funcMaps)
		partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

		partialErrors := ValidateTemplateFile(
			fullPath,
			scopeVarsToTemplateVars(partialVarMap),
			tmplName,
			baseDir,
			templateRoot,
			registry, // pass through — ValidateTemplateFile already handles merge
			funcMaps,
		)
		errors = append(errors, pinCallSite(partialErrors)...)
	}

	return errors
}
