import * as fs from 'fs';
import * as vscode from 'vscode';
import {
    FieldInfo,
    NamedBlockEntry,
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
import { ScopeUtils, fieldInfoToTemplateVar, isFileBasedPartial, normalizeDictArg } from './scopeUtils';

/**
 * ValidatorCore performs structural validation of Go templates.
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
                this.validateAssignment(node, vars, scopeStack, blockLocals, errors);
                break;
            }
            case 'variable': {
                this.validateVariable(node, vars, scopeStack, blockLocals, errors);
                break;
            }
            case 'range': {
                this.validateRange(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
                return;
            }
            case 'with': {
                this.validateWith(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
                return;
            }
            case 'if': {
                this.validateIf(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
                return;
            }
            case 'partial': {
                this.validatePartial(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
                return;
            }
            case 'block':
            case 'define': {
                this.validateNamedBlockBody(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
                return;
            }
        }

        if (node.children) {
            this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath, blockLocals);
        }
        if (node.elseChildren) {
            this.validateNodes(node.elseChildren, vars, scopeStack, errors, ctx, filePath, blockLocals);
        }
    }

    // ── Shared Expression Validation ──────────────────────────────────────────

    private validateExpression(
        node: TemplateNode,
        expr: string,
        path: string[],
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[]
    ) {
        if (!expr) return;
        if (path && path.length === 1 && (path[0] === '.' || path[0] === '$')) return;

        const fieldResolver = this.scope.buildFieldResolver(vars, scopeStack);

        // ── Step 1: Type-inference pass ─────────────────────────────────────────
        try {
            const exprType = inferExpressionType(
                expr, vars, scopeStack, blockLocals,
                this.graphBuilder.getGraph().funcMaps,
                fieldResolver
            );
            if (exprType) return;
        } catch (e) {
            this.outputChannel.appendLine(`  inferExpressionType threw error: ${e}`);
        }

        // ── Step 2: Sub-variable resolution pass ────────────────────────────────
        const varRefPattern =
            /(\(index\s+(?:\$|\.)[\w\d_.]+\s+[^)]+\)(?:\.[\w\d_.]+)*|(?:\$|\.)[\w\d_.[\]]*)/g;
        const refs = expr.match(varRefPattern);

        if (refs) {
            let foundMissingRef = false;

            for (const ref of refs) {
                if (/^\.\d+$/.test(ref) || ref === '...') continue;
                if (ref === '.' || ref === '$') continue;

                const subPath = this.parser.parseDotPath(ref);

                const isRootOnlyPath =
                    subPath.length === 0 ||
                    (subPath.length === 1 && (subPath[0] === '.' || subPath[0] === '$'));
                if (isRootOnlyPath) continue;

                const { found } = resolvePath(subPath, vars, scopeStack, blockLocals, fieldResolver);
                if (found) continue;

                const refOffset = node.rawText.indexOf(ref);
                const errCol = refOffset !== -1 ? node.col + refOffset : node.col;

                const displayPath = this.formatDisplayPath(subPath);
                const available = this.scope.formatAvailableVars(vars, scopeStack, blockLocals);

                errors.push({
                    message: `Template variable "${displayPath}" is not defined in the render context. Available: ${available}`,
                    line: node.line,
                    col: errCol,
                    severity: 'error',
                    variable: ref,
                });
                foundMissingRef = true;
            }

            if (foundMissingRef) return;
        }

        // ── Step 3: Bare-path fallback ───────────────────────────────────────────
        const isComplexExpr = /[\s|()]/.test(expr);
        if (isComplexExpr) return;

        const { found: pathResolved } = resolvePath(
            path, vars, scopeStack, blockLocals, fieldResolver
        );

        if (!pathResolved) {
            const displayPath = this.formatDisplayPath(path);
            const available = this.scope.formatAvailableVars(vars, scopeStack, blockLocals);
            errors.push({
                message: `Template variable "${displayPath}" is not defined in the render context. Available: ${available}`,
                line: node.line,
                col: node.col,
                severity: 'error',
                variable: expr,
            });
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
                this.outputChannel.appendLine(`  Assignment path resolution ignored.`);
            }
        }

        if (resolvedType) {
            if (node.assignVars?.length) {
                this.scope.applyAssignmentLocals(node.assignVars, resolvedType, blockLocals);
            }
        } else {
            this.outputChannel.appendLine(`  Assignment validation failed.`);
            const available = this.scope.formatAvailableVars(vars, scopeStack, blockLocals);
            errors.push({
                message: `Expression "${node.assignExpr}" is invalid or undefined. Available: ${available}`,
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
        const cleanExpr = node.rawText
            ? node.rawText.replace(/^\{\{-?\s*/, '').replace(/\s*-?\}\}$/, '').trim()
            : '';
        this.validateExpression(node, cleanExpr, node.path, vars, scopeStack, blockLocals, errors);
    }

    /** Formats a parsed path array into a human-readable `$.x.y` or `.x.y` string. */
    private formatDisplayPath(path: string[]): string {
        if (path[0] === '$') return '$.' + path.slice(1).join('.');
        if (path[0].startsWith('$')) return path.join('.');
        return '.' + path.join('.');
    }

    // ── If / Range / With validation ──────────────────────────────────────────

    private validateIf(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        const inner = node.rawText.replace(/^\{\{-?\s*/, '').replace(/\s*-?\}\}$/, '').trim();
        let expr = '';
        if (inner.startsWith('if ')) expr = inner.slice(3).trim();
        else if (inner.startsWith('else if ')) expr = inner.slice(8).trim();

        if (expr) {
            this.validateExpression(node, expr, node.path, vars, scopeStack, blockLocals, errors);
        }

        if (node.children) {
            this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath, blockLocals);
        }
        if (node.elseChildren) {
            this.validateNodes(node.elseChildren, vars, scopeStack, errors, ctx, filePath, blockLocals);
        }
    }

    private validateRange(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        if (node.assignExpr) {
            this.validateExpression(node, node.assignExpr, node.path, vars, scopeStack, blockLocals, errors);
        }

        const elemScope = this.scope.buildRangeElemScope(node, vars, scopeStack, blockLocals);
        if (elemScope && node.children) {
            this.validateNodes(
                node.children, vars, [...scopeStack, elemScope], errors, ctx, filePath, blockLocals
            );
        }

        if (node.elseChildren) {
            this.validateNodes(node.elseChildren, vars, scopeStack, errors, ctx, filePath, blockLocals);
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
        if (node.assignExpr) {
            this.validateExpression(node, node.assignExpr, node.path, vars, scopeStack, blockLocals, errors);
        }

        if (node.path.length > 0) {
            const result = resolvePath(
                node.path, vars, scopeStack, blockLocals,
                this.scope.buildFieldResolver(vars, scopeStack)
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
                        elemType: result.elemType,
                        keyType: result.keyType,
                    });
                }
                if (node.children) {
                    this.validateNodes(
                        node.children, vars, [...scopeStack, childScope], errors, ctx, filePath, blockLocals
                    );
                }
            }
        }

        if (node.elseChildren) {
            this.validateNodes(node.elseChildren, vars, scopeStack, errors, ctx, filePath, blockLocals);
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
                node.children, childScope.childVars, childScope.childStack, errors, ctx, filePath
            );
        }
        // When the context cannot be determined we intentionally skip body
        // validation here.  The body will be validated with the correct context
        // when we encounter the call site during validateNamedBlockFromCurrentFile.
        // Falling back to root vars (the old else branch) caused global template
        // vars to appear as "available" inside partials that only have access to
        // what was explicitly passed — which is not how Go template scoping works.
    }

    private resolveNamedBlockChildScope(
        node: TemplateNode,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        currentFilePath: string
    ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
        const name = node.blockName!;

        const graph = this.graphBuilder.getGraph();
        const blockCtx = graph.templates.get(name);

        const synthFields = (): FieldInfo[] =>
            blockCtx ? ([...blockCtx.vars.values()] as unknown as FieldInfo[]) : [];

        const wrapCallCtx = (callCtx: {
            typeStr: string;
            fields?: FieldInfo[];
            isMap?: boolean;
            keyType?: string;
            elemType?: string;
            isSlice?: boolean;
        }): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } => {
            let fields = callCtx.fields ?? [];
            if (fields.length === 0) {
                const sf = synthFields();
                if (sf.length > 0) fields = sf;
            }
            return {
                childVars: this.scope.fieldsToVarMap(fields),
                childStack: [{
                    key: '.',
                    typeStr: callCtx.typeStr,
                    fields,
                    isMap: callCtx.isMap,
                    keyType: callCtx.keyType,
                    elemType: callCtx.elemType,
                    isSlice: callCtx.isSlice,
                }],
            };
        };

        // 1. Context-file definition.
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

        let currentFileVars = vars;
        for (const [, templateCtx] of graph.templates) {
            if (templateCtx.absolutePath === currentFilePath) {
                currentFileVars = templateCtx.vars;
                break;
            }
        }

        const localCallCtx = this.scope.findCallSiteContext(
            currentFileNodes, name, currentFileVars, []
        );
        if (localCallCtx) {
            return wrapCallCtx(localCallCtx);
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
                    return wrapCallCtx(callCtx);
                }
            } catch { /* ignore */ }
        }

        // 4. Block's own path argument.
        if (node.kind === 'block' || (node.kind === 'define' && node.path.length > 0)) {
            const result = resolvePath(
                node.path, vars, scopeStack, blockLocals,
                this.scope.buildFieldResolver(vars, scopeStack)
            );

            if (result.found) {
                let fields = result.fields ?? [];
                if (fields.length === 0 && result.typeStr === 'context') {
                    fields = [...vars.values()] as unknown as FieldInfo[];
                }
                if (fields.length === 0) {
                    const sf = synthFields();
                    if (sf.length > 0) fields = sf;
                }
                return {
                    childVars: this.scope.fieldsToVarMap(fields),
                    childStack: [{
                        key: '.', typeStr: result.typeStr, fields: fields,
                        isMap: result.isMap, keyType: result.keyType,
                        elemType: result.elemType, isSlice: result.isSlice,
                    }],
                };
            }
        }

        // 5. Ultimate fallback: empty context to catch obvious typos.
        return wrapCallCtx({ typeStr: 'any', fields: [] });
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
        const normalizedCtx = normalizeDictArg(contextArg);

        if (normalizedCtx !== '.' && normalizedCtx !== '$') {
            this.validateExpression(node, normalizedCtx, this.parser.parseDotPath(normalizedCtx), vars, scopeStack, blockLocals, errors);
        }

        // 1. Try to find it as a Named Block first
        const { entry } = this.scope.resolveNamedBlock(node.partialName);
        if (entry) { // FIX: Don't check entry.node, it might be missing after incremental updates
            this.validateNamedBlockCall(node, entry, vars, scopeStack, blockLocals, errors, ctx, filePath);
            return;
        }

        // 2. If not a named block, try to find it as a file-based partial
        const partialCtx = this.graphBuilder.findPartialContext(node.partialName, filePath);
        if (partialCtx && fs.existsSync(partialCtx.absolutePath)) {
            this.validateFilePartialCall(node, partialCtx, contextArg, vars, scopeStack, blockLocals, errors, filePath);
            return;
        }

        // 3. If neither can be found, report an error
        let errCol = node.col;
        if (node.rawText) {
            const nameIdx = node.rawText.indexOf(`"${node.partialName}"`);
            if (nameIdx !== -1) errCol = node.col + nameIdx + 1;
        }
        errors.push({
            message: `Template or partial "${node.partialName}" could not be found`,
            line: node.line,
            col: errCol,
            severity: 'warning',
            variable: node.partialName,
        });
    }

    private validateNamedBlockCall(
        callNode: TemplateNode,
        entry: NamedBlockEntry,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        ctx: TemplateContext,
        filePath: string
    ) {
        const contextArg = callNode.partialContext ?? '.';
        const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
        const partialVars = resolved.vars;

        try {
            // Robustly find the block node by parsing the file where it's defined
            const openDoc = vscode.workspace.textDocuments.find(
                d => d.uri.fsPath === entry.absolutePath
            );
            let content = '';
            if (openDoc) content = openDoc.getText();
            else if (fs.existsSync(entry.absolutePath)) content = fs.readFileSync(entry.absolutePath, 'utf8');
            else return;

            const blockNode = this.scope.findDefineNodeInAST(
                this.parser.parse(content), callNode.partialName!
            );

            if (!blockNode || !blockNode.children) return;

            const normalizedCtx = normalizeDictArg(contextArg);
            let childStack: ScopeFrame[];

            if (normalizedCtx === '.' || normalizedCtx === '$') {
                childStack = scopeStack;
            } else if (normalizedCtx.startsWith('dict ')) {
                const dictType = inferExpressionType(
                    normalizedCtx, vars, scopeStack, blockLocals,
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
                    this.parser.parseDotPath(normalizedCtx),
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
                callNode.partialName!, partialVars, childStack
            );

            const blockErrors: ValidationError[] = [];
            this.validateNodes(
                blockNode.children, partialVars, childStack, blockErrors, ctx, entry.absolutePath
            );

            // Path normalization for safe comparison
            const isSameFile = require('path').normalize(entry.absolutePath).toLowerCase() === require('path').normalize(filePath).toLowerCase();

            for (const e of blockErrors) {
                if (isSameFile) {
                    // 1. Error on the exact variable inside the block definition
                    errors.push({
                        ...e,
                        line: e.line,
                        col: e.col,
                    });
                    // 2. Trace error at the call site
                    errors.push({
                        ...e,
                        line: callNode.line,
                        col: callNode.col,
                        message: `[in block "${callNode.partialName}"] ${e.message}`,
                    });
                } else {
                    // Block is in another file. We can only show the error at the call site in this document.
                    errors.push({
                        ...e,
                        line: callNode.line,
                        col: callNode.col,
                        message: `[in block "${callNode.partialName}"] ${e.message}`,
                    });
                }
            }
        } catch (err) {
            this.outputChannel.appendLine(`Error validating named block: ${err}`);
        }
    }

    private validateFilePartialCall(
        callNode: TemplateNode,
        partialCtx: TemplateContext,
        contextArg: string,
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar>,
        errors: ValidationError[],
        filePath: string
    ) {
        const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
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

            const isSameFile = require('path').normalize(partialCtx.absolutePath).toLowerCase() === require('path').normalize(filePath).toLowerCase();

            for (const e of partialErrors) {
                if (isSameFile) {
                    // 1. Error on the exact variable
                    errors.push({
                        ...e,
                        line: e.line,
                        col: e.col,
                    });
                    // 2. Trace error at the call site
                    errors.push({
                        ...e,
                        line: callNode.line,
                        col: callNode.col,
                        message: `[in partial "${callNode.partialName}"] ${e.message}`,
                    });
                } else {
                    // Partial is in another file
                    errors.push({
                        ...e,
                        line: callNode.line,
                        col: callNode.col,
                        message: `[in partial "${callNode.partialName}"] ${e.message}`,
                    });
                }
            }
        } catch { /* ignore read errors */ }
    }

    // ── Partial variable resolution ───────────────────────────────────────────

    /**
     * Resolves the variable map and type metadata for a partial/named-block invocation.
     *
     * FIX: normalise the contextArg before the dict check so that (dict ...) forms
     * (parenthesised by the template parser) are correctly handled.
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
        const normalizedCtx = normalizeDictArg(contextArg);

        if (normalizedCtx === '.' || normalizedCtx === '$') {
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

        // FIX: use normalized (paren-stripped) contextArg for dict detection
        if (normalizedCtx.startsWith('dict ')) {
            const dictType = inferExpressionType(
                normalizedCtx, vars, scopeStack, blockLocals,
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
            this.parser.parseDotPath(normalizedCtx),
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
