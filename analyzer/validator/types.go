package validator

import "github.com/rex-template-analyzer/ast"

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
	Fields []ast.FieldInfo

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
