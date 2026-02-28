package ast

import (
	goast "go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
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

// FindGoFiles recursively finds all .go files in a directory tree.
func FindGoFiles(root string) ([]string, error) {
	var files []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, don't fail entire walk
		}

		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}

		return nil
	})
	return files, err
}
