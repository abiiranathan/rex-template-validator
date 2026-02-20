// Package validator
package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// NamedTemplate stores information about defined blocks and named templates
type NamedTemplate struct {
	Name     string
	Content  string
	FilePath string
	LineNum  int
}

// countLines counts newlines in a string
func countLines(s string) int {
	return strings.Count(s, "\n")
}

// parseAllNamedTemplates extracts all define and block declarations from template files
func parseAllNamedTemplates(baseDir, templateRoot string) map[string]NamedTemplate {
	registry := make(map[string]NamedTemplate)
	root := filepath.Join(baseDir, templateRoot)

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !isFileBasedPartial(path) {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		extractNamedTemplatesFromContent(string(content), rel, registry)
		return nil
	})

	return registry
}

// extractNamedTemplatesFromContent finds defined templates within content
func extractNamedTemplatesFromContent(content, templateName string, registry map[string]NamedTemplate) {
	actionPattern := regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
	matches := actionPattern.FindAllStringSubmatchIndex(content, -1)

	var activeName string
	var startIndex int
	var startLine int
	depth := 0

	for _, match := range matches {
		if len(match) < 4 {
			continue
		}

		fullActionStart := match[0]
		fullActionEnd := match[1]
		actionStart := match[2]
		actionEnd := match[3]

		action := strings.TrimSpace(content[actionStart:actionEnd])
		if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
			continue
		}

		words := strings.Fields(action)
		if len(words) == 0 {
			continue
		}

		first := words[0]

		switch first {
		case "if", "with", "range", "block":
			if activeName != "" {
				depth++
			} else if first == "block" && len(words) >= 2 {
				activeName = strings.Trim(words[1], `"`)
				startIndex = fullActionEnd
				startLine = countLines(content[:fullActionEnd]) + 1
				depth = 1
			}
		case "define":
			if activeName != "" {
				depth++
			} else if len(words) >= 2 {
				activeName = strings.Trim(words[1], `"`)
				startIndex = fullActionEnd
				startLine = countLines(content[:fullActionEnd]) + 1
				depth = 1
			}
		case "end":
			if activeName != "" {
				depth--
				if depth == 0 {
					registry[activeName] = NamedTemplate{
						Name:     activeName,
						Content:  content[startIndex:fullActionStart],
						FilePath: templateName,
						LineNum:  startLine,
					}
					activeName = ""
				}
			}
		}
	}
}

// ValidateTemplates validates all templates against their render calls
func ValidateTemplates(renderCalls []RenderCall, baseDir string, templateRoot string) []ValidationResult {
	namedTemplates := parseAllNamedTemplates(baseDir, templateRoot)

	var allErrors = []ValidationResult{}
	for _, rc := range renderCalls {
		templatePath := filepath.Join(baseDir, templateRoot, rc.Template)
		errors := validateTemplateFile(templatePath, rc.Vars, rc.Template, baseDir, templateRoot, namedTemplates)
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
func validateTemplateFile(templatePath string, vars []TemplateVar, templateName string, baseDir, templateRoot string, registry map[string]NamedTemplate) []ValidationResult {
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
	return validateTemplateContent(string(content), varMap, templateName, baseDir, templateRoot, 1, registry)
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
func validateTemplateContent(content string, varMap map[string]TemplateVar, templateName string, baseDir, templateRoot string, lineOffset int, registry map[string]NamedTemplate) []ValidationResult {
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

	skipDepth := 0

	for i, line := range lines {
		actualLineNum := i + lineOffset
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

			words := strings.Fields(action)
			first := ""
			if len(words) > 0 {
				first = words[0]
			}

			if first == "define" || first == "block" {
				skipDepth++
				continue
			}

			if skipDepth > 0 {
				switch first {
				case "if", "with", "range", "block":
					skipDepth++
				case "end":
					skipDepth--
				}
				continue
			}

			// Validate all variables in the action against the CURRENT scope before modifying it
			varsInAction := extractVariablesFromAction(action)
			for _, v := range varsInAction {
				if err := validateVariableInScope(v, scopeStack, varMap, actualLineNum, col, templateName); err != nil {
					errors = append(errors, *err)
				}
			}

			// Handle range
			if first == "range" {
				rangeExpr := strings.TrimSpace(action[6:])
				newScope := createScopeFromRange(rangeExpr, scopeStack, varMap)
				scopeStack = append(scopeStack, newScope)
				continue
			}

			// Handle with
			if first == "with" {
				withExpr := strings.TrimSpace(action[5:])
				newScope := createScopeFromWith(withExpr, scopeStack, varMap)
				scopeStack = append(scopeStack, newScope)
				continue
			}

			// Handle block (pushes scope like with, validates pipeline)
			// Wait, if block is skipped by skipDepth, we never reach here during the initial pass.
			// However, if we recursively validate the block's content from the registry,
			// the block string itself shouldn't be in the registry's content body (only the inside).
			// If it IS, we would need to not skip it. But let's check what the registry holds!

			// Handle if (pushes copy of current scope since `if` needs an `end`)
			if first == "if" {
				if len(scopeStack) > 0 {
					scopeStack = append(scopeStack, scopeStack[len(scopeStack)-1])
				} else {
					scopeStack = append(scopeStack, ScopeType{})
				}
				continue
			}

			// Handle end
			if first == "end" {
				if len(scopeStack) > 1 {
					scopeStack = scopeStack[:len(scopeStack)-1]
				}
				continue
			}

			// Handle template calls
			if first == "template" {
				parts := parseTemplateAction(action)

				if len(parts) >= 1 {
					tmplName := parts[0]

					if nt, ok := registry[tmplName]; ok {
						// Named template block found in registry
						var contextArg string
						if len(parts) >= 2 {
							contextArg = parts[1]
						}

						// Resolve the scope that will be passed as "." to the partial
						partialScope := resolvePartialScope(contextArg, scopeStack, varMap)

						// Build a varMap for the partial based on the resolved scope
						partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

						// Recursively validate the named template
						partialErrors := validateTemplateContent(nt.Content, partialVarMap, nt.FilePath, baseDir, templateRoot, nt.LineNum, registry)
						errors = append(errors, partialErrors...)

					} else if isFileBasedPartial(tmplName) {
						// File-based partial: check existence
						fullPath := filepath.Join(baseDir, templateRoot, tmplName)
						if _, err := os.Stat(fullPath); os.IsNotExist(err) {
							errors = append(errors, ValidationResult{
								Template: templateName, // caller template name (relative)
								Line:     actualLineNum,
								Column:   col,
								Variable: tmplName,
								Message:  fmt.Sprintf(`Partial template "%s" could not be found at %s`, tmplName, fullPath),
								Severity: "error",
							})
							continue
						}

						var contextArg string
						if len(parts) >= 2 {
							contextArg = parts[1]
						}

						// Resolve the scope that will be passed as "." to the partial
						partialScope := resolvePartialScope(contextArg, scopeStack, varMap)

						// Build a varMap for the partial based on the resolved scope
						partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

						// Recursively validate the partial with the resolved scope
						partialErrors := validateTemplateFile(fullPath, scopeVarsToTemplateVars(partialVarMap), tmplName, baseDir, templateRoot, registry)
						errors = append(errors, partialErrors...)
					}
				}
				continue
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

	if varExpr == "." || varExpr == "$" {
		return nil
	}

	varExpr = strings.TrimRight(varExpr, ".")

	parts := strings.Split(varExpr, ".")
	if len(parts) < 2 {
		return nil
	}

	isRootAccess := parts[0] == "$"

	if !isRootAccess && len(scopeStack) > 1 {
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

// extractVariablesFromAction extracts all valid variables from an action string,
// ignoring string literals and correctly splitting on operators and parentheses.
func extractVariablesFromAction(action string) []string {
	var vars []string
	var current strings.Builder
	inString := false
	stringChar := rune(0)

	for _, r := range action {
		switch {
		case !inString && (r == '"' || r == '`'):
			inString = true
			stringChar = r
			if current.Len() > 0 {
				vars = append(vars, current.String())
				current.Reset()
			}
		case inString && r == stringChar:
			inString = false
			current.Reset()
		case !inString && (r == ' ' || r == '(' || r == ')' || r == '|' || r == '=' || r == ',' || r == '+' || r == '-' || r == '*' || r == '/' || r == '!' || r == '<' || r == '>' || r == '%' || r == '&'):
			if current.Len() > 0 {
				vars = append(vars, current.String())
				current.Reset()
			}
		default:
			if !inString {
				current.WriteRune(r)
			}
		}
	}

	if current.Len() > 0 && !inString {
		vars = append(vars, current.String())
	}

	var validVars []string
	for _, v := range vars {
		v = strings.TrimSpace(v)
		if (strings.HasPrefix(v, ".") || strings.HasPrefix(v, "$.")) && v != "." && v != "$" && !strings.HasPrefix(v, "..") {
			validVars = append(validVars, v)
		}
	}

	return validVars
}

// ValidateTemplateFileStr exposes internal method for testing
func ValidateTemplateFileStr(content string, vars []TemplateVar, templateName string, baseDir, templateRoot string, registry map[string]NamedTemplate) []ValidationResult {
	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return validateTemplateContent(string(content), varMap, templateName, baseDir, templateRoot, 1, registry)
}

// ParseAllNamedTemplates exposes for testing
func ParseAllNamedTemplates(baseDir, templateRoot string) map[string]NamedTemplate {
	return parseAllNamedTemplates(baseDir, templateRoot)
}

// ExtractNamedTemplatesFromContent exposes for testing
func ExtractNamedTemplatesFromContent(content, templateName string, registry map[string]NamedTemplate) {
	extractNamedTemplatesFromContent(content, templateName, registry)
}
