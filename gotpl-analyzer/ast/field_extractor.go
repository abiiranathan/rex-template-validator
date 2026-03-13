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
// a type, leveraging caching to avoid redundant work.
func extractFieldsWithDocs(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
) ([]FieldInfo, string) {
	return extractFieldsWithDocsDepth(t, structIndex, fc, seen, fset, 0)
}

// extractFieldsWithDocsDepth is the internal version with depth tracking.
//
// OPTIMISATION: Cache key is checked BEFORE the seen-map check so repeated
// top-level lookups (e.g. the same struct used in 200 render calls) return
// immediately without acquiring the seen-map entry at all.
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

	// Check field cache FIRST — avoids the seen-map dance for already-processed types.
	// This is the hot path: the same struct appears in many render calls.
	if cached, ok := fc.get(cacheKey); ok {
		return cached.fields, cached.doc
	}

	// Cycle detection (path-based).
	if seen[cacheKey] {
		return nil, ""
	}
	seen[cacheKey] = true
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
		doc := ""
		if entry, exists := structIndex[astKey]; exists {
			doc = entry.doc
		}
		return extractMethodFields(named, structIndex, fc, seen, fset, depth), doc
	}

	entry := structIndex[astKey]
	fields := extractStructFieldsDepth(strct, entry, structIndex, fc, seen, fset, depth)
	fields = append(fields, extractMethodFields(named, structIndex, fc, seen, fset, depth)...)
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

		if field.Embedded() {
			fields = append(fields, fi.Fields...)
		}
	}

	return fields
}

// buildFieldInfoDepth constructs a FieldInfo for a single struct field with depth tracking.
//
// OPTIMISATION: Only allocate a copySeenMap for slice/map branches where an
// independent recursion path is needed. Regular struct fields continue with the
// shared seen map (cheaper, still correct because defer delete cleans up).
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

	if pos := field.Pos(); pos.IsValid() && fset != nil {
		position := fset.Position(pos)
		fi.DefFile = position.Filename
		fi.DefLine = position.Line
		fi.DefCol = position.Column
	}

	ft := field.Type()
	if ptr, ok := ft.(*types.Pointer); ok {
		ft = ptr.Elem()
	}

	if slice, ok := ft.(*types.Slice); ok {
		fi.IsSlice = true
		fi.ElemType = normalizeTypeStr(slice.Elem())
		// Independent branch needs its own seen map copy to avoid cross-contamination.
		elemSeen := copySeenMap(seen)
		fi.Fields, _ = extractFieldsWithDocsDepth(slice.Elem(), structIndex, fc, elemSeen, fset, depth+1)
	} else if keyType, elemType := getMapTypes(ft); keyType != nil && elemType != nil {
		fi.IsMap = true
		fi.KeyType = normalizeTypeStr(keyType)
		fi.ElemType = normalizeTypeStr(elemType)
		elemSeen := copySeenMap(seen)
		fi.Fields, _ = extractFieldsWithDocsDepth(elemType, structIndex, fc, elemSeen, fset, depth+1)
	} else {
		// Regular struct field: reuse the shared seen map — no copy needed.
		fi.Fields, _ = extractFieldsWithDocsDepth(ft, structIndex, fc, seen, fset, depth+1)
	}

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

		if sig, ok := method.Type().(*types.Signature); ok {
			fi.Params, fi.Returns, _ = extractSignatureInfoWithFields(sig, structIndex, fc, seen, fset, depth+1)

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
// the struct fields for any returned types.
func extractSignatureInfoWithFields(
	sig *types.Signature,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	depth int,
) (params, returns []ParamInfo, args []string) {
	params, returns, args = extractSignatureInfo(sig)

	for i := 0; i < sig.Results().Len(); i++ {
		rt := sig.Results().At(i).Type()
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

		if typeInfo, ok := info.Types[kv.Value]; ok {
			clear(seen)

			tv.TypeStr = normalizeTypeStr(typeInfo.Type)
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

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
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)
		vars = append(vars, tv)
	}

	return vars
}

// extractFieldsWithDocsPreservingDoc extracts fields while preserving existing doc.
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
