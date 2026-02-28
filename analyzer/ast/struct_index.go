package ast

import (
	goast "go/ast"
	"go/token"
	"runtime"
	"sync"
)

// buildStructIndex performs concurrent extraction of struct metadata across
// all AST files. The index maps each struct type to its documentation and
// field/method information.
//
// Two-pass algorithm:
// Pass 1: Extract struct fields and documentation (concurrent)
// Pass 2: Attach method documentation (sequential, typically small)
//
// Concurrency: Workers write directly to sync.Map to avoid coordination overhead.
func buildStructIndex(fset *token.FileSet, files map[string]*goast.File) map[string]structIndexEntry {
	numWorkers := max(runtime.NumCPU(), 1)
	fileChan := make(chan *goast.File, len(files))

	var sharedIndex sync.Map // Concurrent-safe map for worker writes
	var wg sync.WaitGroup

	// Pass 1: Extract struct fields concurrently
	for range numWorkers {
		wg.Add(1)
		go extractStructFieldsWorker(fileChan, fset, &sharedIndex, &wg)
	}

	// Feed files to workers
	for _, f := range files {
		fileChan <- f
	}
	close(fileChan)
	wg.Wait()

	// Convert sync.Map to regular map for fast O(1) reads
	finalIndex := convertSyncMapToMap(&sharedIndex, len(files))

	// Pass 2: Attach method documentation
	attachMethodDocs(files, fset, finalIndex)

	return finalIndex
}

// extractStructFieldsWorker is a worker function that processes files to extract
// struct type declarations and their field metadata.
func extractStructFieldsWorker(
	fileChan <-chan *goast.File,
	fset *token.FileSet,
	sharedIndex *sync.Map,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for f := range fileChan {
		pkgName := f.Name.Name

		goast.Inspect(f, func(n goast.Node) bool {
			genDecl, ok := n.(*goast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				return true
			}

			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*goast.TypeSpec)
				if !ok {
					continue
				}

				structType, ok := typeSpec.Type.(*goast.StructType)
				if !ok {
					continue
				}

				// Build struct index entry
				entry := structIndexEntry{
					doc:    extractTypeDoc(genDecl, typeSpec),
					fields: make(map[string]fieldInfo, len(structType.Fields.List)),
				}

				// Extract field metadata
				for _, field := range structType.Fields.List {
					pos := fset.Position(field.Pos())
					doc := extractFieldDoc(field)

					for _, name := range field.Names {
						entry.fields[name.Name] = fieldInfo{
							file: pos.Filename,
							line: pos.Line,
							col:  pos.Column,
							doc:  doc,
						}
					}
				}

				// Store in shared index (using base name for AST lookup)
				key := pkgName + "." + typeSpec.Name.Name
				sharedIndex.Store(key, entry)
			}

			return true
		})
	}
}

// convertSyncMapToMap converts sync.Map to regular map for optimized reads.
func convertSyncMapToMap(sharedIndex *sync.Map, estimatedSize int) map[string]structIndexEntry {
	finalIndex := make(map[string]structIndexEntry, estimatedSize*4)

	sharedIndex.Range(func(k, v any) bool {
		finalIndex[k.(string)] = v.(structIndexEntry)
		return true
	})

	return finalIndex
}

// attachMethodDocs walks all files to find method declarations and attach
// their documentation to the corresponding struct entries.
func attachMethodDocs(files map[string]*goast.File, fset *token.FileSet, index map[string]structIndexEntry) {
	for _, f := range files {
		pkgName := f.Name.Name

		goast.Inspect(f, func(n goast.Node) bool {
			funcDecl, ok := n.(*goast.FuncDecl)
			if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
				return true
			}

			// Extract receiver type name
			recvType := funcDecl.Recv.List[0].Type
			if starExpr, ok := recvType.(*goast.StarExpr); ok {
				recvType = starExpr.X
			}

			// Handle generic receivers (e.g., *MyStruct[T])
			var ident *goast.Ident
			switch rt := recvType.(type) {
			case *goast.Ident:
				ident = rt
			case *goast.IndexExpr:
				ident, _ = rt.X.(*goast.Ident)
			case *goast.IndexListExpr:
				ident, _ = rt.X.(*goast.Ident)
			}

			if ident == nil {
				return true
			}

			// Find corresponding struct entry
			key := pkgName + "." + ident.Name
			entry, exists := index[key]
			if !exists {
				return true
			}

			// Extract method documentation
			doc := ""
			if funcDecl.Doc != nil {
				doc = funcDecl.Doc.Text()
			}

			// Only update if we have documentation to add
			if doc != "" {
				pos := fset.Position(funcDecl.Pos())
				entry.fields[funcDecl.Name.Name] = fieldInfo{
					file: pos.Filename,
					line: pos.Line,
					col:  pos.Column,
					doc:  doc,
				}
			}

			return true
		})
	}
}

// extractTypeDoc retrieves documentation from type declaration.
// Checks genDecl.Doc, typeSpec.Doc, and typeSpec.Comment in order.
func extractTypeDoc(genDecl *goast.GenDecl, typeSpec *goast.TypeSpec) string {
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

// extractFieldDoc retrieves documentation from field declaration.
// Checks field.Doc and field.Comment in order.
func extractFieldDoc(field *goast.Field) string {
	if field.Doc != nil {
		return field.Doc.Text()
	}
	if field.Comment != nil {
		return field.Comment.Text()
	}
	return ""
}
