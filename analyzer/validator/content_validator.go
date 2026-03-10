package validator

import (
	"fmt"
	"strings"

	"github.com/rex-template-analyzer/ast"
)

// ValidateTemplateContent performs comprehensive validation of template content
// with full scope tracking, nested template support, and LOCAL VARIABLE TRACKING.
//
// NEW: Now tracks {{ $var := expr }} assignments and makes them available to
// the expression parser via the blockLocals parameter.
//
// This is the core validation function that:
//   - Parses all template actions ({{ ... }})
//   - Tracks scope changes (with, range, if blocks)
//   - Tracks local variable assignments ({{ $var := expr }})
//   - Validates all variable references
//   - Validates field access paths
//   - Handles nested template calls recursively
//   - Skips validation inside {{define}} blocks (separate scope)
//
// Scope tracking:
//   - Root scope: All top-level variables from varMap
//   - with scope: Changes dot context to specific variable
//   - range scope: Creates iteration scope with collection elements
//   - if scope: Maintains current scope (just for depth tracking)
//   - Local variables: Tracked per scope level and passed to expression parser
//
// Parameters:
//   - content: Template content string to validate
//   - varMap: Available variables (map for O(1) lookup)
//   - templateName: Template name for error reporting
//   - baseDir: Project root directory
//   - templateRoot: Template subdirectory
//   - lineOffset: Starting line number (for nested blocks)
//   - registry: Named block registry
//
// Returns: Slice of validation errors found in this content.
//
// Thread-safety: Read-only operations on shared data (varMap, registry).
// This function can be called concurrently for different templates.
func ValidateTemplateContent(
	content string,
	varMap map[string]ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	lineOffset int,
	registry map[string][]NamedBlockEntry,
	funcMaps ...FuncMapRegistry,
) []ValidationResult {
	var errors []ValidationResult
	effectiveFuncMaps := optionalFuncMapRegistry(funcMaps...)
	effectiveRegistry := mergeNamedBlockRegistry(registry, content, templateName)

	// Initialize scope stack with root scope
	var scopeStack []ScopeType
	rootScope := buildRootScope(varMap)
	scopeStack = append(scopeStack, rootScope)

	// Track depth inside {{define}} blocks (validation is skipped)
	defineSkipDepth := 0

	// openingActions tracks the keyword that opened each scope-stack frame
	openingActions := []string{"root"}

	cur := 0
	lineNum := 0 // 0-based offset from the start of this content block

	for cur < len(content) {
		openRel := strings.Index(content[cur:], "{{")
		if openRel == -1 {
			break
		}
		openIdx := cur + openRel

		// Count newlines between cur and openIdx
		lineNum += strings.Count(content[cur:openIdx], "\n")
		actualLineNum := lineNum + lineOffset

		closeRel := strings.Index(content[openIdx:], "}}")
		if closeRel == -1 {
			panic(fmt.Sprintf(
				"template %q: unclosed action tag '{{' at line %d — add the closing '}}'",
				templateName, actualLineNum,
			))
		}
		closeIdx := openIdx + closeRel

		// Trim whitespace and the optional '-' trim markers from the action body
		contentStart := openIdx + 2
		if contentStart < closeIdx && content[contentStart] == '-' {
			contentStart++
		}
		for contentStart < closeIdx && isWhitespace(content[contentStart]) {
			contentStart++
		}

		contentEnd := closeIdx
		if contentEnd > contentStart && content[contentEnd-1] == '-' {
			contentEnd--
		}
		for contentEnd > contentStart && isWhitespace(content[contentEnd-1]) {
			contentEnd--
		}

		// Column is relative to the start of the action content
		lastNewline := strings.LastIndexByte(content[:openIdx], '\n')
		col := contentStart - lastNewline

		var action string
		if contentStart < contentEnd {
			action = content[contentStart:contentEnd]
		}

		// Advance cursor and count newlines inside the tag
		lineNumInside := strings.Count(content[openIdx:closeIdx+2], "\n")
		cur = closeIdx + 2

		// Skip template comments
		if strings.Contains(action, "/*") && strings.Contains(action, "*/") {
			lineNum += lineNumInside
			continue
		}

		words := strings.Fields(action)
		first := ""
		if len(words) > 0 {
			first = words[0]
			// Handle cases like `if(eq` where there is no space before parenthesis
			if idx := strings.IndexByte(first, '('); idx != -1 {
				first = first[:idx]
			}
		}

		// ── Skip everything inside {{define}} / {{block}} bodies ────────────
		if defineSkipDepth > 0 {
			switch first {
			case "if", "with", "range", "block", "define":
				defineSkipDepth++
			case "end":
				defineSkipDepth--
				lineNum += lineNumInside
				continue
			case "else":
				// Intentionally ignored
			}
			lineNum += lineNumInside
			continue
		}

		// ── Handle scope popping (else, end) BEFORE validation ──────────
		isElse := first == "else"
		var elseAction string

		if isElse {
			if len(scopeStack) <= 1 {
				panic(fmt.Sprintf(
					"template %q: {{else}} at line %d has no matching opening block",
					templateName, actualLineNum,
				))
			}
			scopeStack = scopeStack[:len(scopeStack)-1]
			openingActions = openingActions[:len(openingActions)-1]
			if len(words) > 1 {
				elseAction = words[1]
				// Handle cases like `else if(eq`
				if idx := strings.IndexByte(elseAction, '('); idx != -1 {
					elseAction = elseAction[:idx]
				}
			}
		} else if first == "end" {
			if len(scopeStack) <= 1 {
				panic(fmt.Sprintf(
					"template %q: unexpected {{end}} at line %d — no open block to close",
					templateName, actualLineNum,
				))
			}
			scopeStack = scopeStack[:len(scopeStack)-1]
			openingActions = openingActions[:len(openingActions)-1]
			lineNum += lineNumInside
			continue
		}

		// ── Validate variables in action ────────────────────────────────
		// Extract and validate all variable references in this action.
		assignmentTargets := assignmentTargetSet(action)
		errors = append(errors, validateActionFunctions(action, first, templateName, actualLineNum, col, effectiveFuncMaps)...)
		extractVariablesFromAction(action, func(v string) {
			if assignmentTargets[v] {
				return
			}
			if err := validateVariableInScope(v, scopeStack, varMap); err != nil {
				err.Template = templateName
				err.Line = actualLineNum
				err.Column = col + strings.Index(action, v)
				if err.Column < col {
					err.Column = col
				}
				errors = append(errors, *err)
			}
		})

		if first == "block" {
			syntheticAction := "template " + strings.TrimSpace(strings.TrimPrefix(action, "block"))
			parts := parseTemplateAction(syntheticAction)
			if len(parts) >= 2 {
				// If other {{template "name"}} calls exist in the content for this
				// block, skip validation at the definition site — the template
				// call sites will each validate the block with their own scope.
				// This avoids false positives when the block is designed to be
				// called from a narrower scope (e.g. inside a range).
				blockName := parts[0]
				if !hasTemplateCallForBlock(content, blockName) {
					partialErrs := validateTemplateCall(syntheticAction, scopeStack, varMap, actualLineNum, col, templateName, baseDir, templateRoot, effectiveRegistry, effectiveFuncMaps)
					errors = append(errors, partialErrs...)
				}
			}
		}

		if first != "range" && first != "with" && first != "if" {
			registerInlineLocalAssignments(action, scopeStack, varMap, effectiveFuncMaps, templateName, actualLineNum, col, &errors)
		}

		// ── {{block}} and {{define}} open a skip region ──────────────────────
		if first == "block" || first == "define" {
			defineSkipDepth++
			lineNum += lineNumInside
			continue
		}

		// ── Push new scope for if / with / range ─────────────────────────────
		actionToPush := first
		exprToParse := action

		if isElse {
			if elseAction != "" {
				// Handle `else if`, `else with`, `else range`
				actionToPush = elseAction
				idx := strings.Index(action, words[1])
				if idx != -1 {
					exprToParse = action[idx:]
				}
			} else {
				// Plain {{else}}: inherit current scope
				top := ScopeType{}
				if len(scopeStack) > 0 {
					top = scopeStack[len(scopeStack)-1]
				}
				scopeStack = append(scopeStack, top)
				openingActions = append(openingActions, "else")
				lineNum += lineNumInside
				continue
			}
		}

		switch actionToPush {
		case "range":
			rangeExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "range"))
			assignmentNames, rangePipeline, hasAssignment := splitAssignment(rangeExpr)
			if hasAssignment {
				rangeExpr = rangePipeline
			}
			newScope := childScope(createScopeFromRange(rangeExpr, scopeStack, varMap, effectiveFuncMaps))
			if hasAssignment {
				registerRangeLocals(&newScope, assignmentNames, rangeExpr, scopeStack, varMap, effectiveFuncMaps, templateName, actualLineNum, col, &errors)
			}
			scopeStack = append(scopeStack, newScope)
			openingActions = append(openingActions, "range")

		case "with":
			withExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "with"))
			assignmentNames, withPipeline, hasAssignment := splitAssignment(withExpr)
			if hasAssignment {
				withExpr = withPipeline
			}
			newScope := childScope(createScopeFromWith(withExpr, scopeStack, varMap, effectiveFuncMaps))
			if hasAssignment {
				registerAssignedLocals(&newScope, assignmentNames, withExpr, scopeStack, varMap, effectiveFuncMaps, templateName, actualLineNum, col, &errors)
			}
			scopeStack = append(scopeStack, newScope)
			openingActions = append(openingActions, "with")

		case "if":
			// {{if}} does not change the dot context
			top := ScopeType{}
			if len(scopeStack) > 0 {
				top = childScope(scopeStack[len(scopeStack)-1])
			}
			ifExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "if"))
			assignmentNames, ifPipeline, hasAssignment := splitAssignment(ifExpr)
			if hasAssignment {
				registerAssignedLocals(&top, assignmentNames, ifPipeline, scopeStack, varMap, effectiveFuncMaps, templateName, actualLineNum, col, &errors)
			}
			scopeStack = append(scopeStack, top)
			openingActions = append(openingActions, "if")
		}

		// ── Handle {{template}} calls ───────────────────────────────────
		// Template calls invoke other templates/named blocks with a context.
		if first == "template" {
			partialErrs := validateTemplateCall(action, scopeStack, varMap, actualLineNum, col, templateName, baseDir, templateRoot, effectiveRegistry, effectiveFuncMaps)
			errors = append(errors, partialErrs...)
		}

		lineNum += lineNumInside
	}

	// ── Post-parse structural check ──────────────────────────────────────────
	if len(scopeStack) > 1 {
		unclosed := make([]string, 0, len(openingActions)-1)
		for _, a := range openingActions[1:] {
			unclosed = append(unclosed, "{{"+a+"}}")
		}
		panic(fmt.Sprintf(
			"template %q: %d unclosed scope block(s) at end of template — missing {{end}} for: %s",
			templateName, len(scopeStack)-1, strings.Join(unclosed, ", "),
		))
	}

	return errors
}

// hasTemplateCallForBlock reports whether the content contains a
// {{template "name" ...}} call (not a {{block}}) for the given block name.
func hasTemplateCallForBlock(content, blockName string) bool {
	// Look for {{ template "blockName" ... }} patterns, excluding {{ block "blockName" ... }}.
	searchQuoted := `"` + blockName + `"`
	idx := 0
	for idx < len(content) {
		pos := strings.Index(content[idx:], "{{")
		if pos == -1 {
			break
		}
		pos += idx
		closePos := strings.Index(content[pos:], "}}")
		if closePos == -1 {
			break
		}
		closePos += pos

		inner := strings.TrimSpace(content[pos+2 : closePos])
		// Strip trim markers
		inner = strings.TrimPrefix(inner, "-")
		inner = strings.TrimSuffix(inner, "-")
		inner = strings.TrimSpace(inner)

		if strings.HasPrefix(inner, "template ") && strings.Contains(inner, searchQuoted) {
			return true
		}
		idx = closePos + 2
	}
	return false
}

func optionalFuncMapRegistry(funcMaps ...FuncMapRegistry) FuncMapRegistry {
	if len(funcMaps) == 0 {
		return nil
	}
	if funcMaps[0] == nil {
		return nil
	}
	return funcMaps[0]
}

func mergeNamedBlockRegistry(registry map[string][]NamedBlockEntry, content, templateName string) map[string][]NamedBlockEntry {
	merged := make(map[string][]NamedBlockEntry)
	for name, entries := range registry {
		merged[name] = append([]NamedBlockEntry(nil), entries...)
	}
	extractNamedTemplatesFromContent(content, templateName, templateName, merged)
	return merged
}

func assignmentTargetSet(action string) map[string]bool {
	targets := make(map[string]bool)
	assignmentNames, _, ok := splitAssignment(action)
	if !ok {
		return targets
	}
	for _, name := range assignmentNames {
		targets[name] = true
	}
	return targets
}

func splitAssignment(action string) ([]string, string, bool) {
	parts := strings.SplitN(action, ":=", 2)
	if len(parts) != 2 {
		return nil, "", false
	}

	lhs := strings.TrimSpace(parts[0])
	rhs := strings.TrimSpace(parts[1])
	if lhs == "" || rhs == "" {
		return nil, "", false
	}

	fields := strings.Split(lhs, ",")
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		for _, token := range strings.Fields(strings.TrimSpace(field)) {
			if strings.HasPrefix(token, "$") {
				names = append(names, token)
			}
		}
	}
	if len(names) == 0 {
		return nil, "", false
	}

	return names, rhs, true
}

func registerInlineLocalAssignments(action string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry, templateName string, line int, col int, errors *[]ValidationResult) {
	if len(scopeStack) == 0 {
		return
	}
	assignmentNames, rhs, ok := splitAssignment(action)
	if !ok {
		return
	}
	registerAssignedLocals(&scopeStack[len(scopeStack)-1], assignmentNames, rhs, scopeStack, varMap, funcMaps, templateName, line, col, errors)
}

func registerAssignedLocals(frame *ScopeType, names []string, rhs string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry, templateName string, line int, col int, errors *[]ValidationResult) {
	funcErrs := validateExpressionFunctions(rhs, templateName, line, col, funcMaps)
	if len(funcErrs) > 0 {
		*errors = append(*errors, funcErrs...)
		return
	}
	if frame.Locals == nil {
		frame.Locals = make(map[string]ast.TemplateVar)
	}
	resolved := scopeToTemplateVar("", resolveScopeFromExpression(rhs, scopeStack, varMap, funcMaps))
	for _, name := range names {
		local := resolved
		local.Name = name
		frame.Locals[name] = local
	}
}

func registerRangeLocals(frame *ScopeType, names []string, rangeExpr string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry, templateName string, line int, col int, errors *[]ValidationResult) {
	if frame.Locals == nil {
		frame.Locals = make(map[string]ast.TemplateVar)
	}

	funcErrs := validateExpressionFunctions(rangeExpr, templateName, line, col, funcMaps)
	if len(funcErrs) > 0 {
		*errors = append(*errors, funcErrs...)
		return
	}

	collectionScope := resolveScopeFromExpression(rangeExpr, scopeStack, varMap, funcMaps)
	valueVar := scopeToTemplateVar("", *frame)

	if len(names) == 1 {
		valueVar.Name = names[0]
		frame.Locals[names[0]] = valueVar
		return
	}

	if len(names) >= 2 {
		keyType := ""
		switch {
		case collectionScope.IsMap:
			keyType = collectionScope.KeyType
		case collectionScope.IsSlice:
			keyType = "int"
		}
		frame.Locals[names[0]] = ast.TemplateVar{Name: names[0], TypeStr: keyType}
		valueVar.Name = names[1]
		frame.Locals[names[1]] = valueVar
	}
}

var templateBuiltins = map[string]bool{
	"and":      true,
	"or":       true,
	"not":      true,
	"eq":       true,
	"ne":       true,
	"lt":       true,
	"le":       true,
	"gt":       true,
	"ge":       true,
	"index":    true,
	"slice":    true,
	"len":      true,
	"print":    true,
	"printf":   true,
	"println":  true,
	"html":     true,
	"js":       true,
	"urlquery": true,
	"dict":     true,
	"add":      true,
	"sub":      true,
	"mul":      true,
	"div":      true,
	"mod":      true,
	"call":     true,
}

func validateActionFunctions(action, first, templateName string, line, col int, funcMaps FuncMapRegistry) []ValidationResult {
	trimmed := strings.TrimSpace(action)
	if first == "template" || first == "block" || first == "define" || first == "end" {
		return nil
	}
	if first == "else" {
		switch {
		case strings.HasPrefix(trimmed, "else if "):
			return validateExpressionFunctions(strings.TrimSpace(strings.TrimPrefix(trimmed, "else if")), templateName, line, col, funcMaps)
		case strings.HasPrefix(trimmed, "else with "):
			return validateExpressionFunctions(strings.TrimSpace(strings.TrimPrefix(trimmed, "else with")), templateName, line, col, funcMaps)
		case strings.HasPrefix(trimmed, "else range "):
			return validateExpressionFunctions(strings.TrimSpace(strings.TrimPrefix(trimmed, "else range")), templateName, line, col, funcMaps)
		default:
			return nil
		}
	}
	if first == "if" || first == "with" || first == "range" {
		return validateExpressionFunctions(strings.TrimSpace(strings.TrimPrefix(trimmed, first)), templateName, line, col, funcMaps)
	}
	return validateExpressionFunctions(trimmed, templateName, line, col, funcMaps)
}

func validateExpressionFunctions(expr, templateName string, line, col int, funcMaps FuncMapRegistry) []ValidationResult {
	if funcMaps == nil {
		return nil
	}
	var errors []ValidationResult
	for _, candidate := range functionCandidates(expr) {
		if templateBuiltins[candidate.name] {
			continue
		}
		if _, ok := funcMaps[candidate.name]; ok {
			continue
		}
		errors = append(errors, ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col + candidate.offset,
			Variable: candidate.name,
			Message:  fmt.Sprintf("Template function %q is not defined in the current FuncMap", candidate.name),
			Severity: "error",
		})
	}
	return errors
}

type functionCandidate struct {
	name   string
	offset int
}

func functionCandidates(expr string) []functionCandidate {
	var candidates []functionCandidate
	segments := strings.Split(expr, "|")
	segmentOffset := 0
	for index, segment := range segments {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" {
			segmentOffset += len(segment) + 1
			continue
		}

		tokens := strings.Fields(trimmed)
		if len(tokens) == 0 {
			segmentOffset += len(segment) + 1
			continue
		}

		candidate := ""
		if tokens[0] == "call" {
			if len(tokens) > 1 {
				candidate = strings.Trim(tokens[1], "()")
			}
		} else if index > 0 || len(tokens) > 1 {
			candidate = strings.Trim(tokens[0], "()")
		}

		if isFunctionIdentifier(candidate) {
			if offset := strings.Index(segment, candidate); offset >= 0 {
				candidates = append(candidates, functionCandidate{name: candidate, offset: segmentOffset + offset})
			}
		}

		segmentOffset += len(segment) + 1
	}
	return candidates
}

func isFunctionIdentifier(value string) bool {
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, ".") || strings.HasPrefix(value, "$") || strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "`") {
		return false
	}
	for index, char := range value {
		if index == 0 {
			if !(char == '_' || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z')) {
				return false
			}
			continue
		}
		if !(char == '_' || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

// buildRootScope creates the root scope from the available variables.
// The root scope contains all top-level variables accessible via $.VarName.
func buildRootScope(varMap map[string]ast.TemplateVar) ScopeType {
	// If the context was passed as a single value (e.g. via a partial call),
	// use it directly as the root scope.
	if dot, ok := varMap["."]; ok {
		return ScopeType{
			IsRoot:   true,
			TypeStr:  dot.TypeStr,
			Fields:   dot.Fields,
			IsSlice:  dot.IsSlice,
			IsMap:    dot.IsMap,
			KeyType:  dot.KeyType,
			ElemType: dot.ElemType,
		}
	}

	rootScope := ScopeType{
		IsRoot: true,
		Fields: make([]ast.FieldInfo, 0, len(varMap)),
	}

	for name, v := range varMap {
		rootScope.Fields = append(rootScope.Fields, ast.FieldInfo{
			Name:     name,
			TypeStr:  v.TypeStr,
			IsSlice:  v.IsSlice,
			IsMap:    v.IsMap,
			KeyType:  v.KeyType,
			ElemType: v.ElemType,
			Fields:   v.Fields,
		})
	}

	return rootScope
}
