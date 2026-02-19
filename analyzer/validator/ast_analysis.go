package validator

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
)

// AnalyzeDir analyzes a Go source directory for c.Render calls
func AnalyzeDir(dir string) AnalysisResult {
	result := AnalysisResult{}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.AllErrors)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("parse error: %v", err))
		return result
	}

	// Collect all files
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}

	// Type-check
	cfg := &types.Config{
		Importer: importer.ForCompiler(fset, "gc", nil),
		Error: func(err error) {
			result.Errors = append(result.Errors, fmt.Sprintf("type error: %v", err))
		},
	}

	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}

	var allFiles []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			allFiles = append(allFiles, f)
		}
	}

	_, typeErr := cfg.Check(dir, fset, allFiles, info)
	if typeErr != nil {
		// Non-fatal, we still try to extract info
		result.Errors = append(result.Errors, fmt.Sprintf("type check warning: %v", typeErr))
	}

	// Walk AST looking for c.Render(...)  calls
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			if sel.Sel.Name != "Render" {
				return true
			}

			if len(call.Args) < 2 {
				return true
			}

			// First arg should be template path string
			templatePath := extractString(call.Args[0])
			if templatePath == "" {
				return true
			}

			// Second arg should be rex.Map{...}
			vars := extractMapVars(call.Args[1], info, fset)

			pos := fset.Position(call.Pos())
			rc := RenderCall{
				File:     pos.Filename,
				Line:     pos.Line,
				Template: templatePath,
				Vars:     vars,
			}
			result.RenderCalls = append(result.RenderCalls, rc)
			return true
		})
	}

	return result
}

func extractString(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok {
		return ""
	}
	if lit.Kind != token.STRING {
		return ""
	}
	s := lit.Value
	if len(s) >= 2 {
		return s[1 : len(s)-1]
	}
	return ""
}

func extractMapVars(expr ast.Expr, info *types.Info, fset *token.FileSet) []TemplateVar {
	var vars []TemplateVar

	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return vars
	}

	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		keyLit, ok := kv.Key.(*ast.BasicLit)
		if !ok {
			continue
		}

		name := strings.Trim(keyLit.Value, `"`)

		tv := TemplateVar{Name: name}

		// Try to get type from type checker
		if typeInfo, ok := info.Types[kv.Value]; ok {
			tv.TypeStr = typeInfo.Type.String()
			tv.Fields = extractFields(typeInfo.Type)

			// Check if it's a slice (direct or named type)
			elemType := getElementType(typeInfo.Type)
			if elemType != nil {
				tv.IsSlice = true
				tv.ElemType = elemType.String()
				tv.Fields = extractFields(elemType)
			}
		} else {
			// Fallback: infer from AST
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		vars = append(vars, tv)
	}

	return vars
}

// getElementType returns the element type if the given type is a slice, or nil otherwise
func getElementType(t types.Type) types.Type {
	if t == nil {
		return nil
	}

	// Direct slice type
	if sliceT, ok := t.(*types.Slice); ok {
		return sliceT.Elem()
	}

	// Pointer to slice
	if ptr, ok := t.(*types.Pointer); ok {
		return getElementType(ptr.Elem())
	}

	// Named type - check underlying type
	if named, ok := t.(*types.Named); ok {
		return getElementType(named.Underlying())
	}

	return nil
}

func extractFields(t types.Type) []FieldInfo {
	if t == nil {
		return nil
	}

	// Dereference pointer
	if ptr, ok := t.(*types.Pointer); ok {
		return extractFields(ptr.Elem())
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}

	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		// Collect methods
		var fields []FieldInfo
		for i := 0; i < named.NumMethods(); i++ {
			m := named.Method(i)
			if m.Exported() {
				fields = append(fields, FieldInfo{
					Name:    m.Name(),
					TypeStr: m.Type().String(),
				})
			}
		}
		return fields
	}

	var fields []FieldInfo
	for i := 0; i < strct.NumFields(); i++ {
		f := strct.Field(i)
		if !f.Exported() {
			continue
		}

		fi := FieldInfo{
			Name:    f.Name(),
			TypeStr: f.Type().String(),
		}

		// Recurse into nested structs
		ft := f.Type()
		if ptr, ok := ft.(*types.Pointer); ok {
			ft = ptr.Elem()
		}
		if slice, ok := ft.(*types.Slice); ok {
			fi.IsSlice = true
			fi.Fields = extractFields(slice.Elem())
		} else {
			fi.Fields = extractFields(ft)
		}

		fields = append(fields, fi)
	}

	// Also collect methods
	for i := 0; i < named.NumMethods(); i++ {
		m := named.Method(i)
		if m.Exported() {
			fields = append(fields, FieldInfo{
				Name:    m.Name(),
				TypeStr: "method",
			})
		}
	}

	return fields
}

func inferTypeFromAST(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		switch e.Kind {
		case token.STRING:
			return "string"
		case token.INT:
			return "int"
		case token.FLOAT:
			return "float64"
		}
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return fmt.Sprintf("%v.%s", e.X, e.Sel.Name)
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			return fmt.Sprintf("call:%s", sel.Sel.Name)
		}
	case *ast.CompositeLit:
		if e.Type != nil {
			return fmt.Sprintf("%v", e.Type)
		}
	case *ast.UnaryExpr:
		return "unary"
	}
	return "unknown"
}

// FindGoFiles recursively finds .go files
func FindGoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}
