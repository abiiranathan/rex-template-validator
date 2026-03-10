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

	// pinCallSite rewrites every diagnostic that came from inside the
	// named block / partial so it points at the {{ template }} call site
	// in the CALLER, not at an internal line inside the callee.
	// The callee path is preserved in the Message so it's still visible.
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
		// Aggregate validation results for all call sites (contexts)
		anyValid := false
		allErrors := make([]ValidationResult, 0)
		for _, nt := range entries {
			partialScope := resolvePartialScope(contextArg, scopeStack, varMap, funcMaps)
			partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)
			partialErrors := ValidateTemplateContent(
				nt.Content,
				partialVarMap,
				nt.TemplatePath,
				baseDir,
				templateRoot,
				nt.Line,
				registry,
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
			registry,
			funcMaps,
		)
		errors = append(errors, pinCallSite(partialErrors)...)
	}

	return errors
}
