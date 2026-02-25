package validator

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
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

// ── Interned key type ──────────────────────────────────────────────────────
type structKeyHandle = unique.Handle[string]

type structIndexEntry struct {
	doc    string
	fields map[string]fieldInfo
}

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

// ── Field cache ────────────────────────────────────────────────────────────
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

// ── Optimized seen map pool ────────────────────────────────────────────────
// Reuse seen maps instead of allocating new ones
type seenMapPool struct {
	pool sync.Pool
}

func newSeenMapPool() *seenMapPool {
	return &seenMapPool{
		pool: sync.Pool{
			New: func() interface{} {
				return make(map[structKeyHandle]bool, 16)
			},
		},
	}
}

func (smp *seenMapPool) get() map[structKeyHandle]bool {
	m := smp.pool.Get().(map[structKeyHandle]bool)
	// Clear the map
	for k := range m {
		delete(m, k)
	}
	return m
}

func (smp *seenMapPool) put(m map[structKeyHandle]bool) {
	smp.pool.Put(m)
}

// ── Helpers ────────────────────────────────────────────────────────────────
func getExprColumnRange(fset *token.FileSet, expr ast.Expr) (startCol, endCol int) {
	pos := fset.Position(expr.Pos())
	endPos := fset.Position(expr.End())
	return pos.Column, endPos.Column
}

func AnalyzeDir(dir string, contextFile string, config AnalysisConfig) AnalysisResult {
	result := AnalysisResult{}
	fset := token.NewFileSet()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
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

	// ── Merge type info ────────────────────────────────────────────────────
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

	// ── Build file map ─────────────────────────────────────────────────────
	filesMap := make(map[string]*ast.File, len(allFiles))
	for _, f := range allFiles {
		if pos := fset.File(f.Pos()); pos != nil {
			filesMap[pos.Name()] = f
		}
	}

	fc := newFieldCache()
	seenPool := newSeenMapPool()

	// 1. Build struct index concurrently
	structIndex := buildStructIndex(fset, filesMap)

	// 2. Collect scopes with seen map pooling
	scopes := collectFuncScopesOptimized(allFiles, info, fset, structIndex, fc, config, filesMap, seenPool)

	// ── Identify global implicit vars ──────────────────────────────────────
	var globalImplicitVars []TemplateVar
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 && len(scope.SetVars) > 0 {
			globalImplicitVars = append(globalImplicitVars, scope.SetVars...)
		}
	}

	// ── Pre-count render calls ─────────────────────────────────────────────
	totalRenders := 0
	totalFuncMaps := 0
	for _, scope := range scopes {
		totalRenders += len(scope.RenderNodes)
		totalFuncMaps += len(scope.FuncMaps)
	}

	result.RenderCalls = make([]RenderCall, 0, totalRenders)
	result.FuncMaps = make([]FuncMapInfo, 0, totalFuncMaps)

	// ── Aggregate FuncMaps ─────────────────────────────────────────────────
	for _, scope := range scopes {
		result.FuncMaps = append(result.FuncMaps, scope.FuncMaps...)
	}

	// ── Generate render calls ──────────────────────────────────────────────
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 {
			continue
		}

		for _, rr := range scope.RenderNodes {
			call := rr.Node
			templateArgIdx := rr.TemplateArgIdx

			if len(rr.TemplateNames) == 0 {
				continue
			}

			if templateArgIdx < 0 || templateArgIdx >= len(call.Args) {
				continue
			}

			templatePathExpr := call.Args[templateArgIdx]
			tplNameStartCol, tplNameEndCol := getExprColumnRange(fset, templatePathExpr)

			if lit, ok := templatePathExpr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				tplNameStartCol++
				tplNameEndCol--
			}

			for _, templatePath := range rr.TemplateNames {
				if templatePath == "" {
					continue
				}

				dataArgIdx := templateArgIdx + 1
				var vars []TemplateVar
				if dataArgIdx < len(call.Args) {
					seen := seenPool.get()
					vars = extractMapVars(call.Args[dataArgIdx], info, fset, structIndex, fc, seen)
					seenPool.put(seen)
				}

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
	}

	// ── Deduplicate FuncMaps ───────────────────────────────────────────────
	seenFuncMaps := make(map[string]bool, len(result.FuncMaps))
	uniqueFuncMaps := make([]FuncMapInfo, 0, len(result.FuncMaps))
	for _, fm := range result.FuncMaps {
		if !seenFuncMaps[fm.Name] {
			seenFuncMaps[fm.Name] = true
			uniqueFuncMaps = append(uniqueFuncMaps, fm)
		}
	}
	result.FuncMaps = uniqueFuncMaps

	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(result.RenderCalls, contextFile, pkgs, structIndex, fc, fset, config, seenPool)
	}

	return result
}

type FuncScope struct {
	SetVars     []TemplateVar
	RenderNodes []ResolvedRender
	FuncMaps    []FuncMapInfo
}

type ResolvedRender struct {
	Node           *ast.CallExpr
	TemplateNames  []string
	TemplateArgIdx int
}

type funcWorkUnit struct {
	node ast.Node
}

// collectFuncScopesOptimized with seen map pooling
func collectFuncScopesOptimized(
	files []*ast.File,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
	seenPool *seenMapPool,
) []FuncScope {
	// Phase 1: collect function nodes
	funcNodes := make([]funcWorkUnit, 0, len(files)*8)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				funcNodes = append(funcNodes, funcWorkUnit{node: node})
			case *ast.GenDecl:
				if node.Tok == token.VAR || node.Tok == token.CONST {
					funcNodes = append(funcNodes, funcWorkUnit{node: node})
				}
			}
			return true
		})
	}

	if len(funcNodes) == 0 {
		return nil
	}

	// Phase 2: process concurrently with better work distribution
	numWorkers := max(runtime.NumCPU(), 1)
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
				scope := processFuncOptimized(unit.node, info, fset, structIndex, fc, config, filesMap, seenPool)
				if len(scope.RenderNodes) > 0 || len(scope.SetVars) > 0 || len(scope.FuncMaps) > 0 {
					localScopes = append(localScopes, scope)
				}
			}

			sliceResultChan <- localScopes
		}(chunk)
	}

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

// processFuncOptimized with seen map pooling
func processFuncOptimized(
	n ast.Node,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*ast.File,
	seenPool *seenMapPool,
) FuncScope {
	var scope FuncScope

	// Pre-allocate with reasonable capacity
	stringAssignments := make(map[string][]string, 8)
	funcMapAssignments := make(map[string]*ast.CompositeLit, 4)

	// Phase 1: Collect assignments (unchanged but with better allocation)
	ast.Inspect(n, func(child ast.Node) bool {
		if child != n {
			if _, isFunc := child.(*ast.FuncLit); isFunc {
				return false
			}
		}

		if assign, ok := child.(*ast.AssignStmt); ok {
			for i, lhs := range assign.Lhs {
				// Handle map assignment
				if indexExpr, ok := lhs.(*ast.IndexExpr); ok {
					if info != nil {
						if tv, ok := info.Types[indexExpr.X]; ok && tv.Type != nil {
							if strings.HasSuffix(tv.Type.String(), "template.FuncMap") {
								if keyLit, ok := indexExpr.Index.(*ast.BasicLit); ok && keyLit.Kind == token.STRING {
									name := strings.Trim(keyLit.Value, "\"")
									fInfo := FuncMapInfo{Name: name}
									if i < len(assign.Rhs) {
										rhs := assign.Rhs[i]
										fInfo.DefFile, fInfo.DefLine, fInfo.DefCol = resolveFuncDefLocation(rhs, info, fset)
										if rtv, ok := info.Types[rhs]; ok && rtv.Type != nil {
											underlying := rtv.Type
											if ptr, ok2 := underlying.(*types.Pointer); ok2 {
												underlying = ptr.Elem()
											}
											if sig, ok2 := underlying.(*types.Signature); ok2 {
												fInfo.Params, fInfo.Returns, fInfo.Args = extractSignatureInfo(sig)
											}
										}
									}
									scope.FuncMaps = append(scope.FuncMaps, fInfo)
								}
							}
						}
					}
				}

				ident, ok := lhs.(*ast.Ident)
				if !ok || i >= len(assign.Rhs) {
					continue
				}

				rhs := assign.Rhs[i]
				if s := extractStringFast(rhs); s != "" {
					stringAssignments[ident.Name] = append(stringAssignments[ident.Name], s)
				}

				if comp, ok := rhs.(*ast.CompositeLit); ok {
					funcMapAssignments[ident.Name] = comp
					if info != nil {
						if tv, ok := info.Types[ident]; ok && tv.Type != nil {
							if strings.HasSuffix(tv.Type.String(), "template.FuncMap") {
								scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
							}
						}
					}
				}
			}
		}

		if decl, ok := child.(*ast.GenDecl); ok && (decl.Tok == token.VAR || decl.Tok == token.CONST) {
			for _, spec := range decl.Specs {
				vspec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vspec.Names {
					if i >= len(vspec.Values) {
						continue
					}
					rhs := vspec.Values[i]
					if s := extractStringFast(rhs); s != "" {
						stringAssignments[name.Name] = append(stringAssignments[name.Name], s)
					}
					if comp, ok := rhs.(*ast.CompositeLit); ok {
						funcMapAssignments[name.Name] = comp
						if info != nil {
							if tv, ok := info.Defs[name]; ok && tv.Type() != nil {
								if strings.HasSuffix(tv.Type().String(), "template.FuncMap") {
									scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
								}
							}
						}
					}
				}
			}
		}
		return true
	})

	// Phase 2: Find render and FuncMap calls
	ast.Inspect(n, func(child ast.Node) bool {
		if child != n {
			if _, isFunc := child.(*ast.FuncLit); isFunc {
				return false
			}
		}

		if comp, ok := child.(*ast.CompositeLit); ok {
			if info != nil {
				if tv, ok := info.Types[comp]; ok && tv.Type != nil {
					if strings.HasSuffix(tv.Type.String(), "template.FuncMap") {
						scope.FuncMaps = append(scope.FuncMaps, extractFuncMaps(comp, info, fset, filesMap)...)
					}
				}
			}
		}

		call, ok := child.(*ast.CallExpr)
		if !ok {
			return true
		}

		if isRenderCall(call, config) {
			resolved := ResolvedRender{Node: call, TemplateArgIdx: -1}
			templateArgIdx := -1

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
					if ident, ok := arg.(*ast.Ident); ok {
						if _, ok := stringAssignments[ident.Name]; ok {
							templateArgIdx = i
							break
						}
					}
				}
			}

			if templateArgIdx >= 0 && templateArgIdx < len(call.Args) {
				resolved.TemplateArgIdx = templateArgIdx
				arg := call.Args[templateArgIdx]

				if s := extractStringFast(arg); s != "" {
					resolved.TemplateNames = []string{s}
				} else if ident, ok := arg.(*ast.Ident); ok {
					if info != nil {
						if obj := info.ObjectOf(ident); obj != nil {
							if c, ok := obj.(*types.Const); ok {
								val := c.Val()
								if val.Kind() == constant.String {
									resolved.TemplateNames = []string{constant.StringVal(val)}
								}
							}
						}
					}
					if len(resolved.TemplateNames) == 0 {
						if vals, ok := stringAssignments[ident.Name]; ok {
							resolved.TemplateNames = vals
						}
					}
				}
			}

			scope.RenderNodes = append(scope.RenderNodes, resolved)
		}

		if setVar := extractSetCallVarOptimized(call, info, fset, structIndex, fc, config, seenPool); setVar != nil {
			scope.SetVars = append(scope.SetVars, *setVar)
		}

		return true
	})

	return scope
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

	// Second pass: attach method docs to the correct structIndexEntry
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			funcDecl, ok := n.(*ast.FuncDecl)
			if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
				return true
			}

			// Extract receiver type name
			recvType := funcDecl.Recv.List[0].Type
			if starExpr, ok := recvType.(*ast.StarExpr); ok {
				recvType = starExpr.X
			}

			if ident, ok := recvType.(*ast.Ident); ok {
				pkgName := f.Name.Name
				key := unique.Make(pkgName + "." + ident.Name)

				if entry, exists := finalIndex[key]; exists {
					methodName := funcDecl.Name.Name
					doc := ""
					if funcDecl.Doc != nil {
						doc = funcDecl.Doc.Text()
					}
					pos := fset.Position(funcDecl.Pos())

					// Ensure we only update if there's a doc string to avoid overwriting existing fields accidentally
					if doc != "" {
						entry.fields[methodName] = fieldInfo{
							file: pos.Filename,
							line: pos.Line,
							col:  pos.Column,
							doc:  doc,
						}
					}
				}
			}
			return true
		})
	}

	return finalIndex
}

// extractStringFast - optimized version with fewer allocations
func extractStringFast(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	if len(lit.Value) < 2 {
		return ""
	}
	return lit.Value[1 : len(lit.Value)-1]
}

// extractSetCallVarOptimized with seen map pooling
func extractSetCallVarOptimized(
	call *ast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	seenPool *seenMapPool,
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

	key := extractStringFast(call.Args[0])
	if key == "" {
		return nil
	}

	valArg := call.Args[1]
	tv := TemplateVar{Name: key}

	if typeInfo, ok := info.Types[valArg]; ok && typeInfo.Type != nil {
		tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())
		seen := seenPool.get()
		tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

		if elemType := getElementType(typeInfo.Type); elemType != nil {
			tv.IsSlice = true
			tv.ElemType = normalizeTypeStr(elemType.String())
			// Reuse the same seen map
			for skh := range seen {
				delete(seen, skh)
			}
			tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
		} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
			tv.IsMap = true
			tv.KeyType = normalizeTypeStr(keyType.String())
			tv.ElemType = normalizeTypeStr(elemType.String())
			// Reuse the same seen map
			for key := range seen {
				delete(seen, key)
			}
			tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
		}
		seenPool.put(seen)
	} else {
		tv.TypeStr = inferTypeFromAST(valArg)
	}

	tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(valArg, info, fset)

	return &tv
}

// ... (rest of the functions remain the same but use optimized helpers)

// enrichRenderCallsWithContext with seen map pooling
func enrichRenderCallsWithContext(
	calls []RenderCall,
	contextFile string,
	pkgs []*packages.Package,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	config AnalysisConfig,
	seenPool *seenMapPool,
) []RenderCall {
	data, err := os.ReadFile(contextFile)
	if err != nil {
		return calls
	}

	var contextConfig map[string]map[string]string
	if err := json.Unmarshal(data, &contextConfig); err != nil {
		return calls
	}

	// Sequential BFS (same as before)
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

	globalVars := buildTemplateVarsOptimized(contextConfig[config.GlobalTemplateName], typeMap, structIndex, fc, fset, seenPool)

	seenTpls := make(map[string]bool, len(calls))
	for i, call := range calls {
		seenTpls[call.Template] = true

		base := make([]TemplateVar, 0, len(globalVars)+len(call.Vars)+8)
		base = append(base, globalVars...)

		if tplVars, ok := contextConfig[call.Template]; ok {
			base = append(base, buildTemplateVarsOptimized(tplVars, typeMap, structIndex, fc, fset, seenPool)...)
		}

		base = append(base, call.Vars...)
		calls[i].Vars = base
	}

	// Synthetic render calls
	for tplName, tplVars := range contextConfig {
		if tplName == config.GlobalTemplateName || seenTpls[tplName] {
			continue
		}

		newVars := make([]TemplateVar, 0, len(globalVars)+len(tplVars))
		newVars = append(newVars, globalVars...)
		newVars = append(newVars, buildTemplateVarsOptimized(tplVars, typeMap, structIndex, fc, fset, seenPool)...)

		calls = append(calls, RenderCall{
			File:     "context-file",
			Line:     1,
			Template: tplName,
			Vars:     newVars,
		})
	}

	return calls
}

// buildTemplateVarsOptimized with seen map pooling
func buildTemplateVarsOptimized(
	varDefs map[string]string,
	typeMap map[string]*types.TypeName,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	seenPool *seenMapPool,
) []TemplateVar {
	vars := make([]TemplateVar, 0, len(varDefs))

	for name, typeStr := range varDefs {
		tv := TemplateVar{Name: name, TypeStr: typeStr}
		baseTypeStr := typeStr
		isSlice := false

		// Faster prefix stripping
		for {
			if len(baseTypeStr) >= 2 && baseTypeStr[0] == '[' && baseTypeStr[1] == ']' {
				isSlice = true
				baseTypeStr = baseTypeStr[2:]
			} else if len(baseTypeStr) >= 1 && baseTypeStr[0] == '*' {
				baseTypeStr = baseTypeStr[1:]
			} else {
				break
			}
		}

		if len(baseTypeStr) >= 4 && baseTypeStr[:4] == "map[" {
			if idx := strings.IndexByte(baseTypeStr, ']'); idx != -1 {
				tv.IsMap = true
				tv.KeyType = strings.TrimSpace(baseTypeStr[4:idx])
				tv.ElemType = strings.TrimSpace(baseTypeStr[idx+1:])

				valLookup := strings.TrimLeft(tv.ElemType, "*")
				if typeNameObj, ok := typeMap[valLookup]; ok {
					seen := seenPool.get()
					tv.Fields, tv.Doc = extractFieldsWithDocs(typeNameObj.Type(), structIndex, fc, seen, fset)
					seenPool.put(seen)
				}

				vars = append(vars, tv)
				continue
			}
		}

		if typeNameObj, ok := typeMap[baseTypeStr]; ok {
			t := typeNameObj.Type()
			seen := seenPool.get()
			tv.Fields, tv.Doc = extractFieldsWithDocs(t, structIndex, fc, seen, fset)
			seenPool.put(seen)

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
		if sig, ok := m.Type().(*types.Signature); ok {
			fi.Params, fi.Returns, _ = extractSignatureInfo(sig)
		}
		if pos := m.Pos(); pos.IsValid() && fset != nil {
			position := fset.Position(pos)
			fi.DefFile = position.Filename
			fi.DefLine = position.Line
			fi.DefCol = position.Column
		}

		if pos, ok2 := entry.fields[m.Name()]; ok2 {
			if fi.DefFile == "" {
				fi.DefFile = pos.file
				fi.DefLine = pos.line
				fi.DefCol = pos.col
			}
			fi.Doc = pos.doc
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

// extractMapVars with seen map passed as parameter
func extractMapVars(
	expr ast.Expr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[structKeyHandle]structIndexEntry,
	fc *fieldCache,
	seen map[structKeyHandle]bool,
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
			// Clear the map for reuse.
			// For each variable, we need a new map.
			for key := range seen {
				delete(seen, key)
			}
			tv.TypeStr = normalizeTypeStr(typeInfo.Type.String())
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType.String())
				tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
				tv.IsMap = true
				tv.KeyType = normalizeTypeStr(keyType.String())
				tv.ElemType = normalizeTypeStr(elemType.String())
				tv.Fields, tv.Doc = extractFieldsWithDocsDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
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

// resolveFuncDefLocation returns the best available definition position for a
// function value expression. For named references (ident or selector) it
// resolves through TypesInfo to the actual declaration site; for literals it
// falls back to the expression's own position.
func resolveFuncDefLocation(expr ast.Expr, info *types.Info, fset *token.FileSet) (file string, line, col int) {
	if fset == nil {
		return
	}
	switch e := expr.(type) {
	case *ast.Ident:
		if info != nil {
			if obj := info.ObjectOf(e); obj != nil && obj.Pos().IsValid() {
				pos := fset.Position(obj.Pos())
				return pos.Filename, pos.Line, pos.Column
			}
		}
	case *ast.SelectorExpr:
		if info != nil {
			if obj := info.ObjectOf(e.Sel); obj != nil && obj.Pos().IsValid() {
				pos := fset.Position(obj.Pos())
				return pos.Filename, pos.Line, pos.Column
			}
		}
	}
	pos := fset.Position(expr.Pos())
	return pos.Filename, pos.Line, pos.Column
}

// resolveFuncDoc attempts to extract the doc comment for a function value
// expression. For named references (bare ident or pkg.Func selector) it finds
// the *ast.FuncDecl in the parsed file set and returns its doc text. Anonymous
// function literals carry no doc comment, so the empty string is returned.
func resolveFuncDoc(expr ast.Expr, info *types.Info, filesMap map[string]*ast.File) string {
	if info == nil {
		return ""
	}

	var obj types.Object
	switch e := expr.(type) {
	case *ast.Ident:
		obj = info.ObjectOf(e)
	case *ast.SelectorExpr:
		obj = info.ObjectOf(e.Sel)
	default:
		return ""
	}

	if obj == nil || !obj.Pos().IsValid() {
		return ""
	}

	for _, file := range filesMap {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Name.Obj != nil && fd.Name.Obj.Pos() == obj.Pos() {
				if fd.Doc != nil {
					return strings.TrimSpace(fd.Doc.Text())
				}
				return ""
			}
		}
	}
	return ""
}

func extractFuncMaps(comp *ast.CompositeLit, info *types.Info, fset *token.FileSet, filesMap map[string]*ast.File) []FuncMapInfo {
	var result []FuncMapInfo
	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}

		name := strings.Trim(key.Value, "\"")
		fInfo := FuncMapInfo{Name: name}

		fInfo.DefFile, fInfo.DefLine, fInfo.DefCol = resolveFuncDefLocation(kv.Value, info, fset)
		fInfo.Doc = resolveFuncDoc(kv.Value, info, filesMap)

		if info != nil {
			if tv, ok := info.Types[kv.Value]; ok && tv.Type != nil {
				underlying := tv.Type
				if ptr, ok2 := underlying.(*types.Pointer); ok2 {
					underlying = ptr.Elem()
				}
				if sig, ok2 := underlying.(*types.Signature); ok2 {
					fInfo.Params, fInfo.Returns, fInfo.Args = extractSignatureInfo(sig)
				}
			}
		}
		result = append(result, fInfo)
	}
	return result
}

// extractSignatureInfo extracts parameter names, types, and return types from
// a *types.Signature. Names are preserved when present; unnamed params get an
// empty Name field. The legacy Args []string slice is also populated for
// backward compatibility.
func extractSignatureInfo(sig *types.Signature) (params, returns []ParamInfo, args []string) {
	params = make([]ParamInfo, sig.Params().Len())
	args = make([]string, sig.Params().Len())
	for i := range sig.Params().Len() {
		p := sig.Params().At(i)
		ts := normalizeTypeStr(p.Type().String())
		params[i] = ParamInfo{Name: p.Name(), TypeStr: ts}
		args[i] = ts
	}
	returns = make([]ParamInfo, sig.Results().Len())
	for i := range sig.Results().Len() {
		r := sig.Results().At(i)
		ts := normalizeTypeStr(r.Type().String())
		returns[i] = ParamInfo{Name: r.Name(), TypeStr: ts}
	}
	return
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
