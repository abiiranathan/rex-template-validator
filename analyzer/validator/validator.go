// Package validator
package validator

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// parseAllNamedTemplates extracts all define and block declarations from template files
func parseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	registry := make(map[string][]NamedBlockEntry)
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
		// Normalize rel path to use forward slashes for cross-platform TS consistency
		rel = filepath.ToSlash(rel)

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		extractNamedTemplatesFromContent(string(content), path, rel, registry)
		return nil
	})

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

	return registry, errors
}

// extractNamedTemplatesFromContent finds defined templates within content
func extractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry map[string][]NamedBlockEntry) {
	var activeName string
	var startIndex int
	var startLine int
	var startCol int
	depth := 0

	lineStart := 0
	lineNum := 1

	for lineStart < len(content) {
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		var line string
		if lineEnd == -1 {
			line = content[lineStart:]
		} else {
			lineEnd += lineStart
			line = content[lineStart:lineEnd]
		}

		cur := 0
		for {
			openRel := strings.Index(line[cur:], "{{")
			if openRel == -1 {
				break
			}
			open := cur + openRel

			closeRel := strings.Index(line[open:], "}}")
			if closeRel == -1 {
				break
			}
			close := open + closeRel
			fullActionEndRel := close + 2

			// Absolute positions
			fullActionStart := lineStart + open
			fullActionEnd := lineStart + fullActionEndRel

			// Content extraction
			contentStart := open + 2
			for contentStart < close {
				r := line[contentStart]
				if r != ' ' && r != '\t' {
					break
				}
				contentStart++
			}

			contentEnd := close
			for contentEnd > contentStart {
				r := line[contentEnd-1]
				if r != ' ' && r != '\t' {
					break
				}
				contentEnd--
			}

			action := line[contentStart:contentEnd]

			// Advance for next iteration
			cur = fullActionEndRel

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
					startLine = lineNum
					startCol = open + 1
					depth = 1
				}
			case "define":
				if activeName != "" {
					depth++
				} else if len(words) >= 2 {
					activeName = strings.Trim(words[1], `"`)
					startIndex = fullActionEnd
					startLine = lineNum
					startCol = open + 1
					depth = 1
				}
			case "end":
				if activeName != "" {
					depth--
					if depth == 0 {
						registry[activeName] = append(registry[activeName], NamedBlockEntry{
							Name:         activeName,
							Content:      content[startIndex:fullActionStart],
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

		if lineEnd == -1 {
			lineStart = len(content)
		} else {
			lineStart = lineEnd + 1
		}
		lineNum++
	}
}

// ValidateTemplates validates all templates against their render calls
func ValidateTemplates(renderCalls []RenderCall, baseDir string, templateRoot string) ([]ValidationResult, map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	namedBlocks, namedBlockErrors := parseAllNamedTemplates(baseDir, templateRoot)

	var allErrors = []ValidationResult{}
	for _, rc := range renderCalls {
		templatePath := filepath.Join(baseDir, templateRoot, rc.Template)
		errors := validateTemplateFile(templatePath, rc.Vars, rc.Template, baseDir, templateRoot, namedBlocks)
		for i := range errors {
			errors[i].GoFile = rc.File
			errors[i].GoLine = rc.Line
		}
		allErrors = append(allErrors, errors...)
	}
	return allErrors, namedBlocks, namedBlockErrors
}

// validateTemplateFile validates a single template file
func validateTemplateFile(templatePath string, vars []TemplateVar, templateName string, baseDir, templateRoot string, registry map[string][]NamedBlockEntry) []ValidationResult {
	content, err := os.ReadFile(templatePath)
	if err != nil {
		ext := filepath.Ext(templateName)
		// namedTemplates are not real files.
		if ext != "" {
			return []ValidationResult{{
				Template: templateName,
				Line:     0,
				Column:   0,
				Variable: "",
				Message:  fmt.Sprintf("Could not read template file: %v", err),
				Severity: "error",
			}}
		}
	}

	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return validateTemplateContent(string(content), varMap, templateName, baseDir, templateRoot, 1, registry)
}

// isFileBasedPartial returns true if the template name looks like a file path
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
func validateTemplateContent(content string, varMap map[string]TemplateVar, templateName string, baseDir, templateRoot string, lineOffset int, registry map[string][]NamedBlockEntry) []ValidationResult {
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

	// defineSkipDepth tracks depth inside {{ define "..." }} blocks.
	defineSkipDepth := 0

	lineStart := 0
	lineNum := 0

	for lineStart < len(content) {
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		var line string
		if lineEnd == -1 {
			line = content[lineStart:]
			// Move lineStart to end to break loop after this
		} else {
			lineEnd += lineStart
			line = content[lineStart:lineEnd]
		}

		actualLineNum := lineNum + lineOffset
		lineNum++

		cur := 0

		for {
			openRel := strings.Index(line[cur:], "{{")
			if openRel == -1 {
				break
			}
			open := cur + openRel

			closeRel := strings.Index(line[open:], "}}")
			if closeRel == -1 {
				break
			}
			close := open + closeRel
			fullActionEnd := close + 2

			// Mimic regex behavior: find content start/end skipping whitespace
			contentStart := open + 2
			for contentStart < close {
				r := line[contentStart]
				if r != ' ' && r != '\t' {
					break
				}
				contentStart++
			}

			contentEnd := close
			for contentEnd > contentStart {
				r := line[contentEnd-1]
				if r != ' ' && r != '\t' {
					break
				}
				contentEnd--
			}

			// action is the trimmed content
			action := line[contentStart:contentEnd]
			col := contentStart + 1

			// Advance for next iteration
			cur = fullActionEnd

			if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
				continue
			}

			words := strings.Fields(action)
			first := ""
			if len(words) > 0 {
				first = words[0]
			}

			// ── define skip logic ──────────────────────────────────────────────
			if first == "define" {
				defineSkipDepth++
				continue
			}

			if defineSkipDepth > 0 {
				switch first {
				case "if", "with", "range", "block":
					defineSkipDepth++
				case "end":
					defineSkipDepth--
				}
				continue
			}

			// ── block handling ─────────────────────────────────────────────────────
			if first == "block" {
				defineSkipDepth++
				continue
			}

			// Validate all variables in the action against the CURRENT scope.
			// Skip this for `template` actions — the dedicated handler below
			// validates the context argument itself, so scanning here would
			// produce duplicate errors for invalid context args like .NonExistent.
			if first != "template" {
				extractVariablesFromAction(action, func(v string) {
					if err := validateVariableInScope(v, scopeStack, varMap, actualLineNum, col, templateName); err != nil {
						errors = append(errors, *err)
					}
				})
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

					var contextArg string
					if len(parts) >= 2 {
						contextArg = parts[1]
					}

					if contextArg != "" && contextArg != "." {
						if !validateContextArg(contextArg, scopeStack, varMap) {
							errors = append(errors, ValidationResult{
								Template: templateName,
								Line:     actualLineNum,
								Column:   col,
								Variable: contextArg,
								Message:  fmt.Sprintf(`Template variable "%s" is not defined in the render context`, contextArg),
								Severity: "error",
							})
							continue
						}
					}

					if entries, ok := registry[tmplName]; ok && len(entries) > 0 {
						nt := entries[0]
						partialScope := resolvePartialScope(contextArg, scopeStack, varMap)
						partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)
						partialErrors := validateTemplateContent(nt.Content, partialVarMap, nt.TemplatePath, baseDir, templateRoot, nt.Line, registry)
						errors = append(errors, partialErrors...)

					} else if isFileBasedPartial(tmplName) {
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
							continue
						}

						partialScope := resolvePartialScope(contextArg, scopeStack, varMap)
						partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)
						partialErrors := validateTemplateFile(fullPath, scopeVarsToTemplateVars(partialVarMap), tmplName, baseDir, templateRoot, registry)
						errors = append(errors, partialErrors...)
					}
				}
				continue
			}
		}

		if lineEnd == -1 {
			lineStart = len(content)
		} else {
			lineStart = lineEnd + 1
		}
	}

	return errors
}

// resolvePartialScope resolves what scope/type the context argument refers to
func resolvePartialScope(contextArg string, scopeStack []ScopeType, varMap map[string]TemplateVar) ScopeType {
	if contextArg == "." {
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

// buildPartialVarMap builds a varMap for a partial based on the context argument.
func buildPartialVarMap(contextArg string, partialScope ScopeType, scopeStack []ScopeType, varMap map[string]TemplateVar) map[string]TemplateVar {
	result := make(map[string]TemplateVar)

	if contextArg == "." {
		if len(scopeStack) > 0 {
			currentScope := scopeStack[len(scopeStack)-1]
			if currentScope.IsRoot {
				maps.Copy(result, varMap)
			} else {
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

	// ".SomeVar[.Nested...]" — the partial's dot IS that value, so its fields become top-level
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

// createScopeFromExpression creates a scope from a variable expression with
// full path traversal — supports arbitrary depth (e.g. ".User.Profile.Address").
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

	// parts[0] is "" (leading dot), parts[1] is the first name segment
	var currentField *FieldInfo
	firstPart := parts[1]

	if len(scopeStack) > 0 {
		currentScope := scopeStack[len(scopeStack)-1]
		for _, f := range currentScope.Fields {
			if f.Name == firstPart {
				fCopy := f
				currentField = &fCopy
				break
			}
		}
	}

	if currentField == nil {
		if v, ok := varMap[firstPart]; ok {
			currentField = &FieldInfo{
				Name:     v.Name,
				TypeStr:  v.TypeStr,
				Fields:   v.Fields,
				IsSlice:  v.IsSlice,
				IsMap:    v.IsMap,
				KeyType:  v.KeyType,
				ElemType: v.ElemType,
			}
		}
	}

	if currentField == nil {
		return ScopeType{Fields: []FieldInfo{}}
	}

	// Traverse remaining path segments (parts[2:])
	for _, part := range parts[2:] {
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
		IsRoot:   false,
		VarName:  expr,
		TypeStr:  currentField.TypeStr,
		Fields:   currentField.Fields,
		IsSlice:  currentField.IsSlice,
		IsMap:    currentField.IsMap,
		KeyType:  currentField.KeyType,
		ElemType: currentField.ElemType,
	}
}

// validateVariableInScope validates a variable access in the current scope.
// Supports unlimited path depth.
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

	// Non-root access inside a scoped block: check current scope first.
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
				return validateNestedFields(parts[2:], foundField.Fields, foundField.TypeStr, foundField.IsMap, foundField.ElemType, varExpr, line, col, templateName)
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
				return validateNestedFields(parts[2:], f.Fields, f.TypeStr, f.IsMap, f.ElemType, varExpr, line, col, templateName)
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

	return validateNestedFields(parts[2:], rootVarInfo.Fields, rootVarInfo.TypeStr, rootVarInfo.IsMap, rootVarInfo.ElemType, varExpr, line, col, templateName)
}

// validateNestedFields validates a field path against available fields.
// Supports unlimited depth by recursing through the FieldInfo tree.
func validateNestedFields(fieldParts []string, fields []FieldInfo, parentTypeName string, isMap bool, elemType string, fullExpr string, line, col int, templateName string) *ValidationResult {
	currentFields := fields
	parentType := parentTypeName
	currentIsMap := isMap
	currentElemType := elemType

	for _, fieldName := range fieldParts {
		if currentIsMap {
			// This part is a map key access.
			// The result is the value type.
			// We need to parse currentElemType to know if the value is itself a map, slice, or struct.

			// Unwrap pointers from type string first
			baseType := currentElemType
			for strings.HasPrefix(baseType, "*") {
				baseType = baseType[1:]
			}

			newIsMap := false
			newElemType := ""

			if strings.HasPrefix(baseType, "map[") {
				// Parse map[Key]Value with balanced brackets
				depth := 0
				splitIdx := -1
				// baseType starts with "map[", so the first bracket is at index 3
				for i := 3; i < len(baseType); i++ {
					if baseType[i] == '[' {
						depth++
					} else if baseType[i] == ']' {
						depth--
						if depth == 0 {
							splitIdx = i
							break
						}
					}
				}

				if splitIdx != -1 {
					// keyType := baseType[4:splitIdx]
					valType := baseType[splitIdx+1:]
					newIsMap = true
					newElemType = strings.TrimSpace(valType)
				}
			} else if strings.HasPrefix(baseType, "[]") {
				// Slice
				newElemType = baseType[2:]
			}

			currentIsMap = newIsMap
			if newElemType != "" {
				currentElemType = newElemType
			} else {
				// It's a struct or basic type.
				// Parent type becomes the element type.
				parentType = currentElemType
			}

			// Fields remain the same because they represent the struct fields at the bottom
			continue
		}

		found := false
		var nextFields []FieldInfo
		var nextIsMap bool
		var nextElemType string

		for _, f := range currentFields {
			if f.Name == fieldName {
				found = true
				nextFields = f.Fields
				parentType = f.TypeStr
				nextIsMap = f.IsMap
				nextElemType = f.ElemType
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
		currentIsMap = nextIsMap
		currentElemType = nextElemType
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

// extractVariablesFromAction extracts all valid variables from an action string.
func extractVariablesFromAction(action string, onVar func(string)) {
	start := -1
	inString := false
	stringChar := rune(0)

	for i, r := range action {
		if inString {
			if r == stringChar {
				inString = false
			}
			continue
		}

		switch r {
		case '"', '`':
			if start != -1 {
				emitVar(action[start:i], onVar)
				start = -1
			}
			inString = true
			stringChar = r

		case ' ', '(', ')', '|', '=', ',', '+', '-', '*', '/', '!', '<', '>', '%', '&':
			if start != -1 {
				emitVar(action[start:i], onVar)
				start = -1
			}

		default:
			if start == -1 {
				start = i
			}
		}
	}

	if start != -1 {
		emitVar(action[start:], onVar)
	}
}

func emitVar(v string, onVar func(string)) {
	v = strings.TrimSpace(v)
	if (strings.HasPrefix(v, ".") || strings.HasPrefix(v, "$.")) && v != "." && v != "$" && !strings.HasPrefix(v, "..") {
		onVar(v)
	}
}

// validateContextArg checks whether a template call context expression resolves
// in the current scope.
func validateContextArg(contextArg string, scopeStack []ScopeType, varMap map[string]TemplateVar) bool {
	if contextArg == "" || contextArg == "." || contextArg == "$" {
		return true
	}
	result := validateVariableInScope(contextArg, scopeStack, varMap, 0, 0, "")
	return result == nil
}

// ValidateTemplateFileStr exposes internal method for testing
func ValidateTemplateFileStr(content string, vars []TemplateVar, templateName string, baseDir, templateRoot string, registry map[string][]NamedBlockEntry) []ValidationResult {
	varMap := make(map[string]TemplateVar)
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return validateTemplateContent(string(content), varMap, templateName, baseDir, templateRoot, 1, registry)
}

// ParseAllNamedTemplates exposes for testing
func ParseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	return parseAllNamedTemplates(baseDir, templateRoot)
}

// ExtractNamedTemplatesFromContent exposes for testing
func ExtractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry map[string][]NamedBlockEntry) {
	extractNamedTemplatesFromContent(content, absolutePath, templatePath, registry)
}
