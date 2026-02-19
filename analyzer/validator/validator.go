package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ValidateTemplates validates all templates against their render calls
func ValidateTemplates(renderCalls []RenderCall, baseDir string) []ValidationResult {
	var allErrors []ValidationResult

	for _, rc := range renderCalls {
		templatePath := filepath.Join(baseDir, rc.Template)
		errors := validateTemplateFile(templatePath, rc.Vars, rc.Template, baseDir)
		allErrors = append(allErrors, errors...)
	}

	return allErrors
}

// validateTemplateFile validates a single template file
func validateTemplateFile(templatePath string, vars []TemplateVar, templateName string, baseDir string) []ValidationResult {
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

	// Build root scope from vars
	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}

	// Parse template and validate
	return validateTemplateContent(string(content), varMap, templateName, baseDir)
}

// validateTemplateContent validates template content with proper scope tracking
func validateTemplateContent(content string, varMap map[string]TemplateVar, templateName string, baseDir string) []ValidationResult {
	var errors []ValidationResult

	// Build a stack of scopes
	var scopeStack []ScopeType

	// Initialize with root scope containing all top-level variables
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

	// Find all template actions
	actionPattern := regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
	lines := strings.Split(content, "\n")

	for lineNum, line := range lines {
		matches := actionPattern.FindAllStringSubmatchIndex(line, -1)

		for _, match := range matches {
			if len(match) < 4 {
				continue
			}

			action := strings.TrimSpace(line[match[2]:match[3]])
			col := match[2] + 1 // 1-based column

			// Skip comments
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

			// Handle if/else - they don't change scope
			if strings.HasPrefix(action, "if ") || action == "else" || strings.HasPrefix(action, "else if") {
				continue
			}

			// Handle template calls
			if strings.HasPrefix(action, "template ") {
				// Parse template call arguments
				parts := parseTemplateAction(action)

				// Check partial existence
				if len(parts) >= 1 {
					tmplName := parts[0]
					// Assuming partials are relative to baseDir (standard in many Go setups, or relative to views)
					// rex uses "views/..." usually.
					// We check relative to baseDir.
					fullPath := filepath.Join(baseDir, tmplName)
					if _, err := os.Stat(fullPath); os.IsNotExist(err) {
						errors = append(errors, ValidationResult{
							Template: templateName,
							Line:     lineNum + 1,
							Column:   col,
							Variable: tmplName,
							Message:  fmt.Sprintf(`Partial template "%s" could not be found at %s`, tmplName, fullPath),
							Severity: "error",
						})
					}
				}

				if len(parts) >= 2 {
					// Check if the context argument exists
					contextArg := parts[1]
					if strings.HasPrefix(contextArg, ".") && contextArg != "." {
						if err := validateVariableInScope(contextArg, scopeStack, varMap, lineNum+1, col, templateName); err != nil {
							errors = append(errors, *err)
						}
					}
				}
				continue
			}

			// Check for variable access
			// Pattern: starts with . or $ (but $ is always root)
			if strings.HasPrefix(action, ".") && !strings.HasPrefix(action, "..") {
				// This is a variable access
				if err := validateVariableInScope(action, scopeStack, varMap, lineNum+1, col, templateName); err != nil {
					errors = append(errors, *err)
				}
				continue
			}

			// Check function calls that might contain variable references
			// e.g., "not .IsLast", "eq .Name "foo""
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

// createScopeFromRange creates a new scope for a range block
func createScopeFromRange(expr string, scopeStack []ScopeType, varMap map[string]TemplateVar) ScopeType {
	expr = strings.TrimSpace(expr)

	// Handle variable assignment like "$item := .Items"
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

	// Handle root reference "."
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
	// parts[0] is empty. parts[1] is the first segment.
	if len(parts) < 2 {
		return ScopeType{Fields: []FieldInfo{}}
	}

	var currentField *FieldInfo
	var remainingParts []string

	firstPart := parts[1]

	// 1. Try finding in current scope (context-relative)
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

	// 2. If not found, try root scope (top-level variables)
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
		// Not found
		return ScopeType{Fields: []FieldInfo{}}
	}

	// 3. Traverse remaining path
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

	// Root reference "." is always valid
	if varExpr == "." {
		return nil
	}

	// Remove trailing dots or other punctuation
	varExpr = strings.TrimRight(varExpr, ".")

	parts := strings.Split(varExpr, ".")
	if len(parts) < 2 {
		return nil
	}

	// If we're inside a range block (more than just root scope), check against current scope first
	if len(scopeStack) > 1 {
		currentScope := scopeStack[len(scopeStack)-1]
		fieldName := parts[1]

		// Check if this field exists in the current scope
		var foundField *FieldInfo
		for _, f := range currentScope.Fields {
			if f.Name == fieldName {
				fCopy := f
				foundField = &fCopy
				break
			}
		}

		if foundField != nil {
			// Found in current scope, now validate the rest of the path
			if len(parts) > 2 {
				return validateNestedFields(parts[2:], foundField.Fields, foundField.TypeStr, varExpr, line, col, templateName)
			}
			return nil
		}

		// If not found in current scope, fall through to check root scope
	}

	// Check if this is accessing root context (.VarName) - only 1 level deep
	if len(parts) == 2 {
		rootVar := parts[1]

		// Check if it exists in the root scope
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				return nil
			}
		}

		// Check varMap directly
		if _, ok := varMap[rootVar]; ok {
			return nil
		}

		// Not found in root
		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	// Multi-part access like .visit.Patient.Name
	// Validate each part
	rootVar := parts[1]

	// First check if root exists
	var rootVarInfo *TemplateVar
	if v, ok := varMap[rootVar]; ok {
		rootVarInfo = &v
	} else {
		// Check root scope
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				// Found, now check nested fields
				return validateNestedFields(parts[2:], f.Fields, f.TypeStr, varExpr, line, col, templateName)
			}
		}

		// Not found
		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	// Validate the rest of the path
	if rootVarInfo != nil {
		return validateNestedFields(parts[2:], rootVarInfo.Fields, rootVarInfo.TypeStr, varExpr, line, col, templateName)
	}

	return nil
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
			// Build the partial expression up to this point
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
	// Remove "template " prefix
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
