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

	// Phase 1: extract the template name (first token, possibly quoted)
	var tmplName string
	remaining := rest

	if len(rest) > 0 && (rest[0] == '"' || rest[0] == '`') {
		// Quoted template name — find the closing quote
		quoteChar := rest[0]
		endIdx := strings.IndexByte(rest[1:], quoteChar)
		if endIdx == -1 {
			return []string{rest} // malformed, return as-is
		}
		tmplName = rest[1 : endIdx+1]
		remaining = strings.TrimSpace(rest[endIdx+2:])
	} else {
		// Unquoted template name — take until whitespace
		idx := strings.IndexAny(rest, " \t\n\r")
		if idx == -1 {
			return []string{rest}
		}
		tmplName = rest[:idx]
		remaining = strings.TrimSpace(rest[idx+1:])
	}

	if remaining == "" {
		return []string{tmplName}
	}

	// Phase 2: everything after the template name is the context expression
	return []string{tmplName, remaining}
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
	if v == "." || v == "$" || strings.HasPrefix(v, "..") {
		return
	}

	if strings.HasPrefix(v, ".") || strings.HasPrefix(v, "$.") {
		onVar(v)
		return
	}

	if strings.HasPrefix(v, "$") && len(v) > 1 && v[1] != '.' && v[1] != '$' {
		onVar(v)
	}
}
