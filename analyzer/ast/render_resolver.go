package ast

import (
	goast "go/ast"
	"go/constant"
	"go/token"
	"go/types"
)

// resolveRenderCall analyzes a render call expression to extract:
// - Template name(s) being rendered
// - Index of the template name argument
//
// Template names can come from:
// 1. String literals: c.Render("template.html", data)
// 2. Constants: c.Render(TemplateName, data)
// 3. Variables: c.Render(tplName, data)
func resolveRenderCall(
	call *goast.CallExpr,
	info *types.Info,
	stringAssignments map[string][]string,
) *ResolvedRender {
	resolved := &ResolvedRender{
		Node:           call,
		TemplateArgIdx: -1,
	}

	// Determine expected position of template argument
	templateArgIdx := inferTemplateArgIdx(call)

	// Find actual template argument position
	templateArgIdx = findTemplateArg(call, templateArgIdx, stringAssignments)

	if templateArgIdx < 0 || templateArgIdx >= len(call.Args) {
		return nil
	}

	resolved.TemplateArgIdx = templateArgIdx
	arg := call.Args[templateArgIdx]

	// Resolve template name(s)
	resolved.TemplateNames = resolveTemplateName(arg, info, stringAssignments)

	if len(resolved.TemplateNames) == 0 {
		return nil
	}

	return resolved
}

// inferTemplateArgIdx determines the likely index of the template argument
// based on the function call syntax.
func inferTemplateArgIdx(call *goast.CallExpr) int {
	switch call.Fun.(type) {
	case *goast.SelectorExpr:
		// Method call: obj.Render(template, ...)
		return 0
	case *goast.Ident:
		// Function call: Render(obj, template, ...)
		return -1
	default:
		return -1
	}
}

// findTemplateArg locates the template name argument in the call.
// If initial index is -1, searches for first string-like argument.
func findTemplateArg(
	call *goast.CallExpr,
	initialIdx int,
	stringAssignments map[string][]string,
) int {
	if initialIdx >= 0 {
		return initialIdx
	}

	// Search for first string argument or known string variable
	for i, arg := range call.Args {
		// String literal
		if lit, ok := arg.(*goast.BasicLit); ok && lit.Kind == token.STRING {
			return i
		}

		// Variable with known string value
		if ident, ok := arg.(*goast.Ident); ok {
			if _, ok := stringAssignments[ident.Name]; ok {
				return i
			}
		}
	}

	return -1
}

// resolveTemplateName extracts template name(s) from an argument expression.
// Handles string literals, constants, and variables.
func resolveTemplateName(
	arg goast.Expr,
	info *types.Info,
	stringAssignments map[string][]string,
) []string {
	// Try direct string extraction
	if s := extractStringFast(arg); s != "" {
		return []string{s}
	}

	// Try identifier resolution
	ident, ok := arg.(*goast.Ident)
	if !ok {
		return nil
	}

	// Try constant resolution
	if info != nil {
		if obj := info.ObjectOf(ident); obj != nil {
			if c, ok := obj.(*types.Const); ok {
				val := c.Val()
				if val.Kind() == constant.String {
					return []string{constant.StringVal(val)}
				}
			}
		}
	}

	// Try variable resolution
	if vals, ok := stringAssignments[ident.Name]; ok {
		return vals
	}

	return nil
}

// isRenderCall checks if a call expression is a template render call
// based on configured function names.
func isRenderCall(call *goast.CallExpr, config AnalysisConfig) bool {
	funcName := ""

	switch fn := call.Fun.(type) {
	case *goast.SelectorExpr:
		funcName = fn.Sel.Name
	case *goast.Ident:
		funcName = fn.Name
	}

	return (funcName == config.RenderFunctionName || funcName == config.ExecuteTemplateFunctionName) &&
		len(call.Args) >= 2
}
