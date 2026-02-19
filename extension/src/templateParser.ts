import { TemplateNode } from './types';

/**
 * Go template parser that produces a proper nested tree.
 * range/with/if blocks contain their children so the validator
 * can push/pop scope correctly.
 */
export class TemplateParser {
  parse(content: string): TemplateNode[] {
    const tokens = this.tokenize(content);
    const { nodes } = this.buildTree(tokens, 0);
    return nodes;
  }

  // ── Tokenizer ────────────────────────────────────────────────────────────

  private tokenize(content: string): Token[] {
    const tokens: Token[] = [];
    const actionRe = /\{\{-?\s*(.*?)\s*-?\}\}/gs;

    const lines = content.split('\n');
    const lineOffsets: number[] = [0];
    for (const l of lines) {
      lineOffsets.push(lineOffsets[lineOffsets.length - 1] + l.length + 1);
    }

    const getPos = (offset: number) => {
      let lo = 0, hi = lineOffsets.length - 1;
      while (lo < hi) {
        const mid = (lo + hi + 1) >> 1;
        if (lineOffsets[mid] <= offset) lo = mid; else hi = mid - 1;
      }
      return { line: lo + 1, col: offset - lineOffsets[lo] + 1 };
    };

    let m: RegExpExecArray | null;
    while ((m = actionRe.exec(content)) !== null) {
      const inner = m[1].trim();
      const pos = getPos(m.index);
      tokens.push({ inner, line: pos.line, col: pos.col, raw: m[0] });
    }

    return tokens;
  }

  // ── Tree builder ─────────────────────────────────────────────────────────

  /**
   * Recursively consume tokens into a node list.
   * Returns when it hits {{end}} or runs out of tokens.
   * `pos` is the current index into `tokens` (mutated via returned nextPos).
   */
  private buildTree(tokens: Token[], pos: number): { nodes: TemplateNode[]; nextPos: number; endToken?: Token } {
    const nodes: TemplateNode[] = [];

    while (pos < tokens.length) {
      const tok = tokens[pos];
      const inner = tok.inner;

      // end / else → stop this level
      if (inner === 'end' || inner === 'else') {
        return { nodes, nextPos: pos + 1, endToken: tok };
      }

      // Comments
      if (inner.startsWith('/*') || inner.startsWith('-/*')) {
        pos++; continue;
      }

      // ── Block openers ──────────────────────────────────────────────────

      if (inner.startsWith('range ')) {
        const expr = inner.slice(6).trim();
        // strip "$_, $v :=" or "$v :=" assignment prefix
        const cleanExpr = expr.replace(/^\$\w+\s*(?:,\s*\$\w+)?\s*:=\s*/, '').trim();
        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'range',
          path: this.parseDotPath(cleanExpr),
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
        });
        pos = child.nextPos;
        continue;
      }

      if (inner.startsWith('with ')) {
        const expr = inner.slice(5).trim().replace(/^\$\w+\s*:=\s*/, '').trim();
        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'with',
          path: this.parseDotPath(expr),
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
        });
        pos = child.nextPos;
        continue;
      }

      if (inner.startsWith('if ')) {
        const expr = inner.slice(3).trim();
        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'if',
          path: this.parseDotPath(expr),
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
        });
        pos = child.nextPos;
        continue;
      }

      if (inner.startsWith('block ')) {
        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'block',
          path: [],
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
        });
        pos = child.nextPos;
        continue;
      }

      // ── Partials ───────────────────────────────────────────────────────

      if (inner.startsWith('template ')) {
        const tplMatch = inner.match(/template\s+"([^"]+)"\s*(.*)/);
        if (tplMatch) {
          nodes.push({
            kind: 'partial',
            path: tplMatch[2].trim() ? this.parseDotPath(tplMatch[2].trim()) : ['.'],
            rawText: tok.raw,
            line: tok.line,
            col: tok.col,
            partialName: tplMatch[1],
          });
        }
        pos++; continue;
      }

      // ── Variable / pipeline ────────────────────────────────────────────

      const varNode = this.tryParseVariable(tok);
      if (varNode) nodes.push(varNode);

      pos++;
    }

    return { nodes, nextPos: pos };
  }

  // ── Helpers ──────────────────────────────────────────────────────────────

  private tryParseVariable(tok: Token): TemplateNode | null {
    const inner = tok.inner;

    if (inner.startsWith('.')) {
      return { kind: 'variable', path: this.parseDotPath(inner), rawText: tok.raw, line: tok.line, col: tok.col };
    }

    // Pipelines / builtins that reference a dot path: "len .Items", "index .Map key"
    if (inner.includes('.')) {
      const dotParts = inner.match(/(\.[A-Za-z_][A-Za-z0-9_.]*)/g);
      if (dotParts) {
        return { kind: 'variable', path: this.parseDotPath(dotParts[0]), rawText: tok.raw, line: tok.line, col: tok.col };
      }
    }

    return null;
  }

  /**
   * Parse ".Visit.Doctor.Name" → ["Visit", "Doctor", "Name"]
   * "."                        → ["."]
   */
  parseDotPath(expr: string): string[] {
    expr = expr.replace(/^\$\w+\s*:=\s*/, '').trim();

    // (call .Method args) → .Method
    const callMatch = expr.match(/\(call\s+(\.[^\s)]+)/);
    if (callMatch) expr = callMatch[1];

    if (expr === '.' || expr === '') return ['.'];
    if (!expr.startsWith('.')) return [];

    // Take path segment up to first space/pipe/paren
    const pathPart = expr.split(/[\s|()|,]/)[0];
    return pathPart.split('.').filter(p => p.length > 0);
  }
}

interface Token {
  inner: string;
  line: number;
  col: number;
  raw: string;
}

/**
 * Resolve a dot-path against the context variables map to find the type.
 * Returns null if resolution fails.
 */
export function resolvePath(
  path: string[],
  vars: Map<string, import('./types').TemplateVar>,
  scopeStack: ScopeFrame[]
): { typeStr: string; found: boolean; fields?: import('./types').FieldInfo[] } {
  if (path.length === 0 || (path.length === 1 && path[0] === '.')) {
    return { typeStr: 'context', found: true };
  }

  // Check scope stack for "." (range/with element)
  if (path[0] === '.') {
    const frame = [...scopeStack].reverse().find(f => f.key === '.');
    if (frame) return { typeStr: frame.typeStr, found: true, fields: frame.fields };
    return { typeStr: 'context', found: true };
  }

  // Check scope stack for named variables
  for (let i = scopeStack.length - 1; i >= 0; i--) {
    const frame = scopeStack[i];
    if (frame.key === path[0]) {
      if (path.length === 1) return { typeStr: frame.typeStr, found: true, fields: frame.fields };
      return resolveFields(path.slice(1), frame.fields ?? []);
    }
  }

  // Check innermost dot context (current scope)
  for (let i = scopeStack.length - 1; i >= 0; i--) {
    if (scopeStack[i].key === '.') {
        const frame = scopeStack[i];
        if (frame.fields) {
            const res = resolveFields(path, frame.fields);
            if (res.found) return res;
        }
        break; // Only check the innermost dot scope
    }
  }

  // Check top-level vars
  const topVar = vars.get(path[0]);
  if (!topVar) {
    return { typeStr: 'unknown', found: false };
  }

  if (path.length === 1) {
    return { typeStr: topVar.type, found: true, fields: topVar.fields };
  }

  return resolveFields(path.slice(1), topVar.fields ?? []);
}

interface ScopeFrame {
  key: string;
  typeStr: string;
  fields?: import('./types').FieldInfo[];
  isRange?: boolean;
}

function resolveFields(
  parts: string[],
  fields: import('./types').FieldInfo[]
): { typeStr: string; found: boolean; fields?: import('./types').FieldInfo[] } {
  let current = fields;

  for (let i = 0; i < parts.length; i++) {
    const part = parts[i];
    const field = current.find(
      (f) => f.name === part || f.name.toLowerCase() === part.toLowerCase()
    );
    if (!field) {
      return { typeStr: 'unknown', found: false };
    }
    if (i === parts.length - 1) {
      return { typeStr: field.type, found: true, fields: field.fields };
    }
    current = field.fields ?? [];
  }

  return { typeStr: 'unknown', found: false };
}
