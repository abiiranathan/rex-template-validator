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
) []ValidationResult {
	var errors []ValidationResult

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
		extractVariablesFromAction(action, func(v string) {
			if err := validateVariableInScope(
				v,
				scopeStack,
				varMap,
				actualLineNum,
				col,
				templateName,
			); err != nil {
				errors = append(errors, *err)
			}
		})

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
				actionToPush = elseAction
				idx := strings.Index(action, elseAction)
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
			scopeStack = append(scopeStack, createScopeFromRange(rangeExpr, scopeStack, varMap))
			openingActions = append(openingActions, "range")

		case "with":
			withExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "with"))
			scopeStack = append(scopeStack, createScopeFromWith(withExpr, scopeStack, varMap))
			openingActions = append(openingActions, "with")

		case "if":
			// {{if}} does not change the dot context
			top := ScopeType{}
			if len(scopeStack) > 0 {
				top = scopeStack[len(scopeStack)-1]
			}
			scopeStack = append(scopeStack, top)
			openingActions = append(openingActions, "if")
		}

		// ── Handle {{template}} calls ───────────────────────────────────
		// Template calls invoke other templates/named blocks with a context.
		if first == "template" {
			partialErrs := validateTemplateCall(action, scopeStack, varMap, actualLineNum, col, templateName, baseDir, templateRoot, registry)
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

// buildRootScope creates the root scope from the available variables.
// The root scope contains all top-level variables accessible via $.VarName.
func buildRootScope(varMap map[string]ast.TemplateVar) ScopeType {
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
