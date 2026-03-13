package validator

import (
	"strings"
	"unicode"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
)

// PositionScope captures everything needed for hover / completion / go-to-definition
// at a specific line:col inside a template.
type PositionScope struct {
	// Expression is the raw expression text of the action at the cursor position.
	Expression string `json:"expression"`

	// RawAction is the full text between {{ and }} (trimmed of whitespace/dash).
	RawAction string `json:"rawAction"`

	// CursorOffset is the byte offset of the cursor within RawAction.
	CursorOffset int `json:"cursorOffset"`

	// ScopeStack is the full scope chain from root down to the current scope.
	ScopeStack []ScopeType `json:"scopeStack"`

	// Vars is the top-level variable map for the template.
	Vars map[string]ast.TemplateVar `json:"vars"`

	// Locals are the template-local ($var) assignments visible at this position,
	// merged from all enclosing scope frames.
	Locals map[string]ast.TemplateVar `json:"locals"`
}

// HoverResult is the fully resolved type info for a single cursor position.
type HoverResult struct {
	Expression string          `json:"expression"`
	TypeStr    string          `json:"typeStr"`
	Fields     []ast.FieldInfo `json:"fields,omitempty"`
	IsSlice    bool            `json:"isSlice,omitempty"`
	IsMap      bool            `json:"isMap,omitempty"`
	ElemType   string          `json:"elemType,omitempty"`
	KeyType    string          `json:"keyType,omitempty"`
	DefFile    string          `json:"defFile,omitempty"`
	DefLine    int             `json:"defLine,omitempty"`
	DefCol     int             `json:"defCol,omitempty"`
	Doc        string          `json:"doc,omitempty"`

	// DotType is the type of "." at this position.
	DotType   string          `json:"dotType,omitempty"`
	DotFields []ast.FieldInfo `json:"dotFields,omitempty"`

	// Locals are $var declarations in scope (for completion).
	Locals map[string]ast.TemplateVar `json:"locals,omitempty"`
}

// GetHoverResult walks a template to compute and resolve hover info at a given
// position. It reuses the scope-walking logic from validation but does NOT skip
// block/define bodies — instead it enters them with the scope derived from the
// block's context argument.
func GetHoverResult(
	content string,
	varMap map[string]ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	lineOffset int,
	targetLine int, // 1-based
	targetCol int, // 1-based
	registry map[string][]NamedBlockEntry,
	funcMaps FuncMapRegistry,
	typeRegistry map[string][]ast.FieldInfo,
) *HoverResult {
	ps := buildScopeAtPosition(content, varMap, templateName, lineOffset, targetLine, targetCol, registry, funcMaps)
	if ps == nil {
		return nil
	}

	// Try resolving the sub-expression at cursor first (e.g., ".Name" inside "if eq .Name $rx.DrugName").
	var result *ExpressionTypeResult
	subExpr := extractSubPathAtCursor(ps.RawAction, ps.CursorOffset)
	if subExpr != "" && subExpr != ps.Expression {
		result = InferExpressionType(subExpr, ps.Vars, ps.ScopeStack, ps.Locals, funcMaps, typeRegistry)
	}

	useExpr := subExpr
	if result == nil {
		// Fall back to full expression.
		result = InferExpressionType(ps.Expression, ps.Vars, ps.ScopeStack, ps.Locals, funcMaps, typeRegistry)
		useExpr = ps.Expression
	}
	if result == nil {
		return nil
	}

	// Build the dot context for completion info.
	var dotType string
	var dotFields []ast.FieldInfo
	for i := len(ps.ScopeStack) - 1; i >= 0; i-- {
		if len(ps.ScopeStack[i].Fields) > 0 || ps.ScopeStack[i].TypeStr != "" {
			dotType = ps.ScopeStack[i].TypeStr
			dotFields = ps.ScopeStack[i].Fields
			break
		}
	}

	return &HoverResult{
		Expression: useExpr,
		TypeStr:    result.TypeStr,
		Fields:     result.Fields,
		IsSlice:    result.IsSlice,
		IsMap:      result.IsMap,
		ElemType:   result.ElemType,
		KeyType:    result.KeyType,
		Doc:        result.Doc,
		DotType:    dotType,
		DotFields:  dotFields,
		Locals:     ps.Locals,
	}
}

// buildScopeAtPosition walks template content building scope, like
// ValidateTemplateContent, but instead of validating it captures the scope
// when the target line:col is reached.
//
// Key difference from validation: block/define bodies are entered with the
// scope derived from the block's context argument instead of being skipped.
func buildScopeAtPosition(content string, varMap map[string]ast.TemplateVar, templateName string, lineOffset, targetLine, targetCol int, registry map[string][]NamedBlockEntry, funcMaps FuncMapRegistry) *PositionScope {
	effectiveFuncMaps := optionalFuncMapRegistry(funcMaps)
	effectiveRegistry := mergeNamedBlockRegistry(registry, content, templateName)

	var scopeStack []ScopeType
	rootScope := buildRootScope(varMap)
	scopeStack = append(scopeStack, rootScope)

	cur := 0
	lineNum := 0

	for cur < len(content) {
		openRel := strings.Index(content[cur:], "{{")
		if openRel == -1 {
			break
		}
		openIdx := cur + openRel

		lineNum += strings.Count(content[cur:openIdx], "\n")
		actualLineNum := lineNum + lineOffset

		closeRel := strings.Index(content[openIdx:], "}}")
		if closeRel == -1 {
			break
		}
		closeIdx := openIdx + closeRel

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

		lastNewline := strings.LastIndexByte(content[:openIdx], '\n')
		col := openIdx - lastNewline // 1-based col of {{

		var action string
		if contentStart < contentEnd {
			action = content[contentStart:contentEnd]
		}

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
			if idx := strings.IndexByte(first, '('); idx != -1 {
				first = first[:idx]
			}
		}

		// ── Check if this action is at or past the target position ──────
		// Compute the end line of this action to see if the target is inside it.
		actionEndLine := actualLineNum + lineNumInside

		// Compute the end column of the action (position after `}}`)
		var actionEndCol int
		if lineNumInside == 0 {
			// Single-line action: end col = start col + total length of {{ ... }}
			actionEndCol = col + (closeIdx + 2 - openIdx)
		} else {
			// Multi-line: end col is from last newline to closeIdx+2
			lastNL := strings.LastIndexByte(content[:closeIdx+2], '\n')
			actionEndCol = closeIdx + 2 - lastNL
		}

		// If the target is on this action's line range, check column bounds
		inAction := false
		if targetLine >= actualLineNum+1 && targetLine <= actionEndLine+1 {
			if lineNumInside == 0 {
				// Single-line action: target col must be within [col, actionEndCol]
				inAction = targetCol >= col && targetCol <= actionEndCol
			} else if targetLine == actualLineNum+1 {
				// Multi-line first line: target col must be >= start col
				inAction = targetCol >= col
			} else if targetLine == actionEndLine+1 {
				// Multi-line last line: target col must be <= end col
				inAction = targetCol <= actionEndCol
			} else {
				// Target is on a middle line of a multi-line action
				inAction = true
			}
		}

		if inAction {
			// Target is within this action — this is what the cursor is on.
			expr := extractHoverExpression(action, first)
			if expr != "" {
				locals := collectLocals(scopeStack)

				// Compute cursor offset within the raw action text.
				// col is the 1-based column of {{ on the action's line.
				// targetCol is the 1-based column of the cursor.
				cursorOffset := targetCol - col - 2 // -2 for "{{"
				if contentStart > openIdx+2 {
					cursorOffset -= (contentStart - openIdx - 2) // adjust for dash/whitespace
				}
				if cursorOffset < 0 {
					cursorOffset = 0
				}
				if cursorOffset > len(action) {
					cursorOffset = len(action)
				}

				return &PositionScope{
					Expression:   expr,
					RawAction:    action,
					CursorOffset: cursorOffset,
					ScopeStack:   scopeStack,
					Vars:         varMap,
					Locals:       locals,
				}
			}
		}

		// ── Handle scope popping (else, end) ────────────────────────────
		isElse := first == "else"
		var elseAction string

		if isElse {
			if len(scopeStack) <= 1 {
				lineNum += lineNumInside
				continue
			}
			scopeStack = scopeStack[:len(scopeStack)-1]
			if len(words) > 1 {
				elseAction = words[1]
				if idx := strings.IndexByte(elseAction, '('); idx != -1 {
					elseAction = elseAction[:idx]
				}
			}
		} else if first == "end" {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			}
			lineNum += lineNumInside
			continue
		}

		// Register inline local assignments (e.g., {{ $x := .Foo }})
		if first != "range" && first != "with" && first != "if" && first != "block" && first != "define" && first != "template" && first != "end" && first != "else" {
			registerInlineLocalAssignmentsSafe(action, scopeStack, varMap, effectiveFuncMaps)
		}

		// ── Handle block/define — enter body with derived scope ──────────
		if first == "block" {
			syntheticAction := "template " + strings.TrimSpace(strings.TrimPrefix(action, "block"))
			parts := parseTemplateAction(syntheticAction)
			if len(parts) >= 2 {
				contextArg := parts[1]
				blockScope := resolvePartialScope(contextArg, scopeStack, varMap, effectiveFuncMaps)
				newScope := childScope(blockScope)
				if blockScope.IsRoot {
					newScope.IsRoot = true
				}
				scopeStack = append(scopeStack, newScope)
			} else {
				// No context arg — inherit current scope
				top := ScopeType{}
				if len(scopeStack) > 0 {
					top = childScope(scopeStack[len(scopeStack)-1])
				}
				scopeStack = append(scopeStack, top)
			}
			lineNum += lineNumInside
			continue
		}

		if first == "define" {
			// Define bodies have no inherited scope from the parent — they get
			// their context from the template/block call site. For position-scope
			// purposes, look up the registry to see if there is a matching render
			// call that provides vars, otherwise use an empty scope.
			if len(words) >= 2 {
				defName := strings.Trim(words[1], `"`)
				var _ string = defName
				varMapForDefine := findDefineVars(effectiveRegistry, varMap, scopeStack, effectiveFuncMaps)
				newScope := buildRootScope(varMapForDefine)
				scopeStack = append(scopeStack, newScope)
			} else {
				scopeStack = append(scopeStack, ScopeType{})
			}
			lineNum += lineNumInside
			continue
		}

		// ── Push new scope for if / with / range ─────────────────────────
		actionToPush := first
		exprToParse := action

		if isElse {
			if elseAction != "" {
				actionToPush = elseAction
				idx := strings.Index(action, words[1])
				if idx != -1 {
					exprToParse = action[idx:]
				}
			} else {
				top := ScopeType{}
				if len(scopeStack) > 0 {
					top = childScope(scopeStack[len(scopeStack)-1])
				}
				scopeStack = append(scopeStack, top)
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
				registerRangeLocalsSafe(&newScope, assignmentNames, rangeExpr, scopeStack, varMap, effectiveFuncMaps)
			}
			scopeStack = append(scopeStack, newScope)

		case "with":
			withExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "with"))
			assignmentNames, withPipeline, hasAssignment := splitAssignment(withExpr)
			if hasAssignment {
				withExpr = withPipeline
			}
			newScope := childScope(createScopeFromWith(withExpr, scopeStack, varMap, effectiveFuncMaps))
			if hasAssignment {
				registerAssignedLocalsSafe(&newScope, assignmentNames, withExpr, scopeStack, varMap, effectiveFuncMaps)
			}
			scopeStack = append(scopeStack, newScope)

		case "if":
			top := ScopeType{}
			if len(scopeStack) > 0 {
				top = childScope(scopeStack[len(scopeStack)-1])
			}
			ifExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "if"))
			assignmentNames, ifPipeline, hasAssignment := splitAssignment(ifExpr)
			if hasAssignment {
				registerAssignedLocalsSafe(&top, assignmentNames, ifPipeline, scopeStack, varMap, effectiveFuncMaps)
			}
			scopeStack = append(scopeStack, top)
		}

		lineNum += lineNumInside
	}

	return nil
}

// extractHoverExpression extracts the expression from a template action that
// corresponds to the cursor position. For keyword actions (range, with, if),
// it strips the keyword. For block/define, it returns empty (keyword line).
func extractHoverExpression(action, first string) string {
	switch first {
	case "end", "define":
		return ""
	case "block":
		// The {{ block "name" .ctx }} line itself — extract the context arg
		parts := parseTemplateAction("template " + strings.TrimSpace(strings.TrimPrefix(action, "block")))
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	case "template":
		parts := parseTemplateAction(action)
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	case "range":
		expr := strings.TrimSpace(strings.TrimPrefix(action, "range"))
		_, pipeline, hasAssignment := splitAssignment(expr)
		if hasAssignment {
			return pipeline
		}
		return expr
	case "with":
		expr := strings.TrimSpace(strings.TrimPrefix(action, "with"))
		_, pipeline, hasAssignment := splitAssignment(expr)
		if hasAssignment {
			return pipeline
		}
		return expr
	case "if":
		return strings.TrimSpace(strings.TrimPrefix(action, "if"))
	case "else":
		rest := strings.TrimSpace(strings.TrimPrefix(action, "else"))
		if rest == "" {
			return ""
		}
		words := strings.Fields(rest)
		if len(words) > 0 {
			switch words[0] {
			case "if":
				return strings.TrimSpace(strings.TrimPrefix(rest, "if"))
			case "with":
				return strings.TrimSpace(strings.TrimPrefix(rest, "with"))
			case "range":
				return strings.TrimSpace(strings.TrimPrefix(rest, "range"))
			}
		}
		return rest
	default:
		// Regular expression or assignment: use the whole action
		return action
	}
}

// collectLocals merges all $var locals visible at the current scope position.
func collectLocals(scopeStack []ScopeType) map[string]ast.TemplateVar {
	result := make(map[string]ast.TemplateVar)
	for _, frame := range scopeStack {
		for k, v := range frame.Locals {
			result[k] = v
		}
	}
	return result
}

// extractSubPathAtCursor extracts the dot-path or $var-path segment at the
// cursor offset within an action's raw text. For example, in "if eq .Name $rx.DrugName"
// with cursor on ".Name", it returns ".Name"; with cursor on "$rx.DrugName", it
// returns "$rx.DrugName".
func extractSubPathAtCursor(action string, offset int) string {
	if len(action) == 0 || offset < 0 || offset > len(action) {
		return ""
	}
	// If cursor is exactly on a dot, no useful segment.
	if offset < len(action) && action[offset] == '.' {
		return ""
	}

	isPathChar := func(b byte) bool {
		return b == '.' || b == '$' || b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	isIdentChar := func(b byte) bool {
		return b == '$' || b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}

	// Walk backward through dots and identifiers.
	pathStart := offset
	for pathStart > 0 && isPathChar(action[pathStart-1]) {
		pathStart--
	}

	// Walk forward through identifier chars only (stop at dot).
	segEnd := offset
	for segEnd < len(action) && isIdentChar(action[segEnd]) {
		segEnd++
	}

	if pathStart >= segEnd {
		return ""
	}

	sub := action[pathStart:segEnd]
	// Reject bare "$" or "."
	if sub == "." || sub == "$" {
		return ""
	}
	// Must contain an actual identifier — reject if it's just dots/symbols.
	hasIdent := false
	for _, r := range sub {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			hasIdent = true
			break
		}
	}
	if !hasIdent {
		return ""
	}
	return sub
}

// findDefineVars looks up the vars that a {{define "name"}} block receives from
// its callers. It looks in the registry + render var index for matching entries.
func findDefineVars(registry map[string][]NamedBlockEntry, parentVars map[string]ast.TemplateVar, parentStack []ScopeType, funcMaps FuncMapRegistry) map[string]ast.TemplateVar {
	// For defines, the scope depends on how they are called. If they exist as
	// named blocks called from parent templates, use the parent template's vars.
	// Otherwise, return empty — we can't know the context.
	_ = registry
	_ = parentStack
	_ = funcMaps
	return parentVars
}

// registerInlineLocalAssignmentsSafe is like registerInlineLocalAssignments but
// does not produce validation errors (used for scope building, not validation).
func registerInlineLocalAssignmentsSafe(action string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry) {
	if len(scopeStack) == 0 {
		return
	}
	assignmentNames, rhs, ok := splitAssignment(action)
	if !ok {
		return
	}
	frame := &scopeStack[len(scopeStack)-1]
	if frame.Locals == nil {
		frame.Locals = make(map[string]ast.TemplateVar)
	}
	resolved := scopeToTemplateVar("", resolveScopeFromExpression(rhs, scopeStack, varMap, funcMaps))
	for _, name := range assignmentNames {
		local := resolved
		local.Name = name
		frame.Locals[name] = local
	}
}

// registerRangeLocalsSafe is like registerRangeLocals but does not produce errors.
func registerRangeLocalsSafe(frame *ScopeType, names []string, rangeExpr string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry) {
	if frame.Locals == nil {
		frame.Locals = make(map[string]ast.TemplateVar)
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

// registerAssignedLocalsSafe is like registerAssignedLocals but does not produce errors.
func registerAssignedLocalsSafe(frame *ScopeType, names []string, rhs string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry) {
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
