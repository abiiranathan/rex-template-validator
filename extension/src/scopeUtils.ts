/**
 * Package scopeUtils provides shared scope resolution and AST traversal helpers
 * used by all template analysis providers (validation, hover, completion, definition).
 *
 * Nothing in this file emits diagnostics or VS Code UI objects — it is purely
 * concerned with walking the template AST and resolving types/scopes.
 */

import * as fs from 'fs';
import * as vscode from 'vscode';
import {
    FieldInfo,
    ScopeFrame,
    TemplateContext,
    TemplateNode,
    TemplateVar,
    extractBareType,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { inferExpressionType } from './compiler/expressionParser';

// ── Module-level helpers ───────────────────────────────────────────────────────

/** Converts a FieldInfo record into a TemplateVar. */
export function fieldInfoToTemplateVar(f: FieldInfo): TemplateVar {
    return {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
        defFile: f.defFile,
        defLine: f.defLine,
        defCol: f.defCol,
        doc: f.doc,
    };
}

/** Returns true when the partial name looks like a file path rather than a named block. */
export function isFileBasedPartial(name: string): boolean {
    if (name.includes('/') || name.includes('\\')) return true;
    return ['.html', '.tmpl', '.gohtml', '.tpl', '.htm'].some(ext =>
        name.toLowerCase().endsWith(ext)
    );
}

/**
 * Normalises a template context argument so that dict calls wrapped in
 * parentheses — e.g. `(dict "key" .Value)` — are unwrapped to `dict "key" .Value`.
 */
export function normalizeDictArg(contextArg: string): string {
    let s = contextArg.trim();
    while (s.startsWith('(') && s.endsWith(')')) {
        const inner = s.slice(1, -1).trim();
        let depth = 0;
        let balanced = false;
        for (let i = 0; i < s.length; i++) {
            if (s[i] === '(') depth++;
            else if (s[i] === ')') {
                depth--;
                if (depth === 0) {
                    balanced = i === s.length - 1;
                    break;
                }
            }
        }
        if (balanced) {
            s = inner;
        } else {
            break;
        }
    }
    return s;
}

// ── ScopeUtils class ──────────────────────────────────────────────────────────

export class ScopeUtils {
    readonly parser: TemplateParser;
    readonly graphBuilder: KnowledgeGraphBuilder;

    constructor(parser: TemplateParser, graphBuilder: KnowledgeGraphBuilder) {
        this.parser = parser;
        this.graphBuilder = graphBuilder;
    }

    // ── Named block registry ──────────────────────────────────────────────────

    resolveNamedBlock(name: string): {
        entry: import('./types').NamedBlockEntry | undefined;
        isDuplicate: boolean;
        duplicateMessage?: string;
    } {
        const graph = this.graphBuilder.getGraph();
        const entries = graph.namedBlocks.get(name);

        if (!entries || entries.length === 0) {
            return { entry: undefined, isDuplicate: false };
        }

        if (entries.length > 1) {
            const locs = entries.map(e => `${e.templatePath}:${e.line}`).join(', ');
            return {
                entry: entries[0],
                isDuplicate: true,
                duplicateMessage: `Named block "${name}" is declared in multiple files: ${locs}. Only one declaration is allowed.`,
            };
        }

        return { entry: entries[0], isDuplicate: false };
    }

    // ── Scope frame builders ──────────────────────────────────────────────────

    buildChildScope(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        rootNodes: TemplateNode[]
    ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
        switch (node.kind) {
            case 'range': {
                if (node.path.length > 0) {
                    const elemScope = this.buildRangeElemScope(node, vars, scopeStack, blockLocals);
                    if (elemScope) return { childVars: vars, childStack: [...scopeStack, elemScope] };
                }
                return null;
            }

            case 'with': {
                if (node.path.length > 0) {
                    const result = resolvePath(
                        node.path, vars, scopeStack, blockLocals,
                        this.buildFieldResolver(vars, scopeStack)
                    );
                    if (result.found) {
                        const childScope: ScopeFrame = {
                            key: '.',
                            typeStr: result.typeStr,
                            fields: result.fields ?? [],
                            isMap: result.isMap,
                            keyType: result.keyType,
                            elemType: result.elemType,
                            isSlice: result.isSlice,
                        };
                        if (node.valVar) {
                            childScope.locals = new Map();
                            childScope.locals.set(node.valVar, {
                                name: node.valVar,
                                type: result.typeStr,
                                fields: result.fields,
                                isSlice: result.isSlice ?? false,
                                isMap: result.isMap,
                                elemType: result.elemType,
                                keyType: result.keyType,
                            });
                        }
                        return { childVars: vars, childStack: [...scopeStack, childScope] };
                    }
                }
                return null;
            }

            default:
                return null;
        }
    }

    buildRangeElemScope(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals?: Map<string, TemplateVar>
    ): ScopeFrame | null {
        const fieldResolver = this.buildFieldResolver(vars, scopeStack);

        if (node.assignExpr) {
            try {
                const exprType = inferExpressionType(
                    node.assignExpr,
                    vars,
                    scopeStack,
                    blockLocals,
                    this.graphBuilder.getGraph().funcMaps,
                    fieldResolver,
                );
                if (exprType && (exprType.isSlice || exprType.isMap)) {
                    return this.buildRangeElemScopeFromType(
                        node, exprType, vars, scopeStack, fieldResolver
                    );
                }
            } catch { /* fall through to path-based resolution */ }
        }

        const result = resolvePath(node.path, vars, scopeStack, blockLocals, fieldResolver);
        if (!result.found) return null;
        return this.buildRangeElemScopeFromType(node, result, vars, scopeStack, fieldResolver);
    }

    private buildRangeElemScopeFromType(
        node: TemplateNode,
        result: {
            typeStr: string;
            isSlice?: boolean;
            isMap?: boolean;
            elemType?: string;
            keyType?: string;
            fields?: FieldInfo[];
        },
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        fieldResolver: (typeStr: string) => FieldInfo[] | undefined,
    ): ScopeFrame | null {
        let sourceVar: TemplateVar | undefined = vars.get(node.path[0]);
        if (!sourceVar) {
            const df = scopeStack.slice().reverse().find(f => f.key === '.');
            const field = df?.fields?.find(f => f.name === node.path[0]);
            if (field) sourceVar = fieldInfoToTemplateVar(field);
        }

        const rawTypeStr = result.typeStr.startsWith('*')
            ? result.typeStr.slice(1)
            : result.typeStr;

        let elemTypeStr = result.elemType;
        let mapKeyType = result.keyType;

        if (!elemTypeStr) {
            if (result.isSlice && rawTypeStr.startsWith('[]')) {
                elemTypeStr = rawTypeStr.slice(2);
            } else if (result.isMap && rawTypeStr.startsWith('map[')) {
                let depth = 0;
                let splitIdx = -1;
                for (let i = 4; i < rawTypeStr.length; i++) {
                    if (rawTypeStr[i] === '[') depth++;
                    else if (rawTypeStr[i] === ']') {
                        if (depth === 0) { splitIdx = i; break; }
                        depth--;
                    }
                }
                if (splitIdx !== -1) {
                    mapKeyType = rawTypeStr.slice(4, splitIdx).trim();
                    elemTypeStr = rawTypeStr.slice(splitIdx + 1).trim();
                } else {
                    elemTypeStr = rawTypeStr;
                }
            } else {
                elemTypeStr = rawTypeStr;
            }
        }

        while (elemTypeStr && elemTypeStr.startsWith('*')) {
            elemTypeStr = elemTypeStr.slice(1);
        }

        let isElemSlice = false;
        let isElemMap = false;
        let elemKeyType: string | undefined;
        let elemInnerType = elemTypeStr;

        if (elemTypeStr.startsWith('[]')) {
            isElemSlice = true;
            elemInnerType = elemTypeStr.slice(2);
            while (elemInnerType.startsWith('*')) elemInnerType = elemInnerType.slice(1);
        } else if (elemTypeStr.startsWith('map[')) {
            isElemMap = true;
            let depth = 0;
            let splitIdx = -1;
            for (let i = 4; i < elemTypeStr.length; i++) {
                if (elemTypeStr[i] === '[') depth++;
                else if (elemTypeStr[i] === ']') {
                    if (depth === 0) { splitIdx = i; break; }
                    depth--;
                }
            }
            if (splitIdx !== -1) {
                elemKeyType = elemTypeStr.slice(4, splitIdx).trim();
                elemInnerType = elemTypeStr.slice(splitIdx + 1).trim();
                while (elemInnerType.startsWith('*')) elemInnerType = elemInnerType.slice(1);
            }
        }

        const elemFields: FieldInfo[] =
            (result.fields && result.fields.length > 0)
                ? result.fields
                : fieldResolver(elemTypeStr) ?? [];

        const elemScope: ScopeFrame = {
            key: '.',
            typeStr: elemTypeStr,
            fields: elemFields,
            isRange: true,
            sourceVar,
            isMap: isElemMap,
            keyType: elemKeyType,
            elemType: elemInnerType,
            isSlice: isElemSlice,
        };

        if (node.keyVar || node.valVar) {
            elemScope.locals = new Map();
            if (node.keyVar && node.valVar) {
                elemScope.locals.set(node.keyVar, {
                    name: node.keyVar,
                    type: result.isMap ? (result.keyType ?? 'unknown') : 'unknown',
                    isSlice: false,
                });
                elemScope.locals.set(node.valVar, {
                    name: node.valVar,
                    type: elemScope.typeStr,
                    fields: elemScope.fields,
                    isSlice: isElemSlice,
                    isMap: isElemMap,
                    keyType: elemKeyType,
                    elemType: elemInnerType,
                });
            } else if (node.valVar) {
                elemScope.locals.set(node.valVar, {
                    name: node.valVar,
                    type: elemScope.typeStr,
                    fields: elemScope.fields,
                    isSlice: isElemSlice,
                    isMap: isElemMap,
                    keyType: elemKeyType,
                    elemType: elemInnerType,
                });
            }
        }

        return elemScope;
    }

    // ── Assignment helpers ────────────────────────────────────────────────────

    applyAssignmentLocals(
        assignVars: string[],
        result: import('./compiler/expressionParser').TypeResult | import('./templateParser').ResolveResult,
        blockLocals: Map<string, TemplateVar>
    ) {
        if (assignVars.length === 1) {
            blockLocals.set(assignVars[0], {
                name: assignVars[0],
                type: result.typeStr,
                fields: result.fields,
                isSlice: result.isSlice ?? false,
                isMap: result.isMap,
                elemType: result.elemType,
                keyType: result.keyType,
            });
        } else if (assignVars.length === 2 && result.isMap) {
            blockLocals.set(assignVars[0], {
                name: assignVars[0],
                type: result.keyType ?? 'unknown',
                isSlice: false,
            });
            blockLocals.set(assignVars[1], {
                name: assignVars[1],
                type: result.elemType ?? 'unknown',
                fields: result.fields,
                isSlice: false,
            });
        } else if (assignVars.length === 2 && result.isSlice) {
            blockLocals.set(assignVars[0], { name: assignVars[0], type: 'int', isSlice: false });
            blockLocals.set(assignVars[1], {
                name: assignVars[1],
                type: result.elemType ?? 'unknown',
                fields: result.fields,
                isSlice: false,
            });
        }
    }

    processAssignment(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>
    ) {
        if (node.kind !== 'assignment') return;

        let resolvedType:
            | import('./compiler/expressionParser').TypeResult
            | import('./templateParser').ResolveResult
            | null = null;

        try {
            if (node.assignExpr) {
                const exprType = inferExpressionType(
                    node.assignExpr,
                    vars,
                    scopeStack,
                    blockLocals,
                    this.graphBuilder.getGraph().funcMaps,
                    this.buildFieldResolver(vars, scopeStack),
                );
                if (exprType) resolvedType = exprType;
            }
        } catch { }

        if (!resolvedType) {
            const result = resolvePath(
                node.path, vars, scopeStack, blockLocals,
                this.buildFieldResolver(vars, scopeStack)
            );
            const isExplicitContext = node.assignExpr === '.' || node.assignExpr === '$';
            const isValidPath =
                (node.path.length > 0 &&
                    !(node.path.length === 1 && node.path[0] === '.' && !isExplicitContext)) ||
                isExplicitContext;

            const isCollectionOp = /^\s*(?:index|slice)\s+/.test(node.assignExpr ?? '');
            if (!isCollectionOp && result.found && isValidPath) {
                resolvedType = result;
            }
        }

        if (resolvedType && node.assignVars?.length) {
            this.applyAssignmentLocals(node.assignVars, resolvedType, blockLocals);
        }
    }

    // ── AST queries ───────────────────────────────────────────────────────────

    findEnclosingBlockOrDefine(
        nodes: TemplateNode[],
        position: vscode.Position
    ): TemplateNode | null {
        for (const node of nodes) {
            if (node.endLine === undefined) continue;
            const afterStart =
                position.line > node.line - 1 ||
                (position.line === node.line - 1 && position.character >= node.col - 1);
            const beforeEnd =
                position.line < node.endLine - 1 ||
                (position.line === node.endLine - 1 &&
                    position.character <= (node.endCol ?? 0) - 1);
            if (afterStart && beforeEnd) {
                if (node.kind === 'block' || node.kind === 'define') {
                    return (
                        this.findEnclosingBlockOrDefine(node.children ?? [], position) || node
                    );
                } else if (node.children) {
                    const found = this.findEnclosingBlockOrDefine(node.children, position);
                    if (found) return found;
                }
            }
        }
        return null;
    }

    findCallSiteContext(
        nodes: TemplateNode[],
        blockName: string,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar> = new Map(),
        _rootNodes?: TemplateNode[],
        _visitedDefines?: Set<string>,
        _rootVars?: Map<string, TemplateVar>
    ): {
        typeStr: string;
        fields?: FieldInfo[];
        isMap?: boolean;
        keyType?: string;
        elemType?: string;
        isSlice?: boolean;
    } | null {
        const rootNodes = _rootNodes ?? nodes;
        const rootVars = _rootVars ?? vars;
        const visitedDefines = _visitedDefines ?? new Set<string>();

        for (const node of nodes) {
            this.processAssignment(node, vars, scopeStack, blockLocals);

            const isTemplateCall = node.kind === 'partial' && node.partialName === blockName;
            const isBlockDecl = node.kind === 'block' && node.blockName === blockName;

            if (isTemplateCall || isBlockDecl) {
                let contextArg: string;
                if (isTemplateCall) {
                    contextArg = node.partialContext ?? '.';
                } else {
                    contextArg =
                        node.path.length === 0
                            ? '.'
                            : node.path[0] === '.'
                                ? '.' + node.path.slice(1).join('.')
                                : '.' + node.path.join('.');
                }

                const normalizedCtx = normalizeDictArg(contextArg);

                if (normalizedCtx === '.' || normalizedCtx === '') {
                    const frame = scopeStack.slice().reverse().find(f => f.key === '.');
                    if (frame) {
                        return {
                            typeStr: frame.typeStr,
                            fields: frame.fields,
                            isMap: frame.isMap,
                            keyType: frame.keyType,
                            elemType: frame.elemType,
                            isSlice: frame.isSlice,
                        };
                    }
                    return { typeStr: 'context', fields: [...vars.values()] as unknown as FieldInfo[] };
                }

                if (normalizedCtx.startsWith('dict ')) {
                    const dictType = inferExpressionType(
                        normalizedCtx, vars, scopeStack, blockLocals,
                        this.graphBuilder.getGraph().funcMaps,
                        this.buildFieldResolver(vars, scopeStack)
                    );
                    if (dictType) {
                        return {
                            typeStr: dictType.typeStr,
                            fields: dictType.fields,
                            isMap: dictType.isMap,
                            keyType: dictType.keyType,
                            elemType: dictType.elemType,
                            isSlice: dictType.isSlice,
                        };
                    }
                }

                const result = resolvePath(
                    this.parser.parseDotPath(normalizedCtx), vars, scopeStack, blockLocals,
                    this.buildFieldResolver(vars, scopeStack)
                );
                return result.found
                    ? {
                        typeStr: result.typeStr,
                        fields: result.fields,
                        isMap: result.isMap,
                        keyType: result.keyType,
                        elemType: result.elemType,
                        isSlice: result.isSlice,
                    }
                    : null;
            }

            if (node.children) {
                let childStack = scopeStack;
                let childVars = vars;
                const childLocals = new Map(blockLocals);

                if (node.kind === 'range') {
                    const elemScope = this.buildRangeElemScope(node, vars, scopeStack, childLocals);
                    if (elemScope) childStack = [...scopeStack, elemScope];
                } else if (node.kind === 'with') {
                    if (node.path.length > 0) {
                        const result = resolvePath(
                            node.path, vars, scopeStack, childLocals,
                            this.buildFieldResolver(vars, scopeStack)
                        );
                        if (result.found && result.fields !== undefined) {
                            const childScope: ScopeFrame = {
                                key: '.',
                                typeStr: result.typeStr,
                                fields: result.fields,
                                isMap: result.isMap,
                                keyType: result.keyType,
                                elemType: result.elemType,
                                isSlice: result.isSlice,
                            };
                            if (node.valVar) {
                                childScope.locals = new Map();
                                childScope.locals.set(node.valVar, {
                                    name: node.valVar,
                                    type: result.typeStr,
                                    fields: result.fields,
                                    isSlice: result.isSlice ?? false,
                                    isMap: result.isMap,
                                    keyType: result.keyType,
                                    elemType: result.elemType,
                                });
                            }
                            childStack = [...scopeStack, childScope];
                        }
                    }
                } else if (node.kind === 'block') {
                    if (node.path.length > 0) {
                        const result = resolvePath(
                            node.path, vars, scopeStack, childLocals,
                            this.buildFieldResolver(vars, scopeStack)
                        );
                        if (result.found && result.fields !== undefined) {
                            childStack = [
                                ...scopeStack,
                                { key: '.', typeStr: result.typeStr, fields: result.fields },
                            ];
                        }
                    }
                } else if (node.kind === 'define' && node.blockName && !visitedDefines.has(node.blockName)) {
                    const newVisited = new Set(visitedDefines);
                    newVisited.add(node.blockName);

                    const defineCtx = this.findCallSiteContext(
                        rootNodes, node.blockName, rootVars, [],
                        new Map(), rootNodes, newVisited, rootVars
                    );

                    if (defineCtx && (defineCtx.fields?.length ?? 0) > 0) {
                        childStack = [{
                            key: '.',
                            typeStr: defineCtx.typeStr,
                            fields: defineCtx.fields,
                            isMap: defineCtx.isMap,
                            keyType: defineCtx.keyType,
                            elemType: defineCtx.elemType,
                            isSlice: defineCtx.isSlice,
                        }];
                        childVars = this.fieldsToVarMap(defineCtx.fields ?? []);
                    }
                }

                const found = this.findCallSiteContext(
                    node.children, blockName, childVars, childStack, childLocals,
                    rootNodes, visitedDefines, rootVars
                );
                if (found) return found;
            }
        }
        return null;
    }

    findDefineNodeInAST(
        nodes: TemplateNode[],
        name: string
    ): TemplateNode | null {
        for (const node of nodes) {
            if (
                (node.kind === 'define' || node.kind === 'block') &&
                node.blockName === name
            )
                return node;
            if (node.children) {
                const found = this.findDefineNodeInAST(node.children, name);
                if (found) return found;
            }
        }
        return null;
    }

    findNodeAtPosition(
        nodes: TemplateNode[],
        position: vscode.Position,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        rootNodes: TemplateNode[],
        inheritedLocals?: Map<string, TemplateVar>,
        currentFilePath?: string
    ): {
        node: TemplateNode;
        stack: ScopeFrame[];
        vars: Map<string, TemplateVar>;
        locals: Map<string, TemplateVar>;
    } | null {
        const blockLocals = inheritedLocals
            ? new Map(inheritedLocals)
            : new Map<string, TemplateVar>();

        for (const node of nodes) {
            const startLine = node.line - 1;
            const startCol = node.col - 1;

            this.processAssignment(node, vars, scopeStack, blockLocals);

            if (node.kind === 'variable') {
                const endCol = startCol + node.rawText.length;
                if (
                    position.line === startLine &&
                    position.character >= startCol &&
                    position.character <= endCol
                ) {
                    return { node, stack: scopeStack, vars, locals: blockLocals };
                }
                continue;
            }

            if (node.endLine === undefined) {
                const endCol = startCol + (node.rawText?.length ?? 0);
                if (
                    position.line === startLine &&
                    position.character >= startCol &&
                    position.character <= endCol
                ) {
                    return { node, stack: scopeStack, vars, locals: blockLocals };
                }
                continue;
            }

            const endLine = node.endLine - 1;
            const afterStart =
                position.line > startLine ||
                (position.line === startLine && position.character >= startCol);
            const beforeEnd =
                position.line < endLine ||
                (position.line === endLine && position.character <= (node.endCol ?? 0) - 1);
            if (!afterStart || !beforeEnd) continue;

            if (
                position.line === startLine &&
                position.character <= startCol + node.rawText.length
            ) {
                return { node, stack: scopeStack, vars, locals: blockLocals };
            }

            let childVars = vars;
            let childStack = scopeStack;
            let childLocals = new Map(blockLocals);

            if ((node.kind === 'define' || node.kind === 'block') && node.blockName) {
                const callCtx = this.resolveNamedBlockCallCtxForPosition(
                    node.blockName, vars, rootNodes, currentFilePath
                );
                if (callCtx) {
                    childVars = this.fieldsToVarMap(callCtx.fields ?? []);
                    childStack = [{
                        key: '.',
                        typeStr: callCtx.typeStr,
                        fields: callCtx.fields ?? [],
                        isMap: callCtx.isMap,
                        keyType: callCtx.keyType,
                        elemType: callCtx.elemType,
                        isSlice: callCtx.isSlice,
                    }];
                    childLocals = new Map();
                }
            } else {
                const childScope = this.buildChildScope(
                    node, vars, scopeStack, childLocals, rootNodes
                );
                if (childScope) {
                    childVars = childScope.childVars;
                    childStack = childScope.childStack;
                }
            }

            const elseLine = node.elseLine;
            let inElse = false;
            if (elseLine !== undefined) {
                const eLine = elseLine - 1;
                const eCol = (node.elseCol ?? 1) - 1;
                if (position.line > eLine || (position.line === eLine && position.character >= eCol)) {
                    inElse = true;
                }
            }

            if (inElse) {
                if (node.elseChildren && node.elseChildren.length > 0) {
                    const found = this.findNodeAtPosition(
                        node.elseChildren, position, vars, scopeStack, rootNodes, blockLocals, currentFilePath
                    );
                    if (found) return found;
                }
            } else {
                if (node.children) {
                    const found = this.findNodeAtPosition(
                        node.children, position, childVars, childStack, rootNodes, childLocals, currentFilePath
                    );
                    if (found) return found;
                }
            }
        }

        return null;
    }

    findScopeAtPosition(
        nodes: TemplateNode[],
        position: vscode.Position,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        rootNodes: TemplateNode[],
        ctx: TemplateContext,
        inheritedLocals?: Map<string, TemplateVar>
    ): { stack: ScopeFrame[]; locals: Map<string, TemplateVar> } {
        const blockLocals = inheritedLocals
            ? new Map(inheritedLocals)
            : new Map<string, TemplateVar>();

        for (const node of nodes) {
            if (node.kind === 'assignment') {
                this.processAssignment(node, vars, scopeStack, blockLocals);
            }

            const startLine = node.line - 1;
            const startCol = node.col - 1;

            if (
                startLine > position.line ||
                (startLine === position.line && startCol > position.character)
            ) {
                continue;
            }

            let childVars = vars;
            let childStack = scopeStack;
            let childLocals = new Map(blockLocals);

            if ((node.kind === 'define' || node.kind === 'block') && node.blockName) {
                const graph = this.graphBuilder.getGraph();
                const blockCtx = graph.templates.get(node.blockName);
                const cfCall = blockCtx?.renderCalls.find(rc => rc.file === 'context-file');

                if (cfCall && cfCall.vars) {
                    childVars = this.fieldsToVarMap(cfCall.vars as unknown as FieldInfo[]);
                    childStack = [{
                        key: '.',
                        typeStr: 'context',
                        fields: cfCall.vars as unknown as FieldInfo[],
                        isMap: false,
                        isSlice: false,
                    }];
                    childLocals = new Map();
                } else {
                    const callCtx = this.resolveNamedBlockCallCtxForPosition(
                        node.blockName, vars, rootNodes, ctx.absolutePath
                    );
                    if (callCtx) {
                        childVars = this.fieldsToVarMap(callCtx.fields ?? []);
                        childStack = [{
                            key: '.',
                            typeStr: callCtx.typeStr,
                            fields: callCtx.fields ?? [],
                            isMap: callCtx.isMap,
                            keyType: callCtx.keyType,
                            elemType: callCtx.elemType,
                            isSlice: callCtx.isSlice,
                        }];
                        childLocals = new Map();
                    }
                }
            } else {
                const childScopeBuild = this.buildChildScope(
                    node, vars, scopeStack, childLocals, rootNodes
                );
                if (childScopeBuild) {
                    childVars = childScopeBuild.childVars;
                    childStack = childScopeBuild.childStack;
                }
            }

            let isInside = false;

            if (node.endLine === undefined) {
                const endCol = startCol + (node.rawText?.length ?? 0);
                isInside =
                    position.line === startLine &&
                    position.character >= startCol &&
                    position.character <= endCol;
            } else {
                const endLine = node.endLine - 1;
                const endCol = node.endCol ?? 999999;
                isInside =
                    (position.line > startLine ||
                        (position.line === startLine && position.character >= startCol)) &&
                    (position.line < endLine ||
                        (position.line === endLine && position.character <= endCol));
            }

            if (isInside) {
                const elseLine = node.elseLine;
                let inElse = false;
                if (elseLine !== undefined) {
                    const eLine = elseLine - 1;
                    const eCol = (node.elseCol ?? 1) - 1;
                    if (position.line > eLine || (position.line === eLine && position.character >= eCol)) {
                        inElse = true;
                    }
                }

                if (inElse) {
                    if (node.elseChildren && node.elseChildren.length > 0) {
                        return this.findScopeAtPosition(
                            node.elseChildren, position, vars, scopeStack, rootNodes, ctx, blockLocals
                        );
                    }
                    return { stack: scopeStack, locals: blockLocals };
                }

                if (node.children && node.children.length > 0) {
                    return this.findScopeAtPosition(
                        node.children, position, childVars, childStack,
                        rootNodes, ctx, childLocals
                    );
                }
                return { stack: childStack, locals: childLocals };
            }
        }

        return { stack: scopeStack, locals: blockLocals };
    }

    // ── Utilities ─────────────────────────────────────────────────────────────

    fieldsToVarMap(fields: FieldInfo[]): Map<string, TemplateVar> {
        const m = new Map<string, TemplateVar>();
        for (const f of fields) m.set(f.name, fieldInfoToTemplateVar(f));
        return m;
    }

    /**
     * Builds a field-resolver closure that maps a bare Go type name (e.g. "User")
     * to its FieldInfo array.
     *
     * Resolution order:
     *  1. Fields indexed inline from vars + scope frames (fast, local)
     *  2. Global type registry from the Go analyzer (covers types whose fields
     *     were stripped from the serialized TemplateVar to reduce payload size)
     */
    buildFieldResolver(
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[]
    ): (typeStr: string) => FieldInfo[] | undefined {
        const typeIndex = new Map<string, FieldInfo[]>();

        const indexVar = (v: TemplateVar | FieldInfo) => {
            let bare = extractBareType(v.type);
            let retFields: FieldInfo[] | undefined;

            if (v.type === 'method' && (v as FieldInfo).returns?.length) {
                bare = extractBareType((v as FieldInfo).returns![0].type);
                retFields = (v as FieldInfo).returns![0].fields;
            } else if (v.type.startsWith('func(')) {
                const match = v.type.match(/func\([^)]*\)\s*(.+)/);
                if (match && match[1]) {
                    let retType = match[1].trim();
                    if (retType.startsWith('(')) {
                        const commaIdx = retType.indexOf(',');
                        const endIdx = retType.indexOf(')');
                        const cutIdx = commaIdx !== -1 ? commaIdx : endIdx;
                        retType = retType.slice(1, cutIdx).trim();
                    }
                    bare = extractBareType(retType);
                }
            }

            const fieldsToIndex = retFields || v.fields;

            if (bare && bare !== 'method' && !bare.startsWith('func(') && fieldsToIndex && fieldsToIndex.length > 0) {
                const existing = typeIndex.get(bare);
                if (existing) {
                    const existingNames = new Set(existing.map(f => f.name));
                    for (const f of fieldsToIndex) {
                        if (!existingNames.has(f.name)) {
                            existing.push(f);
                            indexVar(f);
                        }
                    }
                } else {
                    typeIndex.set(bare, [...fieldsToIndex]);
                    for (const f of fieldsToIndex) indexVar(f);
                }
            } else if (fieldsToIndex && fieldsToIndex.length > 0) {
                for (const f of fieldsToIndex) indexVar(f);
            }
        };

        for (const v of vars.values()) indexVar(v);

        for (const frame of scopeStack) {
            if (frame.key === '.' && frame.fields) {
                for (const f of frame.fields) indexVar(f);
            }
        }

        const graph = this.graphBuilder.getGraph();
        for (const [, ctx] of graph.templates) {
            for (const v of ctx.vars.values()) indexVar(v);
        }

        for (const [, fn] of graph.funcMaps) {
            if (!fn.returnTypeFields || fn.returnTypeFields.length === 0) continue;
            const retType = fn.returns?.[0]?.type ?? '';
            const bare = extractBareType(retType);
            if (bare) {
                const existing = typeIndex.get(bare);
                if (existing) {
                    const existingNames = new Set(existing.map(f => f.name));
                    for (const f of fn.returnTypeFields) {
                        if (!existingNames.has(f.name)) {
                            existing.push(f);
                            indexVar(f);
                        }
                    }
                } else {
                    typeIndex.set(bare, [...fn.returnTypeFields]);
                    for (const f of fn.returnTypeFields) indexVar(f);
                }
            }
        }

        return (typeStr: string) => {
            const bare = extractBareType(typeStr);
            // Check locally-indexed fields first (built from vars + scope frames),
            // then fall back to the global type registry from the Go analyzer.
            return typeIndex.get(bare) ?? graph.typeRegistry.get(bare);
        };
    }

    formatAvailableVars(
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>
    ): string {
        const names: string[] = [];

        for (const k of blockLocals.keys()) names.push(k);
        for (const frame of scopeStack) {
            if (frame.locals) {
                for (const k of frame.locals.keys()) {
                    if (!names.includes(k)) names.push(k);
                }
            }
        }

        const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
        const contextFields: FieldInfo[] = dotFrame?.fields?.length
            ? dotFrame.fields
            : [...vars.values()].map(v => ({ name: v.name, type: v.type, isSlice: v.isSlice ?? false }));

        const MAX_FIELDS = 10;
        for (const f of contextFields.slice(0, MAX_FIELDS)) {
            names.push('.' + f.name);
        }
        if (contextFields.length > MAX_FIELDS) {
            names.push(`...and ${contextFields.length - MAX_FIELDS} more`);
        }

        return names.length > 0 ? names.join(', ') : 'none';
    }

    // ── Private: named-block context resolution for position-based APIs ───────

    resolveNamedBlockCallCtxForPosition(
        blockName: string,
        vars: Map<string, TemplateVar>,
        currentFileNodes: TemplateNode[],
        currentFilePath?: string
    ): {
        typeStr: string;
        fields?: FieldInfo[];
        isMap?: boolean;
        keyType?: string;
        elemType?: string;
        isSlice?: boolean;
    } | null {
        const graph = this.graphBuilder.getGraph();
        const blockCtx = graph.templates.get(blockName);
        if (blockCtx) {
            const cfCall = blockCtx.renderCalls.find(rc => rc.file === 'context-file');
            if (cfCall && cfCall.vars) {
                return {
                    typeStr: 'context',
                    fields: cfCall.vars as unknown as FieldInfo[],
                    isMap: false,
                    isSlice: false,
                };
            }
        }

        let currentFileVars = vars;
        if (currentFilePath) {
            for (const [, templateCtx] of graph.templates) {
                if (templateCtx.absolutePath === currentFilePath) {
                    currentFileVars = templateCtx.vars;
                    break;
                }
            }
        }

        const localCtx = this.findCallSiteContext(currentFileNodes, blockName, currentFileVars, []);
        if (localCtx) {
            if ((!localCtx.fields || localCtx.fields.length === 0) && blockCtx) {
                const synthFields = [...blockCtx.vars.values()] as unknown as FieldInfo[];
                if (synthFields.length > 0) {
                    return { ...localCtx, fields: synthFields };
                }
            }
            return localCtx;
        }

        for (const [, templateCtx] of graph.templates) {
            if (!templateCtx.absolutePath) continue;
            if (currentFilePath && templateCtx.absolutePath === currentFilePath) continue;
            if (!fs.existsSync(templateCtx.absolutePath)) continue;
            try {
                const openDoc = vscode.workspace.textDocuments.find(
                    d => d.uri.fsPath === templateCtx.absolutePath
                );
                const content = openDoc
                    ? openDoc.getText()
                    : fs.readFileSync(templateCtx.absolutePath, 'utf8');
                const fileNodes = this.parser.parse(content);
                const callCtx = this.findCallSiteContext(
                    fileNodes, blockName, templateCtx.vars, []
                );
                if (callCtx) {
                    if ((!callCtx.fields || callCtx.fields.length === 0) && blockCtx) {
                        const synthFields = [...blockCtx.vars.values()] as unknown as FieldInfo[];
                        if (synthFields.length > 0) {
                            return { ...callCtx, fields: synthFields };
                        }
                    }
                    return callCtx;
                }
            } catch { /* ignore */ }
        }
        return null;
    }
}
