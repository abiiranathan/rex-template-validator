package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"maps"
	"strings"
)

// MaxFieldDepth is the maximum depth for field extraction to prevent excessive recursion
const MaxFieldDepth = 4

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
	if depth >= MaxFieldDepth {
		return nil, ""
	}

	t = unwrapType(t)
	// Additional unwrapping for slice and array types so we extract
	// fields of the underlying struct instead of returning empty fields.
	for {
		if s, ok := t.(*types.Slice); ok {
			t = unwrapType(s.Elem())
		} else if a, ok := t.(*types.Array); ok {
			t = unwrapType(a.Elem())
		} else {
			break
		}
	}

	if t == nil {
		return nil, ""
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil, ""
	}

	cacheKey := t.String()

	// Check cache FIRST before cycle detection
	if cached, ok := fc.get(cacheKey); ok {
		return cached.fields, cached.doc
	}

	// Cycle detection (path-based)
	if seen[cacheKey] {
		return nil, ""
	}
	seen[cacheKey] = true
	// Remove from seen map when returning so siblings don't falsely trigger cycles
	defer delete(seen, cacheKey)

	astKey := getASTKey(named)
	fields, doc := extractFieldsUncachedDepth(named, astKey, structIndex, fc, seen, fset, depth)

	fc.set(cacheKey, cachedFields{fields: fields, doc: doc})

	return fields, doc
}

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
		doc := ""
		if entry, exists := structIndex[astKey]; exists {
			doc = entry.doc
		}
		return extractMethodFields(named, structIndex, fc, seen, fset, depth), doc
	}

	// Struct type: extract fields and methods
	entry := structIndex[astKey]
	fields := extractStructFieldsDepth(strct, entry, structIndex, fc, seen, fset, depth)

	// Append methods
	fields = append(fields, extractMethodFields(named, structIndex, fc, seen, fset, depth)...)

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

		// Add fields for embedded structs
		if field.Embedded() {
			fields = append(fields, fi.Fields...)
		}
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
		// Populate ElemType so the validator can resolve the range scope
		fi.ElemType = normalizeTypeStr(slice.Elem())

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
func extractMethodFields(
	named *types.Named,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) []FieldInfo {
	var methodSet *types.MethodSet

	if _, isInterface := named.Underlying().(*types.Interface); isInterface {
		methodSet = types.NewMethodSet(named)
	} else {
		// Use NewMethodSet on a pointer to the named type to include BOTH value
		// and pointer receiver methods, as well as promoted methods from embedded fields.
		methodSet = types.NewMethodSet(types.NewPointer(named))
	}

	fields := make([]FieldInfo, 0, methodSet.Len())

	for i := 0; i < methodSet.Len(); i++ {
		sel := methodSet.At(i)
		if !sel.Obj().Exported() {
			continue
		}

		method, ok := sel.Obj().(*types.Func)
		if !ok {
			continue
		}

		fi := FieldInfo{
			Name:    method.Name(),
			TypeStr: "method",
		}

		// Extract method signature deeply to include return type fields
		if sig, ok := method.Type().(*types.Signature); ok {
			fi.Params, fi.Returns, _ = extractSignatureInfoWithFields(sig, structIndex, fc, seen, fset, depth+1)

			// Resolve method docstring using its actual receiver type (handles promoted methods)
			if recv := sig.Recv(); recv != nil {
				recvType := unwrapType(recv.Type())
				if rt, ok := recvType.(*types.Named); ok {
					astKey := getASTKey(rt)
					if entry, exists := structIndex[astKey]; exists {
						if pos, ok := entry.fields[method.Name()]; ok {
							fi.Doc = pos.doc
							if fi.DefFile == "" {
								fi.DefFile = pos.file
								fi.DefLine = pos.line
								fi.DefCol = pos.col
							}
						}
					}
				}
			}
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

// extractSignatureInfoWithFields extracts signature info and recursively extracts
// the struct fields for any returned types (e.g. methods returning *CBCReport).
func extractSignatureInfoWithFields(
	sig *types.Signature,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) (params, returns []ParamInfo, args []string) {
	// Get base signature info
	params, returns, args = extractSignatureInfo(sig)

	// Deeply extract struct fields for each return value
	for i := 0; i < sig.Results().Len(); i++ {
		rt := sig.Results().At(i).Type()

		// Use a copied seen map to prevent cross-branch loop suppression
		elemSeen := copySeenMap(seen)
		fields, doc := extractFieldsWithDocsDepth(rt, structIndex, fc, elemSeen, fset, depth)

		returns[i].Fields = fields
		returns[i].Doc = doc
	}

	return
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
			// Avoid overriding docstring resolved natively from the embedded struct resolution logic
			if fi.Doc == "" {
				fi.Doc = pos.doc
			}
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
