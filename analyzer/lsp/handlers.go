package lsp

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

// registerHandlers wires every supported JSON-RPC method to its handler.
func (s *Server) registerHandlers() {
	// LSP lifecycle
	s.handlers["initialize"] = s.handleInitialize

	// File-change notification (standard LSP — triggers cache invalidation)
	s.handlers["workspace/didChangeWatchedFiles"] = s.handleDidChangeWatchedFiles

	// Custom rex/* methods
	s.handlers["rex/getTemplateContext"] = s.handleGetTemplateContext
	s.handlers["rex/validate"] = s.handleValidate
	s.handlers["rex/getFuncMaps"] = s.handleGetFuncMaps
	s.handlers["rex/getNamedBlocks"] = s.handleGetNamedBlocks
	s.handlers["rex/getRenderCalls"] = s.handleGetRenderCalls
	s.handlers["rex/invalidateCache"] = s.handleInvalidateCache
}

// ─── LSP lifecycle ────────────────────────────────────────────────────────────

func (s *Server) handleInitialize(params json.RawMessage) (any, *ResponseError) {
	// We accept the params but do not act on rootUri/rootPath because every
	// custom method carries its own "dir" field, making the workspace root
	// opt-in per request.
	return InitializeResult{
		Capabilities: ServerCapabilities{},
		ServerInfo: ServerInfo{
			Name:    "rex-template-analyzer",
			Version: "1.0.0",
		},
	}, nil
}

// handleDidChangeWatchedFiles receives workspace/didChangeWatchedFiles
// notifications from the extension and invalidates the analysis cache for
// every directory that contains a modified Go file.
func (s *Server) handleDidChangeWatchedFiles(params json.RawMessage) (any, *ResponseError) {
	var p DidChangeWatchedFilesParams
	if err := json.Unmarshal(params, &p); err != nil {
		// Non-fatal: log and continue.
		return nil, nil
	}

	invalidated := make(map[string]bool)
	for _, change := range p.Changes {
		dir := dirFromURI(change.URI)
		if dir == "" || invalidated[dir] {
			continue
		}
		invalidated[dir] = true
		s.cache.invalidate(dir)
	}

	return nil, nil
}

// ─── rex/getTemplateContext ───────────────────────────────────────────────────

// handleGetTemplateContext returns the merged TemplateVar list for a given
// template name.  The extension calls this when it needs completions or hover
// information for a variable access inside a template file.
//
// Only the analysis (Go AST pass) is performed; the full template-tree
// validation walk is skipped, keeping latency low.
func (s *Server) handleGetTemplateContext(params json.RawMessage) (any, *ResponseError) {
	var p GetTemplateContextParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: err.Error()}
	}

	absDir, err := filepath.Abs(p.Dir)
	if err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: "invalid dir: " + err.Error()}
	}

	cached := s.ensureAnalysis(absDir, p.ContextFile)

	// Collect and merge vars from every render call that targets this template.
	vars := mergeRenderCallVars(cached.result.RenderCalls, p.TemplateName)

	return GetTemplateContextResult{
		Vars:   toTemplateVarJSONSlice(vars),
		Errors: cached.result.Errors,
	}, nil
}

// ─── rex/validate ─────────────────────────────────────────────────────────────

// handleValidate runs the full validation pass for a single template file.
// The extension calls this on save or open to populate diagnostics.
//
// Named blocks are lazily cached so the first validate call for a given
// (dir, templateRoot) pair pays the directory-walk cost once.
func (s *Server) handleValidate(params json.RawMessage) (any, *ResponseError) {
	var p ValidateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: err.Error()}
	}

	absDir, err := filepath.Abs(p.Dir)
	if err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: "invalid dir: " + err.Error()}
	}

	cached := s.ensureAnalysis(absDir, p.ContextFile)

	namedBlocks, _ := cached.loadNamedBlocks(absDir, p.TemplateRoot)

	vars := mergeRenderCallVars(cached.result.RenderCalls, p.TemplateName)

	templatePath := filepath.Join(absDir, p.TemplateRoot, p.TemplateName)
	rawErrors := validator.ValidateTemplateFile(
		templatePath, vars, p.TemplateName, absDir, p.TemplateRoot, namedBlocks,
	)

	return ValidateResult{Errors: toValidationResultJSONSlice(rawErrors)}, nil
}

// ─── rex/getFuncMaps ──────────────────────────────────────────────────────────

// handleGetFuncMaps returns all template.FuncMap entries discovered in the
// workspace.  The extension uses this to populate function-name completions
// inside {{ call ... }} and pipe expressions.
func (s *Server) handleGetFuncMaps(params json.RawMessage) (any, *ResponseError) {
	var p GetFuncMapsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: err.Error()}
	}

	absDir, err := filepath.Abs(p.Dir)
	if err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: "invalid dir: " + err.Error()}
	}

	cached := s.ensureAnalysis(absDir, p.ContextFile)

	return GetFuncMapsResult{
		FuncMaps: toFuncMapInfoJSONSlice(cached.result.FuncMaps),
		Errors:   cached.result.Errors,
	}, nil
}

// ─── rex/getNamedBlocks ───────────────────────────────────────────────────────

// handleGetNamedBlocks returns all {{define}} / {{block}} entries found in
// the template tree.  The extension uses this for "go-to definition" of
// named block references.
func (s *Server) handleGetNamedBlocks(params json.RawMessage) (any, *ResponseError) {
	var p GetNamedBlocksParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: err.Error()}
	}

	absDir, err := filepath.Abs(p.Dir)
	if err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: "invalid dir: " + err.Error()}
	}

	// Named blocks do not depend on the Go analysis; use a dedicated cache key.
	nbCacheKey := absDir + "|namedblocks|" + p.TemplateRoot
	nbEntry := s.ensureAnalysis(nbCacheKey, "")
	namedBlocks, dupErrors := nbEntry.loadNamedBlocks(absDir, p.TemplateRoot)

	return GetNamedBlocksResult{
		NamedBlocks:     toNamedBlocksJSON(namedBlocks),
		DuplicateErrors: toNamedBlockDuplicateErrorsJSON(dupErrors),
	}, nil
}

// ─── rex/getRenderCalls ───────────────────────────────────────────────────────

// handleGetRenderCalls returns all render calls discovered in the workspace.
// This is the heavier "full knowledge graph" call; prefer getTemplateContext
// when only one template's vars are needed.
func (s *Server) handleGetRenderCalls(params json.RawMessage) (any, *ResponseError) {
	var p GetRenderCallsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: err.Error()}
	}

	absDir, err := filepath.Abs(p.Dir)
	if err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: "invalid dir: " + err.Error()}
	}

	cached := s.ensureAnalysis(absDir, p.ContextFile)

	calls := make([]RenderCallJSON, 0, len(cached.result.RenderCalls))
	for _, rc := range cached.result.RenderCalls {
		calls = append(calls, RenderCallJSON{
			File:                 rc.File,
			Line:                 rc.Line,
			Template:             rc.Template,
			TemplateNameStartCol: rc.TemplateNameStartCol,
			TemplateNameEndCol:   rc.TemplateNameEndCol,
			Vars:                 toTemplateVarJSONSlice(rc.Vars),
		})
	}

	return GetRenderCallsResult{
		RenderCalls: calls,
		Errors:      cached.result.Errors,
	}, nil
}

// ─── rex/invalidateCache ──────────────────────────────────────────────────────

// handleInvalidateCache evicts the cached analysis for a directory.
// Can be sent as either a request or a notification; either way the response
// (if requested) is null.
func (s *Server) handleInvalidateCache(params json.RawMessage) (any, *ResponseError) {
	var p InvalidateCacheParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrInvalidParams, Message: err.Error()}
	}

	absDir, _ := filepath.Abs(p.Dir)
	s.cache.invalidate(absDir)
	return nil, nil
}

// ─── Cache helpers ────────────────────────────────────────────────────────────

// ensureAnalysis returns the cached analysis for (dir, contextFile), running
// ast.AnalyzeDir if the cache is cold.  Concurrent callers for the same
// (dir, contextFile) may both run the analysis; the last writer wins.
// This is intentional: the analysis is deterministic and the cost of a
// redundant run is lower than the complexity of a singleflight barrier.
func (s *Server) ensureAnalysis(dir, contextFile string) *cachedAnalysis {
	if cached, ok := s.cache.get(dir, contextFile); ok {
		return cached
	}

	result := ast.AnalyzeDir(dir, contextFile, ast.DefaultConfig)
	entry := &cachedAnalysis{result: result}
	s.cache.set(dir, contextFile, entry)
	return entry
}

// mergeRenderCallVars collects TemplateVars from every render call that
// targets templateName (exact match or path suffix), deduplicating by name.
func mergeRenderCallVars(calls []ast.RenderCall, templateName string) []ast.TemplateVar {
	seen := make(map[string]bool)
	var out []ast.TemplateVar

	for _, rc := range calls {
		if !templateNameMatches(rc.Template, templateName) {
			continue
		}
		for _, v := range rc.Vars {
			if !seen[v.Name] {
				seen[v.Name] = true
				out = append(out, v)
			}
		}
	}

	return out
}

// templateNameMatches returns true if rcTemplate == name, or if rcTemplate
// ends with "/name" or "\name" (handles relative vs. full paths).
func templateNameMatches(rcTemplate, name string) bool {
	return rcTemplate == name ||
		strings.HasSuffix(rcTemplate, "/"+name) ||
		strings.HasSuffix(rcTemplate, "\\"+name)
}

// dirFromURI converts a file:// URI to an absolute directory path.
// Returns "" if the URI cannot be parsed.
func dirFromURI(rawURI string) string {
	u, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}

	fpath := u.Path

	// On Windows file:///C:/... — strip the leading slash before the drive letter.
	if len(fpath) >= 3 && fpath[0] == '/' && fpath[2] == ':' {
		fpath = fpath[1:]
	}

	return filepath.Dir(fpath)
}

// ─── Conversion helpers (ast → JSON mirror types) ────────────────────────────

func toFieldInfoJSON(f ast.FieldInfo) FieldInfoJSON {
	out := FieldInfoJSON{
		Name:     f.Name,
		TypeStr:  f.TypeStr,
		IsSlice:  f.IsSlice,
		IsMap:    f.IsMap,
		KeyType:  f.KeyType,
		ElemType: f.ElemType,
		DefFile:  f.DefFile,
		DefLine:  f.DefLine,
		DefCol:   f.DefCol,
		Doc:      f.Doc,
	}
	if len(f.Fields) > 0 {
		out.Fields = toFieldInfoJSONSlice(f.Fields)
	}
	if len(f.Params) > 0 {
		out.Params = toParamInfoJSONSlice(f.Params)
	}
	if len(f.Returns) > 0 {
		out.Returns = toParamInfoJSONSlice(f.Returns)
	}
	return out
}

func toFieldInfoJSONSlice(fields []ast.FieldInfo) []FieldInfoJSON {
	if len(fields) == 0 {
		return nil
	}
	out := make([]FieldInfoJSON, len(fields))
	for i, f := range fields {
		out[i] = toFieldInfoJSON(f)
	}
	return out
}

func toParamInfoJSONSlice(params []ast.ParamInfo) []ParamInfoJSON {
	if len(params) == 0 {
		return nil
	}
	out := make([]ParamInfoJSON, len(params))
	for i, p := range params {
		out[i] = ParamInfoJSON{
			Name:    p.Name,
			TypeStr: p.TypeStr,
			Fields:  toFieldInfoJSONSlice(p.Fields),
		}
	}
	return out
}

func toTemplateVarJSON(v ast.TemplateVar) TemplateVarJSON {
	out := TemplateVarJSON{
		Name:     v.Name,
		TypeStr:  v.TypeStr,
		IsSlice:  v.IsSlice,
		IsMap:    v.IsMap,
		KeyType:  v.KeyType,
		ElemType: v.ElemType,
		DefFile:  v.DefFile,
		DefLine:  v.DefLine,
		DefCol:   v.DefCol,
		Doc:      v.Doc,
	}
	if len(v.Fields) > 0 {
		out.Fields = toFieldInfoJSONSlice(v.Fields)
	}
	return out
}

func toTemplateVarJSONSlice(vars []ast.TemplateVar) []TemplateVarJSON {
	if len(vars) == 0 {
		return []TemplateVarJSON{}
	}
	out := make([]TemplateVarJSON, len(vars))
	for i, v := range vars {
		out[i] = toTemplateVarJSON(v)
	}
	return out
}

func toFuncMapInfoJSONSlice(fms []ast.FuncMapInfo) []FuncMapInfoJSON {
	if len(fms) == 0 {
		return []FuncMapInfoJSON{}
	}
	out := make([]FuncMapInfoJSON, len(fms))
	for i, fm := range fms {
		out[i] = FuncMapInfoJSON{
			Name:             fm.Name,
			Args:             fm.Args,
			Doc:              fm.Doc,
			DefFile:          fm.DefFile,
			DefLine:          fm.DefLine,
			DefCol:           fm.DefCol,
			Params:           toParamInfoJSONSlice(fm.Params),
			Returns:          toParamInfoJSONSlice(fm.Returns),
			ReturnTypeFields: toFieldInfoJSONSlice(fm.ReturnTypeFields),
		}
	}
	return out
}

func toValidationResultJSONSlice(errs []validator.ValidationResult) []ValidationResultJSON {
	if len(errs) == 0 {
		return []ValidationResultJSON{}
	}
	out := make([]ValidationResultJSON, len(errs))
	for i, e := range errs {
		out[i] = ValidationResultJSON{
			Template:             e.Template,
			Line:                 e.Line,
			Column:               e.Column,
			Variable:             e.Variable,
			Message:              e.Message,
			Severity:             e.Severity,
			GoFile:               e.GoFile,
			GoLine:               e.GoLine,
			TemplateNameStartCol: e.TemplateNameStartCol,
			TemplateNameEndCol:   e.TemplateNameEndCol,
		}
	}
	return out
}

func toNamedBlocksJSON(blocks map[string][]validator.NamedBlockEntry) map[string][]NamedBlockEntryJSON {
	if len(blocks) == 0 {
		return map[string][]NamedBlockEntryJSON{}
	}
	out := make(map[string][]NamedBlockEntryJSON, len(blocks))
	for name, entries := range blocks {
		jsonEntries := make([]NamedBlockEntryJSON, len(entries))
		for i, e := range entries {
			jsonEntries[i] = NamedBlockEntryJSON{
				Name:         e.Name,
				AbsolutePath: e.AbsolutePath,
				TemplatePath: e.TemplatePath,
				Line:         e.Line,
				Col:          e.Col,
			}
		}
		out[name] = jsonEntries
	}
	return out
}

func toNamedBlockDuplicateErrorsJSON(errs []validator.NamedBlockDuplicateError) []NamedBlockDuplicateErrorJSON {
	if len(errs) == 0 {
		return nil
	}
	out := make([]NamedBlockDuplicateErrorJSON, len(errs))
	for i, e := range errs {
		entries := make([]NamedBlockEntryJSON, len(e.Entries))
		for j, entry := range e.Entries {
			entries[j] = NamedBlockEntryJSON{
				Name:         entry.Name,
				AbsolutePath: entry.AbsolutePath,
				TemplatePath: entry.TemplatePath,
				Line:         entry.Line,
				Col:          entry.Col,
			}
		}
		out[i] = NamedBlockDuplicateErrorJSON{
			Name:    e.Name,
			Entries: entries,
			Message: e.Message,
		}
	}
	return out
}
