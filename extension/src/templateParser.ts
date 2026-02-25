import { FieldInfo, ScopeFrame, TemplateNode, TemplateVar, ParamInfo } from './types';

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
    const actionRe = /\{\{-?\s*([\s\S]*?)\s*-?\}\}/g;

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

      if (inner === 'end' || inner === 'else' || inner.startsWith('else ')) {
        return { nodes, nextPos: pos + 1, endToken: tok };
      }

      if (inner.startsWith('/*') || inner.startsWith('-/*') || inner.startsWith('//')) {
        pos++; continue;
      }

      if (inner.startsWith('range ')) {
        const expr = inner.slice(6).trim();
        let keyVar: string | undefined;
        let valVar: string | undefined;
        const assignMatch = expr.match(/^(\$\w+)\s*(?:,\s*(\$\w+))?\s*:=\s*(.*)/);
        let cleanExpr = expr;
        if (assignMatch) {
          if (assignMatch[2]) {
            keyVar = assignMatch[1];
            valVar = assignMatch[2];
          } else {
            valVar = assignMatch[1];
          }
          cleanExpr = assignMatch[3].trim();
        }

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
          keyVar,
          valVar,
        });
        pos = child.nextPos;
        continue;
      }

      if (inner.startsWith('with ')) {
        const expr = inner.slice(5).trim();
        let valVar: string | undefined;
        const assignMatch = expr.match(/^(\$\w+)\s*:=\s*(.*)/);
        let cleanExpr = expr;
        if (assignMatch) {
          valVar = assignMatch[1];
          cleanExpr = assignMatch[2].trim();
        }
        const child = this.buildTree(tokens, pos + 1);
        nodes.push({
          kind: 'with',
          path: this.parseDotPath(cleanExpr),
          rawText: tok.raw,
          line: tok.line,
          col: tok.col,
          endLine: child.endToken?.line,
          endCol: child.endToken?.col,
          children: child.nodes,
          valVar,
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

      const varNodes = this.tryParseVariables(tok);
      nodes.push(...varNodes);
      pos++;
    }

    return { nodes, nextPos: pos };
  }

  // ── Helpers ────────────────────────────────────────────────────────────────

  private tryParseVariables(tok: Token): TemplateNode[] {
    let inner = tok.inner;
    const results: TemplateNode[] = [];

    const assignMatch = inner.match(/^\s*(?:\$(\w+)(?:\s*,\s*\$(\w+))?)\s*:=\s*(.*)/);
    if (assignMatch) {
      const assignVars = [];
      if (assignMatch[1]) assignVars.push('$' + assignMatch[1]);
      if (assignMatch[2]) assignVars.push('$' + assignMatch[2]);
      const assignExpr = assignMatch[3].trim();

      results.push({
        kind: 'assignment',
        path: this.parseDotPath(assignExpr),
        rawText: tok.raw,
        line: tok.line,
        col: tok.col,
        assignVars,
        assignExpr,
      });
      return results;
    }

    results.push({
      kind: 'variable',
      path: this.parseDotPath(inner),
      rawText: tok.raw,
      line: tok.line,
      col: tok.col,
    });

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
    expr = expr.replace(/^\$\w+\s*:=\s*/, '').trim();

    const callMatch = expr.match(/\(call\s+(\.[^\s)]+)/);
    if (callMatch) expr = callMatch[1];

    let indexMatch;
    while ((indexMatch = expr.match(/\(index\s+((?:\$|\.)[\w.]+)[^)]*\)/))) {
      expr = expr.replace(indexMatch[0], indexMatch[1] + '.[]');
    }

    const pathPart = expr.split(/[\s|(),]/)[0];

    if (pathPart === '.' || pathPart === '') return ['.'];
    if (!pathPart.startsWith('.') && !pathPart.startsWith('$')) return [];

    if (pathPart.startsWith('$.')) {
      const rest = pathPart.slice(2).split('.').filter(p => p.length > 0);
      return ['$', ...rest];
    }

    if (pathPart === '$') return ['$'];

    if (pathPart.startsWith('$')) {
      const parts = pathPart.split('.');
      return parts.filter(p => p.length > 0);
    }

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
  isMap?: boolean;
  elemType?: string;
  keyType?: string;
  params?: ParamInfo[];
  returns?: ParamInfo[];
  defFile?: string;
  defLine?: number;
  defCol?: number;
  doc?: string;
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
  scopeStack: ScopeFrame[],
  blockLocals?: Map<string, TemplateVar>
): ResolveResult {
  if (path.length === 0) {
    return { typeStr: 'context', found: true };
  }

  // Check blockLocals first for $ variables
  if (path[0].startsWith('$') && path[0] !== '$') {
    let local = blockLocals?.get(path[0]);
    if (!local) {
      for (let i = scopeStack.length - 1; i >= 0; i--) {
        if (scopeStack[i].locals?.has(path[0])) {
          local = scopeStack[i].locals!.get(path[0]);
          break;
        }
      }
    }
    if (local) {
      if (path.length === 1) {
        return {
          typeStr: local.type,
          found: true,
          fields: local.fields,
          isSlice: local.isSlice,
          isMap: local.isMap,
          elemType: local.elemType,
          keyType: local.keyType,
          defFile: local.defFile,
          defLine: local.defLine,
          defCol: local.defCol,
          doc: local.doc,
        };
      }
      return resolveFields(path.slice(1), local.fields ?? [], local.isMap, local.elemType);
    }
  }

  // Bare dot → current scope
  if (path[0] === '.') {
    const frame = findDotFrame(scopeStack);
    if (frame) {
      if (path.length === 1) {
        return {
          typeStr: frame.typeStr,
          found: true,
          fields: frame.fields,
          isMap: frame.isMap,
          keyType: frame.keyType,
          elemType: frame.elemType,
          isSlice: frame.isSlice,
        };
      } else { // Handle paths like ".key.subkey"
        const res = resolveFieldsDeep(path.slice(1), frame.fields ?? []);
        if (res.found) return res;
      }
    }
    // If no dotFrame is found but path is just '.', still consider it found (global context)
    if (path.length === 1) {
      return { typeStr: 'context', found: true };
    }
    return { typeStr: 'unknown', found: false };
  }

  // Root context "$" — exposes all top-level vars
  if (path[0] === '$' && path.length === 1) {
    return { typeStr: 'context', found: true };
  }

  // Root-anchored path "$." → resolve against root vars, bypassing scope stack
  if (path[0] === '$') {
    const remaining = path.slice(1);
    const topVar = vars.get(remaining[0]);
    if (!topVar) return { typeStr: 'unknown', found: false };
    if (remaining.length === 1) {
      return {
        typeStr: topVar.type,
        found: true,
        fields: topVar.fields,
        isSlice: topVar.isSlice,
        isMap: topVar.isMap,
        elemType: topVar.elemType,
        keyType: topVar.keyType,
        defFile: topVar.defFile,
        defLine: topVar.defLine,
        defCol: topVar.defCol,
        doc: topVar.doc,
      };
    }
    return resolveFields(remaining.slice(1), topVar.fields ?? [], topVar.isMap, topVar.elemType);
  }

  // Path inside a with/range scope → check the active dot frame first.
  const dotFrame = findDotFrame(scopeStack);
  if (dotFrame) {
    if (dotFrame.isMap) {
      // It's a map context, any key is valid
      if (path.length === 2) { // e.g. [".", "key"]
        return { typeStr: dotFrame.elemType || 'unknown', found: true };
      } else if (path.length > 2) {
        return resolveFields(path.slice(2), [], false, undefined);
      }
    } else if (dotFrame.fields && dotFrame.fields.length > 0) {
      const res = resolveFieldsDeep(path, dotFrame.fields);
      if (res.found) return res;
    }

    // Fallback for isolated block validation:
    // If the dotFrame has no fields, it means the block's context couldn't be statically resolved.
    // We fall back to checking the file's root vars, which contain the injected context-file definitions.
    if (!dotFrame.fields || dotFrame.fields.length === 0) {
      const topVar = vars.get(path[0]);
      if (topVar) {
        if (path.length === 1) {
          return {
            typeStr: topVar.type,
            found: true,
            fields: topVar.fields,
            isSlice: topVar.isSlice,
            isMap: topVar.isMap,
            elemType: topVar.elemType,
            keyType: topVar.keyType,
            defFile: topVar.defFile,
            defLine: topVar.defLine,
            defCol: topVar.defCol,
            doc: topVar.doc,
          };
        }
        return resolveFields(path.slice(1), topVar.fields ?? [], topVar.isMap, topVar.elemType);
      }
    }

    return { typeStr: 'unknown', found: false };
  }

  // Root scope: resolve against top-level vars
  const topVar = vars.get(path[0]);
  if (!topVar) {
    return { typeStr: 'unknown', found: false };
  }

  if (path.length === 1) {
    return {
      typeStr: topVar.type,
      found: true,
      fields: topVar.fields,
      isSlice: topVar.isSlice,
      isMap: topVar.isMap,
      elemType: topVar.elemType,
      keyType: topVar.keyType,
      defFile: topVar.defFile,
      defLine: topVar.defLine,
      defCol: topVar.defCol,
      doc: topVar.doc,
    };
  }

  return resolveFields(path.slice(1), topVar.fields ?? [], topVar.isMap, topVar.elemType);
}

function findDotFrame(scopeStack: ScopeFrame[]): ScopeFrame | undefined {
  for (let i = scopeStack.length - 1; i >= 0; i--) {
    if (scopeStack[i].key === '.') return scopeStack[i];
  }
  return undefined;
}

function resolveFields(
  parts: string[],
  fields: FieldInfo[],
  isMap: boolean = false,
  elemType?: string,
  isSlice: boolean = false
): ResolveResult {
  if (parts.length === 0) {
    return { typeStr: 'unknown', found: false };
  }

  if (parts[0] === '[]' || isMap) {
    const rest = parts[0] === '[]' ? parts.slice(1) : parts.slice(1);

    let nextTypeStr = elemType || 'unknown';
    while (nextTypeStr.startsWith('*')) nextTypeStr = nextTypeStr.slice(1);

    let nextIsMap = false;
    let nextIsSlice = false;
    let nextElemType = nextTypeStr;

    if (nextTypeStr.startsWith('map[')) {
      nextIsMap = true;
      let depth = 0;
      let splitIdx = -1;
      for (let i = 4; i < nextTypeStr.length; i++) {
        if (nextTypeStr[i] === '[') depth++;
        else if (nextTypeStr[i] === ']') {
          if (depth === 0) {
            splitIdx = i;
            break;
          }
          depth--;
        }
      }
      if (splitIdx !== -1) {
        nextElemType = nextTypeStr.slice(splitIdx + 1).trim();
      }
    } else if (nextTypeStr.startsWith('[]')) {
      nextIsSlice = true;
      nextElemType = nextTypeStr.slice(2);
    }

    if (rest.length === 0) {
      return {
        typeStr: elemType || 'unknown',
        found: true,
        fields: fields,
        isSlice: nextIsSlice,
        isMap: nextIsMap,
        elemType: nextElemType,
      };
    }
    return resolveFields(rest, fields, nextIsMap, nextElemType, nextIsSlice);
  }

  if (isMap) {
    if (parts.length === 1) {
      return {
        typeStr: elemType || 'unknown',
        found: true,
        fields: [],
      };
    }
    return resolveFields(parts.slice(1), fields, false, undefined);
  }

  return resolveFieldsDeep(parts, fields);
}

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

  if (tail.length > 0) {
    if (field.isMap) {
      return resolveFields(tail, field.fields ?? [], true, field.elemType);
    }
    return resolveFieldsDeep(tail, field.fields ?? []);
  }

  return {
    typeStr: field.type,
    found: true,
    fields: field.fields,
    isSlice: field.isSlice,
    isMap: field.isMap,
    elemType: field.elemType,
    keyType: field.keyType,
    params: field.params,
    returns: field.returns,
    defFile: field.defFile,
    defLine: field.defLine,
    defCol: field.defCol,
    doc: field.doc,
  };
}
