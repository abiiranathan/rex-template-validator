// Types mirroring the Go analyzer output

export interface FieldInfo {
  name: string;
  type: string;
  fields?: FieldInfo[];
  isSlice: boolean;
  methods?: string[];
}

export interface TemplateVar {
  name: string;
  type: string;
  fields?: FieldInfo[];
  isSlice: boolean;
  elemType?: string;
}

export interface RenderCall {
  file: string;   // relative to sourceDir, e.g. "handler.go"
  line: number;
  template: string; // e.g. "views/inpatient/treatment-chart.html"
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
}

export interface KnowledgeGraph {
  templates: Map<string, TemplateContext>; // keyed by logical templatePath
  analyzedAt: Date;
}

// ─── Template AST ─────────────────────────────────────────────────────────────

export interface TemplateNode {
  kind: 'variable' | 'range' | 'if' | 'with' | 'block' | 'partial' | 'call';
  path: string[];        // e.g. ["Visit", "Doctor", "DisplayName"]
  rawText: string;
  line: number;
  col: number;
  endLine?: number;
  endCol?: number;
  children?: TemplateNode[];
  partialName?: string;   // for 'partial' kind
  partialContext?: string; // raw context arg, e.g. "." or ".User"
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
