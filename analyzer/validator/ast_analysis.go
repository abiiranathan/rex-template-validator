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

// AnalyzeDir analyzes a Go source directory for c.Render calls.
// dir should be an absolute path.
func AnalyzeDir(dir string) AnalysisResult {
	result := AnalysisResult{}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.AllErrors)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("parse error: %v", err))
		return result
	}

	var allFiles []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			allFiles = append(allFiles, f)
		}
	}

	// Type-check with a best-effort importer. Missing third-party packages are
	// expected when running outside the module cache; we still extract useful
	// type information for packages that do resolve.
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}

	cfg := &types.Config{
		// Use the default gc importer (resolves stdlib + anything in GOPATH/module cache).
		Importer: importer.ForCompiler(fset, "gc", nil),
		// Collect type errors but treat them as non-fatal — we fall back to AST
		// inference for values whose types couldn't be resolved.
		Error: func(err error) {
			msg := err.Error()
			if !isImportRelatedError(msg) {
				// Only surface genuine, actionable type errors.
				result.Errors = append(result.Errors, fmt.Sprintf("type error: %v", err))
			}
		},
	}

	// Non-fatal: we still walk the AST even if type-check fails partially.
	cfg.Check(dir, fset, allFiles, info) //nolint:errcheck

	// Walk AST looking for c.Render(...) calls
	for _, f := range allFiles {
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

			templatePath := extractString(call.Args[0])
			if templatePath == "" {
				return true
			}

			vars := extractMapVars(call.Args[1], info, fset)

			pos := fset.Position(call.Pos())

			// Normalise the file path: make it relative to dir so that it is
			// stable regardless of where the binary is invoked from.
			relFile := pos.Filename
			if abs, err := filepath.Abs(pos.Filename); err == nil {
				if rel, err := filepath.Rel(dir, abs); err == nil {
					relFile = rel
				}
			}

			rc := RenderCall{
				File:     relFile,
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

// isImportRelatedError returns true for errors that stem solely from missing
// third-party dependencies rather than from actual code problems.
func isImportRelatedError(msg string) bool {
	lower := strings.ToLower(msg)
	phrases := []string{
		"could not import",
		"can't find import",
		"cannot find package",
		"no required module provides",
	}
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
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

		if typeInfo, ok := info.Types[kv.Value]; ok {
			tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())
			tv.Fields = extractFields(typeInfo.Type)

			elemType := getElementType(typeInfo.Type)
			if elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				tv.Fields = extractFields(elemType)
			}
		} else {
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		vars = append(vars, tv)
	}

	return vars
}

// normalizeTypeStr strips absolute directory paths from type strings produced
// by types.Type.String(), leaving only the short package-qualified name.
//
// e.g. "[]/home/user/project/sample.Drug"  → "[]sample.Drug"
//
//	"*/home/user/project/sample.Visit"   → "*sample.Visit"
//	"/home/user/project/sample.Patient"  → "sample.Patient"
func normalizeTypeStr(s string) string {
	prefix := ""
	base := s
	for {
		if strings.HasPrefix(base, "[]") {
			prefix += "[]"
			base = base[2:]
		} else if strings.HasPrefix(base, "*") {
			prefix += "*"
			base = base[1:]
		} else {
			break
		}
	}
	// If base contains a slash it's an absolute-path-qualified import path.
	// Keep only everything after the last slash ("pkg.TypeName").
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	return prefix + base
}

func getElementType(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	if sliceT, ok := t.(*types.Slice); ok {
		return sliceT.Elem()
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return getElementType(ptr.Elem())
	}
	if named, ok := t.(*types.Named); ok {
		return getElementType(named.Underlying())
	}
	return nil
}

func extractFields(t types.Type) []FieldInfo {
	if t == nil {
		return nil
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return extractFields(ptr.Elem())
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}

	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		var fields []FieldInfo
		for i := 0; i < named.NumMethods(); i++ {
			m := named.Method(i)
			if m.Exported() {
				fields = append(fields, FieldInfo{
					Name:    m.Name(),
					TypeStr: normalizeTypeStr(m.Type().String()),
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
			TypeStr: normalizeTypeStr(f.Type().String()),
		}

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
