package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"maps"
	"strings"
)

// MaxFieldDepth is the maximum depth for field extraction to prevent excessive recursion
// Most templates don't access fields deeper than 4 levels
// But we use default 10 to handle large projects.
const MaxFieldDepth = 10

// extractFieldsWithDocs recursively extracts exported fields and methods from
// a type, leveraging caching to avoid redundant work. The seen map prevents
// infinite recursion on self-referential types.
//
// Caching strategy:
// - Each unique struct type is processed exactly once
// - Results are cached by the full type string (including type arguments)
// - Subsequent requests hit the cache
//
// Recursion handling:
// - seen map tracks types in current path
// - Prevents infinite loops on cyclic types
// - Copies for independent branches (slice elements)
// - Depth limit prevents excessive recursion on deeply nested types
func extractFieldsWithDocs(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
) ([]FieldInfo, string) {
	return extractFieldsWithDocsDepth(t, structIndex, fc, seen, fset, 0)
}

// extractFieldsWithDocsDepth is the internal version with depth tracking
func extractFieldsWithDocsDepth(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) ([]FieldInfo, string) {
	// Early termination: limit recursion depth
	if depth >= MaxFieldDepth {
		return nil, ""
	}

	// Unwrap pointers and maps
	t = unwrapType(t)
	if t == nil {
		return nil, ""
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil, ""
	}

	// Cache key MUST include type arguments for generics (e.g., pkg.Struct[int])
	cacheKey := t.String()

	// Cycle detection
	if seen[cacheKey] {
		return nil, ""
	}
	seen[cacheKey] = true

	// Check cache
	if cached, ok := fc.get(cacheKey); ok {
		return cached.fields, cached.doc
	}

	// AST key is just the base name (pkg.Struct) for looking up docs/source locations
	astKey := getASTKey(named)

	// Extract fields (cache miss)
	fields, doc := extractFieldsUncachedDepth(named, astKey, structIndex, fc, seen, fset, depth)

	// Store in cache
	fc.set(cacheKey, cachedFields{fields: fields, doc: doc})

	return fields, doc
}

// extractFieldsUncachedDepth performs the actual field extraction without cache lookup.
// Handles both struct types and interface types differently.
func extractFieldsUncachedDepth(
	named *types.Named,
	astKey string,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) ([]FieldInfo, string) {
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		// Interface or other named type: expose methods only
		return extractMethodFields(named, fset), ""
	}

	// Struct type: extract fields and methods
	entry := structIndex[astKey]
	fields := extractStructFieldsDepth(strct, entry, structIndex, fc, seen, fset, depth)

	// Append methods
	fields = append(fields, extractMethodFields(named, fset)...)

	// Add method docs from struct index
	addMethodDocs(fields, entry)

	return fields, entry.doc
}

// extractStructFieldsDepth processes all fields in a struct type with depth tracking.
func extractStructFieldsDepth(
	strct *types.Struct,
	entry structIndexEntry,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) []FieldInfo {
	fields := make([]FieldInfo, 0, strct.NumFields())

	for field := range strct.Fields() {
		if !field.Exported() {
			continue
		}

		fi := buildFieldInfoDepth(field, entry, structIndex, fc, seen, fset, depth)
		fields = append(fields, fi)
	}

	return fields
}

// buildFieldInfoDepth constructs a FieldInfo for a single struct field with depth tracking.
func buildFieldInfoDepth(
	field *types.Var,
	entry structIndexEntry,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) FieldInfo {
	fi := FieldInfo{
		Name:    field.Name(),
		TypeStr: normalizeTypeStr(field.Type()),
	}

	// Set definition location
	if pos := field.Pos(); pos.IsValid() && fset != nil {
		position := fset.Position(pos)
		fi.DefFile = position.Filename
		fi.DefLine = position.Line
		fi.DefCol = position.Column
	}

	// Unwrap pointer
	ft := field.Type()
	if ptr, ok := ft.(*types.Pointer); ok {
		ft = ptr.Elem()
	}

	// Handle collection types
	if slice, ok := ft.(*types.Slice); ok {
		fi.IsSlice = true
		// Independent recursion branch for slice elements
		elemSeen := copySeenMap(seen)
		fi.Fields, _ = extractFieldsWithDocsDepth(slice.Elem(), structIndex, fc, elemSeen, fset, depth+1)
	} else if keyType, elemType := getMapTypes(ft); keyType != nil && elemType != nil {
		fi.IsMap = true
		fi.KeyType = normalizeTypeStr(keyType)
		fi.ElemType = normalizeTypeStr(elemType)
		// Independent recursion branch for map values
		elemSeen := copySeenMap(seen)
		fi.Fields, _ = extractFieldsWithDocsDepth(elemType, structIndex, fc, elemSeen, fset, depth+1)
	} else {
		// Regular field: continue with shared seen map
		fi.Fields, _ = extractFieldsWithDocsDepth(ft, structIndex, fc, seen, fset, depth+1)
	}

	// Add field documentation from index
	if pos, ok := entry.fields[field.Name()]; ok {
		if fi.DefFile == "" {
			fi.DefFile = pos.file
			fi.DefLine = pos.line
			fi.DefCol = pos.col
		}
		fi.Doc = pos.doc
	}

	return fi
}

// extractMethodFields extracts exported methods as FieldInfo entries.
func extractMethodFields(named *types.Named, fset *token.FileSet) []FieldInfo {
	fields := make([]FieldInfo, 0, named.NumMethods())

	for method := range named.Methods() {
		if !method.Exported() {
			continue
		}

		fi := FieldInfo{
			Name:    method.Name(),
			TypeStr: "method",
		}

		// Extract method signature
		if sig, ok := method.Type().(*types.Signature); ok {
			fi.Params, fi.Returns, _ = extractSignatureInfo(sig)
		}

		// Set definition location
		if pos := method.Pos(); pos.IsValid() && fset != nil {
			position := fset.Position(pos)
			fi.DefFile = position.Filename
			fi.DefLine = position.Line
			fi.DefCol = position.Column
		}

		fields = append(fields, fi)
	}

	return fields
}

// addMethodDocs enriches method FieldInfo entries with documentation from the index.
func addMethodDocs(fields []FieldInfo, entry structIndexEntry) {
	for i := range fields {
		fi := &fields[i]
		if fi.TypeStr != "method" {
			continue
		}

		if pos, ok := entry.fields[fi.Name]; ok {
			if fi.DefFile == "" {
				fi.DefFile = pos.file
				fi.DefLine = pos.line
				fi.DefCol = pos.col
			}
			fi.Doc = pos.doc
		}
	}
}

// copySeenMap creates an independent copy for separate recursion branches.
func copySeenMap(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	maps.Copy(dst, src)
	return dst
}

// extractMapVars extracts template variables from a map composite literal.
// Example: map[string]interface{}{"user": user, "posts": posts}
// Returns: [TemplateVar{Name:"user",...}, TemplateVar{Name:"posts",...}]
func extractMapVars(
	expr goast.Expr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
) []TemplateVar {
	comp, ok := expr.(*goast.CompositeLit)
	if !ok {
		return nil
	}

	vars := make([]TemplateVar, 0, len(comp.Elts))

	for _, elt := range comp.Elts {
		kv, ok := elt.(*goast.KeyValueExpr)
		if !ok {
			continue
		}

		keyLit, ok := kv.Key.(*goast.BasicLit)
		if !ok {
			continue
		}

		name := strings.Trim(keyLit.Value, `"`)
		tv := TemplateVar{Name: name}

		// Extract type information
		if typeInfo, ok := info.Types[kv.Value]; ok {
			// Clear seen map for this variable
			clear(seen)

			tv.TypeStr = normalizeTypeStr(typeInfo.Type)
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

			// Handle collection types
			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType)
				tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
				tv.IsMap = true
				tv.KeyType = normalizeTypeStr(keyType)
				tv.ElemType = normalizeTypeStr(elemType)
				tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			}
		} else {
			// Fallback: infer from AST
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		// Find definition location
		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)

		vars = append(vars, tv)
	}

	return vars
}

// extractFieldsWithDocsPreservingDoc extracts fields while preserving
// existing documentation if new extraction returns empty doc.
func extractFieldsWithDocsPreservingDoc(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	existingDoc string,
) ([]FieldInfo, string) {
	fields, doc := extractFieldsWithDocs(t, structIndex, fc, seen, fset)
	if doc == "" {
		doc = existingDoc
	}
	return fields, doc
}
