// src/templateParser.ts
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

    private handleElse(childResult: { nodes: TemplateNode[]; nextPos: number; endToken?: Token }, tokens: Token[]): { falseBranch?: TemplateNode[]; elseToken?: Token; finalChild: { nodes: TemplateNode[]; nextPos: number; endToken?: Token } } {
        if (!childResult.endToken || !childResult.endToken.inner.startsWith('else')) {
            return { falseBranch: undefined, elseToken: undefined, finalChild: childResult };
        }

        const elseTok = childResult.endToken;
        const elseInner = elseTok.inner;

        if (elseInner === 'else') {
            const elseChild = this.buildTree(tokens, childResult.nextPos);
            return { falseBranch: elseChild.nodes, elseToken: elseTok, finalChild: elseChild };
        }

        let kind: 'if' | 'with' | 'range' | undefined;
        let expr = '';
        let keyVar: string | undefined;
        let valVar: string | undefined;

        let normalizedInner = elseInner;
        const parenIdx = elseInner.indexOf('(');
        if (parenIdx !== -1) {
            const beforeParen = elseInner.slice(0, parenIdx).trim();
            if (beforeParen === 'else if' || beforeParen === 'else with' || beforeParen === 'else range') {
                normalizedInner = beforeParen + ' ' + elseInner.slice(parenIdx);
            }
        }

        if (normalizedInner.startsWith('else if ')) {
            kind = 'if';
            expr = normalizedInner.slice(8).trim();
        } else if (normalizedInner.startsWith('else with ')) {
            kind = 'with';
            expr = normalizedInner.slice(10).trim();
            const assignMatch = expr.match(/^(\$\w+)\s*:=\s*(.*)/);
            if (assignMatch) {
                valVar = assignMatch[1];
                expr = assignMatch[2].trim();
            }
        } else if (normalizedInner.startsWith('else range ')) {
            kind = 'range';
            expr = normalizedInner.slice(11).trim();
            const assignMatch = expr.match(/^(\$\w+)\s*(?:,\s*(\$\w+))?\s*:=\s*(.*)/);
            if (assignMatch) {
                if (assignMatch[2]) {
                    keyVar = assignMatch[1];
                    valVar = assignMatch[2];
                } else {
                    valVar = assignMatch[1];
                }
                expr = assignMatch[3].trim();
            }
        }

        if (kind) {
            const nextChild = this.buildTree(tokens, childResult.nextPos);
            const nestedElse = this.handleElse(nextChild, tokens);

            const node: TemplateNode = {
                kind: kind,
                path: this.parseDotPath(expr),
                rawText: elseTok.raw,
                line: elseTok.line,
                col: elseTok.col,
                endLine: nestedElse.finalChild.endToken?.line,
                endCol: nestedElse.finalChild.endToken?.col,
                children: nextChild.nodes,
                elseChildren: nestedElse.falseBranch,
                elseLine: nestedElse.elseToken?.line,
                elseCol: nestedElse.elseToken?.col,
                keyVar,
                valVar
            };

            return { falseBranch: [node], elseToken: elseTok, finalChild: nestedElse.finalChild };
        }

        // Fallback for unknown else
        const elseChild = this.buildTree(tokens, childResult.nextPos);
        return { falseBranch: elseChild.nodes, elseToken: elseTok, finalChild: elseChild };
    }

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

                let child = this.buildTree(tokens, pos + 1);
                const elseResult = this.handleElse(child, tokens);

                nodes.push({
                    kind: 'range',
                    path: this.parseDotPath(cleanExpr),
                    rawText: tok.raw,
                    line: tok.line,
                    col: tok.col,
                    endLine: elseResult.finalChild.endToken?.line,
                    endCol: elseResult.finalChild.endToken?.col,
                    children: child.nodes,
                    elseChildren: elseResult.falseBranch,
                    elseLine: elseResult.elseToken?.line,
                    elseCol: elseResult.elseToken?.col,
                    keyVar,
                    valVar,
                } as any);
                pos = elseResult.finalChild.nextPos;
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

                let child = this.buildTree(tokens, pos + 1);
                const elseResult = this.handleElse(child, tokens);

                nodes.push({
                    kind: 'with',
                    path: this.parseDotPath(cleanExpr),
                    rawText: tok.raw,
                    line: tok.line,
                    col: tok.col,
                    endLine: elseResult.finalChild.endToken?.line,
                    endCol: elseResult.finalChild.endToken?.col,
                    children: child.nodes,
                    elseChildren: elseResult.falseBranch,
                    elseLine: elseResult.elseToken?.line,
                    elseCol: elseResult.elseToken?.col,
                    valVar,
                } as any);
                pos = elseResult.finalChild.nextPos;
                continue;
            }

            if (inner.startsWith('if ')) {
                const expr = inner.slice(3).trim();
                let child = this.buildTree(tokens, pos + 1);
                const elseResult = this.handleElse(child, tokens);

                nodes.push({
                    kind: 'if',
                    path: this.parseDotPath(expr),
                    rawText: tok.raw,
                    line: tok.line,
                    col: tok.col,
                    endLine: elseResult.finalChild.endToken?.line,
                    endCol: elseResult.finalChild.endToken?.col,
                    children: child.nodes,
                    elseChildren: elseResult.falseBranch,
                    elseLine: elseResult.elseToken?.line,
                    elseCol: elseResult.elseToken?.col,
                } as any);
                pos = elseResult.finalChild.nextPos;
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

        return { nodes, nextPos: pos, endToken: tokens[pos - 1] };
    }

    // ── Helpers ────────────────────────────────────────────────────────────────

    private tryParseVariables(tok: Token): TemplateNode[] {
        let inner = tok.inner;
        const results: TemplateNode[] = [];

        // Support both := and = for assignments
        const assignMatch = inner.match(/^\s*(?:\$(\w+)(?:\s*,\s*\$(\w+))?)\s*(:=|=)\s*(.*)/);
        if (assignMatch) {
            const assignVars = [];
            if (assignMatch[1]) assignVars.push('$' + assignMatch[1]);
            if (assignMatch[2]) assignVars.push('$' + assignMatch[2]);
            const assignExpr = assignMatch[4].trim();

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
     */
    parseDotPath(expr: string): string[] {
        expr = expr.replace(/^\$\w+\s*(:=|=)\s*/, '').trim();

        const callMatch = expr.match(/\(call\s+(\.[^\s)]+)/);
        if (callMatch) expr = callMatch[1];

        let indexMatch;
        while ((indexMatch = expr.match(/\(index\s+((?:\$|\.)[\w.]+)[^)]*\)/))) {
            expr = expr.replace(indexMatch[0], indexMatch[1] + '.[]');
        }

        // Extract the first valid path token (starting with . or $) to handle expressions like "not .IsLast"
        const tokens = expr.split(/[\s|(),]+/);
        let pathPart = tokens.find(t => t.startsWith('.') || t.startsWith('$')) || tokens[0];

        if (!pathPart) return ['.'];
        if (pathPart === '.' || pathPart === '') return ['.'];
        if (!pathPart.startsWith('.') && !pathPart.startsWith('$')) return [pathPart];

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

// Helper to extract map/slice info robustly from type strings
function extractTypeInfo(typeStr: string, explicitIsSlice?: boolean, explicitIsMap?: boolean, explicitElemType?: string) {
    let isSlice = explicitIsSlice ?? false;
    let isMap = explicitIsMap ?? false;
    let elemType = explicitElemType;

    if (!isSlice && !isMap && typeStr) {
        let bare = typeStr.startsWith('*') ? typeStr.slice(1) : typeStr;
        if (bare.startsWith('[]')) {
            isSlice = true;
            if (!elemType) elemType = bare.slice(2);
        } else if (bare.startsWith('map[')) {
            isMap = true;
            if (!elemType) {
                let depth = 0;
                let splitIdx = -1;
                for (let i = 4; i < bare.length; i++) {
                    if (bare[i] === '[') depth++;
                    else if (bare[i] === ']') {
                        if (depth === 0) {
                            splitIdx = i;
                            break;
                        }
                        depth--;
                    }
                }
                if (splitIdx !== -1) {
                    elemType = bare.slice(splitIdx + 1).trim();
                }
            }
        }
    }
    return { isSlice, isMap, elemType };
}

// Helper to force hydration by returning undefined instead of empty arrays
function cleanFields(fields?: FieldInfo[]): FieldInfo[] | undefined {
    return fields && fields.length > 0 ? fields : undefined;
}

function unwrapField(
    field: FieldInfo,
    fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
): FieldInfo {
    let retType = '';
    if (field.type === 'method' && field.returns && field.returns.length > 0) {
        retType = field.returns[0].type;
    } else if (field.type.startsWith('func(')) {
        const match = field.type.match(/func\([^)]*\)\s*(.+)/);
        if (match && match[1]) {
            retType = match[1].trim();
            if (retType.startsWith('(')) {
                const commaIdx = retType.indexOf(',');
                const endIdx = retType.indexOf(')');
                const cutIdx = commaIdx !== -1 ? commaIdx : endIdx;
                retType = retType.slice(1, cutIdx).trim();
            }
        }
    }

    if (retType) {
        if (retType.startsWith('*')) retType = retType.substring(1);
        return {
            ...field,
            type: retType,
            isSlice: retType.startsWith('[]'),
            isMap: retType.startsWith('map['),
            fields: fieldResolver ? fieldResolver(retType) || [] : []
        };
    }
    return field;
}

function unwrapVar(
    v: TemplateVar,
    fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
): TemplateVar {
    let retType = '';
    if (v.type.startsWith('func(')) {
        const match = v.type.match(/func\([^)]*\)\s*(.+)/);
        if (match && match[1]) {
            retType = match[1].trim();
            if (retType.startsWith('(')) {
                const commaIdx = retType.indexOf(',');
                const endIdx = retType.indexOf(')');
                const cutIdx = commaIdx !== -1 ? commaIdx : endIdx;
                retType = retType.slice(1, cutIdx).trim();
            }
        }
    }

    if (retType) {
        if (retType.startsWith('*')) retType = retType.substring(1);
        return {
            ...v,
            type: retType,
            isSlice: retType.startsWith('[]'),
            isMap: retType.startsWith('map['),
            fields: fieldResolver ? fieldResolver(retType) || [] : []
        };
    }
    return v;
}

/**
 * Resolve a dot-path against the current variable context and scope stack.
 */
export function resolvePath(
    path: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals?: Map<string, TemplateVar>,
    fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
): ResolveResult {
    if (path.length === 0) {
        return { typeStr: 'context', found: true };
    }

    // Handle boolean and basic literals
    if (path.length === 1) {
        if (path[0] === 'true' || path[0] === 'false') {
            return { typeStr: 'bool', found: true };
        }
        if (path[0] === 'nil') {
            return { typeStr: 'any', found: true };
        }

        if (!isNaN(Number(path[0]))) {
            return { typeStr: 'float64', found: true };
        }
        if (path[0].startsWith('"') || path[0].startsWith('`')) {
            return { typeStr: 'string', found: true };
        }
    }

    const getFields = (typeStr: string, existingFields?: FieldInfo[]): FieldInfo[] => {
        if (existingFields && existingFields.length > 0) return existingFields;
        if (!typeStr || !fieldResolver) return [];
        let bare = typeStr.startsWith('*') ? typeStr.slice(1) : typeStr;
        if (bare.startsWith('[]')) bare = bare.slice(2);
        if (bare.startsWith('map[')) bare = bare.slice(bare.indexOf(']') + 1);
        return fieldResolver(bare) || [];
    };

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
            local = unwrapVar(local, fieldResolver);
            const info = extractTypeInfo(local.type, local.isSlice, local.isMap, local.elemType);
            if (path.length === 1) {
                return {
                    typeStr: local.type,
                    found: true,
                    fields: cleanFields(local.fields),
                    isSlice: info.isSlice,
                    isMap: info.isMap,
                    elemType: info.elemType,
                    keyType: local.keyType,
                    defFile: local.defFile,
                    defLine: local.defLine,
                    defCol: local.defCol,
                    doc: local.doc,
                };
            }
            const f = getFields(local.type, local.fields);
            return resolveFields(path.slice(1), f, info.isMap, info.elemType, info.isSlice, fieldResolver);
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
                    fields: cleanFields(frame.fields),
                    isMap: frame.isMap,
                    keyType: frame.keyType,
                    elemType: frame.elemType,
                    isSlice: frame.isSlice,
                };
            } else { // Handle paths like ".key.subkey"
                const f = getFields(frame.typeStr, frame.fields);
                const res = resolveFieldsDeep(path.slice(1), f, fieldResolver);
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
        let topVar = vars.get(remaining[0]);
        if (!topVar) return { typeStr: 'unknown', found: false };

        topVar = unwrapVar(topVar, fieldResolver);
        const info = extractTypeInfo(topVar.type, topVar.isSlice, topVar.isMap, topVar.elemType);

        if (remaining.length === 1) {
            return {
                typeStr: topVar.type,
                found: true,
                fields: cleanFields(topVar.fields),
                isSlice: info.isSlice,
                isMap: info.isMap,
                elemType: info.elemType,
                keyType: topVar.keyType,
                defFile: topVar.defFile,
                defLine: topVar.defLine,
                defCol: topVar.defCol,
                doc: topVar.doc,
            };
        }
        const f = getFields(topVar.type, topVar.fields);
        return resolveFields(remaining.slice(1), f, info.isMap, info.elemType, info.isSlice, fieldResolver);
    }

    // Path inside a with/range scope → check the active dot frame first.
    const dotFrame = findDotFrame(scopeStack);
    if (dotFrame) {
        if (dotFrame.isMap) {
            // If the map has explicit typed fields (e.g. produced by a `dict` call),
            // resolve against the specific key's declared type rather than the generic
            // element type.  This is what makes `.diagnosis.ID` work inside a block
            // called with `dict "diagnosis" .` — without this, every key resolves to
            // `unknown` and intellisense / validation break for all sub-fields.
            const knownField = dotFrame.fields?.find(f => f.name === path[0]);
            if (knownField) {
                const unwrapped = unwrapField(knownField, fieldResolver);
                const info = extractTypeInfo(unwrapped.type, unwrapped.isSlice, unwrapped.isMap, unwrapped.elemType);

                if (path.length === 1) {
                    return {
                        typeStr: unwrapped.type,
                        found: true,
                        // Prefer inline fields; fall back to the type registry so that
                        // struct types returned from dict values are fully explorable.
                        fields: cleanFields(unwrapped.fields) ?? fieldResolver?.(unwrapped.type),
                        isSlice: info.isSlice,
                        isMap: info.isMap,
                        elemType: info.elemType,
                        keyType: unwrapped.keyType,
                        defFile: unwrapped.defFile,
                        defLine: unwrapped.defLine,
                        defCol: unwrapped.defCol,
                        doc: unwrapped.doc,
                    };
                }

                // Resolve the remaining path segments through this field's type.
                const nextFields =
                    cleanFields(unwrapped.fields) ??
                    fieldResolver?.(unwrapped.type) ??
                    [];
                return resolveFieldsDeep(path.slice(1), nextFields, fieldResolver);
            }

            // Generic map with no known typed keys — any key is considered valid.
            if (path.length === 2) {
                return { typeStr: dotFrame.elemType || 'unknown', found: true };
            } else if (path.length > 2) {
                return resolveFields(path.slice(2), [], false, undefined, false, fieldResolver);
            }
        } else {
            const f = getFields(dotFrame.typeStr, dotFrame.fields);
            if (f.length > 0) {
                const res = resolveFieldsDeep(path, f, fieldResolver);
                if (res.found) return res;
            }
        }

        // Fallback for isolated block validation:
        const f = getFields(dotFrame.typeStr, dotFrame.fields);
        if (f.length === 0) {
            let topVar = vars.get(path[0]);
            if (topVar) {
                topVar = unwrapVar(topVar, fieldResolver);
                const info = extractTypeInfo(topVar.type, topVar.isSlice, topVar.isMap, topVar.elemType);
                if (path.length === 1) {
                    return {
                        typeStr: topVar.type,
                        found: true,
                        fields: cleanFields(topVar.fields),
                        isSlice: info.isSlice,
                        isMap: info.isMap,
                        elemType: info.elemType,
                        keyType: topVar.keyType,
                        defFile: topVar.defFile,
                        defLine: topVar.defLine,
                        defCol: topVar.defCol,
                        doc: topVar.doc,
                    };
                }
                const f2 = getFields(topVar.type, topVar.fields);
                return resolveFields(path.slice(1), f2, info.isMap, info.elemType, info.isSlice, fieldResolver);
            }
            // Dot frame exists but has no field metadata — the context type is unknown.
            // Be permissive: we cannot confidently declare a field missing when we don't
            // know the context shape. Returning found:true suppresses false positives
            // without hiding real errors in scopes where we DO have field info.
            return { typeStr: 'unknown', found: true };
        }

        return { typeStr: 'unknown', found: false };
    }

    // Root scope: resolve against top-level vars
    let topVar = vars.get(path[0]);
    if (!topVar) {
        return { typeStr: 'unknown', found: false };
    }

    topVar = unwrapVar(topVar, fieldResolver);
    const info = extractTypeInfo(topVar.type, topVar.isSlice, topVar.isMap, topVar.elemType);

    if (path.length === 1) {
        return {
            typeStr: topVar.type,
            found: true,
            fields: cleanFields(topVar.fields),
            isSlice: info.isSlice,
            isMap: info.isMap,
            elemType: info.elemType,
            keyType: topVar.keyType,
            defFile: topVar.defFile,
            defLine: topVar.defLine,
            defCol: topVar.defCol,
            doc: topVar.doc,
        };
    }

    const f = getFields(topVar.type, topVar.fields);
    return resolveFields(path.slice(1), f, info.isMap, info.elemType, info.isSlice, fieldResolver);
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
    isSlice: boolean = false,
    fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
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
            // Return undefined for fields so inferExpressionType is forced to hydrate them.
            return {
                typeStr: elemType || 'unknown',
                found: true,
                fields: undefined,
                isSlice: nextIsSlice,
                isMap: nextIsMap,
                elemType: nextElemType,
            };
        }
        return resolveFields(rest, fields, nextIsMap, nextElemType, nextIsSlice, fieldResolver);
    }

    if (isMap) {
        if (parts.length === 1) {
            return {
                typeStr: elemType || 'unknown',
                found: true,
                fields: [],
            };
        }
        return resolveFields(parts.slice(1), fields, false, undefined, false, fieldResolver);
    }

    return resolveFieldsDeep(parts, fields, fieldResolver);
}

function resolveFieldsDeep(
    parts: string[],
    fields: FieldInfo[],
    fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
): ResolveResult {
    if (parts.length === 0) {
        return { typeStr: 'unknown', found: false };
    }

    const [head, ...tail] = parts;
    const field = fields.find(f => f.name === head);

    if (!field) {
        return { typeStr: 'unknown', found: false };
    }

    const currentField = unwrapField(field, fieldResolver);

    if (tail.length > 0) {
        let nextFields = currentField.fields ?? [];
        if (nextFields.length === 0 && currentField.type && fieldResolver) {
            let bare = currentField.type.startsWith('*') ? currentField.type.slice(1) : currentField.type;
            if (bare.startsWith('[]')) bare = bare.slice(2);
            if (bare.startsWith('map[')) bare = bare.slice(bare.indexOf(']') + 1);
            nextFields = fieldResolver(bare) || [];
        }

        if (currentField.isMap) {
            return resolveFields(tail, nextFields, true, currentField.elemType, false, fieldResolver);
        }
        return resolveFieldsDeep(tail, nextFields, fieldResolver);
    }

    const info = extractTypeInfo(currentField.type, currentField.isSlice, currentField.isMap, currentField.elemType);

    return {
        typeStr: currentField.type,
        found: true,
        fields: cleanFields(currentField.fields),
        isSlice: info.isSlice,
        isMap: info.isMap,
        elemType: info.elemType,
        keyType: currentField.keyType,
        params: currentField.params,
        returns: currentField.returns,
        defFile: currentField.defFile,
        defLine: currentField.defLine,
        defCol: currentField.defCol,
        doc: currentField.doc,
    };
}
