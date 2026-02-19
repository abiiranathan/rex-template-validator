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
  file: string;
  line: number;
  template: string;
  vars: TemplateVar[];
}

export interface GoValidationError {
  template: string;
  line: number;
  column: number;
  variable: string;
  message: string;
  severity: 'error' | 'warning';
}

export interface AnalysisResult {
  renderCalls: RenderCall[];
  errors: string[];
  validationErrors?: GoValidationError[];
}

// Template knowledge graph
export interface TemplateContext {
  templatePath: string;         // e.g. "views/inpatient/treatment-chart.html"
  vars: Map<string, TemplateVar>;
  renderCalls: RenderCall[];    // all Go render calls pointing here
}

export interface KnowledgeGraph {
  // template path -> context
  templates: Map<string, TemplateContext>;
  // last analysis time
  analyzedAt: Date;
}

// Template parse results
export interface TemplateNode {
  kind: 'variable' | 'range' | 'if' | 'with' | 'block' | 'partial' | 'call';
  path: string[];        // e.g. ["Visit", "Doctor", "DisplayName"]
  rawText: string;
  line: number;
  col: number;
  endLine?: number;
  endCol?: number;
  children?: TemplateNode[];
  partialName?: string;  // for 'partial' kind
}

export interface ValidationError {
  message: string;
  line: number;
  col: number;
  severity: 'error' | 'warning' | 'info';
  variable?: string;
}
