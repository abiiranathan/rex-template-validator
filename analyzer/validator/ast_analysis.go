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
	"runtime"
	"strings"
	"sync"
	"unique"

	"golang.org/x/tools/go/packages"
)

// ── Interned key type ────────────────────────────────────────────────────────
// unique.Handle gives O(1) pointer-equality comparisons and eliminates
// repeated string hashing on the hot structIndex lookup path.
type structKeyHandle = unique.Handle[string]

func makeStructKey(named *types.Named) structKeyHandle {
	return unique.Make(rawStructKey(named))
}

func rawStructKey(named *types.Named) string {
	obj := named.Obj()
	if obj.Pkg() != nil {
		return obj.Pkg().Name() + "." + obj.Name()
	}
	return obj.Name()
}

// ── Field cache ──────────────────────────────────────────────────────────────
// extractFieldsWithDocs is the single hottest function in the analyser.
// Named types are immutable after package load, so we can cache their
// extracted fields globally for the lifetime of one AnalyzeDir call.

type cachedFields struct {
	fields []FieldInfo
	doc    string
}

type fieldCache struct {
	mu    sync.RWMutex
	cache map[structKeyHandle]cachedFields
}

func newFieldCache() *fieldCache {
	return &fieldCache{cache: make(map[structKeyHandle]cachedFields, 256)}
}

func (fc *fieldCache) get(k structKeyHandle) (cachedFields, bool) {
	fc.mu.RLock()
	v, ok := fc.cache[k]
	fc.mu.RUnlock()
	return v, ok
}

func (fc *fieldCache) set(k structKeyHandle, v cachedFields) {
	fc.mu.Lock()
	fc.cache[k] = v
	fc.mu.Unlock()
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func getExprColumnRange(fset *token.FileSet, expr ast.Expr) (startCol, endCol int) {
	pos := fset.Position(expr.Pos())
	endPos := fset.Position(expr.End())
	return pos.Column, endPos.Column
}

// ── Main entry point ─────────────────────────────────────────────────────────

func AnalyzeDir(dir string, contextFile string, config AnalysisConfig) AnalysisResult {
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

	// ── Merge type info ──────────────────────────────────────────────────────
	// Merge all package TypesInfo into one aggregate. This is sequential and
	// happens before any concurrent work, so there is no data race.
	// We pre-size the maps to avoid incremental rehashing.
	totalTypes, totalDefs, totalUses := 0, 0, 0
	for _, pkg := range pkgs {
		if pkg.TypesInfo != nil {
			totalTypes += len(pkg.TypesInfo.Types)
			totalDefs += len(pkg.TypesInfo.Defs)
			totalUses += len(pkg.TypesInfo.Uses)
		}
	}

	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue, totalTypes),
		Defs:  make(map[*ast.Ident]types.Object, totalDefs),
		Uses:  make(map[*ast.Ident]types.Object, totalUses),
	}

	allFiles := make([]*ast.File, 0, totalTypes/10+len(pkgs))

	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			if !isImportRelatedError(e.Msg) {
				result.Errors = append(result.Errors, fmt.Sprintf("type error: %v", e.Msg))
			}
		}
		allFiles = append(allFiles, pkg.Syntax...)
		if pkg.TypesInfo != nil {
			maps.Copy(info.Types, pkg.TypesInfo.Types)
			maps.Copy(info.Defs, pkg.TypesInfo.Defs)
			maps.Copy(info.Uses, pkg.TypesInfo.Uses)
		}
	}

	// ── Build file map for struct indexing ───────────────────────────────────
	filesMap := make(map[string]*ast.File, len(allFiles))
	for _, f := range allFiles {
		if pos := fset.File(f.Pos()); pos != nil {
			filesMap[pos.Name()] = f
		}
	}

	// Shared field cache — passed through the analysis so each unique named
	// type pays extraction cost exactly once.
	fc := newFieldCache()

	// 1. Build struct index concurrently (writes go to sync.Map, no merge copy)
	structIndex := buildStructIndex(fset, filesMap)

	// 2. Collect scopes concurrently, distributing individual functions not files
	scopes := collectFuncScopes(allFiles, info, fset, structIndex, fc, config)

	// ── Identify global implicit vars (scopes with Sets but NO Renders) ──────
	var globalImplicitVars []TemplateVar
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 && len(scope.SetVars) > 0 {
			globalImplicitVars = append(globalImplicitVars, scope.SetVars...)
		}
	}

	// ── Pre-count render calls to avoid slice re-growth ──────────────────────
	totalRenders := 0
	for _, scope := range scopes {
		totalRenders += len(scope.RenderNodes)
	}
	result.RenderCalls = make([]RenderCall, 0, totalRenders)

	// ── Generate render calls from scopes that have renders ──────────────────
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 {
			continue
		}
		for _, call := range scope.RenderNodes {
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
				tplNameStartCol++
				tplNameEndCol--
			}

			if templatePath == "" {
				continue
			}

			dataArgIdx := templateArgIdx + 1
			var vars []TemplateVar
			if dataArgIdx < len(call.Args) {
				vars = extractMapVars(call.Args[dataArgIdx], info, fset, structIndex, fc)
			}

			// Pre-allocate combined vars slice in one shot
			allVars := make([]TemplateVar, 0, len(vars)+len(scope.SetVars)+len(globalImplicitVars))
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

	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(result.RenderCalls, contextFile, pkgs, structIndex, fc, fset, config)
	}

	return result
}

// ── FuncScope ────────────────────────────────────────────────────────────────

type FuncScope struct {
	SetVars     []TemplateVar
	RenderNodes []*ast.CallExpr
}

// funcWorkUnit is a single function node ready to be processed by a worker.
type funcWorkUnit struct {
	node ast.Node
}

// collectFuncScopes distributes individual function nodes (not files) across
// workers for better load-balancing on files that contain many large functions.
func collectFuncScopes(
	files []*ast.File,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
) []FuncScope {
	// ── Phase 1: collect all function nodes sequentially (AST walk is fast) ──
	// Reserve a reasonable capacity to avoid repeated growth.
	funcNodes := make([]funcWorkUnit, 0, len(files)*8)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				funcNodes = append(funcNodes, funcWorkUnit{node: n})
			}
			return true
		})
	}

	if len(funcNodes) == 0 {
		return nil
	}

	// ── Phase 2: process function nodes concurrently ─────────────────────────
	numWorkers := max(runtime.NumCPU(), 1)

	// Partition work by index range — no per-unit channel send overhead.
	chunkSize := (len(funcNodes) + numWorkers - 1) / numWorkers

	sliceResultChan := make(chan []FuncScope, numWorkers)
	var wg sync.WaitGroup

	for w := range numWorkers {
		start := w * chunkSize
		if start >= len(funcNodes) {
			break
		}
		end := min(start+chunkSize, len(funcNodes))
		chunk := funcNodes[start:end]

		wg.Add(1)
		go func(chunk []funcWorkUnit) {
			defer wg.Done()
			localScopes := make([]FuncScope, 0, len(chunk)/2)

			for _, unit := range chunk {
				scope := processFunc(unit.node, info, fset, structIndex, fc, config)
				if len(scope.RenderNodes) > 0 || len(scope.SetVars) > 0 {
					localScopes = append(localScopes, scope)
				}
			}
			sliceResultChan <- localScopes
		}(chunk)
	}

	// Close result channel once all workers finish.
	go func() {
		wg.Wait()
		close(sliceResultChan)
	}()

	var allScopes []FuncScope
	for scopes := range sliceResultChan {
		allScopes = append(allScopes, scopes...)
	}
	return allScopes
}

// processFunc walks a single function node and returns its FuncScope.
// Extracted from the closure so the compiler can inline/optimise it.
func processFunc(
	n ast.Node,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
) FuncScope {
	var scope FuncScope
	ast.Inspect(n, func(child ast.Node) bool {
		// Don't recurse into nested function literals.
		if child != n {
			if _, isFunc := child.(*ast.FuncLit); isFunc {
				return false
			}
		}

		call, ok := child.(*ast.CallExpr)
		if !ok {
			return true
		}

		if isRenderCall(call, config) {
			scope.RenderNodes = append(scope.RenderNodes, call)
		}
		if setVar := extractSetCallVar(call, info, fset, structIndex, fc, config); setVar != nil {
			scope.SetVars = append(scope.SetVars, *setVar)
		}
		return true
	})
	return scope
}

func isRenderCall(call *ast.CallExpr, config AnalysisConfig) bool {
	funcName := ""
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		funcName = fn.Sel.Name
	case *ast.Ident:
		funcName = fn.Name
	}
	return (funcName == config.RenderFunctionName || funcName == config.ExecuteTemplateFunctionName) &&
		len(call.Args) >= 2
}

func extractSetCallVar(
	call *ast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
) *TemplateVar {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	if sel.Sel.Name != config.SetFunctionName {
		return nil
	}

	if info != nil && sel.X != nil {
		if typeAndValue, ok := info.Types[sel.X]; ok {
			t := typeAndValue.Type
			if ptr, ok := t.(*types.Pointer); ok {
				t = ptr.Elem()
			}
			if named, ok := t.(*types.Named); ok {
				if named.Obj().Name() != config.ContextTypeName {
					return nil
				}
			} else {
				return nil
			}
		}
	}

	if len(call.Args) < 2 {
		return nil
	}

	key := extractString(call.Args[0])
	if key == "" {
		return nil
	}

	valArg := call.Args[1]
	tv := TemplateVar{Name: key}

	if typeInfo, ok := info.Types[valArg]; ok && typeInfo.Type != nil {
		tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())
		seen := make(map[structKeyHandle]bool)
		tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

		if elemType := getElementType(typeInfo.Type); elemType != nil {
			tv.IsSlice = true
			tv.ElemType = normalizeTypeStr(elemType.String())
			elemSeen := make(map[structKeyHandle]bool)
			tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, elemSeen, fset, tv.Doc)
		} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
			tv.IsMap = true
			tv.KeyType = normalizeTypeStr(keyType.String())
			tv.ElemType = normalizeTypeStr(elemType.String())
			elemSeen := make(map[structKeyHandle]bool)
			tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, elemSeen, fset, tv.Doc)
		}
	} else {
		tv.TypeStr = inferTypeFromAST(valArg)
	}

	tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(valArg, info, fset)
	return &tv
}

// extractFieldsWithDocsDoc is a thin helper that preserves an existing doc
// string when the recursive extraction returns an empty one.
func extractFieldsWithDocsDoc(
	t types.Type,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
	existingDoc string,
) ([]FieldInfo, string) {
	fields, doc := extractFieldsWithDocs(t, structIndex, fc, seen, fset)
	if doc == "" {
		doc = existingDoc
	}
	return fields, doc
}

// ── enrichRenderCallsWithContext ─────────────────────────────────────────────

func enrichRenderCallsWithContext(
	calls []RenderCall,
	contextFile string,
	pkgs []*packages.Package,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	config AnalysisConfig,
) []RenderCall {
	data, err := os.ReadFile(contextFile)
	if err != nil {
		return calls
	}
	var contextConfig map[string]map[string]string
	if err := json.Unmarshal(data, &contextConfig); err != nil {
		return calls
	}

	// ── Walk the import graph and collect all named types ────────────────────
	// We do a sequential BFS. The previous concurrent-channel approach deadlocked
	// because worker goroutines tried to send to a fixed-capacity channel that was
	// already full (deep import graphs have far more nodes than len(pkgs)*8), while
	// the main goroutine was blocked in bfsWg.Wait() — a classic send-on-full +
	// wait deadlock with no reader running.
	//
	// Sequential BFS is correct, safe, and plenty fast: type-scope iteration is
	// pure in-memory work and the import graph is walked exactly once.
	typeMap := make(map[string]*types.TypeName, len(pkgs)*32)
	{
		visited := make(map[string]bool, len(pkgs)*32)
		queue := make([]*packages.Package, 0, len(pkgs)*8)

		for _, pkg := range pkgs {
			if !visited[pkg.ID] {
				visited[pkg.ID] = true
				queue = append(queue, pkg)
			}
		}

		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]

			if p.Types != nil {
				scope := p.Types.Scope()
				for _, name := range scope.Names() {
					if typeName, ok := scope.Lookup(name).(*types.TypeName); ok {
						typeMap[p.Types.Name()+"."+name] = typeName
					}
				}
			}

			for _, imp := range p.Imports {
				if !visited[imp.ID] {
					visited[imp.ID] = true
					queue = append(queue, imp)
				}
			}
		}
	}

	globalVars := buildTemplateVars(contextConfig[config.GlobalTemplateName], typeMap, structIndex, fc, fset)

	seenTpls := make(map[string]bool, len(calls))
	for i, call := range calls {
		seenTpls[call.Template] = true
		base := make([]TemplateVar, 0, len(globalVars)+len(call.Vars)+8)
		base = append(base, globalVars...)
		if tplVars, ok := contextConfig[call.Template]; ok {
			base = append(base, buildTemplateVars(tplVars, typeMap, structIndex, fc, fset)...)
		}
		base = append(base, call.Vars...)
		calls[i].Vars = base
	}

	// Synthetic render calls for templates defined in JSON but absent from Go AST.
	for tplName, tplVars := range contextConfig {
		if tplName == config.GlobalTemplateName || seenTpls[tplName] {
			continue
		}
		newVars := make([]TemplateVar, 0, len(globalVars)+len(tplVars))
		newVars = append(newVars, globalVars...)
		newVars = append(newVars, buildTemplateVars(tplVars, typeMap, structIndex, fc, fset)...)
		calls = append(calls, RenderCall{
			File:     "context-file",
			Line:     1,
			Template: tplName,
			Vars:     newVars,
		})
	}

	return calls
}

func buildTemplateVars(
	varDefs map[string]string,
	typeMap map[string]*types.TypeName,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
) []TemplateVar {
	vars := make([]TemplateVar, 0, len(varDefs))
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

		if strings.HasPrefix(baseTypeStr, "map[") {
			if idx := strings.Index(baseTypeStr, "]"); idx != -1 {
				tv.IsMap = true
				tv.KeyType = strings.TrimSpace(baseTypeStr[4:idx])
				tv.ElemType = strings.TrimSpace(baseTypeStr[idx+1:])

				valLookup := strings.TrimLeft(tv.ElemType, "*")
				if typeNameObj, ok := typeMap[valLookup]; ok {
					seen := make(map[structKeyHandle]bool)
					tv.Fields, tv.Doc = extractFieldsWithDocs(typeNameObj.Type(), structIndex, fc, seen, fset)
				}
				vars = append(vars, tv)
				continue
			}
		}

		if typeNameObj, ok := typeMap[baseTypeStr]; ok {
			t := typeNameObj.Type()
			seen := make(map[structKeyHandle]bool)
			tv.Fields, tv.Doc = extractFieldsWithDocs(t, structIndex, fc, seen, fset)

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
		} else if isSlice {
			tv.IsSlice = true
			tv.ElemType = baseTypeStr
		}
		vars = append(vars, tv)
	}
	return vars
}

// ── Struct index ─────────────────────────────────────────────────────────────

type structIndexEntry struct {
	doc    string
	fields map[string]fieldInfo
}

// buildStructIndex walks all AST files concurrently. Workers write directly
// into a sync.Map — no per-worker accumulation map, no post-merge copy.
func buildStructIndex(fset *token.FileSet, files map[string]*ast.File) map[structKeyHandle]structIndexEntry {
	numWorkers := max(runtime.NumCPU(), 1)
	fileChan := make(chan *ast.File, len(files))

	var sharedIndex sync.Map // map[structKeyHandle]structIndexEntry
	var wg sync.WaitGroup

	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileChan {
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
							fields: make(map[string]fieldInfo, len(structType.Fields.List)),
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

						key := unique.Make(pkgName + "." + typeSpec.Name.Name)
						sharedIndex.Store(key, entry)
					}
					return true
				})
			}
		}()
	}

	for _, f := range files {
		fileChan <- f
	}
	close(fileChan)
	wg.Wait()

	// Convert to a plain map for fast O(1) reads in the hot analysis path.
	finalIndex := make(map[structKeyHandle]structIndexEntry, len(files)*4)
	sharedIndex.Range(func(k, v any) bool {
		finalIndex[k.(structKeyHandle)] = v.(structIndexEntry)
		return true
	})
	return finalIndex
}

// ── Field extraction with caching ────────────────────────────────────────────

// extractFieldsWithDocs recursively extracts exported fields from a named
// struct type. Results are cached by structKeyHandle so each unique type is
// processed exactly once per AnalyzeDir call.
//
// The seen map prevents infinite loops on self-referential types; callers
// should pass a fresh map per extraction root.
func extractFieldsWithDocs(
	t types.Type,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
	fset *token.FileSet,
) ([]FieldInfo, string) {
	if t == nil {
		return nil, ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return extractFieldsWithDocs(ptr.Elem(), structIndex, fc, seen, fset)
	}
	if mapType, ok := t.(*types.Map); ok {
		return extractFieldsWithDocs(mapType.Elem(), structIndex, fc, seen, fset)
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil, ""
	}

	handle := makeStructKey(named)

	// Cycle guard.
	if seen[handle] {
		return nil, ""
	}
	seen[handle] = true

	// ── Cache hit ────────────────────────────────────────────────────────────
	if cached, ok := fc.get(handle); ok {
		return cached.fields, cached.doc
	}

	// ── Cache miss: compute ──────────────────────────────────────────────────
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		// Interface or other named type: expose exported methods only.
		var fields []FieldInfo
		for m := range named.Methods() {
			if !m.Exported() {
				continue
			}
			fi := FieldInfo{Name: m.Name(), TypeStr: normalizeTypeStr(m.Type().String())}
			if pos := m.Pos(); pos.IsValid() && fset != nil {
				position := fset.Position(pos)
				fi.DefFile = position.Filename
				fi.DefLine = position.Line
				fi.DefCol = position.Column
			}
			fields = append(fields, fi)
		}
		// Don't cache interface results as they don't have struct index entries.
		return fields, ""
	}

	entry := structIndex[handle]
	fields := make([]FieldInfo, 0, strct.NumFields())

	for f := range strct.Fields() {
		f := f
		if !f.Exported() {
			continue
		}

		fi := FieldInfo{
			Name:    f.Name(),
			TypeStr: normalizeTypeStr(f.Type().String()),
		}

		if pos := f.Pos(); pos.IsValid() && fset != nil {
			position := fset.Position(pos)
			fi.DefFile = position.Filename
			fi.DefLine = position.Line
			fi.DefCol = position.Column
		}

		ft := f.Type()
		if ptr, ok2 := ft.(*types.Pointer); ok2 {
			ft = ptr.Elem()
		}

		if slice, ok2 := ft.(*types.Slice); ok2 {
			fi.IsSlice = true
			// Copy seen so sibling slices of the same type don't suppress each other.
			elemSeen := copySeenMap(seen)
			fi.Fields, _ = extractFieldsWithDocs(slice.Elem(), structIndex, fc, elemSeen, fset)
		} else if keyType, elemType := getMapTypes(ft); keyType != nil && elemType != nil {
			fi.IsMap = true
			fi.KeyType = normalizeTypeStr(keyType.String())
			fi.ElemType = normalizeTypeStr(elemType.String())
			elemSeen := copySeenMap(seen)
			fi.Fields, _ = extractFieldsWithDocs(elemType, structIndex, fc, elemSeen, fset)
		} else {
			// Recurse using the shared seen map (cycle detection across the path).
			fi.Fields, _ = extractFieldsWithDocs(ft, structIndex, fc, seen, fset)
		}

		if pos, ok2 := entry.fields[f.Name()]; ok2 {
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
	for m := range named.Methods() {
		if !m.Exported() {
			continue
		}
		fi := FieldInfo{Name: m.Name(), TypeStr: "method"}
		if pos := m.Pos(); pos.IsValid() && fset != nil {
			position := fset.Position(pos)
			fi.DefFile = position.Filename
			fi.DefLine = position.Line
			fi.DefCol = position.Column
		}
		fields = append(fields, fi)
	}

	result := cachedFields{fields: fields, doc: entry.doc}
	fc.set(handle, result)
	return fields, entry.doc
}

// copySeenMap creates a shallow copy of a seen map for independent traversal
// branches (e.g., sibling slice fields of the same element type).
func copySeenMap(src map[structKeyHandle]bool) map[structKeyHandle]bool {
	dst := make(map[structKeyHandle]bool, len(src))
	maps.Copy(dst, src)
	return dst
}

// ── extractMapVars ────────────────────────────────────────────────────────────

func extractMapVars(
	expr ast.Expr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
) []TemplateVar {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}

	vars := make([]TemplateVar, 0, len(comp.Elts))
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

			seen := make(map[structKeyHandle]bool)
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				elemSeen := make(map[structKeyHandle]bool)
				tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, elemSeen, fset, tv.Doc)
			} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
				tv.IsMap = true
				tv.KeyType = normalizeTypeStr(keyType.String())
				tv.ElemType = normalizeTypeStr(elemType.String())
				elemSeen := make(map[structKeyHandle]bool)
				tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, elemSeen, fset, tv.Doc)
			}
		} else {
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)
		vars = append(vars, tv)
	}
	return vars
}

// ── Misc helpers ──────────────────────────────────────────────────────────────

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
