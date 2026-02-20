import { FieldInfo, ScopeFrame, TemplateNode, TemplateVar } from './types';

/**
 * Go template parser that produces a proper nested AST.
 * range/with/if blocks contain their children so the validator
 * can push/pop scope correctly.
 */
export class TemplateParser {
  parse(content: string): TemplateNode[] {
    const tokens = this.tokenize(content);
    const { nodes } = this.buildTree(tokens, 0);
    return nodes;
  }

  // ── Tokenizer ──────────────────────────────────────────────────────────────

  private tokenize(content: string): Token[] {
    const tokens: Token[] = [];
    // Match {{ ... }} with optional whitespace trimming dashes
    const actionRe = /\{\{-?\s*([\s\S]*?)\s*-?\}\}/g;

    // Pre-compute line start offsets for O(log n) position lookup
    const lineOffsets: number[] = [0];
    for (let i = 0; i < content.length; i++) {
      if (content[i] === '\n') {
        lineOffsets.push(i + 1);
      }
    }

    const getPos = (offset: number): { line: number; col: number } => {
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
      if (!inner) continue;
      const pos = getPos(m.index);
      tokens.push({ inner, line: pos.line, col: pos.col, raw: m[0] });
    }

    return tokens;
  }

  // ── Tree builder ───────────────────────────────────────────────────────────

  private buildTree(tokens: Token[], pos: number): { nodes: TemplateNode[]; nextPos: number; endToken?: Token } {
    const nodes: TemplateNode[] = [];

    while (pos < tokens.length) {
      const tok = tokens[pos];
      const inner = tok.inner;

      // end / else → stop this level
      if (inner === 'end' || inner === 'else' || inner.startsWith('else ')) {
        return { nodes, nextPos: pos + 1, endToken: tok };
      }

      // Comments
      if (inner.startsWith('/*') || inner.startsWith('-/*') || inner.startsWith('//')) {
        pos++; continue;
      }

      // ── Block openers ────────────────────────────────────────────────────

      if (inner.startsWith('range ')) {
        const expr = inner.slice(6).trim();
        // Strip "$i, $v :=" or "$v :=" or "$_, $v :=" assignment prefix
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
        const words = inner.slice(6).trim().split(/\s+/);
        let blockName = '';
        let blockExpr = '.';
        if (words.length >= 1) {
          blockName = words[0].replace(/^"|"$/g, '');
        }
        if (words.length >= 2) {
          blockExpr = words.slice(1).join(' ');
        }

        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'block',
          path: this.parseDotPath(blockExpr),
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
          blockName,
        });
        pos = child.nextPos;
        continue;
      }

      if (inner.startsWith('define ')) {
        const words = inner.slice(7).trim().split(/\s+/);
        let blockName = '';
        if (words.length >= 1) {
          blockName = words[0].replace(/^"|"$/g, '');
        }

        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'define',
          path: [],
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
          blockName,
        });
        pos = child.nextPos;
        continue;
      }

      // ── Template / partial calls ─────────────────────────────────────────

      if (inner.startsWith('template ')) {
        const tplMatch = inner.match(/^template\s+"([^"]+)"(?:\s+(.+))?/);
        if (tplMatch) {
          const contextRaw = tplMatch[2]?.trim() ?? '.';
          nodes.push({
            kind: 'partial',
            path: this.parseDotPath(contextRaw),
            rawText: tok.raw,
            line: tok.line,
            col: tok.col,
            partialName: tplMatch[1],
            partialContext: contextRaw,
          });
        }
        pos++; continue;
      }

      // ── Variable / pipeline ──────────────────────────────────────────────

      const varNodes = this.tryParseVariables(tok);
      nodes.push(...varNodes);
      pos++;
    }

    return { nodes, nextPos: pos };
  }

  // ── Helpers ────────────────────────────────────────────────────────────────

  /**
   * Extract all dot-path variable references from a token.
   * A single action may contain multiple refs: `eq .Name "foo"`, `not .IsLast`
   */
  private tryParseVariables(tok: Token): TemplateNode[] {
    let inner = tok.inner;
    const results: TemplateNode[] = [];

    // 1. Strip assignment LHS: $var := ... or $a, $b := ...
    const assignMatch = inner.match(/^\s*(?:\$[\w\d_]+(?:\s*,\s*\$[\w\d_]+)?)\s*:=\s*(.*)/);
    if (assignMatch) {
      inner = assignMatch[1];
    }

    // 2. Scan for references starting with . or $
    // Matches: .Field, $Var, $.Field, ., $
    const refs = inner.match(/((?:\$|\.)[\w\d_.]*)/g);

    if (refs) {
      for (const ref of refs) {
        // Skip purely numeric decimals like .23
        if (/^\.\d+$/.test(ref)) continue;

        // Skip ellipsis "..."
        if (ref === '...') continue;

        results.push({
          kind: 'variable',
          path: this.parseDotPath(ref),
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
        });
      }
    }

    return results;
  }

  /**
   * Parse a dot-path expression into path segments.
   *
   * ".Visit.Doctor.Name" → ["Visit", "Doctor", "Name"]
   * "."                  → ["."]   (bare dot = current scope)
   * ".Items"             → ["Items"]
   * "$.User.Name"        → ["$", "User", "Name"]
   */
  parseDotPath(expr: string): string[] {
    // Strip variable assignment prefix "$v := "
    expr = expr.replace(/^\$\w+\s*:=\s*/, '').trim();

    // Extract path from (call .Method args)
    const callMatch = expr.match(/\(call\s+(\.[^\s)]+)/);
    if (callMatch) expr = callMatch[1];

    // Take only the path part before any space/pipe/paren
    const pathPart = expr.split(/[\s|(),]/)[0];

    if (pathPart === '.' || pathPart === '') return ['.'];
    if (!pathPart.startsWith('.') && !pathPart.startsWith('$')) return [];

    // Handle "$." root access — becomes ["$", ...rest]
    if (pathPart.startsWith('$.')) {
      const rest = pathPart.slice(2).split('.').filter(p => p.length > 0);
      return ['$', ...rest];
    }

    // Handle bare "$" — root context reference
    if (pathPart === '$') return ['$'];

    // Handle "$varName" (local variable, not root access)
    if (pathPart.startsWith('$')) {
      const parts = pathPart.split('.');
      return parts.filter(p => p.length > 0);
    }

    // Normal dot path: ".A.B.C" → ["A", "B", "C"]
    return pathPart.split('.').filter(p => p.length > 0);
  }
}

interface Token {
  inner: string;
  line: number;
  col: number;
  raw: string;
}

// ── Path resolution ────────────────────────────────────────────────────────────

export interface ResolveResult {
  typeStr: string;
  found: boolean;
  fields?: FieldInfo[];
  isSlice?: boolean;
}

/**
 * Resolve a dot-path against the current variable context and scope stack.
 *
 * Scope stack is innermost-last. The topmost frame with key "." represents
 * the current implicit dot (inside range/with blocks).
 *
 * Path semantics:
 *   ["."]              → bare dot, current scope
 *   ["$"]              → root context
 *   ["$", "User", ...] → root-anchored access via $.User...
 *   ["User", "Profile", "Address", "City"] → traverse vars then fields
 */
export function resolvePath(
  path: string[],
  vars: Map<string, TemplateVar>,
  scopeStack: ScopeFrame[]
): ResolveResult {
  if (path.length === 0) {
    return { typeStr: 'context', found: true };
  }

  // Bare dot → current scope
  if (path.length === 1 && path[0] === '.') {
    const frame = findDotFrame(scopeStack);
    if (frame) return { typeStr: frame.typeStr, found: true, fields: frame.fields };
    return { typeStr: 'context', found: true };
  }

  // Root context "$" — exposes all top-level vars
  if (path[0] === '$' && path.length === 1) {
    return { typeStr: 'context', found: true };
  }

  // Root-anchored path "$." → resolve against root vars, bypassing scope stack
  if (path[0] === '$') {
    const remaining = path.slice(1); // e.g. ["User", "Name"]
    const topVar = vars.get(remaining[0]);
    if (!topVar) return { typeStr: 'unknown', found: false };
    if (remaining.length === 1) {
      return { typeStr: topVar.type, found: true, fields: topVar.fields, isSlice: topVar.isSlice };
    }
    return resolveFields(remaining.slice(1), topVar.fields ?? []);
  }

  // Path inside a with/range scope → check the active dot frame first.
  // Only fall through to top-level vars for explicit "$"-prefixed paths.
  const dotFrame = findDotFrame(scopeStack);
  if (dotFrame) {
    if (dotFrame.fields) {
      const res = resolveFieldsDeep(path, dotFrame.fields);
      if (res.found) return res;
    }
    // Not found in dot frame — do NOT silently fall through to root vars.
    // This correctly surfaces errors for unknown fields inside scoped blocks.
    return { typeStr: 'unknown', found: false };
  }

  // Root scope: resolve against top-level vars
  const topVar = vars.get(path[0]);
  if (!topVar) {
    return { typeStr: 'unknown', found: false };
  }

  if (path.length === 1) {
    return { typeStr: topVar.type, found: true, fields: topVar.fields, isSlice: topVar.isSlice };
  }

  return resolveFields(path.slice(1), topVar.fields ?? []);
}

function findDotFrame(scopeStack: ScopeFrame[]): ScopeFrame | undefined {
  for (let i = scopeStack.length - 1; i >= 0; i--) {
    if (scopeStack[i].key === '.') return scopeStack[i];
  }
  return undefined;
}

/**
 * Walk a field path (no leading var name) through a fields array.
 * Handles arbitrary depth: ["Profile", "Address", "City"] against User's fields.
 */
function resolveFields(
  parts: string[],
  fields: FieldInfo[]
): ResolveResult {
  return resolveFieldsDeep(parts, fields);
}

/**
 * Core recursive field resolver — supports unlimited nesting depth.
 *
 * Each element of `parts` is a field name to traverse. When the final
 * part is reached, the field's type info is returned.
 */
function resolveFieldsDeep(
  parts: string[],
  fields: FieldInfo[]
): ResolveResult {
  if (parts.length === 0) {
    return { typeStr: 'unknown', found: false };
  }

  const [head, ...tail] = parts;
  const field = fields.find(f => f.name === head);

  if (!field) {
    return { typeStr: 'unknown', found: false };
  }

  // Reached the target field
  if (tail.length === 0) {
    return {
      typeStr: field.type,
      found: true,
      fields: field.fields,
      isSlice: field.isSlice,
    };
  }

  // Need to go deeper — recurse into this field's own fields
  const nextFields = field.fields ?? [];
  if (nextFields.length === 0) {
    // Field has no sub-fields (primitive type or empty struct) — path is invalid
    return { typeStr: 'unknown', found: false };
  }

  return resolveFieldsDeep(tail, nextFields);
}
