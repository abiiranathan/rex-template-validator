package validator

import (
	"maps"
	"strings"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
)

// resolvePartialScope determines what scope/type the context argument refers to
// for a nested template call.
// resolvePartialScope determines what scope/type the context argument refers to
// for a nested template call.
func resolvePartialScope(
	contextArg string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
) ScopeType {
	if contextArg == "" || contextArg == "." || contextArg == "$" {
		return resolveScopeFromExpression(contextArg, scopeStack, varMap, funcMaps)
	}

	inferred := InferExpressionType(contextArg, varMap, scopeStack, nil, funcMaps, nil)
	if inferred != nil {
		scope := ScopeType{
			TypeStr:  inferred.TypeStr,
			Fields:   inferred.Fields,
			IsSlice:  inferred.IsSlice,
			IsMap:    inferred.IsMap,
			KeyType:  inferred.KeyType,
			ElemType: inferred.ElemType,
		}
		// Ensure KeyType is populated for map[string]any results (e.g. dict).
		// InferExpressionType may leave KeyType empty when TypeStr encodes it.
		if scope.IsMap && scope.KeyType == "" {
			scope.KeyType = unwrapMapKeyType(inferred.TypeStr)
		}
		return scope
	}
	return resolveScopeFromExpression(contextArg, scopeStack, varMap, funcMaps)
}

// buildPartialVarMap constructs the variable map available to a nested template
// based on the context argument.
// buildPartialVarMap constructs the variable map available to a nested template
// based on the context argument.
func buildPartialVarMap(
	contextArg string,
	partialScope ScopeType,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
) map[string]ast.TemplateVar {
	result := make(map[string]ast.TemplateVar)

	if contextArg == "$" {
		maps.Copy(result, varMap)
		return result
	}

	if contextArg == "." {
		if len(scopeStack) > 0 {
			currentScope := scopeStack[len(scopeStack)-1]
			if currentScope.IsRoot {
				maps.Copy(result, varMap)
			} else {
				result["."] = ast.TemplateVar{
					Name:     ".",
					TypeStr:  currentScope.TypeStr,
					Fields:   currentScope.Fields,
					IsSlice:  currentScope.IsSlice,
					IsMap:    currentScope.IsMap,
					KeyType:  currentScope.KeyType,
					ElemType: currentScope.ElemType,
				}
			}
		}
		return result
	}

	// For string-keyed maps with known fields (e.g. dict "key" val ...),
	// promote each field as a top-level variable WITHOUT setting ".".
	// Setting "." causes buildRootScope to return early using only the dot
	// entry's fields, making the promoted keys invisible to the validator.
	if partialScope.IsMap && len(partialScope.Fields) > 0 && partialScope.KeyType == "string" {
		for _, f := range partialScope.Fields {
			result[f.Name] = ast.TemplateVar{
				Name:     f.Name,
				TypeStr:  f.TypeStr,
				Fields:   f.Fields,
				IsSlice:  f.IsSlice,
				IsMap:    f.IsMap,
				KeyType:  f.KeyType,
				ElemType: f.ElemType,
			}
		}
		return result
	}

	// Specific variable: pass as "."
	result["."] = ast.TemplateVar{
		Name:     ".",
		TypeStr:  partialScope.TypeStr,
		Fields:   partialScope.Fields,
		IsSlice:  partialScope.IsSlice,
		IsMap:    partialScope.IsMap,
		KeyType:  partialScope.KeyType,
		ElemType: partialScope.ElemType,
	}

	return result
}

// scopeVarsToTemplateVars converts a variable map back to a TemplateVar slice.
// This is used when recursively validating file-based partials.
func scopeVarsToTemplateVars(varMap map[string]ast.TemplateVar) []ast.TemplateVar {
	vars := make([]ast.TemplateVar, 0, len(varMap))
	for _, v := range varMap {
		vars = append(vars, v)
	}
	return vars
}

// createScopeFromRange creates a new scope for a {{range}} block.
//
// Range syntax:
//   - {{range .Collection}} : Iterate over collection, dot becomes element
//   - {{range $val := .Collection}} : Iterate with named value
//   - {{range $key, $val := .Collection}} : Iterate with named key and value
//
// The new scope represents the type of elements being iterated over.
//
// Returns: ScopeType for the range block body
func createScopeFromRange(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
) ScopeType {
	expr = strings.TrimSpace(expr)
	collectionScope := resolveScopeFromExpression(expr, scopeStack, varMap, funcMaps)

	// If we are iterating over a map or slice, the scope inside the range
	// corresponds to the element type, not the collection type.
	// We need to unwrap the IsMap/IsSlice properties based on the element type.
	if collectionScope.IsMap || collectionScope.IsSlice {
		baseType := collectionScope.ElemType
		// Unwrap pointer types
		for strings.HasPrefix(baseType, "*") {
			baseType = baseType[1:]
		}

		newIsMap := false
		newIsSlice := false
		newElemType := ""
		// KeyType logic omitted for now as it's not critical for IsMap determination

		if strings.HasPrefix(baseType, "map[") {
			// Logic to parse map[Key]Value
			depth := 0
			splitIdx := -1

			// Start after "map[" (index 3)
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
				valType := baseType[splitIdx+1:]
				newIsMap = true
				newElemType = strings.TrimSpace(valType)
			}
		} else if strings.HasPrefix(baseType, "[]") {
			newIsSlice = true
			newElemType = baseType[2:]
		}

		// Return updated scope representing the element
		return ScopeType{
			IsRoot:   false,
			VarName:  expr, // or original varExpr
			TypeStr:  collectionScope.ElemType,
			Fields:   collectionScope.Fields,
			IsSlice:  newIsSlice,
			IsMap:    newIsMap,
			KeyType:  "",          // Not currently parsed
			ElemType: newElemType, // Derived from ElemType string
		}
	}

	return collectionScope
}

// createScopeFromWith creates a new scope for a {{with}} block.
//
// With syntax: {{with .Variable}}
// Changes the dot context to the specified variable.
//
// Returns: ScopeType for the with block body
func createScopeFromWith(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
) ScopeType {
	return resolveScopeFromExpression(expr, scopeStack, varMap, funcMaps)
}

func resolveScopeFromExpression(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
) ScopeType {
	expr = unwrapExpression(expr)
	if expr == "" {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}

	if scope, ok := createScopeFromFunctionExpression(expr, funcMaps); ok {
		return scope
	}

	if strings.HasPrefix(expr, "index ") {
		return createScopeFromIndexExpression(expr, scopeStack, varMap, funcMaps)
	}

	if expr == "$" {
		return rootScopeFromStack(scopeStack, varMap)
	}

	if strings.HasPrefix(expr, "$.") {
		return createScopeFromRootExpression(expr, scopeStack, varMap)
	}

	if strings.HasPrefix(expr, "$") {
		return createScopeFromLocalExpression(expr, scopeStack)
	}

	if strings.HasPrefix(expr, ".") {
		return createScopeFromExpression(expr, scopeStack, varMap)
	}

	return ScopeType{Fields: []ast.FieldInfo{}}
}

func childScope(scope ScopeType) ScopeType {
	return ScopeType{
		IsRoot:   scope.IsRoot,
		VarName:  scope.VarName,
		TypeStr:  scope.TypeStr,
		ElemType: scope.ElemType,
		KeyType:  scope.KeyType,
		Fields:   scope.Fields,
		IsSlice:  scope.IsSlice,
		IsMap:    scope.IsMap,
	}
}

func rootScopeFromStack(scopeStack []ScopeType, varMap map[string]ast.TemplateVar) ScopeType {
	if len(scopeStack) > 0 {
		root := childScope(scopeStack[0])
		root.IsRoot = true
		return root
	}
	return buildRootScope(varMap)
}

func createScopeFromRootExpression(expr string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar) ScopeType {
	parts := strings.Split(strings.TrimPrefix(expr, "$."), ".")
	if len(parts) == 0 || parts[0] == "" {
		return rootScopeFromStack(scopeStack, varMap)
	}

	rootVar, ok := lookupRootVar(parts[0], scopeStack, varMap)
	if !ok {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}

	return walkScopePath(scopeFromTemplateVar(rootVar), parts[1:])
}

func createScopeFromLocalExpression(expr string, scopeStack []ScopeType) ScopeType {
	localVar, remainder, ok := lookupLocalVar(expr, scopeStack)
	if !ok {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}
	return walkScopePath(scopeFromTemplateVar(localVar), remainder)
}

func createScopeFromIndexExpression(expr string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar, funcMaps FuncMapRegistry) ScopeType {
	parts := strings.Fields(expr)
	if len(parts) < 2 {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}

	baseScope := resolveScopeFromExpression(parts[1], scopeStack, varMap, funcMaps)
	return elementScopeFromCollection(baseScope)
}

func createScopeFromFunctionExpression(expr string, funcMaps FuncMapRegistry) (ScopeType, bool) {
	if len(funcMaps) == 0 {
		return ScopeType{}, false
	}

	tokens := strings.Fields(unwrapExpression(expr))
	if len(tokens) == 0 {
		return ScopeType{}, false
	}

	funcName := strings.Trim(tokens[0], "()")
	if funcName == "call" {
		if len(tokens) < 2 {
			return ScopeType{}, false
		}
		funcName = strings.Trim(tokens[1], "()")
	}

	if !isFunctionIdentifier(funcName) {
		return ScopeType{}, false
	}

	funcMap, ok := funcMaps[funcName]
	if !ok || len(funcMap.Returns) == 0 {
		return ScopeType{}, false
	}

	primaryReturn := funcMap.Returns[0]
	returnFields := primaryReturn.Fields
	if len(returnFields) == 0 && len(funcMap.ReturnTypeFields) > 0 {
		returnFields = funcMap.ReturnTypeFields
	}

	return ScopeType{
		TypeStr: primaryReturn.TypeStr,
		Fields:  returnFields,
	}, true
}

func elementScopeFromCollection(scope ScopeType) ScopeType {
	if !scope.IsMap && !scope.IsSlice {
		return scope
	}

	baseType := scope.ElemType
	for strings.HasPrefix(baseType, "*") {
		baseType = baseType[1:]
	}

	newScope := childScope(scope)
	newScope.IsRoot = false
	newScope.VarName = scope.VarName
	newScope.TypeStr = scope.ElemType
	newScope.KeyType = ""
	newScope.IsMap = false
	newScope.IsSlice = false
	newScope.ElemType = ""

	if strings.HasPrefix(baseType, "map[") {
		depth := 0
		splitIdx := -1
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
			newScope.IsMap = true
			newScope.ElemType = strings.TrimSpace(baseType[splitIdx+1:])
		}
	} else if strings.HasPrefix(baseType, "[]") {
		newScope.IsSlice = true
		newScope.ElemType = baseType[2:]
	}

	return newScope
}

func walkScopePath(scope ScopeType, parts []string) ScopeType {
	current := childScope(scope)
	for _, part := range parts {
		if part == "" {
			continue
		}

		if current.IsMap {
			current = elementScopeFromCollection(current)
			continue
		}

		found := false
		for _, f := range current.Fields {
			if f.Name == part {
				current = ScopeType{
					VarName:  current.VarName,
					TypeStr:  f.TypeStr,
					Fields:   f.Fields,
					IsSlice:  f.IsSlice,
					IsMap:    f.IsMap,
					KeyType:  f.KeyType,
					ElemType: f.ElemType,
				}
				found = true
				break
			}
		}
		if !found {
			return ScopeType{Fields: []ast.FieldInfo{}}
		}
	}
	return current
}

func scopeFromTemplateVar(v ast.TemplateVar) ScopeType {
	return ScopeType{
		VarName:  v.Name,
		TypeStr:  v.TypeStr,
		Fields:   v.Fields,
		IsSlice:  v.IsSlice,
		IsMap:    v.IsMap,
		KeyType:  v.KeyType,
		ElemType: v.ElemType,
	}
}

func scopeToTemplateVar(name string, scope ScopeType) ast.TemplateVar {
	return ast.TemplateVar{
		Name:     name,
		TypeStr:  scope.TypeStr,
		Fields:   scope.Fields,
		IsSlice:  scope.IsSlice,
		IsMap:    scope.IsMap,
		KeyType:  scope.KeyType,
		ElemType: scope.ElemType,
	}
}

func lookupRootVar(name string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar) (ast.TemplateVar, bool) {
	if v, ok := varMap[name]; ok {
		return v, true
	}

	if len(scopeStack) == 0 {
		return ast.TemplateVar{}, false
	}

	for _, f := range scopeStack[0].Fields {
		if f.Name == name {
			return ast.TemplateVar{
				Name:     f.Name,
				TypeStr:  f.TypeStr,
				Fields:   f.Fields,
				IsSlice:  f.IsSlice,
				IsMap:    f.IsMap,
				KeyType:  f.KeyType,
				ElemType: f.ElemType,
			}, true
		}
	}

	return ast.TemplateVar{}, false
}

func lookupLocalVar(expr string, scopeStack []ScopeType) (ast.TemplateVar, []string, bool) {
	parts := strings.Split(expr, ".")
	if len(parts) == 0 || parts[0] == "" {
		return ast.TemplateVar{}, nil, false
	}

	name := parts[0]
	for i := len(scopeStack) - 1; i >= 0; i-- {
		locals := scopeStack[i].Locals
		if locals == nil {
			continue
		}
		if v, ok := locals[name]; ok {
			return v, parts[1:], true
		}
	}

	return ast.TemplateVar{}, nil, false
}

func unwrapExpression(expr string) string {
	trimmed := strings.TrimSpace(expr)
	for strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if inner == trimmed {
			break
		}
		trimmed = inner
	}
	return trimmed
}

// createScopeFromExpression creates a scope by resolving a variable expression.
// Supports arbitrary nesting depth (e.g., .User.Profile.Address.City).
//
// Expression types:
//   - "." : Current scope
//   - ".VarName" : Top-level variable
//   - ".Var.Field" : Nested field access
//   - ".Var.Field.SubField" : Deep nested access
//
// Algorithm:
//  1. Split expression into path segments
//  2. Resolve first segment in current scope or varMap
//  3. Traverse remaining segments through field hierarchy
//  4. Return scope representing the final type
//
// Returns: ScopeType representing the resolved expression's type
func createScopeFromExpression(
	expr string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
) ScopeType {
	expr = strings.TrimSpace(expr)

	// Handle dot (current scope)
	if expr == "." {
		if len(scopeStack) > 0 {
			return scopeStack[len(scopeStack)-1]
		}
		return ScopeType{IsRoot: true}
	}

	// Must start with dot for variable access
	if !strings.HasPrefix(expr, ".") {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}

	// Split into path segments
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}

	// Resolve first segment (parts[0] is empty due to leading dot)
	var currentField *ast.FieldInfo
	firstPart := parts[1]

	// Look in current scope first
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

	// Fall back to varMap if not in current scope
	if currentField == nil {
		if v, ok := varMap[firstPart]; ok {
			currentField = &ast.FieldInfo{
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

	// Variable not found
	if currentField == nil {
		return ScopeType{Fields: []ast.FieldInfo{}}
	}

	// Traverse remaining path segments
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
			// Path segment not found
			return ScopeType{Fields: []ast.FieldInfo{}}
		}
	}

	// Return scope representing the resolved type
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
