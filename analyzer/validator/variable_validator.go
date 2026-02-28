package validator

import (
	"fmt"
	"strings"

	"github.com/rex-template-analyzer/ast"
)

// validateVariableInScope validates a variable access expression in the
// current scope context.
//
// This function handles:
//   - Root variable access: $.VarName
//   - Current scope access: .VarName
//   - Nested field access: .Var.Field.SubField
//   - Map access: .MapVar.key
//   - Unlimited nesting depth
//
// Validation logic:
//  1. Parse expression into path segments
//  2. Determine if root ($) or scoped (.) access
//  3. Validate first segment exists in appropriate scope
//  4. Validate remaining segments exist in field hierarchy
//
// Parameters:
//   - varExpr: Variable expression to validate (e.g., ".User.Name")
//   - scopeStack: Current scope stack
//   - varMap: Root variable map
//   - line, col: Source location for error reporting
//   - templateName: Template name for error reporting
//
// Returns: ValidationResult pointer if error found, nil if valid
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateVariableInScope(
	varExpr string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	line, col int,
	templateName string,
) *ValidationResult {
	varExpr = strings.TrimSpace(varExpr)

	// Skip special variables
	if varExpr == "." || varExpr == "$" {
		return nil
	}

	// Normalize expression (remove trailing dots)
	varExpr = strings.TrimRight(varExpr, ".")

	// Split into path segments
	parts := strings.Split(varExpr, ".")
	if len(parts) < 2 {
		return nil
	}

	isRootAccess := parts[0] == "$"

	// ── Scoped access in nested block ──────────────────────────────────────
	// When inside a with/range block, check current scope first
	if !isRootAccess && len(scopeStack) > 1 {
		currentScope := scopeStack[len(scopeStack)-1]
		fieldName := parts[1]

		// Handle map access
		if currentScope.IsMap {
			// Map key access is always valid
			// Validate nested access if present
			if len(parts) > 2 {
				return validateNestedFields(
					parts[2:],
					nil,
					currentScope.ElemType,
					false,
					"",
					varExpr,
					line,
					col,
					templateName,
				)
			}
			return nil
		}

		// Look for field in current scope
		var foundField *ast.FieldInfo
		for _, f := range currentScope.Fields {
			if f.Name == fieldName {
				fCopy := f
				foundField = &fCopy
				break
			}
		}

		// Found in current scope
		if foundField != nil {
			// Validate nested access if present
			if len(parts) > 2 {
				return validateNestedFields(
					parts[2:],
					foundField.Fields,
					foundField.TypeStr,
					foundField.IsMap,
					foundField.ElemType,
					varExpr,
					line,
					col,
					templateName,
				)
			}
			return nil
		}
	}

	// ── Root variable access ───────────────────────────────────────────────
	// Access to top-level variables (either $ or . at root)

	if len(parts) == 2 {
		// Simple access: .VarName or $.VarName
		rootVar := parts[1]

		// Check root scope
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				return nil
			}
		}

		// Check varMap
		if _, ok := varMap[rootVar]; ok {
			return nil
		}

		// Variable not found
		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	// ── Nested access: .Var.Field.SubField ─────────────────────────────────
	rootVar := parts[1]

	// Look up root variable
	var rootVarInfo *ast.TemplateVar
	if v, ok := varMap[rootVar]; ok {
		rootVarInfo = &v
	} else {
		// Try root scope fields
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				// Handle map with single key access
				if f.IsMap && len(parts) == 3 {
					return nil
				}
				// Validate nested fields
				return validateNestedFields(
					parts[2:],
					f.Fields,
					f.TypeStr,
					f.IsMap,
					f.ElemType,
					varExpr,
					line,
					col,
					templateName,
				)
			}
		}

		// Root variable not found
		return &ValidationResult{
			Template: templateName,
			Line:     line,
			Column:   col,
			Variable: varExpr,
			Message:  fmt.Sprintf(`Template variable %q is not defined in the render context`, varExpr),
			Severity: "error",
		}
	}

	// Handle map with single key access
	if rootVarInfo.IsMap && len(parts) == 3 {
		return nil
	}

	// Validate nested fields
	return validateNestedFields(
		parts[2:],
		rootVarInfo.Fields,
		rootVarInfo.TypeStr,
		rootVarInfo.IsMap,
		rootVarInfo.ElemType,
		varExpr,
		line,
		col,
		templateName,
	)
}

// validateNestedFields validates a field access path through a type hierarchy.
// Supports unlimited nesting depth and handles maps, slices, and structs.
//
// This function recursively traverses the field path, validating each segment
// exists on the parent type.
//
// Special handling:
//   - Map types: Any key is valid, validates the value type for further nesting
//   - Slice types: Element type is used for validation
//   - Struct types: Field must exist in Fields slice
//
// Parameters:
//   - fieldParts: Remaining field path segments to validate
//   - fields: Available fields at current level
//   - parentTypeName: Type name for error messages
//   - isMap: Whether current type is a map
//   - elemType: Element/value type for maps/slices
//   - fullExpr: Complete original expression for error messages
//   - line, col: Source location for error reporting
//   - templateName: Template name for error reporting
//
// Returns: ValidationResult pointer if error found, nil if valid
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateNestedFields(
	fieldParts []string,
	fields []ast.FieldInfo,
	parentTypeName string,
	isMap bool,
	elemType string,
	fullExpr string,
	line, col int,
	templateName string,
) *ValidationResult {
	currentFields := fields
	parentType := parentTypeName
	currentIsMap := isMap
	currentElemType := elemType

	// Traverse each field in the path
	for _, fieldName := range fieldParts {
		if currentIsMap {
			// ── Map key access ─────────────────────────────────────────────
			// Any key is valid for map access
			// Parse element type to determine if further nesting is valid

			baseType := currentElemType
			// Unwrap pointer types
			for strings.HasPrefix(baseType, "*") {
				baseType = baseType[1:]
			}

			newIsMap := false
			newElemType := ""

			if strings.HasPrefix(baseType, "map[") {
				// Nested map: parse map[Key]Value
				// Use bracket counting to handle complex key types like map[string]
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
				// Slice: element type is after []
				newElemType = baseType[2:]
			}

			currentIsMap = newIsMap
			if newElemType != "" {
				currentElemType = newElemType
			} else {
				// Basic type or struct: use element type as parent type
				parentType = currentElemType
			}

			continue
		}

		// ── Struct field access ────────────────────────────────────────────
		// Field must exist in Fields slice

		found := false
		var nextFields []ast.FieldInfo
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
			// Field doesn't exist on this type
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

		// Move to next level in hierarchy
		currentFields = nextFields
		currentIsMap = nextIsMap
		currentElemType = nextElemType
	}

	return nil
}

// validateContextArg checks whether a template call context expression
// resolves in the current scope.
//
// Used to validate that context arguments in {{template "name" .Context}}
// actually exist before recursively validating the nested template.
//
// Returns true if valid, false if undefined.
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateContextArg(
	contextArg string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
) bool {
	// Special cases always valid
	if contextArg == "" || contextArg == "." || contextArg == "$" {
		return true
	}

	// Validate using standard validation logic
	result := validateVariableInScope(contextArg, scopeStack, varMap, 0, 0, "")
	return result == nil
}
