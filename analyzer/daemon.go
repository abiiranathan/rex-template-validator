package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
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

type analyzerDaemon struct {
	mu                   sync.RWMutex
	initialized          bool
	dir                  string
	baseDir              string
	templateRoot         string
	contextFile          string
	validate             bool
	output               ValidationOutput
	renderVarsByTemplate map[string][]ast.TemplateVar
	funcMaps             validator.FuncMapRegistry
	typeRegistry         map[string][]ast.FieldInfo
	namedBlocks          map[string][]validator.NamedBlockEntry
	templateOverlays     map[string]string
	partialTargets       map[string]bool
}

func runDaemon(stdin io.Reader, stdout io.Writer) error {
	server := &analyzerDaemon{}
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

		line = bytesTrimSpace(line)
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

	// Build the render-var index before Flatten() so each variable retains its
	// full field tree. Flatten() strips Fields from all vars to reduce JSON size;
	// if we built the index afterwards the daemon would validate templates with
	// field-less vars, silently passing every field-access check and clearing
	// the analyzer diagnostics on the first live-edit cycle.
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

	d.mu.Lock()
	d.initialized = true
	d.dir = params.Dir
	d.baseDir = baseDir
	d.templateRoot = params.TemplateRoot
	d.contextFile = params.ContextFile
	d.validate = params.Validate
	d.output = output
	d.renderVarsByTemplate = renderVarIndex // use pre-flatten index
	d.funcMaps = validator.BuildFuncMapRegistry(result.FuncMaps)
	d.typeRegistry = cloneTypeRegistry(result.Types)
	d.namedBlocks = namedBlocks
	d.partialTargets = validator.FindPartialTargets(baseDir, params.TemplateRoot)
	if d.templateOverlays == nil {
		d.templateOverlays = make(map[string]string)
	}
	d.mu.Unlock()

	return output, nil
}

func (d *analyzerDaemon) validateTemplate(params daemonValidateTemplateParams) (result daemonValidateTemplateResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("validateTemplate panic: %v", recovered)
		}
	}()

	d.mu.RLock()
	if !d.initialized {
		d.mu.RUnlock()
		return daemonValidateTemplateResult{}, fmt.Errorf("daemon not initialized")
	}

	baseDir := d.baseDir
	templateRoot := d.templateRoot
	validate := d.validate
	renderVarsByTemplate := cloneRenderVarIndex(d.renderVarsByTemplate)
	funcMaps := mapsCloneFuncMaps(d.funcMaps)
	registry := cloneRegistry(d.namedBlocks)
	overlays := cloneTemplateOverlays(d.templateOverlays)
	partialTargets := clonePartialTargets(d.partialTargets)
	d.mu.RUnlock()

	if !validate {
		return daemonValidateTemplateResult{HasContext: false}, nil
	}

	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return daemonValidateTemplateResult{}, err
	}

	templateBase := filepath.Join(baseDir, templateRoot)
	rel, err := filepath.Rel(templateBase, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	overlays[absPath] = params.Content
	applyTemplateOverlays(registry, overlays, baseDir, templateRoot)

	var errors []validator.ValidationResult
	hasContext := false

	if _, vars, ok := findRenderVarsForTemplate(renderVarsByTemplate, absPath, baseDir, templateRoot); ok {
		hasContext = true
		errors = append(errors, validator.ValidateTemplateFileStr(
			params.Content,
			vars,
			rel,
			baseDir,
			templateRoot,
			registry,
			funcMaps,
		)...)
	}

	for _, entry := range registryEntriesForFile(registry, absPath) {
		if entry.Name == entry.TemplatePath {
			continue
		}
		// Skip named blocks that are partial targets (called via {{ template "name" }}
		// from other templates). These blocks will be validated via their callers'
		// recursive validation with the correct scope, matching CLI behavior.
		if partialTargets[entry.Name] {
			continue
		}
		vars, ok := renderVarsByTemplate[entry.Name]
		if !ok {
			continue
		}
		hasContext = true
		errors = append(errors, validator.ValidateNamedBlockContent(
			entry.Content,
			vars,
			entry.TemplatePath,
			baseDir,
			templateRoot,
			entry.Line,
			registry,
			funcMaps,
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

	d.mu.Lock()
	if d.templateOverlays == nil {
		d.templateOverlays = make(map[string]string)
	}
	d.templateOverlays[absPath] = params.Content
	d.mu.Unlock()
	return nil
}

func (d *analyzerDaemon) clearTemplate(params daemonClearTemplateParams) error {
	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return err
	}

	d.mu.Lock()
	delete(d.templateOverlays, absPath)
	d.mu.Unlock()
	return nil
}

func (d *analyzerDaemon) inferExpressionType(params daemonInferExpressionParams) (*validator.ExpressionTypeResult, error) {
	d.mu.RLock()
	if !d.initialized {
		d.mu.RUnlock()
		return nil, fmt.Errorf("daemon not initialized")
	}
	funcMaps := mapsCloneFuncMaps(d.funcMaps)
	typeRegistry := cloneTypeRegistry(d.typeRegistry)
	d.mu.RUnlock()

	return validator.InferExpressionType(
		params.Expression,
		params.Vars,
		params.ScopeStack,
		params.BlockLocals,
		funcMaps,
		typeRegistry,
	), nil
}

func (d *analyzerDaemon) getHoverInfo(params daemonGetHoverInfoParams) (*validator.HoverResult, error) {
	d.mu.RLock()
	if !d.initialized {
		d.mu.RUnlock()
		return nil, fmt.Errorf("daemon not initialized")
	}
	baseDir := d.baseDir
	templateRoot := d.templateRoot
	renderVarsByTemplate := cloneRenderVarIndex(d.renderVarsByTemplate)
	funcMaps := mapsCloneFuncMaps(d.funcMaps)
	typeRegistry := cloneTypeRegistry(d.typeRegistry)
	registry := cloneRegistry(d.namedBlocks)
	overlays := cloneTemplateOverlays(d.templateOverlays)
	d.mu.RUnlock()

	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return nil, err
	}

	content := params.Content
	if content == "" {
		if overlay, ok := overlays[absPath]; ok {
			content = overlay
		}
	}
	if content == "" {
		return nil, fmt.Errorf("no content for %s", absPath)
	}

	templateBase := filepath.Join(baseDir, templateRoot)
	rel, err := filepath.Rel(templateBase, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	applyTemplateOverlays(registry, overlays, baseDir, templateRoot)

	// Find the render vars for this template file.
	_, vars, ok := findRenderVarsForTemplate(renderVarsByTemplate, absPath, baseDir, templateRoot)
	if !ok {
		return nil, nil
	}

	varMap := make(map[string]ast.TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}

	result := validator.GetHoverResult(
		content, varMap, rel, baseDir, templateRoot,
		0, // lineOffset — top-level file
		params.Line, params.Col,
		registry, funcMaps, typeRegistry,
	)
	return result, nil
}

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

func cloneTypeRegistry(input map[string][]ast.FieldInfo) map[string][]ast.FieldInfo {
	if input == nil {
		return nil
	}
	cloned := make(map[string][]ast.FieldInfo, len(input))
	for key, value := range input {
		fields := make([]ast.FieldInfo, len(value))
		copy(fields, value)
		cloned[key] = fields
	}
	return cloned
}

func cloneRenderVarIndex(in map[string][]ast.TemplateVar) map[string][]ast.TemplateVar {
	out := make(map[string][]ast.TemplateVar, len(in))
	for key, vars := range in {
		out[key] = append([]ast.TemplateVar(nil), vars...)
	}
	return out
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

func clonePartialTargets(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mapsCloneFuncMaps(in validator.FuncMapRegistry) validator.FuncMapRegistry {
	out := make(validator.FuncMapRegistry, len(in))
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

func dedupeValidationErrors(in []validator.ValidationResult) []validator.ValidationResult {
	seen := make(map[string]bool, len(in))
	out := make([]validator.ValidationResult, 0, len(in))
	for _, err := range in {
		key := fmt.Sprintf("%s|%d|%d|%s|%s", err.Template, err.Line, err.Column, err.Variable, err.Message)
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

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}
