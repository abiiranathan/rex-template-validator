/**
 * Package validatorCore provides the core template validation logic for the Rex Template Validator.
 * It handles AST traversal, scope resolution, and diagnostic generation.
 *
 * Named blocks ({{ define "name" }} / {{ block "name" ... }}) are resolved
 * from a cross-file NamedBlockRegistry built by KnowledgeGraphBuilder.
 *
 * Duplicate block-name detection: if the same name is declared in more than one
 * file, a diagnostic error is surfaced on every call-site that references it.
 */

import * as fs from 'fs';
import * as vscode from 'vscode';
import {
    FieldInfo,
    ScopeFrame,
    TemplateContext,
    TemplateNode,
    TemplateVar,
    ValidationError,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { inferExpressionType, TypeResult } from './compiler/expressionParser';
import { ResolveResult } from './templateParser';
import { ScopeUtils, fieldInfoToTemplateVar, isFileBasedPartial } from './scopeUtils';

/**
 * ValidatorCore performs structural validation of Go templates.
 * It emits ValidationError values; conversion to vscode.Diagnostic is handled by TemplateValidator.
 */
export class ValidatorCore {
    private readonly parser: TemplateParser;
    private readonly graphBuilder: KnowledgeGraphBuilder;
    private readonly outputChannel: vscode.OutputChannel;
    private readonly scope: ScopeUtils;

    constructor(
        outputChannel: vscode.OutputChannel,
        graphBuilder: KnowledgeGraphBuilder,
        scope: ScopeUtils
    ) {
        this.outputChannel = outputChannel;
        this.graphBuilder = graphBuilder;
        this.parser = scope.parser;
        this.scope = scope;
    }

    // ── Public API ─────────────────────────────────────────────────────────────

    /**
     * Validates a template document's content against the provided context.
     * Returns a list of ValidationError values; never throws.
     */
    validate(content: string, ctx: TemplateContext, filePath: string): ValidationError[] {
        const errors: ValidationError[] = [];
        const nodes = this.parser.parse(content);

        const rootScope: ScopeFrame[] = [];
        if (ctx.isMap || ctx.isSlice || ctx.rootTypeStr) {
            rootScope.push({
                key: '.',
                typeStr: ctx.rootTypeStr ?? 'context',
                fields: [...ctx.vars.values()] as unknown as FieldInfo[],
                isMap: ctx.isMap,
                keyType: ctx.keyType,
                elemType: ctx.elemType,
                isSlice: ctx.isSlice,
            });
        }

        this.validateNodes(nodes, ctx.vars, rootScope, errors, ctx, filePath);
        return errors;
    }

    // ── Node traversal ─────────────────────────────────────────────────────────

    /**
     * Recursively validates a list of template nodes, maintaining inherited
     * blockLocals across siblings.
     */
    validateNodes(
        nodes: TemplateNode[],
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string,
        inheritedLocals?: Map<string, TemplateVar>
    ) {
        const blockLocals = inheritedLocals
            ? new Map(inheritedLocals)
            : new Map<string, TemplateVar>();
        for (const node of nodes) {
            this.validateNode(
                node, vars, scopeStack, blockLocals, errors, ctx, filePath
            );
        }
    }

    private validateNode(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        switch (node.kind) {
            case 'assignment': {
                this.validateAssignment(
                    node, vars, scopeStack, blockLocals, errors
                );
                break;
            }

            case 'variable': {
                this.validateVariable(
                    node, vars, scopeStack, blockLocals, errors
                );
                break;
            }

            case 'range': {
                this.validateRange(
                    node, vars, scopeStack, blockLocals, errors, ctx, filePath
                );
                return; // Children handled inside.
            }

            case 'with': {
                this.validateWith(
                    node, vars, scopeStack, blockLocals, errors, ctx, filePath
                );
                return; // Children handled inside.
            }

            case 'if': {
                if (node.path.length > 0) {
                    if (!resolvePath(
                        node.path, vars, scopeStack, blockLocals,
                        this.scope.buildFieldResolver(vars, scopeStack)
                    ).found) {
                        errors.push({
                            message: `".${node.path.join('.')}" is not defined`,
                            line: node.line,
                            col: node.col,
                            severity: 'warning',
                            variable: node.path[0],
                        });
                    }
                }
                break;
            }

            case 'partial': {
                this.validatePartial(
                    node, vars, scopeStack, blockLocals, errors, ctx, filePath
                );
                return;
            }

            case 'block':
            case 'define': {
                this.validateNamedBlockBody(
                    node, vars, scopeStack, blockLocals, errors, ctx, filePath
                );
                return;
            }
        }

        if (node.children) {
            this.validateNodes(
                node.children, vars, scopeStack, errors, ctx, filePath, blockLocals
            );
        }
    }

    // ── Assignment validation ─────────────────────────────────────────────────

    private validateAssignment(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[]
    ) {
        this.outputChannel.appendLine(
            `Validating assignment: "${node.assignExpr}" at ${node.line}:${node.col}`
        );

        let resolvedType: TypeResult | ResolveResult | null = null;

        // 1. Try expression evaluation first.
        try {
            if (node.assignExpr) {
                const exprType = inferExpressionType(
                    node.assignExpr, vars, scopeStack, blockLocals,
                    this.graphBuilder.getGraph().funcMaps,
                    this.scope.buildFieldResolver(vars, scopeStack)
                );
                if (exprType) {
                    resolvedType = exprType;
                    this.outputChannel.appendLine(
                        `  Assignment expression inferred: ${JSON.stringify(exprType)}`
                    );
                }
            }
        } catch (e) {
            this.outputChannel.appendLine(`  Assignment inference error: ${e}`);
        }

        // 2. Fallback to path resolution.
        if (!resolvedType) {
            const result = resolvePath(
                node.path, vars, scopeStack, blockLocals,
                this.scope.buildFieldResolver(vars, scopeStack)
            );
            const isExplicitContext = node.assignExpr === '.' || node.assignExpr === '$';
            const isValidPath =
                (node.path.length > 0 &&
                    !(node.path.length === 1 && node.path[0] === '.' && !isExplicitContext)) ||
                isExplicitContext;

            if (result.found && isValidPath) {
                resolvedType = result;
                this.outputChannel.appendLine(
                    `  Assignment resolved via path: ${JSON.stringify(result)}`
                );
            } else {
                this.outputChannel.appendLine(
                    `  Assignment path resolution ignored (empty path or invalid).`
                );
            }
        }

        if (resolvedType) {
            if (node.assignVars?.length) {
                this.scope.applyAssignmentLocals(node.assignVars, resolvedType, blockLocals);
            }
        } else {
            this.outputChannel.appendLine(`  Assignment validation failed.`);
            errors.push({
                message: `Expression "${node.assignExpr}" is invalid or undefined`,
                line: node.line,
                col: node.col,
                severity: 'error',
                variable: node.assignExpr,
            });
        }
    }

    // ── Variable validation ───────────────────────────────────────────────────

    private validateVariable(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[]
    ) {
        if (node.path.length === 0) return;
        if (node.path[0] === '.') return;
        if (node.path[0] === '$' && node.path.length === 1) return;

        const cleanExpr = node.rawText
            ? node.rawText.replace(/^\{\{-?\s*/, '').replace(/\s*-?\}\}$/, '')
            : '';

        this.outputChannel.appendLine(
            `Validating variable/expression: "${cleanExpr}" at ${node.line}:${node.col}`
        );

        // 1. Always attempt expression evaluation first.
        let exprType = null;
        try {
            if (cleanExpr) {
                this.outputChannel.appendLine(
                    `  Attempting inferExpressionType for: "${cleanExpr}"`
                );
                exprType = inferExpressionType(
                    cleanExpr, vars, scopeStack, blockLocals,
                    this.graphBuilder.getGraph().funcMaps,
                    this.scope.buildFieldResolver(vars, scopeStack)
                );
                this.outputChannel.appendLine(
                    `  inferExpressionType result: ${exprType ? JSON.stringify(exprType) : 'null'}`
                );
            }
        } catch (e) {
            this.outputChannel.appendLine(`  inferExpressionType threw error: ${e}`);
        }

        if (exprType) {
            this.outputChannel.appendLine(`  Expression valid.`);
            return;
        }

        this.outputChannel.appendLine(
            `  Expression inference failed. Checking for sub-variables...`
        );

        // 2. If expression failed, check for specific missing sub-variables.
        if (cleanExpr) {
            const refs = cleanExpr.match(
                /(\(index\s+(?:\$|\.)[\w\d_.]+\s+[^)]+\)(?:\.[\w\d_.]+)*|(?:\$|\.)[\w\d_.[\]]*)/g
            );

            let subVarError = false;
            if (refs) {
                for (const ref of refs) {
                    if (/^\.\d+$/.test(ref)) continue;
                    if (ref === '...') continue;

                    const subPath = this.parser.parseDotPath(ref);
                    if (
                        subPath.length === 0 ||
                        (subPath.length === 1 && (subPath[0] === '.' || subPath[0] === '$'))
                    ) continue;

                    const subResult = resolvePath(
                        subPath, vars, scopeStack, blockLocals,
                        this.scope.buildFieldResolver(vars, scopeStack)
                    );
                    this.outputChannel.appendLine(
                        `  Checking sub-variable "${ref}" -> Found: ${subResult.found}`
                    );

                    if (!subResult.found) {
                        let errCol = node.col;
                        const refIdx = node.rawText.indexOf(ref);
                        if (refIdx !== -1) errCol = node.col + refIdx;

                        const displayPath =
                            subPath[0] === '$'
                                ? '$.' + subPath.slice(1).join('.')
                                : subPath[0].startsWith('$')
                                    ? subPath.join('.')
                                    : '.' + subPath.join('.');

                        const errMsg = `Template variable "${displayPath}" is not defined in the render context`;
                        this.outputChannel.appendLine(`    Error: ${errMsg}`);

                        errors.push({
                            message: errMsg,
                            line: node.line,
                            col: errCol,
                            severity: 'error',
                            variable: ref,
                        });
                        subVarError = true;
                    }
                }
            }
            if (subVarError) return;
        }

        // 3. Fallback: check if it is a simple path that failed evaluation.
        const pathResolved = resolvePath(
            node.path, vars, scopeStack, blockLocals,
            this.scope.buildFieldResolver(vars, scopeStack)
        ).found;
        this.outputChannel.appendLine(
            `  Fallback pathResolved: ${pathResolved} (path: ${node.path.join('.')})`
        );

        const isComplex = cleanExpr && /[\s|()]/.test(cleanExpr);

        if (!pathResolved || isComplex) {
            this.outputChannel.appendLine(
                `  Validation FAILED. isComplex: ${isComplex}`
            );

            let message: string;
            if (isComplex) {
                message = `Undefined or invalid function call: "${cleanExpr}"`;
            } else {
                const displayPath =
                    node.path[0] === '$'
                        ? '$.' + node.path.slice(1).join('.')
                        : node.path[0].startsWith('$')
                            ? node.path.join('.')
                            : '.' + node.path.join('.');
                message = `Template variable "${displayPath}" is not defined in the render context`;
            }

            errors.push({
                message,
                line: node.line,
                col: node.col,
                severity: 'error',
                variable: node.rawText,
            });
        }
    }

    // ── Range / With validation ───────────────────────────────────────────────

    private validateRange(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        const isBareDotsRange =
            node.path.length === 0 ||
            (node.path.length === 1 && node.path[0] === '.');

        if (!isBareDotsRange) {
            const result = resolvePath(
                node.path, vars, scopeStack, blockLocals,
                this.scope.buildFieldResolver(vars, scopeStack)
            );
            if (!result.found) {
                errors.push({
                    message: `Range target ".${node.path.join('.')}" is not defined`,
                    line: node.line,
                    col: node.col,
                    severity: 'error',
                    variable: node.path[0],
                });
                return;
            }
        }

        const elemScope = this.scope.buildRangeElemScope(
            node, vars, scopeStack, blockLocals
        );
        if (elemScope && node.children) {
            this.validateNodes(
                node.children, vars, [...scopeStack, elemScope],
                errors, ctx, filePath, blockLocals
            );
        }
    }

    private validateWith(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        if (node.path.length === 0) return;

        const result = resolvePath(
            node.path, vars, scopeStack, blockLocals,
            this.scope.buildFieldResolver(vars, scopeStack)
        );
        if (!result.found) {
            errors.push({
                message: `".${node.path.join('.')}" is not defined`,
                line: node.line,
                col: node.col,
                severity: 'warning',
                variable: node.path[0],
            });
            return;
        }

        if (result.fields !== undefined) {
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
                    elemType: result.elemType,
                    keyType: result.keyType,
                });
            }
            if (node.children) {
                this.validateNodes(
                    node.children, vars, [...scopeStack, childScope],
                    errors, ctx, filePath, blockLocals
                );
            }
        }
    }

    // ── Named block validation ────────────────────────────────────────────────

    private validateNamedBlockBody(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        if (!node.blockName) return;

        const { isDuplicate, duplicateMessage } = this.scope.resolveNamedBlock(node.blockName);
        if (isDuplicate && duplicateMessage) {
            errors.push({
                message: duplicateMessage,
                line: node.line,
                col: node.col,
                severity: 'error',
                variable: node.blockName,
            });
        }

        if (!node.children || node.children.length === 0) return;

        const childScope = this.resolveNamedBlockChildScope(
            node, vars, scopeStack, blockLocals, filePath
        );

        if (childScope) {
            this.validateNodes(
                node.children, childScope.childVars, childScope.childStack,
                errors, ctx, filePath
            );
        } else {
            this.validateNodes(
                node.children, vars, scopeStack, errors, ctx, filePath
            );
        }
    }

    private resolveNamedBlockChildScope(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        currentFilePath: string
    ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
        const name = node.blockName!;

        // 1. Context-file definition takes highest priority.
        const graph = this.graphBuilder.getGraph();
        const blockCtx = graph.templates.get(name);
        if (blockCtx) {
            const cfCall = blockCtx.renderCalls.find(rc => rc.file === 'context-file');
            if (cfCall && cfCall.vars) {
                const childVars = new Map<string, TemplateVar>();
                for (const v of cfCall.vars) childVars.set(v.name, v);
                return {
                    childVars,
                    childStack: [{
                        key: '.',
                        typeStr: 'context',
                        fields: cfCall.vars as unknown as FieldInfo[],
                        isMap: false,
                        isSlice: false,
                    }],
                };
            }
        }

        // 2. Local call-site in the current file.
        let currentFileNodes: TemplateNode[] = [];
        try {
            const openDoc = vscode.workspace.textDocuments.find(
                d => d.uri.fsPath === currentFilePath
            );
            const content = openDoc
                ? openDoc.getText()
                : fs.existsSync(currentFilePath)
                    ? fs.readFileSync(currentFilePath, 'utf8')
                    : '';
            if (content) currentFileNodes = this.parser.parse(content);
        } catch { /* ignore */ }

        const localCallCtx = this.scope.findCallSiteContext(
            currentFileNodes, name, vars, scopeStack, new Map(blockLocals)
        );
        if (localCallCtx) {
            return {
                childVars: this.scope.fieldsToVarMap(localCallCtx.fields ?? []),
                childStack: [{
                    key: '.',
                    typeStr: localCallCtx.typeStr,
                    fields: localCallCtx.fields ?? [],
                    isMap: localCallCtx.isMap,
                    keyType: localCallCtx.keyType,
                    elemType: localCallCtx.elemType,
                    isSlice: localCallCtx.isSlice,
                }],
            };
        }

        // 3. Scan other template files.
        for (const [, templateCtx] of graph.templates) {
            if (!templateCtx.absolutePath || templateCtx.absolutePath === currentFilePath) continue;
            if (!fs.existsSync(templateCtx.absolutePath)) continue;

            try {
                const openDoc = vscode.workspace.textDocuments.find(
                    d => d.uri.fsPath === templateCtx.absolutePath
                );
                const content = openDoc
                    ? openDoc.getText()
                    : fs.readFileSync(templateCtx.absolutePath, 'utf8');

                const fileNodes = this.parser.parse(content);
                const callCtx = this.scope.findCallSiteContext(
                    fileNodes, name, templateCtx.vars, []
                );
                if (callCtx) {
                    return {
                        childVars: this.scope.fieldsToVarMap(callCtx.fields ?? []),
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
            } catch { /* ignore */ }
        }

        // 4. Fall back to the block's own path argument.
        if (node.kind === 'block' && node.path.length > 0) {
            const result = resolvePath(
                node.path, vars, scopeStack, blockLocals,
                this.scope.buildFieldResolver(vars, scopeStack)
            );
            if (result.found) {
                return {
                    childVars: this.scope.fieldsToVarMap(result.fields ?? []),
                    childStack: [{
                        key: '.', typeStr: result.typeStr, fields: result.fields ?? [],
                    }],
                };
            }
        }

        return null;
    }

    private enrichScopeWithContextFile(
        blockName: string,
        partialVars: Map<string, TemplateVar>,
        childStack: ScopeFrame[]
    ): ScopeFrame[] {
        const graph = this.graphBuilder.getGraph();
        const blockCtx = graph.templates.get(blockName);
        if (!blockCtx) return childStack;

        const cfCall = blockCtx.renderCalls.find(rc => rc.file === 'context-file');
        if (!cfCall || !cfCall.vars) return childStack;

        for (const v of cfCall.vars) {
            if (!partialVars.has(v.name)) partialVars.set(v.name, v);
        }

        const globalFields: FieldInfo[] = cfCall.vars.map(v => ({
            name: v.name,
            type: v.type,
            fields: v.fields,
            isSlice: v.isSlice ?? false,
            doc: v.doc,
            defFile: v.defFile,
            defLine: v.defLine,
            defCol: v.defCol,
        }));

        const activeDotFrame = childStack.slice().reverse().find(f => f.key === '.');
        if (!activeDotFrame) {
            return [
                ...childStack,
                { key: '.', typeStr: 'context', fields: globalFields, isMap: false, isSlice: false },
            ];
        }

        const mergedFields = [...(activeDotFrame.fields ?? [])];
        const existingNames = new Set(mergedFields.map(f => f.name));
        for (const gf of globalFields) {
            if (!existingNames.has(gf.name)) mergedFields.push(gf);
        }

        return [...childStack, { ...activeDotFrame, fields: mergedFields }];
    }

    // ── Partial validation ────────────────────────────────────────────────────

    private validatePartial(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        if (!node.partialName) return;

        const contextArg = node.partialContext ?? '.';
        if (contextArg !== '.' && contextArg !== '$' && !contextArg.startsWith('dict ')) {
            const contextPath = this.parser.parseDotPath(contextArg);
            if (contextPath.length > 0 && contextPath[0] !== '.') {
                const result = resolvePath(
                    contextPath, vars, scopeStack, blockLocals,
                    this.scope.buildFieldResolver(vars, scopeStack)
                );
                if (!result.found) {
                    let errCol = node.col;
                    if (node.rawText) {
                        const nameIdx = node.rawText.indexOf(`"${node.partialName}"`);
                        const searchStart =
                            nameIdx !== -1 ? nameIdx + node.partialName!.length + 2 : 0;
                        const ctxIdx = node.rawText.indexOf(contextArg, searchStart);
                        if (ctxIdx !== -1) {
                            const p = '.' + contextPath.join('.');
                            const pIdx = contextArg.indexOf(p);
                            errCol = node.col + ctxIdx + (pIdx !== -1 ? pIdx : 0);
                        }
                    }
                    errors.push({
                        message: `Template variable "${contextArg}" is not defined in the render context`,
                        line: node.line,
                        col: errCol,
                        severity: 'error',
                        variable: contextArg,
                    });
                    return;
                }
            }
        }

        if (!isFileBasedPartial(node.partialName)) {
            this.validateNamedBlockCallSite(
                node, vars, scopeStack, blockLocals, errors, ctx, filePath
            );
            return;
        }

        const partialCtx = this.graphBuilder.findPartialContext(node.partialName, filePath);
        if (!partialCtx) {
            let errCol = node.col;
            if (node.rawText && node.partialName) {
                const nameIdx = node.rawText.indexOf(`"${node.partialName}"`);
                if (nameIdx !== -1) errCol = node.col + nameIdx + 1;
            }
            errors.push({
                message: `Partial template "${node.partialName}" could not be found`,
                line: node.line,
                col: errCol,
                severity: 'warning',
                variable: node.partialName,
            });
            return;
        }

        if (!fs.existsSync(partialCtx.absolutePath)) return;

        const resolved = this.resolvePartialVars(
            contextArg, vars, scopeStack, blockLocals
        );
        try {
            const openDoc = vscode.workspace.textDocuments.find(
                d => d.uri.fsPath === partialCtx.absolutePath
            );
            const content = openDoc
                ? openDoc.getText()
                : fs.readFileSync(partialCtx.absolutePath, 'utf8');
            const partialErrors = this.validate(
                content,
                {
                    ...partialCtx,
                    vars: resolved.vars,
                    isMap: resolved.isMap,
                    keyType: resolved.keyType,
                    elemType: resolved.elemType,
                    isSlice: resolved.isSlice,
                    rootTypeStr: resolved.rootTypeStr,
                },
                partialCtx.absolutePath
            );
            for (const e of partialErrors) {
                errors.push({
                    ...e,
                    message: `[in partial "${node.partialName}"] ${e.message}`,
                });
            }
        } catch { /* ignore read errors */ }
    }

    private validateNamedBlockCallSite(
        callNode: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        if (!callNode.partialName) return;

        const { entry, isDuplicate, duplicateMessage } =
            this.scope.resolveNamedBlock(callNode.partialName);

        if (isDuplicate && duplicateMessage) {
            errors.push({
                message: duplicateMessage,
                line: callNode.line,
                col: callNode.col,
                severity: 'error',
                variable: callNode.partialName,
            });
        }

        if (!entry) {
            this.validateNamedBlockFromCurrentFile(
                callNode, vars, scopeStack, blockLocals, errors, ctx, filePath
            );
            return;
        }

        const contextArg = callNode.partialContext ?? '.';
        const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
        const partialVars = resolved.vars;

        let childStack: ScopeFrame[];
        if (contextArg === '.' || contextArg === '$') {
            childStack = scopeStack;
        } else if (contextArg.startsWith('dict ')) {
            const dictType = inferExpressionType(
                contextArg, vars, scopeStack, blockLocals,
                this.graphBuilder.getGraph().funcMaps,
                this.scope.buildFieldResolver(vars, scopeStack)
            );
            childStack = dictType && dictType.fields
                ? [{
                    key: '.',
                    typeStr: dictType.typeStr,
                    fields: dictType.fields,
                    isMap: dictType.isMap,
                    keyType: dictType.keyType,
                    elemType: dictType.elemType,
                    isSlice: dictType.isSlice,
                }]
                : scopeStack;
        } else {
            const result = resolvePath(
                this.parser.parseDotPath(contextArg),
                vars, scopeStack, blockLocals,
                this.scope.buildFieldResolver(vars, scopeStack)
            );
            childStack = result.found
                ? [{
                    key: '.',
                    typeStr: result.typeStr,
                    fields: result.fields ?? [],
                    isMap: result.isMap,
                    keyType: result.keyType,
                    elemType: result.elemType,
                    isSlice: result.isSlice,
                }]
                : scopeStack;
        }

        childStack = this.enrichScopeWithContextFile(
            callNode.partialName, partialVars, childStack
        );

        if (!entry?.node?.children || entry.node.children.length === 0) return;

        if (entry.absolutePath === filePath) {
            const blockErrors: ValidationError[] = [];
            this.validateNodes(
                entry.node.children, partialVars, childStack, blockErrors, ctx, filePath
            );
            for (const e of blockErrors) {
                errors.push({
                    ...e,
                    message: `[in block "${callNode.partialName}"] ${e.message}`,
                });
            }
            return;
        }

        try {
            const openDoc = vscode.workspace.textDocuments.find(
                d => d.uri.fsPath === entry.absolutePath
            );
            const blockFileContent = openDoc
                ? openDoc.getText()
                : fs.readFileSync(entry.absolutePath, 'utf8');

            const blockFileNodes = this.parser.parse(blockFileContent);
            const freshBlockNode = this.scope.findDefineNodeInAST(
                blockFileNodes, callNode.partialName
            );
            if (!freshBlockNode?.children) return;

            const blockCtx: TemplateContext = {
                templatePath: entry.templatePath,
                absolutePath: entry.absolutePath,
                vars: partialVars,
                renderCalls: ctx.renderCalls,
            };

            const blockErrors: ValidationError[] = [];
            this.validateNodes(
                freshBlockNode.children, partialVars, childStack,
                blockErrors, blockCtx, entry.absolutePath
            );
            for (const e of blockErrors) {
                errors.push({
                    ...e,
                    message: `[in block "${callNode.partialName}" @ ${entry.templatePath}] ${e.message}`,
                });
            }
        } catch { /* ignore */ }
    }

    private validateNamedBlockFromCurrentFile(
        callNode: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        if (!callNode.partialName) return;

        const contextArg = callNode.partialContext ?? '.';
        const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
        const partialVars = resolved.vars;

        try {
            const openDoc = vscode.workspace.textDocuments.find(
                d => d.uri.fsPath === filePath
            );
            let content = '';
            if (openDoc) content = openDoc.getText();
            else if (fs.existsSync(filePath)) content = fs.readFileSync(filePath, 'utf8');
            else return;

            const blockNode = this.scope.findDefineNodeInAST(
                this.parser.parse(content), callNode.partialName
            );
            if (!blockNode) {
                let errCol = callNode.col;
                if (callNode.rawText && callNode.partialName) {
                    const nameIdx = callNode.rawText.indexOf(`"${callNode.partialName}"`);
                    if (nameIdx !== -1) errCol = callNode.col + nameIdx + 1;
                }
                errors.push({
                    message: `Template "${callNode.partialName}" not found`,
                    line: callNode.line,
                    col: errCol,
                    severity: 'error',
                    variable: callNode.partialName,
                });
                return;
            }
            if (!blockNode.children) return;

            let childStack: ScopeFrame[];
            if (contextArg === '.' || contextArg === '$') {
                childStack = scopeStack;
            } else if (contextArg.startsWith('dict ')) {
                const dictType = inferExpressionType(
                    contextArg, vars, scopeStack, blockLocals,
                    this.graphBuilder.getGraph().funcMaps,
                    this.scope.buildFieldResolver(vars, scopeStack)
                );
                childStack = dictType && dictType.fields
                    ? [{
                        key: '.',
                        typeStr: dictType.typeStr,
                        fields: dictType.fields,
                        isMap: dictType.isMap,
                        keyType: dictType.keyType,
                        elemType: dictType.elemType,
                        isSlice: dictType.isSlice,
                    }]
                    : scopeStack;
            } else {
                const result = resolvePath(
                    this.parser.parseDotPath(contextArg),
                    vars, scopeStack, blockLocals,
                    this.scope.buildFieldResolver(vars, scopeStack)
                );
                childStack = result.found
                    ? [{
                        key: '.',
                        typeStr: result.typeStr,
                        fields: result.fields ?? [],
                        isMap: result.isMap,
                        keyType: result.keyType,
                        elemType: result.elemType,
                        isSlice: result.isSlice,
                    }]
                    : scopeStack;
            }

            childStack = this.enrichScopeWithContextFile(
                callNode.partialName, partialVars, childStack
            );

            const blockErrors: ValidationError[] = [];
            this.validateNodes(
                blockNode.children, partialVars, childStack, blockErrors, ctx, filePath
            );
            for (const e of blockErrors) {
                errors.push({
                    ...e,
                    message: `[in block "${callNode.partialName}"] ${e.message}`,
                });
            }
        } catch { /* ignore */ }
    }

    // ── Partial variable resolution ───────────────────────────────────────────

    /**
     * Resolves the variable map and type metadata that should be used as the
     * render context when validating a partial or named-block invocation.
     */
    resolvePartialVars(
        contextArg: string,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>
    ): {
        vars: Map<string, TemplateVar>;
        isMap?: boolean;
        keyType?: string;
        elemType?: string;
        isSlice?: boolean;
        rootTypeStr?: string;
    } {
        if (contextArg === '.' || contextArg === '$') {
            const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
            if (dotFrame) {
                const result = new Map<string, TemplateVar>();
                if (dotFrame.fields) {
                    for (const f of dotFrame.fields) result.set(f.name, fieldInfoToTemplateVar(f));
                }
                return {
                    vars: result,
                    isMap: dotFrame.isMap,
                    keyType: dotFrame.keyType,
                    elemType: dotFrame.elemType,
                    isSlice: dotFrame.isSlice,
                    rootTypeStr: dotFrame.typeStr,
                };
            }
            return { vars: new Map(vars) };
        }

        if (contextArg.startsWith('dict ')) {
            const dictType = inferExpressionType(
                contextArg, vars, scopeStack, blockLocals,
                this.graphBuilder.getGraph().funcMaps,
                this.scope.buildFieldResolver(vars, scopeStack)
            );
            if (dictType && dictType.fields) {
                const partialVars = new Map<string, TemplateVar>();
                for (const f of dictType.fields) partialVars.set(f.name, fieldInfoToTemplateVar(f));
                return {
                    vars: partialVars,
                    isMap: dictType.isMap,
                    keyType: dictType.keyType,
                    elemType: dictType.elemType,
                    isSlice: dictType.isSlice,
                    rootTypeStr: dictType.typeStr,
                };
            }
        }

        const result = resolvePath(
            this.parser.parseDotPath(contextArg),
            vars, scopeStack, blockLocals,
            this.scope.buildFieldResolver(vars, scopeStack)
        );

        const partialVars = new Map<string, TemplateVar>();
        if (result.found && result.fields) {
            for (const f of result.fields) partialVars.set(f.name, fieldInfoToTemplateVar(f));
        }

        return {
            vars: partialVars,
            isMap: result.isMap,
            keyType: result.keyType,
            elemType: result.elemType,
            isSlice: result.isSlice,
            rootTypeStr: result.typeStr,
        };
    }
}
