package validator

// TemplateVar represents a variable available in a template context, including its type, fields, and definition location.
type TemplateVar struct {
	// Name is the name of the template variable.
	Name string `json:"name"`
	// TypeStr is the string representation of the variable's type (e.g., "string", "*MyStruct").
	TypeStr string `json:"type"`
	// Fields contains information about the variable's exported fields if it is a struct or an embedded struct.
	Fields []FieldInfo `json:"fields,omitempty"`
	// IsSlice indicates if the variable is a slice type.
	IsSlice bool `json:"isSlice"`
	// IsMap indicates if the variable is a map type.
	IsMap bool `json:"isMap"`
	// KeyType is the string representation of the map's key type, if IsMap is true.
	KeyType string `json:"keyType,omitempty"`
	// ElemType is the string representation of the slice's or map's element type, if IsSlice or IsMap is true.
	ElemType string `json:"elemType,omitempty"`

	// DefFile is the Go file where the variable is defined.
	DefFile string `json:"defFile,omitempty"`
	// DefLine is the line number where the variable is defined.
	DefLine int `json:"defLine,omitempty"`
	// DefCol is the column number where the variable is defined.
	DefCol int `json:"defCol,omitempty"`
	// Doc is the documentation comment for the type of the variable.
	Doc string `json:"doc,omitempty"`
}

// FieldInfo represents an exported field or method within a struct type.
type FieldInfo struct {
	// Name is the name of the field or method.
	Name string `json:"name"`
	// TypeStr is the string representation of the field's or method's type (e.g., "string", "func(int) string").
	TypeStr string `json:"type"`
	// Fields contains information about nested exported fields if this field is a struct or an embedded struct.
	Fields []FieldInfo `json:"fields,omitempty"`
	// IsSlice indicates if the field is a slice type.
	IsSlice bool `json:"isSlice"`
	// IsMap indicates if the field is a map type.
	IsMap bool `json:"isMap"`
	// KeyType is the string representation of the map's key type, if IsMap is true.
	KeyType string `json:"keyType,omitempty"`
	// ElemType is the string representation of the slice's or map's element type, if IsSlice or IsMap is true.
	ElemType string `json:"elemType,omitempty"`
	// Methods (deprecated) - will be empty, method info is now in Fields.
	Methods []string `json:"methods,omitempty"`
	// Params are the parameters of the method, if this FieldInfo represents a method.
	Params []ParamInfo `json:"params,omitempty"`
	// Returns are the return values of the method, if this FieldInfo represents a method.
	Returns []ParamInfo `json:"returns,omitempty"`
	// DefFile is the Go file where the field or method is defined.
	DefFile string `json:"defFile,omitempty"`
	// DefLine is the line number where the field or method is defined.
	DefLine int `json:"defLine,omitempty"`
	// DefCol is the column number where the field or method is defined.
	DefCol int `json:"defCol,omitempty"`
	// Doc is the documentation comment for the field or method.
	Doc string `json:"doc,omitempty"`
}

// RenderCall represents a detected template rendering invocation in Go source code.
type RenderCall struct {
	// File is the path to the Go file where the render call occurs.
	File string `json:"file"`
	// Line is the line number in the Go file where the render call starts.
	Line int `json:"line"`
	// Template is the name or path of the template being rendered.
	Template string `json:"template"`
	// TemplateNameStartCol is the starting column of the template name literal in the Go file.
	TemplateNameStartCol int `json:"templateNameStartCol,omitempty"`
	// TemplateNameEndCol is the ending column of the template name literal in the Go file.
	TemplateNameEndCol int `json:"templateNameEndCol,omitempty"`
	// Vars are the template variables explicitly passed to this render call.
	Vars []TemplateVar `json:"vars"`
}

// ValidationResult represents a single diagnostic (error or warning) found during template validation.
type ValidationResult struct {
	// Template is the name or path of the template where the issue was found.
	Template string `json:"template"`
	// Line is the line number within the template file where the issue occurs.
	Line int `json:"line"`
	// Column is the column number within the template file where the issue occurs.
	Column int `json:"column"`
	// Variable is the name of the template variable or expression that caused the issue.
	Variable string `json:"variable"`
	// Message is a human-readable description of the validation issue.
	Message string `json:"message"`
	// Severity indicates the severity of the issue (e.g., "error", "warning").
	Severity string `json:"severity"`
	// GoFile is the path to the Go file that rendered the template, if applicable.
	GoFile string `json:"goFile,omitempty"`
	// GoLine is the line number in the Go file that rendered the template, if applicable.
	GoLine int `json:"goLine,omitempty"`
	// TemplateNameStartCol is the starting column of the template name literal in the Go file, if applicable.
	TemplateNameStartCol int `json:"templateNameStartCol,omitempty"`
	// TemplateNameEndCol is the ending column of the template name literal in the Go file, if applicable.
	TemplateNameEndCol int `json:"templateNameEndCol,omitempty"`
}

// AnalysisResult is the top-level output structure containing all static analysis findings.
type AnalysisResult struct {
	// RenderCalls lists all identified template rendering invocations.
	RenderCalls []RenderCall `json:"renderCalls"`
	// FuncMaps lists all discovered template function map declarations.
	FuncMaps []FuncMapInfo `json:"funcMaps"`
	// Errors contains any non-fatal errors encountered during the analysis process.
	Errors []string `json:"errors"`
}

// FuncMapInfo represents a template function registered in a `template.FuncMap`.
type FuncMapInfo struct {
	// Name is the name of the function as it appears in the template.FuncMap.
	Name string `json:"name"`
	// Params describes the parameters of the function.
	Params []ParamInfo `json:"params,omitempty"`
	// Args (deprecated) - a slice of parameter type strings, use Params instead.
	Args []string `json:"args"`
	// Returns describes the return values of the function.
	Returns []ParamInfo `json:"returns"`
	// Doc is the documentation comment for the function.
	Doc string `json:"doc,omitempty"`
	// DefFile is the Go file where the function is defined.
	DefFile string `json:"defFile,omitempty"`
	// DefLine is the line number where the function is defined.
	DefLine int `json:"defLine,omitempty"`
	// DefCol is the column number where the function is defined.
	DefCol int `json:"defCol,omitempty"`
}

// ParamInfo represents a single function parameter or return value with its
// name (which may be empty for unnamed params) and resolved type string.
type ParamInfo struct {
	// Name is the name of the parameter or return value (can be empty for unnamed).
	Name string `json:"name,omitempty"`
	// TypeStr is the string representation of the parameter's or return value's type.
	TypeStr string `json:"type"`
}

// ScopeType represents the contextual scope within a template, tracking available variables and their types.
type ScopeType struct {
	// IsRoot indicates if this is the top-level scope.
	IsRoot bool
	// VarName is the name of the variable that established this scope (e.g., in a `with` or `range` action).
	VarName string
	// TypeStr is the string representation of the scope's underlying type.
	TypeStr string
	// ElemType is the element type if the scope is over a slice (e.g., "handlers.Breadcrumb").
	ElemType string
	// KeyType is the key type if the scope is over a map.
	KeyType string
	// Fields lists the exported fields of the current scope's type.
	Fields []FieldInfo
	// IsSlice indicates if the current scope represents a slice.
	IsSlice bool
	// IsMap indicates if the current scope represents a map.
	IsMap bool
}

// NamedBlockEntry represents a {{define}} or {{block}} declaration found within a template file.
type NamedBlockEntry struct {
	// Name is the name of the defined or blocked template.
	Name string `json:"name"`
	// AbsolutePath is the absolute path to the template file containing the block.
	AbsolutePath string `json:"absolutePath"`
	// TemplatePath is the relative path or logical name of the template.
	TemplatePath string `json:"templatePath"`
	// Line is the starting line number of the block declaration in the template file.
	Line int `json:"line"`
	// Col is the starting column number of the block declaration in the template file.
	Col int `json:"col"`
	// Content is the raw content of the named block. It is omitted from JSON output.
	Content string `json:"-"`
}

// NamedBlockDuplicateError is reported when multiple template blocks with the same name are found across the project.
type NamedBlockDuplicateError struct {
	// Name is the name of the duplicated block.
	Name string `json:"name"`
	// Entries lists all occurrences of the duplicated named block.
	Entries []NamedBlockEntry `json:"entries"`
	// Message is a human-readable error message describing the duplication.
	Message string `json:"message"`
}

// AnalysisConfig defines customizable function and type names used by the analyzer to identify template-related constructs.
type AnalysisConfig struct {
	// RenderFunctionName is the name of the function or method used to render templates (default: "Render").
	RenderFunctionName string
	// ExecuteTemplateFunctionName is an alternative function name for rendering templates (default: "ExecuteTemplate").
	ExecuteTemplateFunctionName string
	// SetFunctionName is the name of the method used to explicitly set context variables within a template (default: "Set").
	SetFunctionName string
	// ContextTypeName is the name of the Go type that represents the template execution context (default: "Context").
	ContextTypeName string
	// GlobalTemplateName is the special key used in the context file to define global template variables (default: "global").
	GlobalTemplateName string
}

// DefaultConfig provides the default configuration for the Rex template analyzer, tailored for common Rex framework conventions.
var DefaultConfig = AnalysisConfig{
	RenderFunctionName:          "Render",
	ExecuteTemplateFunctionName: "ExecuteTemplate",
	SetFunctionName:             "Set",
	ContextTypeName:             "Context",
	GlobalTemplateName:          "global",
}
