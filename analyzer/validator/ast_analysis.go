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
			packages.NeedTypesSizes |
			packages.NeedImports,
		Dir:   dir,
		Fset:  fset,
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, "./...")
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

	filesMap := make(map[string]*ast.File)
	for _, f := range allFiles {
		if pos := fset.File(f.Pos()); pos != nil {
			filesMap[pos.Name()] = f
		}
	}

	structIndex := buildStructIndex(fset, filesMap)

	// Collect scopes (local functions) and analyze their Sets and Renders
	scopes := collectFuncScopes(allFiles, info, fset, structIndex)

	// Identify global implicit variables (from scopes with Sets but NO Renders)
	var globalImplicitVars []TemplateVar
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 && len(scope.SetVars) > 0 {
			globalImplicitVars = append(globalImplicitVars, scope.SetVars...)
		}
	}

	// Generate render calls from scopes that HAVE renders
	for _, scope := range scopes {
		if len(scope.RenderNodes) > 0 {
			for _, call := range scope.RenderNodes {
				// Process this render call
				// Duplicate logic from original AnalyzeDir loop but using 'call' directly

				// Re-extract template path and data arg (since we only stored the CallExpr)
				templateArgIdx := 0

				switch call.Fun.(type) {
				case *ast.SelectorExpr:
					templateArgIdx = 0
				case *ast.Ident:
					templateArgIdx = -1
				}

				if templateArgIdx == -1 {
					for i, arg := range call.Args {
						if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							templateArgIdx = i
							break
						}
					}
				}

				if templateArgIdx == -1 || templateArgIdx >= len(call.Args) {
					continue
				}

				templatePathExpr := call.Args[templateArgIdx]
				templatePath := extractString(templatePathExpr)

				tplNameStartCol, tplNameEndCol := getExprColumnRange(fset, templatePathExpr)
				if lit, ok := templatePathExpr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					tplNameStartCol += 1
					tplNameEndCol -= 1
				}

				if templatePath == "" {
					continue
				}

				// Data arg is next
				dataArgIdx := templateArgIdx + 1
				var vars []TemplateVar
				if dataArgIdx < len(call.Args) {
					vars = extractMapVars(call.Args[dataArgIdx], info, fset, structIndex)
				}

				// Combine variables: Explicit + Local Scope + Global Implicit
				var allVars []TemplateVar
				allVars = append(allVars, vars...)
				allVars = append(allVars, scope.SetVars...)
				allVars = append(allVars, globalImplicitVars...)

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
					Vars:                 allVars,
				})
			}
		}
	}

	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(result.RenderCalls, contextFile, pkgs, structIndex, fset)
	}

	return result
}

// FuncScope represents a function body analysis
type FuncScope struct {
	SetVars     []TemplateVar
	RenderNodes []*ast.CallExpr
}

func collectFuncScopes(files []*ast.File, info *types.Info, fset *token.FileSet, structIndex map[string]structIndexEntry) []FuncScope {
	var scopes []FuncScope

	// Helper to process a function node (FuncDecl or FuncLit)
	processFunc := func(n ast.Node) {
		scope := FuncScope{}

		// Inspect the body of the function
		ast.Inspect(n, func(child ast.Node) bool {
			// Don't recurse into nested function definitions; they will be handled by the outer loop
			if child != n {
				if _, isFunc := child.(*ast.FuncLit); isFunc {
					return false
				}
			}

			call, ok := child.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Check for Render call
			if isRenderCall(call) {
				scope.RenderNodes = append(scope.RenderNodes, call)
			}

			// Check for Set call
			if setVar := extractSetCallVar(call, info, fset, structIndex); setVar != nil {
				scope.SetVars = append(scope.SetVars, *setVar)
			}

			return true
		})
		scopes = append(scopes, scope)
	}

	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncDecl); ok {
				processFunc(n)
			}
			if _, ok := n.(*ast.FuncLit); ok {
				processFunc(n)
			}
			return true
		})
	}
	return scopes
}

func isRenderCall(call *ast.CallExpr) bool {
	funcName := ""
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		funcName = fn.Sel.Name
	case *ast.Ident:
		funcName = fn.Name
	}
	return (funcName == "Render" || funcName == "ExecuteTemplate") && len(call.Args) >= 2
}

func extractSetCallVar(call *ast.CallExpr, info *types.Info, fset *token.FileSet, structIndex map[string]structIndexEntry) *TemplateVar {
	// Look for c.Set("key", value)
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	if sel.Sel.Name != "Set" {
		return nil
	}

	// Check receiver type
	if info != nil && sel.X != nil {
		if typeAndValue, ok := info.Types[sel.X]; ok {
			t := typeAndValue.Type
			if ptr, ok := t.(*types.Pointer); ok {
				t = ptr.Elem()
			}
			if named, ok := t.(*types.Named); ok {
				if named.Obj().Name() != "Context" {
					return nil
				}
			} else {
				// Strict check: if unknown type, ignore to avoid false positives?
				// For now, let's skip if we can't confirm it's Context, assuming 'Set' is rare enough or specific enough.
				// But user might use other libraries with 'Set'.
				// Let's rely on type name "Context" if available.
				return nil
			}
		}
	}

	if len(call.Args) < 2 {
		return nil
	}

	keyArg := call.Args[0]
	key := extractString(keyArg)
	if key == "" {
		return nil
	}

	valArg := call.Args[1]
	tv := TemplateVar{Name: key}

	if typeInfo, ok := info.Types[valArg]; ok && typeInfo.Type != nil {
		tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())
		seen := make(map[string]bool)
		tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, seen, fset)

		if elemType := getElementType(typeInfo.Type); elemType != nil {
			tv.IsSlice = true
			tv.ElemType = normalizeTypeStr(elemType.String())
			elemSeen := make(map[string]bool)
			elemFields, elemDoc := extractFieldsWithDocs(elemType, structIndex, elemSeen, fset)
			tv.Fields = elemFields
			if elemDoc != "" {
				tv.Doc = elemDoc
			}
		} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
			tv.IsMap = true
			tv.KeyType = normalizeTypeStr(keyType.String())
			tv.ElemType = normalizeTypeStr(elemType.String())
			elemSeen := make(map[string]bool)
			elemFields, elemDoc := extractFieldsWithDocs(elemType, structIndex, elemSeen, fset)
			tv.Fields = elemFields
			if elemDoc != "" {
				tv.Doc = elemDoc
			}
		}
	} else {
		tv.TypeStr = inferTypeFromAST(valArg)
	}

	tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(valArg, info, fset)
	return &tv
}

func enrichRenderCallsWithContext(calls []RenderCall, contextFile string, pkgs []*packages.Package, structIndex map[string]structIndexEntry, fset *token.FileSet) []RenderCall {
	data, err := os.ReadFile(contextFile)
	if err != nil {
		return calls
	}
	var config map[string]map[string]string
	if err := json.Unmarshal(data, &config); err != nil {
		return calls
	}

	// Build a global type registry mapping "PackageName.TypeName" to *types.TypeName
	// This allows us to extract both the Type and its Definition Position.
	typeMap := make(map[string]*types.TypeName)
	visited := make(map[string]bool)

	var walkPkg func(p *packages.Package)
	walkPkg = func(p *packages.Package) {
		if visited[p.ID] {
			return
		}
		visited[p.ID] = true

		if p.Types != nil {
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				obj := scope.Lookup(name)
				if typeName, ok := obj.(*types.TypeName); ok {
					key := p.Types.Name() + "." + name
					typeMap[key] = typeName
				}
			}
		}

		// Recursively walk imports to gather types from standard library or other modules
		for _, imp := range p.Imports {
			walkPkg(imp)
		}
	}

	for _, pkg := range pkgs {
		walkPkg(pkg)
	}

	globalVars := buildTemplateVars(config["global"], typeMap, structIndex, fset)

	seenTpls := make(map[string]bool)

	for i, call := range calls {
		seenTpls[call.Template] = true
		var newVars []TemplateVar
		newVars = append(newVars, globalVars...)
		if tplVars, ok := config[call.Template]; ok {
			newVars = append(newVars, buildTemplateVars(tplVars, typeMap, structIndex, fset)...)
		}
		newVars = append(newVars, call.Vars...)
		calls[i].Vars = newVars
	}

	// Add synthetic render calls for templates defined in the JSON but not found in Go AST
	for tplName, tplVars := range config {
		if tplName == "global" {
			continue
		}
		if !seenTpls[tplName] {
			var newVars []TemplateVar
			newVars = append(newVars, globalVars...)
			newVars = append(newVars, buildTemplateVars(tplVars, typeMap, structIndex, fset)...)
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

func buildTemplateVars(varDefs map[string]string, typeMap map[string]*types.TypeName, structIndex map[string]structIndexEntry, fset *token.FileSet) []TemplateVar {
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

		// Check for map type string: map[Key]Value
		if strings.HasPrefix(baseTypeStr, "map[") {
			if idx := strings.Index(baseTypeStr, "]"); idx != -1 {
				keyType := baseTypeStr[4:idx]
				elemType := baseTypeStr[idx+1:]
				tv.IsMap = true
				tv.KeyType = strings.TrimSpace(keyType)
				tv.ElemType = strings.TrimSpace(elemType)

				// Attempt to resolve the value type's fields
				valTypeLookup := tv.ElemType
				for strings.HasPrefix(valTypeLookup, "*") {
					valTypeLookup = valTypeLookup[1:]
				}

				if typeNameObj, ok := typeMap[valTypeLookup]; ok {
					t := typeNameObj.Type()
					seen := make(map[string]bool)
					fields, doc := extractFieldsWithDocs(t, structIndex, seen, fset)
					tv.Fields = fields
					if doc != "" {
						tv.Doc = doc
					}
				}
				vars = append(vars, tv)
				continue
			}
		}

		if typeNameObj, ok := typeMap[baseTypeStr]; ok {
			t := typeNameObj.Type()
			seen := make(map[string]bool)
			fields, doc := extractFieldsWithDocs(t, structIndex, seen, fset)
			tv.Fields = fields
			tv.Doc = doc

			// Capture the definition location of the top-level variable type
			if pos := typeNameObj.Pos(); pos.IsValid() && fset != nil {
				position := fset.Position(pos)
				tv.DefFile = position.Filename
				tv.DefLine = position.Line
				tv.DefCol = position.Column
			}

			if isSlice {
				tv.IsSlice = true
				tv.ElemType = baseTypeStr
			}
		} else {
			// Fallback for built-in types (e.g., "string", "int") or unresolved types
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
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, seen, fset)

			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				// Create a fresh seen map for element type extraction — the slice
				// element type may be the same struct that appears elsewhere.
				elemSeen := make(map[string]bool)
				elemFields, elemDoc := extractFieldsWithDocs(elemType, structIndex, elemSeen, fset)
				tv.Fields = elemFields
				if elemDoc != "" {
					tv.Doc = elemDoc
				}
			} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
				tv.IsMap = true
				tv.KeyType = normalizeTypeStr(keyType.String())
				tv.ElemType = normalizeTypeStr(elemType.String())
				// Create a fresh seen map for element type extraction
				elemSeen := make(map[string]bool)
				elemFields, elemDoc := extractFieldsWithDocs(elemType, structIndex, elemSeen, fset)
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
	var prefix strings.Builder
	base := s
	for {
		switch {
		case strings.HasPrefix(base, "[]"):
			prefix.WriteString("[]")
			base = base[2:]
		case strings.HasPrefix(base, "*"):
			prefix.WriteString("*")
			base = base[1:]
		default:
			if idx := strings.LastIndex(base, "/"); idx >= 0 {
				base = base[idx+1:]
			}
			return prefix.String() + base
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

// getMapTypes unwraps maps (and pointer-to-map) to return the key and element
// types. Returns nil for non-map types.
func getMapTypes(t types.Type) (types.Type, types.Type) {
	switch v := t.(type) {
	case *types.Map:
		return v.Key(), v.Elem()
	case *types.Pointer:
		return getMapTypes(v.Elem())
	case *types.Named:
		return getMapTypes(v.Underlying())
	}
	return nil, nil
}

// extractFieldsWithDocs recursively extracts exported fields (and their nested
// fields) from a named struct type. The seen map prevents infinite loops on
// self-referential types; callers should pass a fresh map per extraction root.
func extractFieldsWithDocs(t types.Type, structIndex map[string]structIndexEntry, seen map[string]bool, fset *token.FileSet) ([]FieldInfo, string) {
	if t == nil {
		return nil, ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return extractFieldsWithDocs(ptr.Elem(), structIndex, seen, fset)
	}
	if mapType, ok := t.(*types.Map); ok {
		return extractFieldsWithDocs(mapType.Elem(), structIndex, seen, fset)
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
				fi := FieldInfo{
					Name:    m.Name(),
					TypeStr: normalizeTypeStr(m.Type().String()),
				}
				if pos := m.Pos(); pos.IsValid() && fset != nil {
					position := fset.Position(pos)
					fi.DefFile = position.Filename
					fi.DefLine = position.Line
					fi.DefCol = position.Column
				}
				fields = append(fields, fi)
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

		// Use the exact position from the type checker for reliable Go-to-Definition
		if pos := f.Pos(); pos.IsValid() && fset != nil {
			position := fset.Position(pos)
			fi.DefFile = position.Filename
			fi.DefLine = position.Line
			fi.DefCol = position.Column
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
			fi.Fields, _ = extractFieldsWithDocs(slice.Elem(), structIndex, elemSeen, fset)
		} else if keyType, elemType := getMapTypes(ft); keyType != nil && elemType != nil {
			// Map field - extract element type's fields
			fi.IsMap = true
			fi.KeyType = normalizeTypeStr(keyType.String())
			fi.ElemType = normalizeTypeStr(elemType.String())
			elemSeen := copySeenMap(seen)
			fi.Fields, _ = extractFieldsWithDocs(elemType, structIndex, elemSeen, fset)
		} else {
			// Non-slice field — recurse into the field's type.
			// Pass the shared seen map so the cycle guard works across the whole
			// current path, but siblings of the same type can still be processed
			// independently (they would each have been added to seen on their own
			// path, not on a sibling path).
			fi.Fields, _ = extractFieldsWithDocs(ft, structIndex, seen, fset)
		}

		// Fallback to AST structIndex for position if missing, and always grab doc comments
		if pos, ok := entry.fields[f.Name()]; ok {
			if fi.DefFile == "" {
				fi.DefFile = pos.file
				fi.DefLine = pos.line
				fi.DefCol = pos.col
			}
			fi.Doc = pos.doc
		}

		fields = append(fields, fi)
	}

	// Append exported methods after fields.
	for i := 0; i < named.NumMethods(); i++ {
		m := named.Method(i)
		if m.Exported() {
			fi := FieldInfo{Name: m.Name(), TypeStr: "method"}
			if pos := m.Pos(); pos.IsValid() && fset != nil {
				position := fset.Position(pos)
				fi.DefFile = position.Filename
				fi.DefLine = position.Line
				fi.DefCol = position.Column
			}
			fields = append(fields, fi)
		}
	}

	return fields, entry.doc
}

// copySeenMap creates a shallow copy of a seen map so that an independent
// traversal branch can track its own cycle detection without affecting siblings.
func copySeenMap(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	maps.Copy(dst, src)
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
