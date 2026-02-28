package ast

import (
	"encoding/json"
	"go/token"
	"go/types"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"
)

// enrichRenderCallsWithContext augments RenderCall entries with variables
// defined in an external JSON context file. Also creates synthetic entries
// for templates defined in context but not found in code.
//
// Context file format:
//
//	{
//	  "template1.html": {"user": "User", "posts": "[]Post"},
//	  "template2.html": {"config": "Config"}
//	}
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
	// Load context file
	data, err := os.ReadFile(contextFile)
	if err != nil {
		return calls
	}

	var contextConfig map[string]map[string]string
	if err := json.Unmarshal(data, &contextConfig); err != nil {
		return calls
	}

	// Build type map from all packages
	typeMap := buildTypeMap(pkgs)

	// Build global variables
	globalVars := buildTemplateVarsOptimized(
		contextConfig[config.GlobalTemplateName],
		typeMap,
		structIndex,
		fc,
		fset,
		seenPool,
	)

	// Enrich existing calls
	seenTpls := make(map[string]bool, len(calls))
	calls = enrichExistingCalls(calls, contextConfig, globalVars, typeMap, structIndex, fc, fset, seenPool, seenTpls)

	// Add synthetic calls for templates in context but not in code
	calls = addSyntheticCalls(calls, contextConfig, globalVars, typeMap, structIndex, fc, fset, config, seenPool, seenTpls)

	return calls
}

// buildTypeMap creates a lookup map from type names to TypeName objects
// by traversing the package import graph via BFS.
func buildTypeMap(pkgs []*packages.Package) map[string]*types.TypeName {
	typeMap := make(map[string]*types.TypeName, len(pkgs)*32)
	visited := make(map[string]bool, len(pkgs)*32)
	queue := make([]*packages.Package, 0, len(pkgs)*8)

	// Initialize with root packages
	for _, pkg := range pkgs {
		if !visited[pkg.ID] {
			visited[pkg.ID] = true
			queue = append(queue, pkg)
		}
	}

	// BFS traversal
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]

		// Extract types from package
		if p.Types != nil {
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				if typeName, ok := scope.Lookup(name).(*types.TypeName); ok {
					typeMap[p.Types.Name()+"."+name] = typeName
				}
			}
		}

		// Add imports to queue
		for _, imp := range p.Imports {
			if !visited[imp.ID] {
				visited[imp.ID] = true
				queue = append(queue, imp)
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

		// Combine global + template-specific + code-extracted variables
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
		// Skip global template and already-seen templates
		if tplName == config.GlobalTemplateName || seenTpls[tplName] {
			continue
		}

		// Build combined variables
		newVars := make([]TemplateVar, 0, len(globalVars)+len(tplVars))
		newVars = append(newVars, globalVars...)
		newVars = append(newVars, buildTemplateVarsOptimized(tplVars, typeMap, structIndex, fc, fset, seenPool)...)

		// Create synthetic entry
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
// definitions in the context file. Resolves types via typeMap and extracts
// full field information.
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

		// Parse type string to identify base type
		baseTypeStr, isSlice := parseTypeString(typeStr)

		// Handle map types
		if strings.HasPrefix(baseTypeStr, "map[") {
			if idx := strings.IndexByte(baseTypeStr, ']'); idx != -1 {
				tv.IsMap = true
				tv.KeyType = strings.TrimSpace(baseTypeStr[4:idx])
				tv.ElemType = strings.TrimSpace(baseTypeStr[idx+1:])

				// Resolve element type fields
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

		// Resolve named type
		if typeNameObj, ok := typeMap[baseTypeStr]; ok {
			t := typeNameObj.Type()
			seen := seenPool.get()
			tv.Fields, tv.Doc = extractFieldsWithDocs(t, structIndex, fc, seen, fset)
			seenPool.put(seen)

			// Set definition location
			if pos := typeNameObj.Pos(); pos.IsValid() && fset != nil {
				position := fset.Position(pos)
				tv.DefFile = position.Filename
				tv.DefLine = position.Line
				tv.DefCol = position.Column
			}

			// Mark as slice if original type string indicated it
			if isSlice {
				tv.IsSlice = true
				tv.ElemType = baseTypeStr
			}
		} else if isSlice {
			// Unknown slice type
			tv.IsSlice = true
			tv.ElemType = baseTypeStr
		}

		vars = append(vars, tv)
	}

	return vars
}
