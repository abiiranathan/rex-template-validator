package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abiiranathan/go-template-lsp/analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/analyzer/validator"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type daemonAnalyzeParams struct {
	Dir             string `json:"dir"`
	TemplateRoot    string `json:"templateRoot"`
	TemplateBaseDir string `json:"templateBaseDir"`
	ContextFile     string `json:"contextFile"`
	Validate        bool   `json:"validate"`
}

type daemonValidateTemplateParams struct {
	AbsolutePath string `json:"absolutePath"`
	Content      string `json:"content"`
}

type daemonUpdateTemplateParams struct {
	AbsolutePath string `json:"absolutePath"`
	Content      string `json:"content"`
}

type daemonClearTemplateParams struct {
	AbsolutePath string `json:"absolutePath"`
}

type daemonInferExpressionParams struct {
	Expression  string                     `json:"expression"`
	Vars        map[string]ast.TemplateVar `json:"vars"`
	ScopeStack  []validator.ScopeType      `json:"scopeStack"`
	BlockLocals map[string]ast.TemplateVar `json:"blockLocals"`
}

type daemonGetHoverInfoParams struct {
	AbsolutePath string `json:"absolutePath"`
	Line         int    `json:"line"` // 1-based
	Col          int    `json:"col"`  // 1-based
	Content      string `json:"content"`
}

type daemonValidateTemplateResult struct {
	ValidationErrors []validator.ValidationResult `json:"validationErrors"`
	HasContext       bool                         `json:"hasContext"`
}

// daemonState is the immutable snapshot of analysis results shared by all
// concurrent read-only operations (validateTemplate, inferExpressionType,
// getHoverInfo).  The pointer is replaced atomically on each analyze call so
// readers always see a consistent snapshot without acquiring a write lock.
//
// OPTIMISATION: Previously every read-only handler performed deep clones of
// renderVarsByTemplate, funcMaps, typeRegistry, namedBlocks, and
// templateOverlays under a write-locked mutex — O(n) allocations per request.
// With an atomic pointer swap, read-only handlers simply load the pointer and
// read the shared snapshot.  Only the mutable templateOverlays map (written
// per file save) is still protected by a lightweight RWMutex.
type daemonState struct {
	dir          string
	baseDir      string
	templateRoot string
	contextFile  string
	validate     bool
	output       ValidationOutput

	renderVarsByTemplate map[string][]ast.TemplateVar
	funcMaps             validator.FuncMapRegistry
	typeRegistry         map[string][]ast.FieldInfo
	namedBlocks          map[string][]validator.NamedBlockEntry
	partialTargets       map[string]bool
}

type analyzerDaemon struct {
	// state is replaced atomically on analyze; read-only handlers load it with
	// atomic.Pointer.Load() which does not block.
	state atomic.Pointer[daemonState]

	// templateOverlays is the only field that mutates after analyze completes
	// (via updateTemplate / clearTemplate).  Protected by its own fine-grained
	// RWMutex instead of the coarse daemon-wide lock.
	overlayMu        sync.RWMutex
	templateOverlays map[string]string
}

func runDaemon(stdin io.Reader, stdout io.Writer) error {
	server := &analyzerDaemon{
		templateOverlays: make(map[string]string),
	}
	reader := bufio.NewReader(stdin)
	writer := bufio.NewWriter(stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if err := writeResponse(writer, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: fmt.Sprintf("invalid request: %v", err)},
			}); err != nil {
				return err
			}
			continue
		}

		resp := server.handle(req)
		if err := writeResponse(writer, resp); err != nil {
			return err
		}

		if req.Method == "shutdown" {
			return nil
		}
	}
}

func writeResponse(writer *bufio.Writer, resp rpcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := writer.Write(append(data, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}

func (d *analyzerDaemon) handle(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	defer func() {
		if recovered := recover(); recovered != nil {
			resp.Error = &rpcError{Code: -32001, Message: fmt.Sprintf("daemon panic during %s: %v", req.Method, recovered)}
		}
	}()

	switch req.Method {
	case "analyze":
		var params daemonAnalyzeParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid analyze params: %v", err)}
			return resp
		}
		result, err := d.analyze(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "validateTemplate":
		var params daemonValidateTemplateParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid validateTemplate params: %v", err)}
			return resp
		}
		result, err := d.validateTemplate(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "updateTemplate":
		var params daemonUpdateTemplateParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid updateTemplate params: %v", err)}
			return resp
		}
		if err := d.updateTemplate(params); err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = map[string]bool{"ok": true}
		return resp

	case "clearTemplate":
		var params daemonClearTemplateParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid clearTemplate params: %v", err)}
			return resp
		}
		if err := d.clearTemplate(params); err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = map[string]bool{"ok": true}
		return resp

	case "inferExpressionType":
		var params daemonInferExpressionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid inferExpressionType params: %v", err)}
			return resp
		}
		result, err := d.inferExpressionType(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "getHoverInfo":
		var params daemonGetHoverInfoParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid getHoverInfo params: %v", err)}
			return resp
		}
		result, err := d.getHoverInfo(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "shutdown":
		resp.Result = map[string]bool{"ok": true}
		return resp

	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("unknown method %q", req.Method)}
		return resp
	}
}

func (d *analyzerDaemon) analyze(params daemonAnalyzeParams) (ValidationOutput, error) {
	baseDir := params.Dir
	if params.TemplateBaseDir != "" {
		baseDir = params.TemplateBaseDir
	}

	result := ast.AnalyzeDir(params.Dir, params.ContextFile, ast.DefaultConfig)
	result.Errors = filterImportErrors(result.Errors)

	validationErrors, namedBlocks, namedBlockErrors := validator.ValidateTemplates(
		result.RenderCalls,
		result.FuncMaps,
		baseDir,
		params.TemplateRoot,
	)

	// Build the render-var index BEFORE Flatten() so field trees are intact.
	renderVarIndex := buildRenderVarIndex(result.RenderCalls)

	result.Flatten()

	output := ValidationOutput{
		RenderCalls:      result.RenderCalls,
		FuncMaps:         result.FuncMaps,
		Errors:           result.Errors,
		NamedBlocks:      namedBlocks,
		NamedBlockErrors: namedBlockErrors,
		Types:            result.Types,
	}
	if params.Validate {
		output.ValidationErrors = validationErrors
		output.NamedBlocks = namedBlocks
	}

	// Build immutable snapshot — no cloning needed by readers.
	snap := &daemonState{
		dir:                  params.Dir,
		baseDir:              baseDir,
		templateRoot:         params.TemplateRoot,
		contextFile:          params.ContextFile,
		validate:             params.Validate,
		output:               output,
		renderVarsByTemplate: renderVarIndex,
		funcMaps:             validator.BuildFuncMapRegistry(result.FuncMaps),
		typeRegistry:         result.Types,
		namedBlocks:          namedBlocks,
		partialTargets:       validator.FindPartialTargets(baseDir, params.TemplateRoot),
	}

	// Atomic swap: readers instantly see the new state without waiting.
	d.state.Store(snap)

	// Preserve existing overlays (don't reset on re-analyze).
	// overlayMu write lock not needed here since analyze is serialised by the
	// single-threaded RPC loop.
	if d.templateOverlays == nil {
		d.overlayMu.Lock()
		d.templateOverlays = make(map[string]string)
		d.overlayMu.Unlock()
	}

	return output, nil
}

func (d *analyzerDaemon) validateTemplate(params daemonValidateTemplateParams) (result daemonValidateTemplateResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("validateTemplate panic: %v", recovered)
		}
	}()

	// OPTIMISATION: Load the immutable snapshot atomically — zero allocation,
	// no mutex acquisition for the read-heavy path.
	snap := d.state.Load()
	if snap == nil {
		return daemonValidateTemplateResult{}, fmt.Errorf("daemon not initialized")
	}

	if !snap.validate {
		return daemonValidateTemplateResult{HasContext: false}, nil
	}

	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return daemonValidateTemplateResult{}, err
	}

	templateBase := filepath.Join(snap.baseDir, snap.templateRoot)
	rel, err := filepath.Rel(templateBase, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	// Load overlays under read lock (cheap: just a map lookup).
	d.overlayMu.RLock()
	overlays := cloneTemplateOverlays(d.templateOverlays)
	d.overlayMu.RUnlock()

	overlays[absPath] = params.Content

	// Build a per-request registry copy only when overlays change the shape.
	// For the common case (no overlays beyond the current file), we reuse the
	// shared namedBlocks snapshot directly — only clone when we must mutate.
	registry := snap.namedBlocks
	if len(overlays) > 0 {
		registry = cloneRegistry(snap.namedBlocks)
		applyTemplateOverlays(registry, overlays, snap.baseDir, snap.templateRoot)
	}

	var errors []validator.ValidationResult
	hasContext := false

	if _, vars, ok := findRenderVarsForTemplate(snap.renderVarsByTemplate, absPath, snap.baseDir, snap.templateRoot); ok {
		hasContext = true
		errors = append(errors, validator.ValidateTemplateFileStr(
			params.Content,
			vars,
			rel,
			snap.baseDir,
			snap.templateRoot,
			registry,
			snap.funcMaps,
		)...)
	}

	for _, entry := range registryEntriesForFile(registry, absPath) {
		if entry.Name == entry.TemplatePath {
			continue
		}
		if snap.partialTargets[entry.Name] {
			continue
		}
		vars, ok := snap.renderVarsByTemplate[entry.Name]
		if !ok {
			continue
		}
		hasContext = true
		errors = append(errors, validator.ValidateNamedBlockContent(
			entry.Content,
			vars,
			entry.TemplatePath,
			snap.baseDir,
			snap.templateRoot,
			entry.Line,
			registry,
			snap.funcMaps,
		)...)
	}

	return daemonValidateTemplateResult{
		ValidationErrors: dedupeValidationErrors(errors),
		HasContext:       hasContext,
	}, nil
}

func (d *analyzerDaemon) updateTemplate(params daemonUpdateTemplateParams) error {
	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return err
	}
	d.overlayMu.Lock()
	if d.templateOverlays == nil {
		d.templateOverlays = make(map[string]string)
	}
	d.templateOverlays[absPath] = params.Content
	d.overlayMu.Unlock()
	return nil
}

func (d *analyzerDaemon) clearTemplate(params daemonClearTemplateParams) error {
	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return err
	}
	d.overlayMu.Lock()
	delete(d.templateOverlays, absPath)
	d.overlayMu.Unlock()
	return nil
}

func (d *analyzerDaemon) inferExpressionType(params daemonInferExpressionParams) (*validator.ExpressionTypeResult, error) {
	snap := d.state.Load()
	if snap == nil {
		return nil, fmt.Errorf("daemon not initialized")
	}

	// Read-only: no cloning needed.
	return validator.InferExpressionType(
		params.Expression,
		params.Vars,
		params.ScopeStack,
		params.BlockLocals,
		snap.funcMaps,
		snap.typeRegistry,
	), nil
}

func (d *analyzerDaemon) getHoverInfo(params daemonGetHoverInfoParams) (*validator.HoverResult, error) {
	snap := d.state.Load()
	if snap == nil {
		return nil, fmt.Errorf("daemon not initialized")
	}

	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return nil, err
	}

	// Load overlays under read lock.
	d.overlayMu.RLock()
	overlays := cloneTemplateOverlays(d.templateOverlays)
	d.overlayMu.RUnlock()

	content := params.Content
	if content == "" {
		if overlay, ok := overlays[absPath]; ok {
			content = overlay
		}
	}
	if content == "" {
		return nil, fmt.Errorf("no content for %s", absPath)
	}

	templateBase := filepath.Join(snap.baseDir, snap.templateRoot)
	rel, err := filepath.Rel(templateBase, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	registry := snap.namedBlocks
	if len(overlays) > 0 {
		registry = cloneRegistry(snap.namedBlocks)
		applyTemplateOverlays(registry, overlays, snap.baseDir, snap.templateRoot)
	}

	_, vars, ok := findRenderVarsForTemplate(snap.renderVarsByTemplate, absPath, snap.baseDir, snap.templateRoot)
	if !ok {
		return nil, nil
	}

	varMap := make(map[string]ast.TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}

	result := validator.GetHoverResult(
		content, varMap, rel, snap.baseDir, snap.templateRoot,
		0,
		params.Line, params.Col,
		registry, snap.funcMaps, snap.typeRegistry,
	)
	return result, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func findRenderVarsForTemplate(
	renderVarsByTemplate map[string][]ast.TemplateVar,
	absPath, baseDir, templateRoot string,
) (string, []ast.TemplateVar, bool) {
	templateBase := filepath.Join(baseDir, templateRoot)
	rel := normalizeTemplateKey(absPath)
	if relPath, err := filepath.Rel(templateBase, absPath); err == nil {
		rel = normalizeTemplateKey(relPath)
	}

	if vars, ok := renderVarsByTemplate[rel]; ok {
		return rel, vars, true
	}

	for key, vars := range renderVarsByTemplate {
		normalizedKey := normalizeTemplateKey(key)
		candidateAbs := filepath.Join(templateBase, normalizedKey)
		if normalizePath(candidateAbs) == normalizePath(absPath) {
			return key, vars, true
		}
		if strings.HasSuffix(rel, normalizedKey) || strings.HasSuffix(normalizedKey, rel) {
			return key, vars, true
		}
	}

	baseName := filepath.Base(absPath)
	for key, vars := range renderVarsByTemplate {
		if filepath.Base(normalizeTemplateKey(key)) == baseName {
			return key, vars, true
		}
	}

	return "", nil, false
}

func buildRenderVarIndex(renderCalls []ast.RenderCall) map[string][]ast.TemplateVar {
	idx := make(map[string][]ast.TemplateVar, len(renderCalls))
	seen := make(map[string]map[string]bool, len(renderCalls))

	for _, rc := range renderCalls {
		if _, ok := idx[rc.Template]; !ok {
			idx[rc.Template] = nil
			seen[rc.Template] = make(map[string]bool)
		}
		for _, v := range rc.Vars {
			if !seen[rc.Template][v.Name] {
				seen[rc.Template][v.Name] = true
				idx[rc.Template] = append(idx[rc.Template], v)
			}
		}
	}

	return idx
}

func cloneRegistry(in map[string][]validator.NamedBlockEntry) map[string][]validator.NamedBlockEntry {
	out := make(map[string][]validator.NamedBlockEntry, len(in))
	for key, entries := range in {
		out[key] = append([]validator.NamedBlockEntry(nil), entries...)
	}
	return out
}

func cloneTemplateOverlays(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func applyTemplateOverlays(registry map[string][]validator.NamedBlockEntry, overlays map[string]string, baseDir, templateRoot string) {
	templateBase := filepath.Join(baseDir, templateRoot)
	for absolutePath, content := range overlays {
		if rel, err := filepath.Rel(templateBase, absolutePath); err == nil {
			replaceRegistryEntriesForFile(registry, absolutePath, content, filepath.ToSlash(rel))
			continue
		}
		replaceRegistryEntriesForFile(registry, absolutePath, content, absolutePath)
	}
}

func replaceRegistryEntriesForFile(registry map[string][]validator.NamedBlockEntry, absolutePath, content, templatePath string) {
	normalizedPath := normalizePath(absolutePath)
	for name, entries := range registry {
		filtered := entries[:0]
		for _, entry := range entries {
			if normalizePath(entry.AbsolutePath) != normalizedPath {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(registry, name)
			continue
		}
		registry[name] = filtered
	}

	validator.ExtractNamedTemplatesFromContent(content, absolutePath, templatePath, registry)

	registry[templatePath] = append(registry[templatePath], validator.NamedBlockEntry{
		Name:         templatePath,
		AbsolutePath: absolutePath,
		TemplatePath: templatePath,
		Line:         1,
		Col:          1,
		Content:      content,
	})
}

func registryEntriesForFile(registry map[string][]validator.NamedBlockEntry, absolutePath string) []validator.NamedBlockEntry {
	normalizedPath := normalizePath(absolutePath)
	entries := make([]validator.NamedBlockEntry, 0)
	for _, blockEntries := range registry {
		for _, entry := range blockEntries {
			if normalizePath(entry.AbsolutePath) == normalizedPath {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// dedupKey is a struct used to identify unique validation errors for deduplication purposes.
// Since all fields are comparable, we can use this as a key in a map without memory allocation.
type dedupKey struct {
	Template string
	Line     int
	Column   int
	Variable string
	Message  string
}

func dedupeValidationErrors(in []validator.ValidationResult) []validator.ValidationResult {
	seen := make(map[dedupKey]bool, len(in))
	out := make([]validator.ValidationResult, 0, len(in))
	for _, err := range in {
		key := dedupKey{err.Template, err.Line, err.Column, err.Variable, err.Message}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, err)
	}
	return out
}

func normalizePath(value string) string {
	return filepath.Clean(strings.ToLower(value))
}

func normalizeTemplateKey(value string) string {
	cleaned := filepath.ToSlash(filepath.Clean(value))
	return strings.TrimPrefix(cleaned, "./")
}
