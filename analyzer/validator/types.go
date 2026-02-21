package validator

// TemplateVar represents a variable available in a template context
type TemplateVar struct {
	Name     string      `json:"name"`
	TypeStr  string      `json:"type"`
	Fields   []FieldInfo `json:"fields,omitempty"`
	IsSlice  bool        `json:"isSlice"`
	IsMap    bool        `json:"isMap"`
	KeyType  string      `json:"keyType,omitempty"`
	ElemType string      `json:"elemType,omitempty"`

	// Definition location in Go source (for go-to-definition)
	DefFile string `json:"defFile,omitempty"` // Go file where the variable is defined
	DefLine int    `json:"defLine,omitempty"` // Line number where the variable is defined
	DefCol  int    `json:"defCol,omitempty"`  // Column number where the variable is defined
	// Documentation
	Doc string `json:"doc,omitempty"` // Documentation comment for the type
}

// FieldInfo represents a field in a struct type
type FieldInfo struct {
	Name     string      `json:"name"`
	TypeStr  string      `json:"type"`
	Fields   []FieldInfo `json:"fields,omitempty"`
	IsSlice  bool        `json:"isSlice"`
	IsMap    bool        `json:"isMap"`
	KeyType  string      `json:"keyType,omitempty"`
	ElemType string      `json:"elemType,omitempty"`
	Methods  []string    `json:"methods,omitempty"`
	// Definition location in Go source (for go-to-definition)
	DefFile string `json:"defFile,omitempty"` // Go file where the field is defined
	DefLine int    `json:"defLine,omitempty"` // Line number where the field is defined
	DefCol  int    `json:"defCol,omitempty"`  // Column number where the field is defined
	// Documentation
	Doc string `json:"doc,omitempty"` // Documentation comment for the field
}

// RenderCall represents a c.Render() call found in Go source
type RenderCall struct {
	File                 string        `json:"file"`
	Line                 int           `json:"line"`
	Template             string        `json:"template"`
	TemplateNameStartCol int           `json:"templateNameStartCol,omitempty"`
	TemplateNameEndCol   int           `json:"templateNameEndCol,omitempty"`
	Vars                 []TemplateVar `json:"vars"`
}

// ValidationResult represents a validation error
type ValidationResult struct {
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

// AnalysisResult is the top-level output
type AnalysisResult struct {
	RenderCalls []RenderCall `json:"renderCalls"`
	Errors      []string     `json:"errors"`
}

// ScopeType represents the type of a scope (root or element type in a range)
type ScopeType struct {
	IsRoot   bool
	VarName  string // Name of the variable (e.g., "breadcrumbs")
	TypeStr  string // Type string (e.g., "handlers.Breadcrumbs")
	ElemType string // For slices: element type (e.g., "handlers.Breadcrumb")
	KeyType  string // For maps: key type
	Fields   []FieldInfo
	IsSlice  bool
	IsMap    bool
}

// AnalysisConfig defines the customizable function and type names for analysis
type AnalysisConfig struct {
	// RenderFunctionName is the name of the function/method used to render templates (default: "Render")
	RenderFunctionName string
	// ExecuteTemplateFunctionName is an alternative rendering method (default: "ExecuteTemplate")
	ExecuteTemplateFunctionName string
	// SetFunctionName is the name of the method used to set context variables (default: "Set")
	SetFunctionName string
	// ContextTypeName is the name of the Context type (default: "Context")
	ContextTypeName string
	// GlobalTemplateName is the key used in context file for global variables (default: "global")
	GlobalTemplateName string
}

// DefaultConfig provides the default configuration for Rex framework
var DefaultConfig = AnalysisConfig{
	RenderFunctionName:          "Render",
	ExecuteTemplateFunctionName: "ExecuteTemplate",
	SetFunctionName:             "Set",
	ContextTypeName:             "Context",
	GlobalTemplateName:          "global",
}
