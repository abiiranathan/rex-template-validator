package ast

import (
	"encoding/json"
	"go/token"
	"go/types"
	"log"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"
)

// enrichRenderCallsWithContext augments RenderCall entries with variables
// defined in an external JSON context file.
func enrichRenderCallsWithContext(
	calls []RenderCall,
	contextFile string,
	pkgs []*packages.Package,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	config AnalysisConfig,
	seenPool *seenMapPool,
) []RenderCall {
	data, err := os.ReadFile(contextFile)
	if err != nil {
		log.Fatalf("context file not found: %v", contextFile)
	}

	var contextConfig map[string]map[string]string
	if err := json.Unmarshal(data, &contextConfig); err != nil {
		log.Fatalf("error parsing context file json: %v: %v", contextFile, err)
	}

	typeMap := buildTypeMap(pkgs)

	globalVars := buildTemplateVarsOptimized(
		contextConfig[config.GlobalTemplateName],
		typeMap,
		structIndex,
		fc,
		fset,
		seenPool,
	)

	seenTpls := make(map[string]bool, len(calls))
	calls = enrichExistingCalls(calls, contextConfig, globalVars, typeMap, structIndex, fc, fset, seenPool, seenTpls)
	calls = addSyntheticCalls(calls, contextConfig, globalVars, typeMap, structIndex, fc, fset, config, seenPool, seenTpls)

	return calls
}

// isStdlibPkg reports whether a package ID looks like a standard library package
// (no dot in the path) and should be skipped for type map building.
//
// OPTIMISATION: The original buildTypeMap did a full BFS over the entire import
// graph including all of the Go standard library.  For a typical application
// this can be thousands of packages.  Since templates never reference stdlib
// types directly (they reference application types that may *embed* stdlib types),
// we skip any package whose import path contains no dot — that is a reliable
// heuristic for stdlib vs third-party/application packages.
func isStdlibPkg(pkgPath string) bool {
	// Standard library packages have no dot in their path (e.g. "fmt", "net/http").
	// Third-party packages always have a dot (e.g. "github.com/...", "golang.org/...").
	// Application packages also have a dot via their module path.
	if strings.Contains(pkgPath, ".") {
		return false
	}
	// Cover "C" pseudo-package and empty.
	return true
}

// buildTypeMap creates a lookup map from type names to TypeName objects by
// traversing the package import graph via BFS.
//
// OPTIMISATION: Stdlib packages are skipped entirely.  For a medium-sized
// application this reduces the BFS from ~2000 packages to ~50–200.
func buildTypeMap(pkgs []*packages.Package) map[string]*types.TypeName {
	typeMap := make(map[string]*types.TypeName, len(pkgs)*32)
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

		// Skip stdlib packages — templates never reference them by short name.
		if isStdlibPkg(p.PkgPath) {
			// Still enqueue their imports only if they are application packages
			// themselves (skip stdlib imports of stdlib).
			continue
		}

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
				if !isStdlibPkg(imp.PkgPath) {
					queue = append(queue, imp)
				}
			}
		}
	}

	return typeMap
}

// enrichExistingCalls adds context-defined variables to existing render calls.
func enrichExistingCalls(
	calls []RenderCall,
	contextConfig map[string]map[string]string,
	globalVars []TemplateVar,
	typeMap map[string]*types.TypeName,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	seenPool *seenMapPool,
	seenTpls map[string]bool,
) []RenderCall {
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

	return calls
}

// addSyntheticCalls creates RenderCall entries for templates defined in
// context but not found in the codebase.
func addSyntheticCalls(
	calls []RenderCall,
	contextConfig map[string]map[string]string,
	globalVars []TemplateVar,
	typeMap map[string]*types.TypeName,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	config AnalysisConfig,
	seenPool *seenMapPool,
	seenTpls map[string]bool,
) []RenderCall {
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

// buildTemplateVarsOptimized constructs TemplateVar entries from type string
// definitions in the context file.
func buildTemplateVarsOptimized(
	varDefs map[string]string,
	typeMap map[string]*types.TypeName,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	fset *token.FileSet,
	seenPool *seenMapPool,
) []TemplateVar {
	vars := make([]TemplateVar, 0, len(varDefs))

	for name, typeStr := range varDefs {
		tv := TemplateVar{Name: name, TypeStr: typeStr}

		baseTypeStr, isSlice := parseTypeString(typeStr)

		if strings.HasPrefix(baseTypeStr, "map[") {
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
