package validator

// TemplateVar represents a variable available in a template context
type TemplateVar struct {
	Name     string      `json:"name"`
	TypeStr  string      `json:"type"`
	Fields   []FieldInfo `json:"fields,omitempty"`
	IsSlice  bool        `json:"isSlice"`
	ElemType string      `json:"elemType,omitempty"`
}

// FieldInfo represents a field in a struct type
type FieldInfo struct {
	Name    string      `json:"name"`
	TypeStr string      `json:"type"`
	Fields  []FieldInfo `json:"fields,omitempty"`
	IsSlice bool        `json:"isSlice"`
	Methods []string    `json:"methods,omitempty"`
}

// RenderCall represents a c.Render() call found in Go source
type RenderCall struct {
	File     string        `json:"file"`
	Line     int           `json:"line"`
	Template string        `json:"template"`
	Vars     []TemplateVar `json:"vars"`
}

// ValidationResult represents a validation error
type ValidationResult struct {
	Template string `json:"template"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Variable string `json:"variable"`
	Message  string `json:"message"`
	Severity string `json:"severity"` // "error" or "warning"
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
	Fields   []FieldInfo
	IsSlice  bool
}
