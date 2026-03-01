package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"path/filepath"
)

// generateRenderCalls transforms collected scope information into structured
// RenderCall entries with full variable information. Each render call is
// associated with:
// - Source location (file, line, column range)
// - Template name(s)
// - Available template variables (local + scope + global)
func generateRenderCalls(
	scopes []FuncScope,
	globalImplicitVars []TemplateVar,
	info *types.Info,
	fset *token.FileSet,
	dir string,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seenPool *seenMapPool,
) []RenderCall {
	// Pre-count total render calls for efficient allocation
	totalRenders := 0
	for _, scope := range scopes {
		totalRenders += len(scope.RenderNodes)
	}

	renderCalls := make([]RenderCall, 0, totalRenders)

	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 {
			continue
		}

		for _, rr := range scope.RenderNodes {
			call := rr.Node
			templateArgIdx := rr.TemplateArgIdx

			// Skip invalid render calls
			if len(rr.TemplateNames) == 0 ||
				templateArgIdx < 0 ||
				templateArgIdx >= len(call.Args) {
				continue
			}

			templatePathExpr := call.Args[templateArgIdx]

			// Calculate precise column range for template name
			tplNameStartCol, tplNameEndCol := getExprColumnRange(fset, templatePathExpr)

			// Adjust for string literal quotes
			if lit, ok := templatePathExpr.(*goast.BasicLit); ok && lit.Kind == token.STRING {
				tplNameStartCol++ // Skip opening quote
				tplNameEndCol--   // Skip closing quote
			}

			// Process each template name (usually one, but can be multiple from variables)
			for _, templatePath := range rr.TemplateNames {
				if templatePath == "" {
					continue
				}

				// Extract variables from data argument if present
				dataArgIdx := templateArgIdx + 1
				var localVars []TemplateVar

				if dataArgIdx < len(call.Args) {
					dataArg := call.Args[dataArgIdx]
					seen := seenPool.get()
					localVars = extractMapVars(dataArg, info, fset, structIndex, fc, seen)

					// Fallback: data arg is an identifier — resolve it to a
					// composite literal tracked during the assignment pass.
					// This handles the common pattern:
					//
					//   ctx := rex.Map{"key": val}
					//   SetTriageContext(ctx, triage, visit)
					//   c.Render("tmpl.html", ctx)
					if len(localVars) == 0 {
						if ident, ok := dataArg.(*goast.Ident); ok {
							if comp, found := scope.MapAssignments[ident.Name]; found {
								clear(seen)
								localVars = extractMapVars(comp, info, fset, structIndex, fc, seen)
							}
						}
					}

					seenPool.put(seen)
				}

				// Combine all available variables: local + scope + global
				allVars := make([]TemplateVar, 0, len(localVars)+len(scope.SetVars)+len(globalImplicitVars))
				allVars = append(allVars, localVars...)
				allVars = append(allVars, scope.SetVars...)
				allVars = append(allVars, globalImplicitVars...)

				// Resolve file path relative to analysis root
				pos := fset.Position(call.Pos())
				relFile := resolveRelativePath(pos.Filename, dir)

				renderCalls = append(renderCalls, RenderCall{
					File:                 relFile,
					Line:                 pos.Line,
					Template:             templatePath,
					TemplateNameStartCol: tplNameStartCol,
					TemplateNameEndCol:   tplNameEndCol,
					Vars:                 allVars,
				})
			}
		}
	}

	return renderCalls
}

// resolveRelativePath attempts to convert an absolute path to a path
// relative to the specified directory. Falls back to the original path
// if conversion fails.
func resolveRelativePath(absPath, baseDir string) string {
	if abs, err := filepath.Abs(absPath); err == nil {
		if rel, err := filepath.Rel(baseDir, abs); err == nil {
			return rel
		}
	}
	return absPath
}

// getExprColumnRange calculates the precise column span of an AST expression.
// This is used for accurate editor highlighting and navigation features.
func getExprColumnRange(fset *token.FileSet, expr goast.Expr) (startCol, endCol int) {
	pos := fset.Position(expr.Pos())
	endPos := fset.Position(expr.End())
	return pos.Column, endPos.Column
}
