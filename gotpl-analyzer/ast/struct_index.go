package ast

import (
	goast "go/ast"
	"go/token"
	"runtime"
	"sync"
)

// buildStructIndex performs concurrent extraction of struct metadata across
// all AST files.
//
// OPTIMISATION: Pass 2 (attachMethodDocs) is now also parallelised.
// Previously it was a sequential loop over all files; with many large files
// this was a significant serial bottleneck.
func buildStructIndex(fset *token.FileSet, files map[string]*goast.File) map[string]structIndexEntry {
	numWorkers := max(runtime.NumCPU(), 1)
	fileChan := make(chan *goast.File, len(files))

	var sharedIndex sync.Map
	var wg sync.WaitGroup

	// Pass 1: Extract struct fields concurrently.
	for range numWorkers {
		wg.Add(1)
		go extractStructFieldsWorker(fileChan, fset, &sharedIndex, &wg)
	}

	for _, f := range files {
		fileChan <- f
	}
	close(fileChan)
	wg.Wait()

	finalIndex := convertSyncMapToMap(&sharedIndex, len(files))

	// Pass 2: Attach method docs — now also concurrent.
	attachMethodDocsConcurrent(files, fset, finalIndex)

	return finalIndex
}

// extractStructFieldsWorker processes files to extract type declarations.
func extractStructFieldsWorker(
	fileChan <-chan *goast.File,
	fset *token.FileSet,
	sharedIndex *sync.Map,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for f := range fileChan {
		pkgName := f.Name.Name

		for _, decl := range f.Decls {
			genDecl, ok := decl.(*goast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}

			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*goast.TypeSpec)
				if !ok {
					continue
				}

				entry := structIndexEntry{
					doc:    extractTypeDoc(genDecl, typeSpec),
					fields: make(map[string]fieldInfo),
				}

				if structType, ok := typeSpec.Type.(*goast.StructType); ok {
					if structType.Fields != nil {
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
					}
				} else if ifaceType, ok := typeSpec.Type.(*goast.InterfaceType); ok {
					if ifaceType.Methods != nil {
						for _, field := range ifaceType.Methods.List {
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
					}
				}

				key := pkgName + "." + typeSpec.Name.Name
				sharedIndex.Store(key, entry)
			}
		}
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

// methodDocWork is a single method documentation attachment task.
type methodDocWork struct {
	file    *goast.File
	pkgName string
}

// attachMethodDocsConcurrent parallelises Pass 2 of buildStructIndex.
//
// OPTIMISATION: The original attachMethodDocs function was a sequential loop.
// Each file is independent so we fan the work out across all CPU cores.
// The finalIndex map is pre-built (no new keys are added here) so concurrent
// reads of the map are safe; the writes are protected per-entry by a
// fine-grained per-key approach: we read the entry, mutate it locally, and
// write it back under a mutex.
func attachMethodDocsConcurrent(files map[string]*goast.File, fset *token.FileSet, index map[string]structIndexEntry) {
	works := make([]methodDocWork, 0, len(files))
	for _, f := range files {
		works = append(works, methodDocWork{file: f, pkgName: f.Name.Name})
	}

	if len(works) == 0 {
		return
	}

	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (len(works) + numWorkers - 1) / numWorkers

	var mu sync.Mutex // protects writes to index
	var wg sync.WaitGroup

	for w := range numWorkers {
		start := w * chunkSize
		if start >= len(works) {
			break
		}
		end := min(start+chunkSize, len(works))
		chunk := works[start:end]

		wg.Add(1)
		go func(chunk []methodDocWork) {
			defer wg.Done()
			attachMethodDocsChunk(chunk, fset, index, &mu)
		}(chunk)
	}

	wg.Wait()
}

// attachMethodDocsChunk processes a slice of files and attaches method docs.
func attachMethodDocsChunk(works []methodDocWork, fset *token.FileSet, index map[string]structIndexEntry, mu *sync.Mutex) {
	// Accumulate updates locally to minimise lock contention.
	type update struct {
		key  string
		name string
		info fieldInfo
	}
	updates := make([]update, 0, 16)

	for _, w := range works {
		goast.Inspect(w.file, func(n goast.Node) bool {
			funcDecl, ok := n.(*goast.FuncDecl)
			if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
				return true
			}

			if funcDecl.Doc == nil {
				return true // no doc to attach
			}

			recvType := funcDecl.Recv.List[0].Type
			if starExpr, ok := recvType.(*goast.StarExpr); ok {
				recvType = starExpr.X
			}

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

			key := w.pkgName + "." + ident.Name
			doc := funcDecl.Doc.Text()
			pos := fset.Position(funcDecl.Pos())

			updates = append(updates, update{
				key:  key,
				name: funcDecl.Name.Name,
				info: fieldInfo{file: pos.Filename, line: pos.Line, col: pos.Column, doc: doc},
			})

			return true
		})
	}

	if len(updates) == 0 {
		return
	}

	mu.Lock()
	for _, u := range updates {
		if entry, exists := index[u.key]; exists {
			entry.fields[u.name] = u.info
			index[u.key] = entry
		}
	}
	mu.Unlock()
}

// extractTypeDoc retrieves documentation from type declaration.
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
func extractFieldDoc(field *goast.Field) string {
	if field.Doc != nil {
		return field.Doc.Text()
	}
	if field.Comment != nil {
		return field.Comment.Text()
	}
	return ""
}
