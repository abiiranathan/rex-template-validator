// Package lsp implements a JSON-RPC 2.0 LSP daemon that exposes the
// rex-template-analyzer on demand, avoiding the cost of a full upfront
// analysis and the memory pressure of keeping a giant knowledge graph alive.
package lsp

import "encoding/json"

const jsonrpcVersion = "2.0"

// RequestMessage is a JSON-RPC 2.0 request (has an id).
type RequestMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ResponseMessage is a JSON-RPC 2.0 response.
type ResponseMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// NotificationMessage is a JSON-RPC 2.0 notification (no id).
type NotificationMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ResponseError is the JSON-RPC 2.0 error object.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC 2.0 error codes.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// ─── Custom method param / result types ───────────────────────────────────────

// GetTemplateContextParams is the request body for rex/getTemplateContext.
// The extension calls this when it needs completions or hover info for a
// specific template file, passing only the template it currently has open.
type GetTemplateContextParams struct {
	// Dir is the Go workspace root (where go.mod lives).
	Dir string `json:"dir"`
	// TemplateName is the relative template path, e.g. "partials/user.html".
	TemplateName string `json:"templateName"`
	// TemplateRoot is the template sub-directory, e.g. "templates".
	TemplateRoot string `json:"templateRoot,omitempty"`
	// ContextFile is an optional JSON file with additional variable hints.
	ContextFile string `json:"contextFile,omitempty"`
}

// GetTemplateContextResult is the response for rex/getTemplateContext.
type GetTemplateContextResult struct {
	Vars   []TemplateVarJSON `json:"vars"`
	Errors []string          `json:"errors,omitempty"`
}

// ValidateParams is the request body for rex/validate.
// The extension calls this whenever a template file is saved or opened.
type ValidateParams struct {
	Dir          string `json:"dir"`
	TemplateName string `json:"templateName"`
	TemplateRoot string `json:"templateRoot,omitempty"`
	ContextFile  string `json:"contextFile,omitempty"`
}

// ValidateResult is the response for rex/validate.
type ValidateResult struct {
	Errors []ValidationResultJSON `json:"errors"`
}

// GetFuncMapsParams is the request body for rex/getFuncMaps.
type GetFuncMapsParams struct {
	Dir         string `json:"dir"`
	ContextFile string `json:"contextFile,omitempty"`
}

// GetFuncMapsResult is the response for rex/getFuncMaps.
type GetFuncMapsResult struct {
	FuncMaps []FuncMapInfoJSON `json:"funcMaps"`
	Errors   []string          `json:"errors,omitempty"`
}

// GetNamedBlocksParams is the request body for rex/getNamedBlocks.
type GetNamedBlocksParams struct {
	Dir          string `json:"dir"`
	TemplateRoot string `json:"templateRoot,omitempty"`
}

// GetNamedBlocksResult is the response for rex/getNamedBlocks.
type GetNamedBlocksResult struct {
	NamedBlocks     map[string][]NamedBlockEntryJSON `json:"namedBlocks"`
	DuplicateErrors []NamedBlockDuplicateErrorJSON   `json:"duplicateErrors,omitempty"`
}

// GetRenderCallsParams is the request body for rex/getRenderCalls.
type GetRenderCallsParams struct {
	Dir         string `json:"dir"`
	ContextFile string `json:"contextFile,omitempty"`
}

// GetRenderCallsResult is the response for rex/getRenderCalls.
type GetRenderCallsResult struct {
	RenderCalls []RenderCallJSON `json:"renderCalls"`
	Errors      []string         `json:"errors,omitempty"`
}

// InvalidateCacheParams is the body for the rex/invalidateCache notification.
// The extension should send this whenever Go source files change on disk.
type InvalidateCacheParams struct {
	Dir string `json:"dir"`
}

// ─── Stable JSON-serialisable mirrors of ast / validator types ───────────────
//
// We re-declare these as plain structs so the protocol layer does not
// directly depend on the internal ast package types, making the boundary
// easy to version independently.

// FieldInfoJSON mirrors ast.FieldInfo.
type FieldInfoJSON struct {
	Name     string          `json:"name"`
	TypeStr  string          `json:"type"`
	Fields   []FieldInfoJSON `json:"fields,omitempty"`
	IsSlice  bool            `json:"isSlice"`
	IsMap    bool            `json:"isMap"`
	KeyType  string          `json:"keyType,omitempty"`
	ElemType string          `json:"elemType,omitempty"`
	Params   []ParamInfoJSON `json:"params,omitempty"`
	Returns  []ParamInfoJSON `json:"returns,omitempty"`
	DefFile  string          `json:"defFile,omitempty"`
	DefLine  int             `json:"defLine,omitempty"`
	DefCol   int             `json:"defCol,omitempty"`
	Doc      string          `json:"doc,omitempty"`
}

// ParamInfoJSON mirrors ast.ParamInfo.
type ParamInfoJSON struct {
	Name    string          `json:"name,omitempty"`
	TypeStr string          `json:"type"`
	Fields  []FieldInfoJSON `json:"fields,omitempty"`
}

// TemplateVarJSON mirrors ast.TemplateVar.
type TemplateVarJSON struct {
	Name     string          `json:"name"`
	TypeStr  string          `json:"type"`
	Fields   []FieldInfoJSON `json:"fields,omitempty"`
	IsSlice  bool            `json:"isSlice"`
	IsMap    bool            `json:"isMap"`
	KeyType  string          `json:"keyType,omitempty"`
	ElemType string          `json:"elemType,omitempty"`
	DefFile  string          `json:"defFile,omitempty"`
	DefLine  int             `json:"defLine,omitempty"`
	DefCol   int             `json:"defCol,omitempty"`
	Doc      string          `json:"doc,omitempty"`
}

// FuncMapInfoJSON mirrors ast.FuncMapInfo.
type FuncMapInfoJSON struct {
	Name             string          `json:"name"`
	Params           []ParamInfoJSON `json:"params,omitempty"`
	Args             []string        `json:"args"`
	Returns          []ParamInfoJSON `json:"returns"`
	Doc              string          `json:"doc,omitempty"`
	DefFile          string          `json:"defFile,omitempty"`
	DefLine          int             `json:"defLine,omitempty"`
	DefCol           int             `json:"defCol,omitempty"`
	ReturnTypeFields []FieldInfoJSON `json:"returnTypeFields,omitempty"`
}

// RenderCallJSON mirrors ast.RenderCall.
type RenderCallJSON struct {
	File                 string            `json:"file"`
	Line                 int               `json:"line"`
	Template             string            `json:"template"`
	TemplateNameStartCol int               `json:"templateNameStartCol,omitempty"`
	TemplateNameEndCol   int               `json:"templateNameEndCol,omitempty"`
	Vars                 []TemplateVarJSON `json:"vars"`
}

// ValidationResultJSON mirrors validator.ValidationResult.
type ValidationResultJSON struct {
	Template             string `json:"template"`
	Line                 int    `json:"line"`
	Column               int    `json:"column"`
	Variable             string `json:"variable"`
	Message              string `json:"message"`
	Severity             string `json:"severity"`
	GoFile               string `json:"goFile,omitempty"`
	GoLine               int    `json:"goLine,omitempty"`
	TemplateNameStartCol int    `json:"templateNameStartCol,omitempty"`
	TemplateNameEndCol   int    `json:"templateNameEndCol,omitempty"`
}

// NamedBlockEntryJSON mirrors validator.NamedBlockEntry (without Content).
type NamedBlockEntryJSON struct {
	Name         string `json:"name"`
	AbsolutePath string `json:"absolutePath"`
	TemplatePath string `json:"templatePath"`
	Line         int    `json:"line"`
	Col          int    `json:"col"`
}

// NamedBlockDuplicateErrorJSON mirrors validator.NamedBlockDuplicateError.
type NamedBlockDuplicateErrorJSON struct {
	Name    string                `json:"name"`
	Entries []NamedBlockEntryJSON `json:"entries"`
	Message string                `json:"message"`
}

// ─── LSP lifecycle types ──────────────────────────────────────────────────────

// InitializeParams are the minimal LSP initialize request params we care about.
type InitializeParams struct {
	RootURI  string `json:"rootUri,omitempty"`
	RootPath string `json:"rootPath,omitempty"`
}

// InitializeResult is the LSP initialize response.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   ServerInfo         `json:"serverInfo"`
}

// ServerCapabilities advertises what this server can do.
// We don't implement any standard LSP capabilities — all features are
// delivered through custom rex/* methods.
type ServerCapabilities struct{}

// ServerInfo identifies the server in the initialize response.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DidChangeWatchedFilesParams is a standard LSP notification for file changes.
// The extension should register a file watcher for **/*.go and send this
// notification whenever Go source files are created/modified/deleted.
type DidChangeWatchedFilesParams struct {
	Changes []FileEvent `json:"changes"`
}

// FileEvent describes a single file change.
type FileEvent struct {
	// URI is a file:// URI.
	URI string `json:"uri"`
	// Type: 1=created, 2=changed, 3=deleted
	Type int `json:"type"`
}
