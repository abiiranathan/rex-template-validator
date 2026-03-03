package ast

import (
	goast "go/ast"
	"go/token"
)

// extractStringFast efficiently extracts string value from a BasicLit.
// Optimized to avoid allocations by direct slicing.
func extractStringFast(expr goast.Expr) string {
	lit, ok := expr.(*goast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}

	// Valid string literal must have at least 2 chars (quotes)
	if len(lit.Value) < 2 {
		return ""
	}

	// Slice to remove surrounding quotes
	return lit.Value[1 : len(lit.Value)-1]
}
