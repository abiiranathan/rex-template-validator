export interface FieldInfo {
  name: string;
  type: string;
  fields?: FieldInfo[];
  isSlice: boolean;
  isMap?: boolean;
  keyType?: string;
  elemType?: string;
  methods?: string[];
  // Definition location in Go source (for go-to-definition)
  defFile?: string;  // Go file where the field is defined
  defLine?: number;  // Line number where the field is defined (1-based)
  defCol?: number;   // Column number where the field is defined (1-based)
  // Documentation
  doc?: string;  // Documentation comment for the field
}

export interface TemplateVar {
  name: string;
  type: string;
  fields?: FieldInfo[];
  isSlice: boolean;
  isMap?: boolean;
  keyType?: string;
  elemType?: string;
  // Definition location in Go source (for go-to-definition)
  defFile?: string;  // Go file where the variable is defined
  defLine?: number;  // Line number where the variable is defined (1-based)
  defCol?: number;   // Column number where the variable is defined (1-based)
  // Documentation
  doc?: string;  // Documentation comment for the type
}

export interface RenderCall {
  file: string;   // relative to sourceDir, e.g. "handler.go"
  line: number;
  template: string; // e.g. "views/inpatient/treatment-chart.html"
  templateNameStartCol: number;
  templateNameEndCol: number;
  vars: TemplateVar[];
}

export interface GoValidationError {
  template: string;
  line: number;
  column: number;
  variable: string;
  message: string;
  severity: 'error' | 'warning';
  goFile?: string;  // relative path to the .go file with the c.Render() call
  goLine?: number;  // line number of the c.Render() call
  templateNameStartCol?: number;
  templateNameEndCol?: number;
}

export interface AnalysisResult {
  renderCalls: RenderCall[];
  errors: string[];
  validationErrors?: GoValidationError[];
}

// ─── Knowledge Graph ──────────────────────────────────────────────────────────

export interface TemplateContext {
  templatePath: string;         // logical path, e.g. "views/inpatient/treatment-chart.html"
  absolutePath: string;         // absolute fs path for opening files
  vars: Map<string, TemplateVar>;
  renderCalls: RenderCall[];
  // For partials: tracks which parent variable was passed as context (e.g., "User" from {{ template "partial" .User }})
  partialSourceVar?: TemplateVar;
}

export interface KnowledgeGraph {
  templates: Map<string, TemplateContext>; // keyed by logical templatePath
  analyzedAt: Date;
}

// ─── Template AST ─────────────────────────────────────────────────────────────

export interface TemplateNode {
  kind: 'variable' | 'range' | 'if' | 'with' | 'block' | 'partial' | 'call' | 'define' | 'assignment';
  path: string[];        // e.g. ["Visit", "Doctor", "DisplayName"]
  rawText: string;
  line: number;
  col: number;
  endLine?: number;
  endCol?: number;
  children?: TemplateNode[];
  partialName?: string;   // for 'partial' kind
  partialContext?: string; // raw context arg, e.g. "." or ".User"
  blockName?: string;     // for 'block' and 'define' kind
  keyVar?: string;        // for 'range' key variable assignment
  valVar?: string;        // for 'range' and 'with' value variable assignment
  assignVars?: string[];  // for 'assignment' kind
  assignExpr?: string;    // for 'assignment' kind
}

export interface ValidationError {
  message: string;
  line: number;
  col: number;
  severity: 'error' | 'warning' | 'info';
  variable?: string;
}

// ─── Scope ────────────────────────────────────────────────────────────────────

export interface ScopeFrame {
  /** "." for range/with implicit dot, or "$varName" for explicit assignments */
  key: string;
  typeStr: string;
  fields?: FieldInfo[];
  isRange?: boolean;
  /** For ranges: the source variable being iterated (e.g., "prescriptions" from {{ range .prescriptions }}) */
  sourceVar?: TemplateVar;
  /** Local variables defined in this scope (e.g. via $name := ...) */
  locals?: Map<string, TemplateVar>;
}

// ─── Definition location ──────────────────────────────────────────────────────

export interface DefinitionLocation {
  /** Absolute path to the Go file */
  file: string;
  /** 0-based line */
  line: number;
  /** 0-based column */
  col: number;
}
