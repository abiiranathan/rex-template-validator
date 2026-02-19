// Package validator
package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ValidateTemplates validates all templates against their render calls
func ValidateTemplates(renderCalls []RenderCall, baseDir string, templateRoot string) []ValidationResult {
	var allErrors []ValidationResult
	for _, rc := range renderCalls {
		templatePath := filepath.Join(baseDir, templateRoot, rc.Template)
		errors := validateTemplateFile(templatePath, rc.Vars, rc.Template, baseDir, templateRoot)
		// Stamp each error with the originating Go file/line
		for i := range errors {
			errors[i].GoFile = rc.File
			errors[i].GoLine = rc.Line
		}
		allErrors = append(allErrors, errors...)
	}
	return allErrors
}

// validateTemplateFile validates a single template file
func validateTemplateFile(templatePath string, vars []TemplateVar, templateName string, baseDir, templateRoot string) []ValidationResult {
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return []ValidationResult{{
			Template: templateName,
			Line:     0,
			Column:   0,
			Variable: "",
			Message:  fmt.Sprintf("Could not read template file: %v", err),
			Severity: "error",
		}}
	}

	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	return validateTemplateContent(string(content), varMap, templateName, baseDir, templateRoot)
}

// isFileBasedPartial returns true if the template name looks like a file path
// (contains a path separator or has a file extension like .html, .tmpl, .gohtml)
func isFileBasedPartial(name string) bool {
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".tmpl", ".gohtml", ".tpl", ".htm":
		return true
	}
	return false
}

// validateTemplateContent validates template content with proper scope tracking
func validateTemplateContent(content string, varMap map[string]TemplateVar, templateName string, baseDir, templateRoot string) []ValidationResult {
	var errors []ValidationResult

	var scopeStack []ScopeType

	rootScope := ScopeType{
		IsRoot: true,
		Fields: make([]FieldInfo, 0),
	}
	for name, v := range varMap {
		rootScope.Fields = append(rootScope.Fields, FieldInfo{
			Name:    name,
			TypeStr: v.TypeStr,
			IsSlice: v.IsSlice,
			Fields:  v.Fields,
		})
	}
	scopeStack = append(scopeStack, rootScope)

	actionPattern := regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
	lines := strings.Split(content, "\n")

	for lineNum, line := range lines {
		matches := actionPattern.FindAllStringSubmatchIndex(line, -1)

		for _, match := range matches {
			if len(match) < 4 {
				continue
			}

			action := strings.TrimSpace(line[match[2]:match[3]])
			col := match[2] + 1

			if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
				continue
			}

			// Handle range
			if strings.HasPrefix(action, "range ") {
				rangeExpr := strings.TrimSpace(action[6:])
				newScope := createScopeFromRange(rangeExpr, scopeStack, varMap)
				scopeStack = append(scopeStack, newScope)
				continue
			}

			// Handle with
			if strings.HasPrefix(action, "with ") {
				withExpr := strings.TrimSpace(action[5:])
				newScope := createScopeFromWith(withExpr, scopeStack, varMap)
				scopeStack = append(scopeStack, newScope)
				continue
			}

			// Handle end
			if action == "end" {
				if len(scopeStack) > 1 {
					scopeStack = scopeStack[:len(scopeStack)-1]
				}
				continue
			}

			// Handle if/else
			if strings.HasPrefix(action, "if ") || action == "else" || strings.HasPrefix(action, "else if") {
				continue
			}

			// Handle template calls
			if strings.HasPrefix(action, "template ") {
				parts := parseTemplateAction(action)

				if len(parts) >= 1 {
					tmplName := parts[0]

					// FIX 1: Only resolve file-based partials, not named blocks
					if !isFileBasedPartial(tmplName) {
						// It's a named block (defined with {{ define "blockName" }}), skip file resolution
						// but still validate the context argument if present
						if len(parts) >= 2 {
							contextArg := parts[1]
							if strings.HasPrefix(contextArg, ".") && contextArg != "." {
								if err := validateVariableInScope(contextArg, scopeStack, varMap, lineNum+1, col, templateName); err != nil {
									errors = append(errors, *err)
								}
							}
						}
						continue
					}

					// File-based partial: check existence
					// FIX 2: Use tmplName (relative) for error reporting, not the full path
					fullPath := filepath.Join(baseDir, templateRoot, tmplName)
					if _, err := os.Stat(fullPath); os.IsNotExist(err) {
						errors = append(errors, ValidationResult{
							Template: templateName, // caller template name (relative)
							Line:     lineNum + 1,
							Column:   col,
							Variable: tmplName,
							Message:  fmt.Sprintf(`Partial template "%s" could not be found at %s`, tmplName, fullPath),
							Severity: "error",
						})
						continue
					}

					// FIX 3: Resolve context passed to partial and recursively validate it
					if len(parts) >= 2 {
						contextArg := parts[1]

						// Resolve the scope that will be passed as "." to the partial
						partialScope := resolvePartialScope(contextArg, scopeStack, varMap)

						// Build a varMap for the partial based on the resolved scope
						partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

						// Validate the context argument itself in current scope
						if strings.HasPrefix(contextArg, ".") && contextArg != "." {
							if err := validateVariableInScope(contextArg, scopeStack, varMap, lineNum+1, col, templateName); err != nil {
								errors = append(errors, *err)
							}
						}

						// Recursively validate the partial with the resolved scope
						// Use tmplName as the logical name for diagnostics (FIX 2)
						partialErrors := validateTemplateFile(fullPath, scopeVarsToTemplateVars(partialVarMap), tmplName, baseDir, templateRoot)
						errors = append(errors, partialErrors...)
					} else {
						// No context passed — validate partial with empty scope
						partialErrors := validateTemplateFile(fullPath, nil, tmplName, baseDir, templateRoot)
						errors = append(errors, partialErrors...)
					}
				}
				continue
			}

			// Variable access starting with .
			if strings.HasPrefix(action, ".") && !strings.HasPrefix(action, "..") {
				if err := validateVariableInScope(action, scopeStack, varMap, lineNum+1, col, templateName); err != nil {
					errors = append(errors, *err)
				}
				continue
			}

			// Check function call arguments for variable references
			words := strings.Fields(action)
			for _, word := range words {
				if strings.HasPrefix(word, ".") && !strings.HasPrefix(word, "..") {
					if err := validateVariableInScope(word, scopeStack, varMap, lineNum+1, col, templateName); err != nil {
						errors = append(errors, *err)
					}
				}
			}
		}
	}

	return errors
}

// resolvePartialScope resolves what scope/type the context argument refers to
func resolvePartialScope(contextArg string, scopeStack []ScopeType, varMap map[string]TemplateVar) ScopeType {
	if contextArg == "." {
		// Pass current scope as-is
		if len(scopeStack) > 0 {
			return scopeStack[len(scopeStack)-1]
		}
		return ScopeType{IsRoot: true}
	}
	if strings.HasPrefix(contextArg, ".") {
		return createScopeFromExpression(contextArg, scopeStack, varMap)
	}
	return ScopeType{Fields: []FieldInfo{}}
}

// buildPartialVarMap builds a varMap for a partial based on the context argument and resolved scope.
// When "." is passed, the partial sees all fields of the current scope as top-level vars.
// When ".SomeVar" is passed, the partial sees SomeVar's fields as top-level vars.
func buildPartialVarMap(contextArg string, partialScope ScopeType, scopeStack []ScopeType, varMap map[string]TemplateVar) map[string]TemplateVar {
	result := make(map[string]TemplateVar)

	if contextArg == "." {
		// The partial receives the current dot — expose all fields of current scope
		// If we're in root scope, that means all top-level vars are available
		if len(scopeStack) > 0 {
			currentScope := scopeStack[len(scopeStack)-1]
			if currentScope.IsRoot {
				// Pass through all root variables
				for k, v := range varMap {
					result[k] = v
				}
			} else {
				// Pass scope fields as top-level vars accessible via .FieldName
				for _, f := range currentScope.Fields {
					result[f.Name] = TemplateVar{
						Name:    f.Name,
						TypeStr: f.TypeStr,
						Fields:  f.Fields,
						IsSlice: f.IsSlice,
					}
				}
			}
		}
		return result
	}

	// ".SomeVar" — the partial's dot IS SomeVar, so its fields become top-level
	for _, f := range partialScope.Fields {
		result[f.Name] = TemplateVar{
			Name:    f.Name,
			TypeStr: f.TypeStr,
			Fields:  f.Fields,
			IsSlice: f.IsSlice,
		}
	}

	return result
}

// scopeVarsToTemplateVars converts a varMap back to a []TemplateVar slice
func scopeVarsToTemplateVars(varMap map[string]TemplateVar) []TemplateVar {
	var vars []TemplateVar
	for _, v := range varMap {
		vars = append(vars, v)
	}
	return vars
}

// createScopeFromRange creates a new scope for a range block
func createScopeFromRange(expr string, scopeStack []ScopeType, varMap map[string]TemplateVar) ScopeType {
	expr = strings.TrimSpace(expr)

	if strings.Contains(expr, ":=") {
		parts := strings.SplitN(expr, ":=", 2)
		if len(parts) == 2 {
			varExpr := strings.TrimSpace(parts[1])
			return createScopeFromExpression(varExpr, scopeStack, varMap)
		}
	}

	return createScopeFromExpression(expr, scopeStack, varMap)
}

// createScopeFromWith creates a new scope for a with block
func createScopeFromWith(expr string, scopeStack []ScopeType, varMap map[string]TemplateVar) ScopeType {
	return createScopeFromExpression(expr, scopeStack, varMap)
}

// createScopeFromExpression creates a scope from a variable expression with path traversal
func createScopeFromExpression(expr string, scopeStack []ScopeType, varMap map[string]TemplateVar) ScopeType {
	expr = strings.TrimSpace(expr)

	if expr == "." {
		if len(scopeStack) > 0 {
			return scopeStack[len(scopeStack)-1]
		}
		return ScopeType{IsRoot: true}
	}

	if !strings.HasPrefix(expr, ".") {
		return ScopeType{Fields: []FieldInfo{}}
	}

	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return ScopeType{Fields: []FieldInfo{}}
	}

	var currentField *FieldInfo
	var remainingParts []string

	firstPart := parts[1]

	if len(scopeStack) > 0 {
		currentScope := scopeStack[len(scopeStack)-1]
		for _, f := range currentScope.Fields {
			if f.Name == firstPart {
				fCopy := f
				currentField = &fCopy
				remainingParts = parts[2:]
				break
			}
		}
	}

	if currentField == nil {
		if v, ok := varMap[firstPart]; ok {
			currentField = &FieldInfo{
				Name:    v.Name,
				TypeStr: v.TypeStr,
				Fields:  v.Fields,
				IsSlice: v.IsSlice,
			}
			remainingParts = parts[2:]
		}
	}

	if currentField == nil {
		return ScopeType{Fields: []FieldInfo{}}
	}

	for _, part := range remainingParts {
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
			return ScopeType{Fields: []FieldInfo{}}
		}
	}

	return ScopeType{
		IsRoot:  false,
		VarName: expr,
		TypeStr: currentField.TypeStr,
		Fields:  currentField.Fields,
		IsSlice: currentField.IsSlice,
	}
}

// validateVariableInScope validates a variable access in the current scope
func validateVariableInScope(varExpr string, scopeStack []ScopeType, varMap map[string]TemplateVar, line, col int, templateName string) *ValidationResult {
	varExpr = strings.TrimSpace(varExpr)

	if varExpr == "." {
		return nil
	}

	varExpr = strings.TrimRight(varExpr, ".")

	parts := strings.Split(varExpr, ".")
	if len(parts) < 2 {
		return nil
	}

	if len(scopeStack) > 1 {
		currentScope := scopeStack[len(scopeStack)-1]
		fieldName := parts[1]

		var foundField *FieldInfo
		for _, f := range currentScope.Fields {
			if f.Name == fieldName {
				fCopy := f
				foundField = &fCopy
				break
			}
		}

		if foundField != nil {
			if len(parts) > 2 {
				return validateNestedFields(parts[2:], foundField.Fields, foundField.TypeStr, varExpr, line, col, templateName)
			}
			return nil
		}
	}

	if len(parts) == 2 {
		rootVar := parts[1]

		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				return nil
			}
		}

		if _, ok := varMap[rootVar]; ok {
			return nil
		}

		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	rootVar := parts[1]

	var rootVarInfo *TemplateVar
	if v, ok := varMap[rootVar]; ok {
		rootVarInfo = &v
	} else {
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				return validateNestedFields(parts[2:], f.Fields, f.TypeStr, varExpr, line, col, templateName)
			}
		}

		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	return validateNestedFields(parts[2:], rootVarInfo.Fields, rootVarInfo.TypeStr, varExpr, line, col, templateName)

}

// validateNestedFields validates a field path against available fields
func validateNestedFields(fieldParts []string, fields []FieldInfo, parentTypeName, fullExpr string, line, col int, templateName string) *ValidationResult {
	currentFields := fields
	parentType := parentTypeName

	for _, fieldName := range fieldParts {
		found := false
		var nextFields []FieldInfo

		for _, f := range currentFields {
			if f.Name == fieldName {
				found = true
				nextFields = f.Fields
				parentType = f.TypeStr
				break
			}
		}

		if !found {
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

		currentFields = nextFields
	}

	return nil
}

// parseTemplateAction parses a template action to extract arguments
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
			inString = true
			stringChar = r
			if current.Len() > 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
			}
		case inString && r == stringChar:
			inString = false
			parts = append(parts, current.String())
			current.Reset()
		case !inString && r == ' ':
			if current.Len() > 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}

	return parts
}
