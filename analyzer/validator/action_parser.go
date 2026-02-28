package validator

import (
	"strings"
)

// parseTemplateAction parses a {{template}} action to extract its arguments.
//
// Template action syntax:
//   - {{template "name"}}
//   - {{template "name" .}}
//   - {{template "name" .Context}}
//
// Returns slice of parsed arguments:
//   - [0]: template name (without quotes)
//   - [1]: context argument (if present)
//
// Handles both quoted strings ("name", `name`) and unquoted identifiers.
//
// Thread-safety: No shared state, safe for concurrent calls.
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
			// Start of string literal
			inString = true
			stringChar = r
			if current.Len() > 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
			}

		case inString && r == stringChar:
			// End of string literal
			inString = false
			parts = append(parts, current.String())
			current.Reset()

		case !inString && (r == ' ' || r == '\n' || r == '\r' || r == '\t'):
			// Whitespace separator (outside string)
			if current.Len() > 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
			}

		default:
			// Regular character
			current.WriteRune(r)
		}
	}

	// Add any remaining content
	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}

	return parts
}

// extractVariablesFromAction extracts all variable references from a template
// action string.
//
// Variable references are identified by:
//   - Starting with . (current scope) or $. (root scope)
//   - Not being . or $ alone (special variables)
//   - Not starting with .. (invalid syntax)
//
// The function parses the action, skipping:
//   - String literals (quoted content)
//   - Operators and delimiters
//   - Keywords
//
// Calls onVar callback for each valid variable found.
//
// Thread-safety: No shared state, safe for concurrent calls.
func extractVariablesFromAction(action string, onVar func(string)) {
	start := -1
	inString := false
	stringChar := rune(0)

	for i, r := range action {
		if inString {
			// Inside string literal: skip until closing quote
			if r == stringChar {
				inString = false
			}
			continue
		}

		switch r {
		case '"', '`':
			// Start of string literal
			if start != -1 {
				emitVar(action[start:i], onVar)
				start = -1
			}
			inString = true
			stringChar = r

		case ' ', '\n', '\r', '\t', '(', ')', '|', '=', ',', '+', '-', '*', '/', '!', '<', '>', '%', '&':
			// Delimiter: emit pending variable
			if start != -1 {
				emitVar(action[start:i], onVar)
				start = -1
			}

		default:
			// Regular character: mark start of potential variable
			if start == -1 {
				start = i
			}
		}
	}

	// Emit any remaining variable
	if start != -1 {
		emitVar(action[start:], onVar)
	}
}

// emitVar checks if a token is a valid variable reference and calls the callback.
//
// Valid variable references:
//   - Start with . or $.
//   - Not exactly . or $ (these are special variables)
//   - Not starting with .. (invalid)
func emitVar(v string, onVar func(string)) {
	v = strings.TrimSpace(v)
	if (strings.HasPrefix(v, ".") || strings.HasPrefix(v, "$.")) &&
		v != "." && v != "$" && !strings.HasPrefix(v, "..") {
		onVar(v)
	}
}
