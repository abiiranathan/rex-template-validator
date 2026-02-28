package validator

import (
	"maps"
	"strings"

	"github.com/rex-template-analyzer/ast"
)

// resolvePartialScope determines what scope/type the context argument refers to
// for a nested template call.
//
// Context argument types:
//   - "." : Current scope (dot context)
//   - ".VarName" : Specific variable from current scope
//   - "$var" : Local pipeline variable (returns empty scope)
//
// Returns: ScopeType representing the context that will be passed to the nested template
func resolvePartialScope(
	contextArg string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
) ScopeType {
	if contextArg == "." {
		// Pass current scope
		if len(scopeStack) > 0 {
			return scopeStack[len(scopeStack)-1]
		}
		return ScopeType{IsRoot: true}
	}

	if strings.HasPrefix(contextArg, ".") {
		// Specific variable access
		return createScopeFromExpression(contextArg, scopeStack, varMap)
	}

	// Untracked local variable or other expression
	return ScopeType{Fields: []ast.FieldInfo{}}
}

// buildPartialVarMap constructs the variable map available to a nested template
// based on the context argument.
//
// The resulting map represents what will be available as the dot (.) context
// in the nested template.
//
// Context semantics:
//   - "." : All variables from current scope
//   - ".VarName" : Fields of VarName become top-level variables
//
// Returns: Map of variables available in the nested template
func buildPartialVarMap(
	contextArg string,
	partialScope ScopeType,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
) map[string]ast.TemplateVar {
	result := make(map[string]ast.TemplateVar)

	if contextArg == "." {
		// Pass entire current scope
		if len(scopeStack) > 0 {
			currentScope := scopeStack[len(scopeStack)-1]
			if currentScope.IsRoot {
				// Root scope: copy all variables
				maps.Copy(result, varMap)
			} else {
				// Non-root scope: convert fields to variables
				for _, f := range currentScope.Fields {
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
			}
		}
		return result
	}

	// Specific variable: its fields become top-level in nested template
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
) ScopeType {
	expr = strings.TrimSpace(expr)

	var collectionScope ScopeType

	// Handle assignment syntax: $var := expr
	if strings.Contains(expr, ":=") {
		parts := strings.SplitN(expr, ":=", 2)
		if len(parts) == 2 {
			varExpr := strings.TrimSpace(parts[1])
			collectionScope = createScopeFromExpression(varExpr, scopeStack, varMap)
		} else {
			// Fallback for malformed assignment
			return ScopeType{Fields: []ast.FieldInfo{}}
		}
	} else {
		// Simple range: {{range .Collection}}
		collectionScope = createScopeFromExpression(expr, scopeStack, varMap)
	}

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
) ScopeType {
	return createScopeFromExpression(expr, scopeStack, varMap)
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
