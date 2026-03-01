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

// ── ScopeUtils class ──────────────────────────────────────────────────────────

/**
 * ScopeUtils encapsulates all shared scope-resolution and AST-traversal logic
 * that is consumed by ValidatorCore, HoverProvider, DefinitionProvider, and
 * CompletionProvider.
 *
 * Not safe for concurrent use from multiple goroutines (N/A — single-threaded JS).
 */
export class ScopeUtils {
    readonly parser: TemplateParser;
    readonly graphBuilder: KnowledgeGraphBuilder;

    constructor(parser: TemplateParser, graphBuilder: KnowledgeGraphBuilder) {
        this.parser = parser;
        this.graphBuilder = graphBuilder;
    }

    // ── Named block registry ──────────────────────────────────────────────────

    /**
     * Looks up a named block entry from the cross-file registry.
     * Returns the entry (or undefined), whether it is duplicated, and a human-readable
     * duplicate message when applicable.
     */
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

    /**
     * Builds the child variable map and scope stack for a container node
     * (range, with, block, define).  Returns null when the node type does not
     * introduce a new scope.
     */
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

            case 'block':
            case 'define': {
                return this.buildNamedBlockScope(node, vars, rootNodes);
            }

            default:
                return null;
        }
    }

    /**
     * Builds the scope for the body of a {{ define }} / {{ block }} node by
     * resolving the call-site context, then falling back to the knowledge graph.
     */
    buildNamedBlockScope(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        rootNodes: TemplateNode[]
    ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
        const name = node.blockName;
        if (!name) return null;

        const callCtx = this.findCallSiteContext(rootNodes, name, vars, []);
        if (callCtx) {
            return {
                childVars: this.fieldsToVarMap(callCtx.fields ?? []),
                childStack: [{
                    key: '.',
                    typeStr: callCtx.typeStr,
                    fields: callCtx.fields ?? [],
                    isMap: callCtx.isMap,
                    keyType: callCtx.keyType,
                    elemType: callCtx.elemType,
                    isSlice: callCtx.isSlice,
                }],
            };
        }

        const graph = this.graphBuilder.getGraph();
        const blockCtx = graph.templates.get(name);
        if (blockCtx) {
            return {
                childVars: blockCtx.vars,
                childStack: [{
                    key: '.',
                    typeStr: blockCtx.rootTypeStr ?? 'context',
                    fields: [...blockCtx.vars.values()] as unknown as FieldInfo[],
                    isMap: blockCtx.isMap,
                    keyType: blockCtx.keyType,
                    elemType: blockCtx.elemType,
                    isSlice: blockCtx.isSlice,
                }],
            };
        }

        if (node.kind === 'block' && node.path.length > 0) {
            const result = resolvePath(
                node.path, vars, [], undefined,
                this.buildFieldResolver(vars, [])
            );
            if (result.found) {
                return {
                    childVars: this.fieldsToVarMap(result.fields ?? []),
                    childStack: [{ key: '.', typeStr: result.typeStr, fields: result.fields ?? [] }],
                };
            }
        }

        return null;
    }

    /**
     * Builds a ScopeFrame representing the element type of a range target.
     * Returns null when the range target cannot be resolved.
     */
    buildRangeElemScope(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals?: Map<string, TemplateVar>
    ): ScopeFrame | null {
        const result = resolvePath(
            node.path, vars, scopeStack, blockLocals,
            this.buildFieldResolver(vars, scopeStack)
        );
        if (!result.found) return null;

        let sourceVar: TemplateVar | undefined = vars.get(node.path[0]);
        if (!sourceVar) {
            const df = scopeStack.slice().reverse().find(f => f.key === '.');
            const field = df?.fields?.find(f => f.name === node.path[0]);
            if (field) sourceVar = fieldInfoToTemplateVar(field);
        }

        const elemTypeStr =
            result.isSlice && result.typeStr.startsWith('[]')
                ? result.typeStr.slice(2)
                : result.isMap && result.elemType
                    ? result.elemType
                    : result.typeStr;

        let isElemSlice = false;
        let isElemMap = false;
        let elemKeyType: string | undefined;
        let elemInnerType = elemTypeStr;

        if (elemTypeStr.startsWith('[]')) {
            isElemSlice = true;
            elemInnerType = elemTypeStr.slice(2);
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
            }
        }

        const elemScope: ScopeFrame = {
            key: '.',
            typeStr: elemTypeStr,
            fields: result.fields ?? [],
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
                    type: result.isMap ? (result.keyType ?? 'unknown') : 'int',
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

    /**
     * Records the inferred type of each assigned variable into blockLocals.
     * Handles single-variable, map-destructure (k, v), and slice-destructure assignments.
     */
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

    /**
     * Processes an assignment node by inferring the RHS type (expression first,
     * then path-resolution fallback) and recording the result into blockLocals.
     */
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

        // 1. Try expression inference.
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

        // 2. Fallback to path resolution.
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

            if (result.found && isValidPath) {
                resolvedType = result;
            }
        }

        if (resolvedType && node.assignVars?.length) {
            this.applyAssignmentLocals(node.assignVars, resolvedType, blockLocals);
        }
    }

    // ── AST queries ───────────────────────────────────────────────────────────

    /**
     * Returns the innermost {{ define }} / {{ block }} node that contains position,
     * or null when the cursor is not inside any named block.
     */
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

    /**
     * Finds the call-site context of a named block by scanning nodes for
     * {{ template "name" <ctx> }} or {{ block "name" <ctx> }} tags.
     * Returns the resolved type/fields of the context argument, or null.
     */
    findCallSiteContext(
        nodes: TemplateNode[],
        blockName: string,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar> = new Map()
    ): {
        typeStr: string;
        fields?: FieldInfo[];
        isMap?: boolean;
        keyType?: string;
        elemType?: string;
        isSlice?: boolean;
    } | null {
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

                if (contextArg === '.' || contextArg === '') {
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

                if (contextArg.startsWith('dict ')) {
                    const dictType = inferExpressionType(
                        contextArg, vars, scopeStack, blockLocals,
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
                    this.parser.parseDotPath(contextArg), vars, scopeStack, blockLocals,
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
                }

                const found = this.findCallSiteContext(
                    node.children, blockName, vars, childStack, childLocals
                );
                if (found) return found;
            }
        }
        return null;
    }

    /**
     * Walks the AST to find the first {{ define "name" }} / {{ block "name" }} node
     * with the given name.  Returns null when not found.
     */
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

    /**
     * Traverses nodes to find the leaf node that spans position, propagating
     * the correct scope stack and blockLocals through range/with/block containers.
     * Returns null when no node covers position.
     */
    findNodeAtPosition(
        nodes: TemplateNode[],
        position: vscode.Position,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        rootNodes: TemplateNode[],
        inheritedLocals?: Map<string, TemplateVar>
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
                    node.blockName, vars, rootNodes
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
                    childLocals = new Map(); // Fresh context for named blocks.
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

            if (node.children) {
                const found = this.findNodeAtPosition(
                    node.children, position, childVars, childStack, rootNodes, childLocals
                );
                if (found) return found;
            }

            // Else branch reverts to parent scope — walk it with vars/scopeStack, not childStack.
            const elseChildren = (node as any).elseChildren as TemplateNode[] | undefined;
            if (elseChildren && elseChildren.length > 0) {
                const found = this.findNodeAtPosition(
                    elseChildren, position, vars, scopeStack, rootNodes, blockLocals
                );
                if (found) return found;
            }
        }

        return null;
    }

    /**
     * Determines the active scope (stack + locals) at a given document position
     * by walking the AST and propagating scopes through container nodes.
     */
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
                        node.blockName, vars, rootNodes
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
            }

            const childScopeBuild = this.buildChildScope(
                node, vars, scopeStack, childLocals, rootNodes
            );
            if (childScopeBuild) {
                childVars = childScopeBuild.childVars;
                childStack = childScopeBuild.childStack;
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
                // If position is in the else branch, use parent scope — the else clause
                // of range/with/if reverts dot back to whatever it was before the block.
                const elseChildren = (node as any).elseChildren as TemplateNode[] | undefined;
                if (elseChildren && elseChildren.length > 0 && position.line >= elseChildren[0].line - 1) {
                    return this.findScopeAtPosition(
                        elseChildren, position, vars, scopeStack, rootNodes, ctx, blockLocals
                    );
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

    /** Converts a FieldInfo array into a TemplateVar map keyed by field name. */
    fieldsToVarMap(fields: FieldInfo[]): Map<string, TemplateVar> {
        const m = new Map<string, TemplateVar>();
        for (const f of fields) m.set(f.name, fieldInfoToTemplateVar(f));
        return m;
    }

    /**
     * Builds a field-resolver closure that maps a bare Go type name (e.g. "User")
     * to its FieldInfo array.  Used to hydrate funcMap return types that carry only
     * a type string with no field metadata.
     */
    buildFieldResolver(
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[]
    ): (typeStr: string) => FieldInfo[] | undefined {
        const typeIndex = new Map<string, FieldInfo[]>();

        const indexVar = (v: TemplateVar | FieldInfo) => {
            const typeName = v.type.startsWith('*') ? v.type.slice(1) : v.type;
            const bare = typeName.startsWith('[]')
                ? typeName.slice(2)
                : typeName.startsWith('map[')
                    ? typeName.slice(typeName.indexOf(']') + 1)
                    : typeName;

            if (v.fields && v.fields.length > 0 && !typeIndex.has(bare)) {
                typeIndex.set(bare, v.fields);
                for (const f of v.fields) indexVar(f);
            }
        };

        for (const v of vars.values()) indexVar(v);

        const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
        if (dotFrame?.fields) {
            for (const f of dotFrame.fields) indexVar(f);
        }

        return (typeStr: string) => {
            const bare = typeStr.startsWith('*') ? typeStr.slice(1) : typeStr;
            return typeIndex.get(bare);
        };
    }

    // ── Private: named-block context resolution for position-based APIs ───────

    /**
     * Resolves the call-site context of a named block for hover/completion/definition
     * consumers.  Checks the context-file registry first, then scans call sites in
     * the current and other template files.
     */
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

        const localCtx = this.findCallSiteContext(currentFileNodes, blockName, vars, []);
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
