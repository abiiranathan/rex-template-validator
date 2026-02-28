package validator

import (
	"fmt"
	"strings"

	"github.com/rex-template-analyzer/ast"
)

// ValidateTemplateContent performs comprehensive validation of template content
// with full scope tracking and nested template support.
//
// This is the core validation function that:
//   - Parses all template actions ({{ ... }})
//   - Tracks scope changes (with, range, if blocks)
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
// Returns: Slice of validation errors found in this content
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

	cur := 0
	lineNum := 0 // 0-based offset from start of this content block

	for cur < len(content) {
		openRel := strings.Index(content[cur:], "{{")
		if openRel == -1 {
			break
		}
		openIdx := cur + openRel

		// Add newlines between cur and openIdx
		lineNum += strings.Count(content[cur:openIdx], "\n")
		actualLineNum := lineNum + lineOffset

		closeRel := strings.Index(content[openIdx:], "}}")
		if closeRel == -1 {
			break // Unclosed tag
		}
		closeIdx := openIdx + closeRel

		// Extract and trim action content
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

		// Calculate column based on contentStart to match expected test behavior
		lastNewline := strings.LastIndexByte(content[:openIdx], '\n')
		col := contentStart - lastNewline

		var action string
		if contentStart < contentEnd {
			action = content[contentStart:contentEnd]
		}

		// Update cur and lineNum for next iteration
		lineNumInside := strings.Count(content[openIdx:closeIdx+2], "\n")
		cur = closeIdx + 2

		// Skip comments
		if strings.HasPrefix(action, "/*") || strings.HasPrefix(action, "//") {
			lineNum += lineNumInside
			continue
		}

		// Parse action into words
		words := strings.Fields(action)
		first := ""
		if len(words) > 0 {
			first = words[0]
		}

		// ── Handle {{define}} blocks ────────────────────────────────────
		// Define blocks create separate scopes and should not be validated
		// in the context of the parent template.
		if first == "define" {
			defineSkipDepth++
			lineNum += lineNumInside
			continue
		}

		// Track nesting depth inside define blocks
		if defineSkipDepth > 0 {
			switch first {
			case "if", "with", "range", "block":
				defineSkipDepth++
			case "end":
				defineSkipDepth--
			}
			lineNum += lineNumInside
			continue
		}

		// ── Handle scope popping (else, end) BEFORE validation ──────────
		isElse := first == "else"
		var elseAction string
		if isElse {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			} else {
				panic(fmt.Sprintf("Template validation error in %s:%d: unexpected {{else}} without matching {{if/with/range}}", templateName, actualLineNum))
			}
			if len(words) > 1 {
				elseAction = words[1] // "if", "with", "range"
			}
		} else if first == "end" {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			} else {
				panic(fmt.Sprintf("Template validation error in %s:%d: unexpected {{end}} without matching {{if/with/range}}", templateName, actualLineNum))
			}
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

		// ── Handle {{block}} blocks ─────────────────────────────────────
		// Block is similar to define but with inline content
		if first == "block" {
			defineSkipDepth++
			lineNum += lineNumInside
			continue
		}

		// ── Handle scope pushing (if, with, range, else) AFTER validation ─────
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
				// Plain else
				if len(scopeStack) > 0 {
					scopeStack = append(scopeStack, scopeStack[len(scopeStack)-1])
				} else {
					scopeStack = append(scopeStack, ScopeType{})
				}
				lineNum += lineNumInside
				continue
			}
		}

		if actionToPush == "range" {
			rangeExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "range"))
			newScope := createScopeFromRange(rangeExpr, scopeStack, varMap)
			scopeStack = append(scopeStack, newScope)
			lineNum += lineNumInside
			continue
		}

		if actionToPush == "with" {
			withExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "with"))
			newScope := createScopeFromWith(withExpr, scopeStack, varMap)
			scopeStack = append(scopeStack, newScope)
			lineNum += lineNumInside
			continue
		}

		if actionToPush == "if" {
			if len(scopeStack) > 0 {
				scopeStack = append(scopeStack, scopeStack[len(scopeStack)-1])
			} else {
				scopeStack = append(scopeStack, ScopeType{})
			}
			lineNum += lineNumInside
			continue
		}

		if isElse {
			lineNum += lineNumInside
			continue
		}

		// ── Handle {{template}} calls ───────────────────────────────────
		// Template calls invoke other templates/named blocks with a context.
		if first == "template" {
			partialErrs := validateTemplateCall(action, scopeStack, varMap, actualLineNum, col, templateName, baseDir, templateRoot, registry)
			errors = append(errors, partialErrs...)
		}

		lineNum += lineNumInside
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
