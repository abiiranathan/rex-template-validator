/**
 * Package hoverProvider implements the VS Code hover provider for Rex templates.
 * It resolves the template variable or function under the cursor and returns a
 * rich Markdown tooltip with type, documentation, and field information.
 */

import * as vscode from 'vscode';
import {
    FieldInfo,
    ScopeFrame,
    TemplateContext,
    TemplateNode,
    TemplateVar,
} from './types';
import { TemplateParser, resolvePath, ResolveResult } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { inferExpressionType, TypeResult } from './compiler/expressionParser';
import { ScopeUtils } from './scopeUtils';

/**
 * HoverProvider resolves hover information for template variables, fields,
 * and funcMap functions.
 */
export class HoverProvider {
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
     * Returns hover information for the template element at position, or null
     * when nothing actionable is under the cursor.
     */
    async getHoverInfo(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.Hover | null> {
        const content = document.getText();
        const nodes = this.parser.parse(content);

        const blockHover = this.findTemplateNameHover(nodes, position);
        if (blockHover) {
            const md = new vscode.MarkdownString();
            md.appendMarkdown(`**Template Block:** \`${blockHover}\`\n`);
            return new vscode.Hover(md);
        }

        const hit = this.scope.findNodeAtPosition(
            nodes, position, ctx.vars, [], nodes
        );
        if (!hit) {
            const enclosing = this.scope.findEnclosingBlockOrDefine(nodes, position);
            if (enclosing?.blockName) {
                const callCtx = this.scope.resolveNamedBlockCallCtxForPosition(
                    enclosing.blockName, ctx.vars, nodes, document.uri.fsPath
                );
                if (callCtx) {
                    const md = new vscode.MarkdownString();
                    md.isTrusted = true;
                    md.appendCodeblock(`. : ${callCtx.typeStr}`, 'go');
                    if (callCtx.fields?.length) {
                        md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
                        for (const f of callCtx.fields.slice(0, 30)) {
                            md.appendMarkdown(
                                `**${f.name}** \`${(f as FieldInfo).isSlice ? `[]${f.type}` : f.type}\`\n\n`
                            );
                        }
                    }
                    return new vscode.Hover(md);
                }
            }
            return null;
        }

        const { node, stack, vars: hitVars, locals: hitLocals } = hit;

        if (node.rawText) {
            const cursorOffset = position.character - (node.col - 1);
            const subPathStr = this.extractPathAtCursor(node.rawText, cursorOffset);

            if (subPathStr) {
                const funcMaps = this.graphBuilder.getGraph().funcMaps;
                if (funcMaps && funcMaps.has(subPathStr)) {
                    return this.buildFuncMapHover(funcMaps.get(subPathStr)!);
                }

                const parts = this.parser.parseDotPath(subPathStr);
                if (
                    parts.length > 0 &&
                    !(parts.length === 1 && parts[0] === '.' && subPathStr !== '.')
                ) {
                    const subResult = resolvePath(
                        parts, hitVars, stack, hitLocals,
                        this.scope.buildFieldResolver(hitVars, stack)
                    );
                    if (subResult.found) {
                        return this.buildHoverForPath(
                            parts, subResult, hitVars, stack, hitLocals
                        );
                    }
                }
            }
        }

        const isBareVarDot =
            node.kind === 'variable' && node.path.length === 1 && node.path[0] === '.';
        const isPartialDotCtx =
            node.kind === 'partial' && (node.partialContext ?? '.') === '.';
        if (isBareVarDot || isPartialDotCtx) {
            return this.buildDotHover(stack, hitVars);
        }

        let result = resolvePath(
            node.path, hitVars, stack, hitLocals,
            this.scope.buildFieldResolver(hitVars, stack)
        );
        let isExpressionFallback = false;
        let exprText = node.rawText;

        if (!result.found && node.rawText) {
            try {
                const cleanExpr = node.rawText
                    .replace(/^\{\{-?\s*/, '')
                    .replace(/\s*-?\}\}$/, '');
                const exprType = inferExpressionType(
                    cleanExpr, hitVars, stack, hitLocals,
                    this.graphBuilder.getGraph().funcMaps,
                    this.scope.buildFieldResolver(hitVars, stack)
                );

                if (exprType) {
                    result = {
                        typeStr: exprType.typeStr,
                        found: true,
                        fields: exprType.fields,
                        isSlice: exprType.isSlice,
                        isMap: exprType.isMap,
                        elemType: exprType.elemType,
                        keyType: exprType.keyType,
                    };
                    isExpressionFallback = true;
                    exprText = cleanExpr;
                }
            } catch { }
        }

        if (!result.found) return null;

        const pathToUse = isExpressionFallback ? ['expression'] : node.path;
        return this.buildHoverForPath(
            pathToUse, result, hitVars, stack, hitLocals, exprText
        );
    }

    // ── Private helpers ────────────────────────────────────────────────────────

    /**
     * Returns the word-like token under cursor offset inside rawText,
     * allowing the characters [a-zA-Z0-9_$.].  Returns null when no token
     * is found.
     */
    extractPathAtCursor(text: string, offset: number): string | null {
        const allowed = /[a-zA-Z0-9_$.]/;
        let start = offset;
        while (start > 0 && allowed.test(text[start - 1])) start--;
        let end = offset;
        while (end < text.length && allowed.test(text[end])) end++;
        if (start >= end) return null;
        return text.substring(start, end);
    }

    /** Builds a hover card for a path that was successfully resolved. */
    buildHoverForPath(
        path: string[],
        result: ResolveResult | TypeResult,
        vars: Map<string, TemplateVar>,
        stack: ScopeFrame[],
        locals?: Map<string, TemplateVar>,
        rawText?: string
    ): vscode.Hover {
        const varName =
            rawText &&
                path.length <= 1 &&
                (path[0] === 'expression' || path[0] === 'unknown' || path.length === 0)
                ? rawText
                : path.length > 0 && path[0] === '.'
                    ? '.'
                    : path.length > 0 && path[0] === '$'
                        ? '$.' + path.slice(1).join('.')
                        : path.length > 0 && path[0].startsWith('$')
                            ? path.join('.')
                            : '.' + path.join('.');

        const md = new vscode.MarkdownString();
        md.isTrusted = true;

        const hasMethodSignature =
            result.typeStr === 'method' ||
            (result.params && result.params.length > 0) ||
            (result.returns && result.returns.length > 0);

        if (hasMethodSignature) {
            const paramsStr = (result.params ?? [])
                .map((p, i) =>
                    p.name ? `${p.name} ${p.type}` : `${String.fromCharCode(97 + i)} ${p.type}`
                )
                .join(', ');
            const returnsStr = this.formatReturns(result.returns ?? []);
            const methodName = path[path.length - 1];
            md.appendCodeblock(
                `func ${methodName}(${paramsStr})${returnsStr ? ' ' + returnsStr : ''}`,
                'go'
            );
        } else {
            md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');
        }

        const varInfo = this.findVariableInfo(path, vars, stack, locals);
        if (varInfo?.doc) {
            md.appendMarkdown('\n\n---\n\n');
            md.appendMarkdown(varInfo.doc);
        }

        // Prefer result.fields; fall back to varInfo.fields for context-file vars where
        // the stack is empty and resolvePath may not hydrate fields for top-level vars.
        const fieldsToShow =
            (result.fields?.length ? result.fields : varInfo?.fields) ?? [];

        if (fieldsToShow.length) {
            md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
            for (const f of fieldsToShow.slice(0, 30)) {
                md.appendMarkdown(
                    `**${f.name}** \`${f.isSlice ? `[]${f.type}` : f.type}\`\n`
                );
                if (f.doc) md.appendMarkdown(`\n${f.doc}\n`);
                md.appendMarkdown('\n');
            }
        }

        return new vscode.Hover(md);
    }

    /** Builds a hover card for the bare "." current-context reference. */
    buildDotHover(
        scopeStack: ScopeFrame[],
        vars: Map<string, TemplateVar>
    ): vscode.Hover | null {
        const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
        const typeStr = dotFrame ? (dotFrame.typeStr ?? 'unknown') : 'RenderContext';
        const fields: FieldInfo[] =
            dotFrame?.fields ??
            [...vars.values()].map(v => ({
                name: v.name,
                type: v.type,
                fields: v.fields,
                isSlice: v.isSlice ?? false,
                doc: v.doc,
                isMap: v.isMap,
                keyType: v.keyType,
                elemType: v.elemType,
            } as FieldInfo));

        const md = new vscode.MarkdownString();
        md.isTrusted = true;
        md.appendCodeblock(`. : ${typeStr}`, 'go');

        if (fields.length > 0) {
            md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
            for (const f of fields.slice(0, 30)) {
                md.appendMarkdown(
                    `**${f.name}** \`${f.isSlice ? `[]${f.type}` : f.type}\`\n`
                );
                if (f.doc) md.appendMarkdown(`\n${f.doc}\n`);
                md.appendMarkdown('\n');
            }
        }

        return new vscode.Hover(md);
    }

    /**
     * Checks whether position falls on the quoted name in a template/block/define tag
     * and returns the name string if so, otherwise null.
     */
    findTemplateNameHover(
        nodes: TemplateNode[],
        position: vscode.Position
    ): string | null {
        for (const node of nodes) {
            if (
                (node.kind === 'partial' || node.kind === 'block' || node.kind === 'define') &&
                (node.partialName || node.blockName)
            ) {
                const name = node.partialName || node.blockName!;
                const nameMatch = node.rawText.match(
                    new RegExp(`\\{\\{\\s*(template|block|define)\\s+"([^"]+)"`)
                );
                if (nameMatch && nameMatch[2] === name) {
                    const nameStartCol =
                        (node.col - 1) + node.rawText.indexOf('"' + name + '"') + 1;
                    if (
                        position.line === node.line - 1 &&
                        position.character >= nameStartCol &&
                        position.character <= nameStartCol + name.length
                    ) {
                        return name;
                    }
                }
            }
            if (node.children) {
                const found = this.findTemplateNameHover(node.children, position);
                if (found) return found;
            }
        }
        return null;
    }

    // ── Private: type/doc lookup ──────────────────────────────────────────────

    private buildFuncMapHover(
        fn: import('./types').FuncMapInfo
    ): vscode.Hover {
        const md = new vscode.MarkdownString();
        md.isTrusted = true;

        const params = fn.params ?? [];
        const returns = fn.returns ?? [];
        const paramsStr = params
            .map((p, i) =>
                p.name ? `${p.name} ${p.type}` : `${String.fromCharCode(97 + i)} ${p.type}`
            )
            .join(', ');
        const returnsStr = this.formatReturns(returns);

        md.appendCodeblock(
            `func ${fn.name}(${paramsStr})${returnsStr ? ' ' + returnsStr : ''}`,
            'go'
        );

        const hasUnnamedParams = params.some(p => !p.name);
        if (fn.doc?.trim()) {
            md.appendMarkdown('\n\n---\n\n');
            md.appendMarkdown(fn.doc.trim());
        } else if (hasUnnamedParams) {
            md.appendMarkdown(
                '\n\n---\n\n*Parameter names unavailable (anonymous function)*'
            );
        }

        return new vscode.Hover(md);
    }

    private formatReturns(
        returns: Array<{ name?: string; type: string }>
    ): string {
        if (returns.length === 0) return '';
        if (returns.length === 1) {
            return returns[0].name
                ? `${returns[0].name} ${returns[0].type}`
                : returns[0].type;
        }
        return `(${returns.map(r => (r.name ? `${r.name} ${r.type}` : r.type)).join(', ')})`;
    }

    /**
     * Looks up documentation and field metadata for a resolved path by walking
     * the variable/field tree.
     */
    private findVariableInfo(
        path: string[],
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals?: Map<string, TemplateVar>
    ): { typeStr: string; doc?: string; fields?: FieldInfo[] } | null {
        if (path.length === 0) return null;

        let topVarName = path[0];
        let searchPath = path;
        if (topVarName === '$' && path.length > 1) {
            topVarName = path[1];
            searchPath = path.slice(1);
        }
        if (topVarName === '.' || topVarName === '$') return null;

        let topVar: TemplateVar | undefined;
        if (topVarName.startsWith('$') && topVarName !== '$') {
            topVar = blockLocals?.get(topVarName);
            if (!topVar) {
                for (let i = scopeStack.length - 1; i >= 0; i--) {
                    if (scopeStack[i].locals?.has(topVarName)) {
                        topVar = scopeStack[i].locals!.get(topVarName);
                        break;
                    }
                }
            }
        } else {
            topVar = vars.get(topVarName);
            if (!topVar) {
                const field = scopeStack
                    .slice()
                    .reverse()
                    .find(f => f.key === '.')
                    ?.fields?.find(f => f.name === topVarName);
                if (field) {
                    return { typeStr: field.type, doc: field.doc, fields: field.fields };
                }
                return null;
            }
        }

        if (!topVar) return null;
        if (searchPath.length === 1) {
            return { typeStr: topVar.type, doc: topVar.doc, fields: topVar.fields };
        }

        let fields = topVar.fields ?? [];
        for (let i = 1; i < searchPath.length; i++) {
            if (searchPath[i] === '[]') continue;
            const field = fields.find(f => f.name === searchPath[i]);
            if (!field) return null;
            if (i === searchPath.length - 1) {
                return { typeStr: field.type, doc: field.doc, fields: field.fields };
            }
            fields = field.fields ?? [];
        }
        return null;
    }
}
