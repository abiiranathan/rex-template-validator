/**
 * Package completionProvider implements the VS Code completion provider for Rex templates.
 * It resolves completion items based on the scope at the cursor position, offering
 * template variables, struct fields, and funcMap functions.
 */

import * as vscode from 'vscode';
import {
    FieldInfo,
    FuncMapInfo,
    ScopeFrame,
    TemplateContext,
    TemplateVar,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { inferExpressionType } from './compiler/expressionParser';
import { ScopeUtils } from './scopeUtils';

/**
 * CompletionProvider supplies IntelliSense completion items for Rex template files.
 */
export class CompletionProvider {
    private readonly parser: TemplateParser;
    private readonly graphBuilder: KnowledgeGraphBuilder;
    private readonly scope: ScopeUtils;

    constructor(graphBuilder: KnowledgeGraphBuilder, scope: ScopeUtils) {
        this.graphBuilder = graphBuilder;
        this.parser = scope.parser;
        this.scope = scope;
    }

    // ── Public API ─────────────────────────────────────────────────────────────

    /**
     * Returns completion items for the cursor position.
     * Handles dot-path completion, dollar-variable completion, and global/function
     * completion when no path prefix is present.
     */
    async getCompletionItems(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.CompletionItem[]> {
        return this.resolveCompletions(document, position, ctx);
    }

    /**
     * Synchronous variant of getCompletionItems for consumers that do not require
     * async.  The implementation is identical; the async overload exists for API
     * symmetry with the VS Code provider interface.
     */
    getCompletions(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): vscode.CompletionItem[] {
        return this.resolveCompletions(document, position, ctx);
    }

    // ── Core resolution ───────────────────────────────────────────────────────

    private resolveCompletions(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): vscode.CompletionItem[] {
        const completionItems: vscode.CompletionItem[] = [];
        const content = document.getText();
        const nodes = this.parser.parse(content);

        // 1. Determine scope at cursor position.
        let scopeResult = this.scope.findScopeAtPosition(
            nodes, position, ctx.vars, [], nodes, ctx
        );

        // Fallback: if we're inside a named block, override with that block's
        // call-site context so completions reflect what the caller passed as ".".
        if (!scopeResult || scopeResult.stack.length === 0) {
            const enclosing = this.scope.findEnclosingBlockOrDefine(nodes, position);
            if (enclosing?.blockName) {
                const callCtx = this.scope.resolveNamedBlockCallCtxForPosition(
                    enclosing.blockName, ctx.vars, nodes, document.uri.fsPath
                );
                if (callCtx) {
                    scopeResult = {
                        stack: [{
                            key: '.',
                            typeStr: callCtx.typeStr,
                            fields: callCtx.fields ?? [],
                            isMap: callCtx.isMap,
                            keyType: callCtx.keyType,
                            elemType: callCtx.elemType,
                            isSlice: callCtx.isSlice,
                        }],
                        locals: new Map(),
                    };
                }
            }
        }

        const { stack, locals } = scopeResult ?? {
            stack: [] as ScopeFrame[],
            locals: new Map<string, TemplateVar>(),
        };

        const lineText = document.lineAt(position.line).text;
        const linePrefix = lineText.slice(0, position.character);
        const replacementRange = new vscode.Range(
            position.line, position.character,
            position.line, position.character
        );

        // 2. Handle complex expressions: after a pipe or open paren.
        const inComplexExpr = /\(\s*[^)]*$|\|\s*[^|}]*$/.test(linePrefix);
        if (inComplexExpr) {
            const match = linePrefix.match(/(?:\(|\|)\s*(.*)$/);
            if (match) {
                const partialExpr = match[1].trim();
                try {
                    const exprType = inferExpressionType(
                        partialExpr, ctx.vars, stack, locals,
                        this.graphBuilder.getGraph().funcMaps,
                        this.scope.buildFieldResolver(ctx.vars, stack)
                    );
                    if (exprType?.fields) {
                        return exprType.fields.map(f =>
                            this.fieldToCompletionItem(f, null)
                        );
                    }
                } catch { /* fall through */ }
            }
        }

        // 3. Identify the path token currently being typed.
        const pathMatch = linePrefix.match(/(?:\$|\.)[\w.]*$/);

        // Case A: No dot/dollar prefix — offer globals, locals, and functions.
        if (!pathMatch) {
            this.addGlobalVariablesToCompletion(
                ctx.vars, completionItems, '', replacementRange
            );
            this.addLocalVariablesToCompletion(
                stack, locals, completionItems, '', replacementRange
            );
            this.addFunctionsToCompletion(
                this.graphBuilder.getGraph().funcMaps, completionItems, '', replacementRange
            );
            return completionItems;
        }

        const rawPath = pathMatch[0];
        const matchStart = position.character - rawPath.length;

        let lookupPath: string[];
        let filterPrefix: string;
        let filterStart: number;

        if (rawPath.endsWith('.')) {
            lookupPath = this.parser.parseDotPath(rawPath);
            filterPrefix = '';
            filterStart = position.character;
        } else {
            const lastDot = rawPath.lastIndexOf('.');
            if (lastDot === -1) {
                filterPrefix = rawPath.startsWith('$') ? rawPath.slice(1) : rawPath.slice(1);
                filterStart = matchStart + 1;
                if (rawPath.startsWith('$')) {
                    const repRange = new vscode.Range(
                        position.line, matchStart, position.line, position.character
                    );
                    this.addGlobalVariablesToCompletion(
                        ctx.vars, completionItems, filterPrefix, repRange
                    );
                    this.addLocalVariablesToCompletion(
                        stack, locals, completionItems, filterPrefix, repRange
                    );
                } else {
                    const repRange = new vscode.Range(
                        position.line, matchStart, position.line, position.character
                    );
                    const dotFrame = stack.slice().reverse().find(f => f.key === '.');
                    const fields: FieldInfo[] = dotFrame?.fields ??
                        [...ctx.vars.values()].map(v => ({
                            name: v.name,
                            type: v.type,
                            fields: v.fields,
                            isSlice: v.isSlice ?? false,
                            doc: v.doc,
                            isMap: v.isMap,
                            keyType: v.keyType,
                            elemType: v.elemType,
                        } as FieldInfo));
                    this.addFieldsToCompletion(
                        { fields }, completionItems, filterPrefix, repRange
                    );
                }
                return completionItems;
            }
            filterPrefix = rawPath.slice(lastDot + 1);
            filterStart = matchStart + lastDot + 1;
            lookupPath = this.parser.parseDotPath(rawPath.slice(0, lastDot + 1));
        }

        const repRange = new vscode.Range(
            position.line, filterStart,
            position.line, position.character
        );

        // Case B: Bare "." — show current dot-context fields.
        if (lookupPath.length === 1 && (lookupPath[0] === '.' || lookupPath[0] === '')) {
            const dotFrame = stack.slice().reverse().find(f => f.key === '.');
            const fields: FieldInfo[] = dotFrame?.fields ??
                [...ctx.vars.values()].map(v => ({
                    name: v.name,
                    type: v.type,
                    fields: v.fields,
                    isSlice: v.isSlice ?? false,
                    doc: v.doc,
                } as FieldInfo));
            this.addFieldsToCompletion({ fields }, completionItems, filterPrefix, repRange);
            return completionItems;
        }

        // Case C: Bare "$" — show root vars and locals.
        if (lookupPath.length === 1 && lookupPath[0] === '$') {
            this.addGlobalVariablesToCompletion(
                ctx.vars, completionItems, filterPrefix, repRange
            );
            this.addLocalVariablesToCompletion(
                stack, locals, completionItems, filterPrefix, repRange
            );
            return completionItems;
        }

        // Case D: Complex path — resolve to a type and show its fields.
        const res = resolvePath(
            lookupPath, ctx.vars, stack, locals,
            this.scope.buildFieldResolver(ctx.vars, stack)
        );
        if (res.found && res.fields) {
            this.addFieldsToCompletion(
                { fields: res.fields }, completionItems, filterPrefix, repRange
            );
        }

        return completionItems;
    }

    // ── Completion item builders ──────────────────────────────────────────────

    /**
     * Adds function completions from the funcMap registry to completionItems.
     * When replacementRange is null VS Code handles insertion automatically.
     */
    addFunctionsToCompletion(
        funcMaps: Map<string, FuncMapInfo> | undefined,
        completionItems: vscode.CompletionItem[],
        partialName: string = '',
        replacementRange: vscode.Range | null
    ) {
        if (!funcMaps) return;
        for (const [name, fn] of funcMaps) {
            if (partialName && !name.startsWith(partialName)) continue;
            const item = new vscode.CompletionItem(name, vscode.CompletionItemKind.Function);

            const paramsStr = (fn.params ?? [])
                .map(p => (p.name ? `${p.name} ${p.type}` : p.type))
                .join(', ');
            const returns = fn.returns ?? [];
            const returnsStr =
                returns.length === 0
                    ? ''
                    : returns.length === 1
                        ? (returns[0].name
                            ? `${returns[0].name} ${returns[0].type}`
                            : returns[0].type)
                        : `(${returns.map(r => (r.name ? `${r.name} ${r.type}` : r.type)).join(', ')})`;

            item.detail = `func(${paramsStr})${returnsStr ? ` ${returnsStr}` : ''}`;
            if (fn.doc) item.documentation = new vscode.MarkdownString(fn.doc);
            if (replacementRange) item.range = replacementRange;
            completionItems.push(item);
        }
    }

    /**
     * Adds top-level template variable completions to completionItems.
     */
    addGlobalVariablesToCompletion(
        vars: Map<string, TemplateVar>,
        completionItems: vscode.CompletionItem[],
        partialName: string = '',
        replacementRange: vscode.Range | null
    ) {
        for (const [name, variable] of vars) {
            if (name.startsWith(partialName)) {
                const item = new vscode.CompletionItem(
                    name, vscode.CompletionItemKind.Variable
                );
                item.detail = variable.type;
                item.documentation = new vscode.MarkdownString(variable.doc);
                if (replacementRange) item.range = replacementRange;
                completionItems.push(item);
            }
        }
    }

    /**
     * Adds block-local and scope-frame-local variable completions to completionItems.
     */
    addLocalVariablesToCompletion(
        scopeStack: ScopeFrame[],
        blockLocals: Map<string, TemplateVar> | undefined,
        completionItems: vscode.CompletionItem[],
        partialName: string = '',
        replacementRange: vscode.Range | null
    ) {
        if (blockLocals) {
            for (const [name, variable] of blockLocals) {
                if (name.startsWith(partialName)) {
                    const item = new vscode.CompletionItem(
                        name, vscode.CompletionItemKind.Variable
                    );
                    item.detail = variable.type;
                    item.documentation = new vscode.MarkdownString(variable.doc);
                    if (replacementRange) item.range = replacementRange;
                    completionItems.push(item);
                }
            }
        }
        for (let i = scopeStack.length - 1; i >= 0; i--) {
            if (scopeStack[i].locals) {
                for (const [name, variable] of scopeStack[i].locals!) {
                    if (name.startsWith(partialName)) {
                        const item = new vscode.CompletionItem(
                            name, vscode.CompletionItemKind.Variable
                        );
                        item.detail = variable.type;
                        item.documentation = new vscode.MarkdownString(variable.doc);
                        if (replacementRange) item.range = replacementRange;
                        completionItems.push(item);
                    }
                }
            }
        }
    }

    /**
     * Adds struct-field completions to completionItems.
     * The replacement range covers only the filter suffix so the preceding "."
     * is always preserved.
     */
    addFieldsToCompletion(
        context: { fields?: FieldInfo[] },
        completionItems: vscode.CompletionItem[],
        partialName: string = '',
        replacementRange: vscode.Range | null
    ) {
        if (!context.fields) return;
        for (const field of context.fields) {
            if (field.name.toLowerCase().startsWith(partialName.toLowerCase())) {
                const item = this.fieldToCompletionItem(field, replacementRange);
                completionItems.push(item);
            }
        }
    }

    // ── Private helpers ───────────────────────────────────────────────────────

    private fieldToCompletionItem(
        field: FieldInfo,
        replacementRange: vscode.Range | null
    ): vscode.CompletionItem {
        const item = new vscode.CompletionItem(
            field.name,
            field.type === 'method'
                ? vscode.CompletionItemKind.Method
                : vscode.CompletionItemKind.Field
        );

        if (field.type === 'method' && (field.params || field.returns)) {
            const paramsStr = (field.params ?? [])
                .map((p, i) =>
                    p.name ? `${p.name} ${p.type}` : `${String.fromCharCode(97 + i)} ${p.type}`
                )
                .join(', ');
            const returnsStr =
                (field.returns ?? []).length === 0
                    ? ''
                    : (field.returns ?? []).length === 1
                        ? (field.returns![0].name
                            ? `${field.returns![0].name} ${field.returns![0].type}`
                            : field.returns![0].type)
                        : `(${field.returns!.map(r =>
                            r.name ? `${r.name} ${r.type}` : r.type
                        ).join(', ')})`;
            item.detail = `func(${paramsStr})${returnsStr ? ` ${returnsStr}` : ''}`;
        } else {
            item.detail = field.isSlice ? `[]${field.type}` : field.type;
        }

        item.documentation = new vscode.MarkdownString(field.doc);
        if (replacementRange) item.range = replacementRange;
        return item;
    }
}
