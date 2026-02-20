package validator

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

func AnalyzeDir(dir string) AnalysisResult {
	result := AnalysisResult{}
	fset := token.NewFileSet()

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedTypesSizes,
		Dir:   dir,
		Fset:  fset,
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
		return result
	}

	var allFiles []*ast.File
	var info *types.Info

	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			if !isImportRelatedError(e.Msg) {
				result.Errors = append(result.Errors, fmt.Sprintf("type error: %v", e.Msg))
			}
		}
		allFiles = append(allFiles, pkg.Syntax...)
		if pkg.TypesInfo != nil {
			if info == nil {
				info = pkg.TypesInfo
			} else {
				maps.Copy(info.Types, pkg.TypesInfo.Types)
				maps.Copy(info.Defs, pkg.TypesInfo.Defs)
				maps.Copy(info.Uses, pkg.TypesInfo.Uses)
			}
		}
	}

	if info == nil {
		info = &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		}
	}

	// Build file map and pre-compute all struct positions once — not per call.
	filesMap := make(map[string]*ast.File)
	for _, f := range allFiles {
		if pos := fset.File(f.Pos()); pos != nil {
			filesMap[pos.Name()] = f
		}
	}

	// structIndex is built once and shared for the lifetime of this analysis.
	// Key: "pkgName.TypeName" to avoid cross-package name collisions.
	structIndex := buildStructIndex(fset, filesMap)

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

			if (sel.Sel.Name != "Render" && sel.Sel.Name != "ExecuteTemplate") || len(call.Args) < 2 {
				return true
			}
			templatePath := extractString(call.Args[0])
			if templatePath == "" {
				return true
			}

			vars := extractMapVars(call.Args[1], info, fset, structIndex)

			pos := fset.Position(call.Pos())
			relFile := pos.Filename
			if abs, err := filepath.Abs(pos.Filename); err == nil {
				if rel, err := filepath.Rel(dir, abs); err == nil {
					relFile = rel
				}
			}

			result.RenderCalls = append(result.RenderCalls, RenderCall{
				File:     relFile,
				Line:     pos.Line,
				Template: templatePath,
				Vars:     vars,
			})
			return true
		})
	}

	return result
}

// structKey returns a stable map key for a named type, qualified by package
// name to prevent collisions between same-named types in different packages.
func structKey(named *types.Named) string {
	obj := named.Obj()
	if obj.Pkg() != nil {
		return obj.Pkg().Name() + "." + obj.Name()
	}
	return obj.Name()
}

// structIndexEntry holds the pre-computed documentation and field positions
// for a single struct type.
type structIndexEntry struct {
	doc    string
	fields map[string]fieldInfo
}

// buildStructIndex walks all AST files once and indexes every struct type by
// "pkgName.TypeName". Call this once per analysis run.
func buildStructIndex(fset *token.FileSet, files map[string]*ast.File) map[string]structIndexEntry {
	index := make(map[string]structIndexEntry)

	for _, f := range files {
		// Derive a package name prefix from the file's package declaration.
		pkgName := f.Name.Name

		ast.Inspect(f, func(n ast.Node) bool {
			genDecl, ok := n.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				return true
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}

				entry := structIndexEntry{
					doc:    getTypeDoc(genDecl, typeSpec),
					fields: make(map[string]fieldInfo),
				}

				for _, field := range structType.Fields.List {
					pos := fset.Position(field.Pos())
					doc := getFieldDoc(field)
					for _, name := range field.Names {
						entry.fields[name.Name] = fieldInfo{
							file: pos.Filename,
							line: pos.Line,
							col:  pos.Column,
							doc:  doc,
						}
					}
				}

				key := pkgName + "." + typeSpec.Name.Name
				index[key] = entry
			}
			return true
		})
	}

	return index
}

func isImportRelatedError(msg string) bool {
	lower := strings.ToLower(msg)
	for _, phrase := range []string{
		"could not import",
		"can't find import",
		"cannot find package",
		"no required module provides",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func extractString(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING || len(lit.Value) < 2 {
		return ""
	}
	return lit.Value[1 : len(lit.Value)-1]
}

func extractMapVars(expr ast.Expr, info *types.Info, fset *token.FileSet, structIndex map[string]structIndexEntry) []TemplateVar {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}

	var vars []TemplateVar
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
			// seen guards against infinite recursion on self-referential types.
			seen := make(map[string]bool)
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, seen)

			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				elemFields, elemDoc := extractFieldsWithDocs(elemType, structIndex, seen)
				tv.Fields = elemFields
				if elemDoc != "" {
					tv.Doc = elemDoc
				}
			}
		} else {
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)
		vars = append(vars, tv)
	}
	return vars
}

func findDefinitionLocation(expr ast.Expr, info *types.Info, fset *token.FileSet) (string, int, int) {
	var ident *ast.Ident
	switch e := expr.(type) {
	case *ast.Ident:
		ident = e
	case *ast.UnaryExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			ident = id
		}
	case *ast.CallExpr:
		pos := fset.Position(e.Pos())
		return pos.Filename, pos.Line, pos.Column
	case *ast.CompositeLit:
		pos := fset.Position(e.Pos())
		return pos.Filename, pos.Line, pos.Column
	case *ast.SelectorExpr:
		pos := fset.Position(e.Sel.Pos())
		return pos.Filename, pos.Line, pos.Column
	}

	if ident != nil {
		if obj, ok := info.Defs[ident]; ok && obj != nil {
			pos := fset.Position(obj.Pos())
			return pos.Filename, pos.Line, pos.Column
		}
		if obj, ok := info.Uses[ident]; ok && obj != nil {
			pos := fset.Position(obj.Pos())
			return pos.Filename, pos.Line, pos.Column
		}
		pos := fset.Position(ident.Pos())
		return pos.Filename, pos.Line, pos.Column
	}

	pos := fset.Position(expr.Pos())
	return pos.Filename, pos.Line, pos.Column
}

func normalizeTypeStr(s string) string {
	prefix := ""
	base := s
	for {
		switch {
		case strings.HasPrefix(base, "[]"):
			prefix += "[]"
			base = base[2:]
		case strings.HasPrefix(base, "*"):
			prefix += "*"
			base = base[1:]
		default:
			if idx := strings.LastIndex(base, "/"); idx >= 0 {
				base = base[idx+1:]
			}
			return prefix + base
		}
	}
}

func getElementType(t types.Type) types.Type {
	switch v := t.(type) {
	case *types.Slice:
		return v.Elem()
	case *types.Pointer:
		return getElementType(v.Elem())
	case *types.Named:
		return getElementType(v.Underlying())
	}
	return nil
}

func extractFieldsWithDocs(t types.Type, structIndex map[string]structIndexEntry, seen map[string]bool) ([]FieldInfo, string) {
	if t == nil {
		return nil, ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return extractFieldsWithDocs(ptr.Elem(), structIndex, seen)
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil, ""
	}

	// Cycle guard: if we've already started processing this type, stop.
	key := structKey(named)
	if seen[key] {
		return nil, ""
	}
	seen[key] = true

	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		// Interface or other named type: expose exported methods only.
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
		return fields, ""
	}

	entry := structIndex[key]

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
			fi.Fields, _ = extractFieldsWithDocs(slice.Elem(), structIndex, seen)
		} else {
			fi.Fields, _ = extractFieldsWithDocs(ft, structIndex, seen)
		}

		if pos, ok := entry.fields[f.Name()]; ok {
			fi.DefFile = pos.file
			fi.DefLine = pos.line
			fi.DefCol = pos.col
			fi.Doc = pos.doc
		}

		fields = append(fields, fi)
	}

	// Append exported methods after fields.
	for i := 0; i < named.NumMethods(); i++ {
		m := named.Method(i)
		if m.Exported() {
			fields = append(fields, FieldInfo{Name: m.Name(), TypeStr: "method"})
		}
	}

	return fields, entry.doc
}

type fieldInfo struct {
	file string
	line int
	col  int
	doc  string
}

func getTypeDoc(genDecl *ast.GenDecl, typeSpec *ast.TypeSpec) string {
	if genDecl.Doc != nil {
		return genDecl.Doc.Text()
	}
	if typeSpec.Doc != nil {
		return typeSpec.Doc.Text()
	}
	if typeSpec.Comment != nil {
		return typeSpec.Comment.Text()
	}
	return ""
}

func getFieldDoc(field *ast.Field) string {
	if field.Doc != nil {
		return field.Doc.Text()
	}
	if field.Comment != nil {
		return field.Comment.Text()
	}
	return ""
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
