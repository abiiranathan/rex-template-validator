package validator

import (
	"encoding/json"
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

func getExprColumnRange(fset *token.FileSet, expr ast.Expr) (startCol, endCol int) {
	pos := fset.Position(expr.Pos())
	endPos := fset.Position(expr.End())
	return pos.Column, endPos.Column
}

func AnalyzeDir(dir string, contextFile string) AnalysisResult {
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

			// Determine the function name and the argument index of the template path.
			//
			// We support two calling conventions:
			//   1. Method call:  c.Render("tmpl.html", data)
			//                    c.ExecuteTemplate(w, "tmpl.html", data)
			//      The function is a SelectorExpr; the template path is Args[0].
			//
			//   2. Plain function call: Render(w, "tmpl.html", data)
			//      The function is an Ident; we look for a string literal among
			//      Args to identify the template path argument.
			//
			// In both cases we require at least 2 arguments total.

			funcName := ""
			templateArgIdx := 0 // index of the template path string in call.Args

			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				funcName = fn.Sel.Name
				// Method call: first arg is the template path.
				templateArgIdx = 0
			case *ast.Ident:
				funcName = fn.Name
				// Plain function call: the template path may be the first or second
				// argument (e.g. Render(w, "tmpl.html", data)). Find the first
				// string-literal argument and use that as the template path, taking
				// the argument immediately after it as the data argument.
				templateArgIdx = -1 // will be resolved below
			}

			if funcName != "Render" && funcName != "ExecuteTemplate" {
				return true
			}

			if len(call.Args) < 2 {
				return true
			}

			// For plain function calls, find the first string-literal argument.
			if templateArgIdx == -1 {
				for i, arg := range call.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						templateArgIdx = i
						break
					}
				}
				if templateArgIdx == -1 {
					return true // no string literal found — not a recognized pattern
				}
			}

			// Ensure we have a data argument after the template path.
			dataArgIdx := templateArgIdx + 1
			if dataArgIdx >= len(call.Args) {
				return true
			}

			templatePathExpr := call.Args[templateArgIdx]
			templatePath := extractString(templatePathExpr)

			tplNameStartCol, tplNameEndCol := getExprColumnRange(fset, templatePathExpr)
			if lit, ok := templatePathExpr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				tplNameStartCol += 1
				tplNameEndCol -= 1
			}

			if templatePath == "" {
				return true
			}

			vars := extractMapVars(call.Args[dataArgIdx], info, fset, structIndex)

			pos := fset.Position(call.Pos())
			relFile := pos.Filename
			if abs, err := filepath.Abs(pos.Filename); err == nil {
				if rel, err := filepath.Rel(dir, abs); err == nil {
					relFile = rel
				}
			}

			result.RenderCalls = append(result.RenderCalls, RenderCall{
				File:                 relFile,
				Line:                 pos.Line,
				Template:             templatePath,
				TemplateNameStartCol: tplNameStartCol,
				TemplateNameEndCol:   tplNameEndCol,
				Vars:                 vars,
			})
			return true
		})
	}

	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(result.RenderCalls, contextFile, pkgs, structIndex)
	}

	return result
}

func enrichRenderCallsWithContext(calls []RenderCall, contextFile string, pkgs []*packages.Package, structIndex map[string]structIndexEntry) []RenderCall {
	data, err := os.ReadFile(contextFile)
	if err != nil {
		return calls
	}
	var config map[string]map[string]string
	if err := json.Unmarshal(data, &config); err != nil {
		return calls
	}

	typeMap := make(map[string]types.Type)
	for _, pkg := range pkgs {
		if pkg.Types != nil {
			scope := pkg.Types.Scope()
			for _, name := range scope.Names() {
				obj := scope.Lookup(name)
				if typeName, ok := obj.(*types.TypeName); ok {
					key := pkg.Types.Name() + "." + name
					typeMap[key] = typeName.Type()
				}
			}
		}
	}

	globalVars := buildTemplateVars(config["global"], typeMap, structIndex)

	seenTpls := make(map[string]bool)

	for i, call := range calls {
		seenTpls[call.Template] = true
		var newVars []TemplateVar
		newVars = append(newVars, globalVars...)
		if tplVars, ok := config[call.Template]; ok {
			newVars = append(newVars, buildTemplateVars(tplVars, typeMap, structIndex)...)
		}
		newVars = append(newVars, call.Vars...)
		calls[i].Vars = newVars
	}

	for tplName, tplVars := range config {
		if tplName == "global" {
			continue
		}
		if !seenTpls[tplName] {
			var newVars []TemplateVar
			newVars = append(newVars, globalVars...)
			newVars = append(newVars, buildTemplateVars(tplVars, typeMap, structIndex)...)
			calls = append(calls, RenderCall{
				File:     "context-file",
				Line:     1,
				Template: tplName,
				Vars:     newVars,
			})
		}
	}

	return calls
}

func buildTemplateVars(varDefs map[string]string, typeMap map[string]types.Type, structIndex map[string]structIndexEntry) []TemplateVar {
	var vars []TemplateVar
	for name, typeStr := range varDefs {
		tv := TemplateVar{Name: name, TypeStr: typeStr}

		baseTypeStr := typeStr
		isSlice := false
		for strings.HasPrefix(baseTypeStr, "[]") || strings.HasPrefix(baseTypeStr, "*") {
			if strings.HasPrefix(baseTypeStr, "[]") {
				isSlice = true
				baseTypeStr = baseTypeStr[2:]
			} else {
				baseTypeStr = baseTypeStr[1:]
			}
		}

		if t, ok := typeMap[baseTypeStr]; ok {
			seen := make(map[string]bool)
			fields, doc := extractFieldsWithDocs(t, structIndex, seen)
			tv.Fields = fields
			tv.Doc = doc
			if isSlice {
				tv.IsSlice = true
				tv.ElemType = baseTypeStr
			}
		} else {
			if isSlice {
				tv.IsSlice = true
				tv.ElemType = baseTypeStr
			}
		}
		vars = append(vars, tv)
	}
	return vars
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

			// Create a fresh seen map per top-level variable so that the same
			// struct type can appear under different variable names without being
			// suppressed. Cycles within a single variable's type tree are still
			// prevented — the same type key cannot appear twice in one path.
			seen := make(map[string]bool)
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, seen)

			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				// Create a fresh seen map for element type extraction — the slice
				// element type may be the same struct that appears elsewhere.
				elemSeen := make(map[string]bool)
				elemFields, elemDoc := extractFieldsWithDocs(elemType, structIndex, elemSeen)
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

// getElementType unwraps slices (and pointer-to-slice) to return the element
// type. Returns nil for non-slice types.
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

// extractFieldsWithDocs recursively extracts exported fields (and their nested
// fields) from a named struct type. The seen map prevents infinite loops on
// self-referential types; callers should pass a fresh map per extraction root.
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

	// Cycle guard: if we've already started processing this type in the current
	// extraction tree, stop to prevent infinite recursion.
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
			// Slice field — extract element type's fields.
			// Use a copy of seen so that processing sibling slices of the same
			// type doesn't suppress each other.
			fi.IsSlice = true
			elemSeen := copySeenMap(seen)
			fi.Fields, _ = extractFieldsWithDocs(slice.Elem(), structIndex, elemSeen)
		} else {
			// Non-slice field — recurse into the field's type.
			// Pass the shared seen map so the cycle guard works across the whole
			// current path, but siblings of the same type can still be processed
			// independently (they would each have been added to seen on their own
			// path, not on a sibling path).
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

// copySeenMap creates a shallow copy of a seen map so that an independent
// traversal branch can track its own cycle detection without affecting siblings.
func copySeenMap(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
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
