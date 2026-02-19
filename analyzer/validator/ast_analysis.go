package validator

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// AnalyzeDir analyzes a Go source directory for c.Render calls.
// dir should be an absolute path.
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
		Dir:       dir,
		Fset:      fset,
		ParseFile: nil, // use default (includes comments)
		Tests:     false,
	}

	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
		return result
	}

	var allFiles []*ast.File
	var info *types.Info

	for _, pkg := range pkgs {
		// Surface package-level errors (type errors, missing imports, etc.)
		for _, e := range pkg.Errors {
			msg := e.Msg
			if !isImportRelatedError(strings.ToLower(msg)) {
				result.Errors = append(result.Errors, fmt.Sprintf("type error: %v", msg))
			}
		}

		allFiles = append(allFiles, pkg.Syntax...)

		// Merge type info — typically only one package for a dir, but handle
		// multiple gracefully (e.g. when build tags produce separate packages).
		if pkg.TypesInfo != nil {
			if info == nil {
				info = pkg.TypesInfo
			} else {
				for k, v := range pkg.TypesInfo.Types {
					info.Types[k] = v
				}
				for k, v := range pkg.TypesInfo.Defs {
					info.Defs[k] = v
				}
				for k, v := range pkg.TypesInfo.Uses {
					info.Uses[k] = v
				}
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

	// Build file map for struct field position lookup
	filesMap := make(map[string]*ast.File)
	for _, f := range allFiles {
		pos := fset.File(f.Pos())
		if pos != nil {
			filesMap[pos.Name()] = f
		}
	}
	setPkgCache(fset, filesMap)

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
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type)

			elemType := getElementType(typeInfo.Type)
			if elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				elemFields, elemDoc := extractFieldsWithDocs(elemType)
				tv.Fields = elemFields
				// Use element documentation for slice element type
				if elemDoc != "" {
					tv.Doc = elemDoc
				}
			}
		} else {
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		// Track definition location for go-to-definition
		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)

		vars = append(vars, tv)
	}

	return vars
}

// findDefinitionLocation finds where a variable/value is defined in the Go source
// Returns file path, line, and column (1-based)
func findDefinitionLocation(expr ast.Expr, info *types.Info, fset *token.FileSet) (string, int, int) {
	// Try to find the definition of an identifier
	var ident *ast.Ident

	switch e := expr.(type) {
	case *ast.Ident:
		// Direct identifier reference
		ident = e
	case *ast.UnaryExpr:
		// &Variable - get the operand
		if id, ok := e.X.(*ast.Ident); ok {
			ident = id
		}
	case *ast.CallExpr:
		// Function call - point to the call itself
		pos := fset.Position(e.Pos())
		if pos.Filename != "" {
			return pos.Filename, pos.Line, pos.Column
		}
	case *ast.CompositeLit:
		// Composite literal - point to the literal itself
		pos := fset.Position(e.Pos())
		if pos.Filename != "" {
			return pos.Filename, pos.Line, pos.Column
		}
	case *ast.SelectorExpr:
		// Field access like obj.Field - use the selector
		pos := fset.Position(e.Sel.Pos())
		if pos.Filename != "" {
			return pos.Filename, pos.Line, pos.Column
		}
	}

	if ident != nil {
		// Look up the definition of this identifier
		if obj, ok := info.Defs[ident]; ok && obj != nil {
			pos := fset.Position(obj.Pos())
			if pos.Filename != "" {
				return pos.Filename, pos.Line, pos.Column
			}
		}
		// If not a definition, check if it's a use of something defined elsewhere
		if obj, ok := info.Uses[ident]; ok && obj != nil {
			pos := fset.Position(obj.Pos())
			if pos.Filename != "" {
				return pos.Filename, pos.Line, pos.Column
			}
		}
		// Fallback: use the identifier position itself
		pos := fset.Position(ident.Pos())
		if pos.Filename != "" {
			return pos.Filename, pos.Line, pos.Column
		}
	}

	// Last resort: use the expression position
	pos := fset.Position(expr.Pos())
	return pos.Filename, pos.Line, pos.Column
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

// Package cache for storing AST files per package
type pkgCache struct {
	fset  *token.FileSet
	files map[string]*ast.File
}

var globalPkgCache *pkgCache

func init() {
	globalPkgCache = &pkgCache{
		files: make(map[string]*ast.File),
	}
}

func setPkgCache(fset *token.FileSet, files map[string]*ast.File) {
	globalPkgCache.fset = fset
	globalPkgCache.files = files
}

func extractFieldsWithDocs(t types.Type) ([]FieldInfo, string) {
	if t == nil {
		return nil, ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return extractFieldsWithDocs(ptr.Elem())
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil, ""
	}

	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		var fields []FieldInfo
		for m := range named.Methods() {
			if m.Exported() {
				fields = append(fields, FieldInfo{
					Name:    m.Name(),
					TypeStr: normalizeTypeStr(m.Type().String()),
				})
			}
		}
		return fields, ""
	}

	// Get the struct name and package to find AST
	structName := named.Obj().Name()
	pkgPath := named.Obj().Pkg().Path()

	// Try to find struct field positions and docs from AST
	var _ = pkgPath
	fieldPositions, typeDoc := findStructFieldPositions(structName)

	var fields []FieldInfo
	for f := range strct.Fields() {
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
			childFields, _ := extractFieldsWithDocs(slice.Elem())
			fi.Fields = childFields
		} else {
			childFields, _ := extractFieldsWithDocs(ft)
			fi.Fields = childFields
		}

		// Add definition location and documentation if we found it
		if pos, ok := fieldPositions[f.Name()]; ok {
			fi.DefFile = pos.file
			fi.DefLine = pos.line
			fi.DefCol = pos.col
			fi.Doc = pos.doc
		}

		fields = append(fields, fi)
	}

	for m := range named.Methods() {
		if m.Exported() {
			fields = append(fields, FieldInfo{
				Name:    m.Name(),
				TypeStr: "method",
			})
		}
	}

	return fields, typeDoc
}

// fieldInfo stores file location and documentation
type fieldInfo struct {
	file string
	line int
	col  int
	doc  string
}

// getTypeDoc extracts documentation for a type declaration
func getTypeDoc(genDecl *ast.GenDecl, typeSpec *ast.TypeSpec) string {
	// Block comment above the declaration (most common, godoc style)
	if genDecl.Doc != nil {
		return genDecl.Doc.Text()
	}
	// TypeSpec-level block comment (inside grouped type blocks)
	if typeSpec.Doc != nil {
		return typeSpec.Doc.Text()
	}
	// Inline comment on the same line as the struct
	if typeSpec.Comment != nil {
		return typeSpec.Comment.Text()
	}
	return ""
}

// getFieldDoc extracts documentation for a struct field
func getFieldDoc(field *ast.Field) string {
	// Block comment above the field
	if field.Doc != nil {
		return field.Doc.Text()
	}
	// Inline comment on the same line as the field
	if field.Comment != nil {
		return field.Comment.Text()
	}
	return ""
}

// findStructFieldPositions searches the AST for struct type definitions and extracts docs
// Returns field positions and the type documentation
func findStructFieldPositions(structName string) (map[string]fieldInfo, string) {
	positions := make(map[string]fieldInfo)
	typeDoc := ""

	if globalPkgCache == nil || globalPkgCache.files == nil {
		return positions, typeDoc
	}

	for _, f := range globalPkgCache.files {
		ast.Inspect(f, func(n ast.Node) bool {
			// Look for type declarations
			genDecl, ok := n.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				return true
			}

			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}

				// Check if this is the struct we're looking for
				if typeSpec.Name.Name != structName {
					continue
				}

				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}

				// Capture type documentation using helper function
				typeDoc = getTypeDoc(genDecl, typeSpec)

				// Record positions and docs for each field
				for _, field := range structType.Fields.List {
					pos := globalPkgCache.fset.Position(field.Pos())
					fieldDoc := getFieldDoc(field)
					for _, name := range field.Names {
						positions[name.Name] = fieldInfo{
							file: pos.Filename,
							line: pos.Line,
							col:  pos.Column,
							doc:  fieldDoc,
						}
					}
				}

				return false
			}

			return true
		})
	}

	return positions, typeDoc
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
