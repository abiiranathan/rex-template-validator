package ast

import goast "go/ast"

// TemplateVar represents a variable available in a template context, including its type, fields, and definition location.
type TemplateVar struct {
	// Name is the name of the template variable.
	Name string `json:"name"`
	// TypeStr is the string representation of the variable's type (e.g., "string", "*MyStruct").
	TypeStr string `json:"type"`
	// Fields contains information about the variable's exported fields if it is a struct or an embedded struct.
	// After Flatten is called this slice is nil; consumers resolve fields via the Types registry.
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
	// After Flatten is called this slice is nil; consumers resolve sub-types via the Types registry.
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

// AnalysisResult is the top-level output structure containing all static analysis findings.
type AnalysisResult struct {
	// RenderCalls lists all identified template rendering invocations.
	RenderCalls []RenderCall `json:"renderCalls"`
	// FuncMaps lists all discovered template function map declarations.
	FuncMaps []FuncMapInfo `json:"funcMaps"`
	// Errors contains any non-fatal errors encountered during the analysis process.
	Errors []string `json:"errors"`

	// Types is the global type registry mapping each named type to its direct
	// (one-level-deep) fields. Populated by BuildTypeRegistry; consumers
	// reconstruct the full type hierarchy by recursively looking up each
	// field's TypeStr in this map, avoiding repeated serialization of identical
	// struct definitions across render calls.
	Types map[string][]FieldInfo `json:"types,omitempty"`
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

	// Fields of the primary return type after unwrapping pointer and slice.
	// e.g. func() *[]MgtHints → fields of MgtHints.
	// The TypeScript extension uses this to provide intellisense inside
	// {{ range $hints }} without needing a separate render-call entry.
	ReturnTypeFields []FieldInfo `json:"returnTypeFields,omitempty"`
}

// ParamInfo represents a single function parameter or return value with its
// name (which may be empty for unnamed params) and resolved type string.
type ParamInfo struct {
	// Name is the name of the parameter or return value (can be empty for unnamed).
	Name string `json:"name,omitempty"`
	// TypeStr is the string representation of the parameter's or return value's type.
	TypeStr string `json:"type"`

	// Fields contains the nested exported fields if this return type is a struct.
	// After Flatten is called this slice is nil; consumers look up via the Types registry.
	Fields []FieldInfo `json:"fields,omitempty"`

	// Doc is the documentation comment for the type.
	Doc string `json:"doc,omitempty"`
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

// FuncScope encapsulates all template-related operations within a single
// function or code block scope.
type FuncScope struct {
	SetVars        []TemplateVar                  // Template variables set via context.Set()
	RenderNodes    []ResolvedRender               // Template render calls found
	FuncMaps       []FuncMapInfo                  // Function map definitions
	MapAssignments map[string]*goast.CompositeLit // Map variable name → composite literal
}

// ResolvedRender represents a template render call with resolved template
// names and argument positions.
type ResolvedRender struct {
	Node           *goast.CallExpr // The actual call expression
	TemplateNames  []string        // Resolved template name(s)
	TemplateArgIdx int             // Index of template name argument
}

// funcWorkUnit wraps an AST node for concurrent processing.
type funcWorkUnit struct {
	node goast.Node
}

// structIndexEntry caches documentation and field metadata for a struct type.
// This prevents redundant AST traversals for the same type.
type structIndexEntry struct {
	doc    string               // Documentation comment for the struct
	fields map[string]fieldInfo // Field name → metadata mapping
}

// fieldInfo captures the source location and documentation of a struct field or method.
type fieldInfo struct {
	file string // Source file path
	line int    // Line number in source
	col  int    // Column number in source
	doc  string // Associated documentation comment
}

// BuildTypeRegistry collects every named type encountered across all render
// calls and function maps into the Types map. Each entry stores only the
// type's direct (one-level-deep) fields; sub-type fields are omitted because
// the consumer resolves the full hierarchy by recursively looking up each
// field's TypeStr in the same registry.
//
// This eliminates the dominant source of JSON bloat: identical struct
// definitions being serialized once per render call rather than once globally.
// A codebase with 200 render calls that all pass a User struct previously
// serialized User's entire field tree 200 times; after this call it is
// serialized exactly once.
//
// BuildTypeRegistry must be called before Flatten so that the full inline
// field trees are still available for traversal. Flatten calls this
// automatically, so direct callers normally do not need to invoke it.
func (r *AnalysisResult) BuildTypeRegistry() {
	if r.Types == nil {
		r.Types = make(map[string][]FieldInfo)
	}

	// registerFieldTree records typeName → shallow fields in the registry and
	// recurses into each field to register referenced sub-types. Registering
	// the type BEFORE recursing ensures cycles (e.g. TreeNode.Children
	// []*TreeNode) are broken on the second visit.
	var registerFieldTree func(typeName string, fields []FieldInfo)
	registerFieldTree = func(typeName string, fields []FieldInfo) {
		if typeName == "" || isPrimitiveType(typeName) {
			return
		}
		if _, exists := r.Types[typeName]; exists {
			return // already registered; also terminates cycles
		}

		// Build a one-level shallow copy: strip nested field trees from each
		// direct field but keep all other metadata (TypeStr, IsSlice, Doc…).
		shallow := make([]FieldInfo, len(fields))
		for i, f := range fields {
			shallow[i] = f
			shallow[i].Fields = nil

			// For method fields, also strip field trees from return ParamInfos
			// now that those types will be registered separately.
			if f.TypeStr == "method" && len(f.Returns) > 0 {
				flatReturns := make([]ParamInfo, len(f.Returns))
				for j, ret := range f.Returns {
					flatReturns[j] = ret
					flatReturns[j].Fields = nil
				}
				shallow[i].Returns = flatReturns
			}
		}
		r.Types[typeName] = shallow

		// Recurse into each direct field to register referenced sub-types.
		for _, f := range fields {
			switch {
			case f.TypeStr == "method":
				// Register the return types of methods.
				for _, ret := range f.Returns {
					registerFieldTree(registryTypeKey(ret.TypeStr), ret.Fields)
				}

			case f.IsSlice || f.IsMap:
				// The inline Fields belong to the element type, not the
				// collection wrapper. Use ElemType when available; fall back
				// to stripping prefixes from TypeStr.
				elemKey := f.ElemType
				if elemKey == "" {
					elemKey = registryTypeKey(f.TypeStr)
				}
				registerFieldTree(registryTypeKey(elemKey), f.Fields)

			default:
				registerFieldTree(registryTypeKey(f.TypeStr), f.Fields)
			}
		}
	}

	// Walk all render call variables.
	for _, rc := range r.RenderCalls {
		for _, v := range rc.Vars {
			var key string
			switch {
			case v.IsSlice || v.IsMap:
				key = v.ElemType
				if key == "" {
					key = registryTypeKey(v.TypeStr)
				}
				key = registryTypeKey(key)
			default:
				key = registryTypeKey(v.TypeStr)
			}
			registerFieldTree(key, v.Fields)
		}
	}

	// Walk FuncMap return types.
	for _, fm := range r.FuncMaps {
		for _, ret := range fm.Returns {
			registerFieldTree(registryTypeKey(ret.TypeStr), ret.Fields)
		}
		// ReturnTypeFields are the unwrapped primary-return-type fields
		// (e.g. func() *[]MgtHints → fields of MgtHints). Register them
		// under the primary return type's key when available.
		if len(fm.Returns) > 0 && len(fm.ReturnTypeFields) > 0 {
			registerFieldTree(registryTypeKey(fm.Returns[0].TypeStr), fm.ReturnTypeFields)
		}
	}
}

// Flatten builds the global type registry (via BuildTypeRegistry) and then
// strips all inline field trees from render call variables, FuncMap entries,
// and the registry entries themselves. The result is a compact JSON payload
// where each named type is serialized exactly once in the Types map rather
// than once per render call.
//
// Flatten must be called after any validation step that relies on inline
// field trees (e.g. validator.ValidateTemplates) and before JSON serialization.
func (r *AnalysisResult) Flatten() {
	// Populate the registry before stripping the inline trees it is built from.
	r.BuildTypeRegistry()

	// Strip inline field trees from render call variables.
	for i := range r.RenderCalls {
		for j := range r.RenderCalls[i].Vars {
			r.RenderCalls[i].Vars[j].Fields = nil
		}
	}

	// Strip inline field trees from FuncMap entries.
	for i := range r.FuncMaps {
		r.FuncMaps[i].ReturnTypeFields = nil
		for j := range r.FuncMaps[i].Params {
			r.FuncMaps[i].Params[j].Fields = nil
		}
		for j := range r.FuncMaps[i].Returns {
			r.FuncMaps[i].Returns[j].Fields = nil
		}
	}
}
