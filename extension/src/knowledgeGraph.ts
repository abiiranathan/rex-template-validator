// knowledgeGraph.ts
import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import {
    AnalysisResult,
    KnowledgeGraph,
    NamedBlockRegistry,
    NamedBlockEntry,
    NamedBlockDuplicateError,
    TemplateContext,
    TemplateVar,
    TemplateNode,
    FieldInfo,
    FuncMapInfo,
    RenderCall,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { normalizeDictArg } from './scopeUtils';
import { inferExpressionType } from './compiler/expressionParser';
import { extractBareType } from './types';

export class KnowledgeGraphBuilder {
    private graph: KnowledgeGraph = {
        templates: new Map(),
        namedBlocks: new Map(),
        namedBlockErrors: [],
        analyzedAt: new Date(),
        funcMaps: new Map(),
        typeRegistry: new Map(),
    };

    private astCache = new Map<string, TemplateNode[]>();

    private workspaceRoot: string;
    private outputChannel: vscode.OutputChannel;

    /**
     * Shared status bar item injected from extension.ts.
     * KnowledgeGraphBuilder never creates or disposes this — extension.ts owns it.
     */
    private readonly statusBarItem: vscode.StatusBarItem;

    /**
     * Cache for findContextForFileAsPartial results.
     * Populated lazily; cleared on full rebuild or template edit.
     */
    private partialContextCache = new Map<string, TemplateContext | null>();

    constructor(
        workspaceRoot: string,
        outputChannel: vscode.OutputChannel,
        statusBarItem: vscode.StatusBarItem,
    ) {
        this.workspaceRoot = workspaceRoot;
        this.outputChannel = outputChannel;
        this.statusBarItem = statusBarItem;
    }

    /**
     * Update the shared status bar with a graph-level progress message and
     * mirror it to the output channel.
     * The caller (extension.ts) owns visibility; we only set the text/icon here.
     */
    updateStatus(message: string): void {
        this.statusBarItem.text = `$(sync~spin) ${message}`;
        this.statusBarItem.show();
        this.outputChannel.appendLine(`[KnowledgeGraph] ${message}`);
    }

    /**
     * Restore the status bar to whatever the extension last set, or hide it
     * if nothing else needs it. Extension.ts can override this after awaiting
     * the async call if it wants to show its own "ready" message.
     */
    clearStatus(): void {
        this.statusBarItem.hide();
    }

    private getTemplateBase(): string {
        const config = vscode.workspace.getConfiguration('rex');
        const sourceDir: string = config.get('sourceDir') ?? '.';
        const templateBaseDir: string = config.get('templateBaseDir') ?? '';
        const templateRoot: string = config.get('templateRoot') ?? '';

        const baseDir = templateBaseDir
            ? path.resolve(this.workspaceRoot, templateBaseDir)
            : path.resolve(this.workspaceRoot, sourceDir);

        return path.join(baseDir, templateRoot);
    }

    /**
     * Get parsed AST nodes for a file — cached.
     */
    private getParsedNodes(absolutePath: string): TemplateNode[] | undefined {
        if (this.astCache.has(absolutePath)) {
            return this.astCache.get(absolutePath);
        }

        let content: string;
        try {
            const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === absolutePath);
            content = openDoc ? openDoc.getText() : fs.readFileSync(absolutePath, 'utf8');
        } catch (err) {
            this.outputChannel.appendLine(`[KnowledgeGraph] Failed to read file ${absolutePath}: ${err}`);
            return undefined;
        }

        try {
            const parser = new TemplateParser();
            const nodes = parser.parse(content);
            this.astCache.set(absolutePath, nodes);
            return nodes;
        } catch (err) {
            this.outputChannel.appendLine(`[KnowledgeGraph] Parse failed for ${absolutePath}: ${err}`);
            return undefined;
        }
    }

    private invalidateAstCache(absolutePath: string): void {
        this.astCache.delete(absolutePath);
    }

    build(analysisResult: AnalysisResult): KnowledgeGraph {
        // Clear caches on full rebuild
        this.partialContextCache.clear();
        this.astCache.clear();

        const templates = new Map<string, TemplateContext>();
        const templateBase = this.getTemplateBase();

        // ── Build type registry ─────────────────────────────────────────────────
        const typeRegistry = new Map<string, FieldInfo[]>();
        if (analysisResult.types) {
            for (const [typeName, fields] of Object.entries(analysisResult.types)) {
                typeRegistry.set(typeName, fields);
            }
            this.outputChannel.appendLine(
                `[KnowledgeGraph] Loaded ${typeRegistry.size} type(s) from global registry`
            );
        }

        const mergeRenderCall = (logicalPath: string, absPath: string, rc: RenderCall) => {
            let ctx = templates.get(logicalPath);
            if (!ctx) {
                ctx = {
                    templatePath: logicalPath,
                    absolutePath: absPath,
                    vars: new Map(),
                    renderCalls: [],
                };
                templates.set(logicalPath, ctx);
            }

            ctx.renderCalls.push(rc);

            for (const v of rc.vars ?? []) {
                const existing = ctx.vars.get(v.name);
                if (!existing || isMoreComplete(v, existing, typeRegistry)) {
                    ctx.vars.set(v.name, hydrateVar(v, typeRegistry));
                }
            }
        };

        // ── Process render calls ────────────────────────────────────────────────
        for (const rc of analysisResult.renderCalls ?? []) {
            const logicalPath = rc.template.replace(/^\.\//, '');
            const absPath = path.join(templateBase, logicalPath);

            mergeRenderCall(logicalPath, absPath, rc);

            if (analysisResult.namedBlocks && analysisResult.namedBlocks[logicalPath]) {
                const entries = analysisResult.namedBlocks[logicalPath];
                if (entries.length > 0) {
                    const entry = entries[0];
                    mergeRenderCall(entry.templatePath, entry.absolutePath, rc);

                    let blockCtx = templates.get(logicalPath);
                    if (!blockCtx) {
                        blockCtx = {
                            templatePath: entry.templatePath,
                            absolutePath: entry.absolutePath,
                            vars: new Map(),
                            renderCalls: [],
                        };
                        templates.set(logicalPath, blockCtx);
                    }
                    blockCtx.renderCalls.push(rc);
                    for (const v of rc.vars ?? []) {
                        const existing = blockCtx.vars.get(v.name);
                        if (!existing || isMoreComplete(v, existing, typeRegistry)) {
                            blockCtx.vars.set(v.name, hydrateVar(v, typeRegistry));
                        }
                    }
                }
            }
        }

        // ── Build named blocks registry ─────────────────────────────────────────
        const namedBlocks: NamedBlockRegistry = new Map();
        if (analysisResult.namedBlocks) {
            for (const [name, entries] of Object.entries(analysisResult.namedBlocks)) {
                const fullEntries: NamedBlockEntry[] = entries.map(e => this.createNamedBlockEntry(e, name));
                namedBlocks.set(name, fullEntries);
            }
        }

        this.scanTemplateDirectoryForNamedBlocks(templateBase, namedBlocks);

        const namedBlockErrors = analysisResult.namedBlockErrors ?? [];

        const funcMaps = new Map<string, FuncMapInfo>();
        for (const fm of analysisResult.funcMaps ?? []) {
            funcMaps.set(fm.name, fm);
        }

        this.graph = { templates, namedBlocks, namedBlockErrors, analyzedAt: new Date(), funcMaps, typeRegistry };

        this.outputChannel.appendLine(
            `[KnowledgeGraph] Built graph with ${templates.size} templates, ` +
            `${namedBlocks.size} named block(s), ` +
            `${funcMaps.size} template functions, ` +
            `${typeRegistry.size} registered type(s)`
        );

        if (namedBlockErrors.length > 0) {
            this.outputChannel.appendLine(`[KnowledgeGraph] ${namedBlockErrors.length} duplicate named block(s) found:`);
            for (const err of namedBlockErrors) {
                this.outputChannel.appendLine(`  ${err.message}`);
            }
        }

        for (const [tpl, ctx] of templates) {
            this.outputChannel.appendLine(
                `  ${tpl}: ${[...ctx.vars.keys()].join(', ')} (${ctx.renderCalls.length} call(s))`
            );
        }

        return this.graph;
    }

    private createNamedBlockEntry(base: any, name: string): NamedBlockEntry {
        return {
            ...base,
            name,
            get node() {
                if (!(this as any)._node) {
                    const nodes = knowledgeGraphBuilder.getParsedNodes(this.absolutePath);
                    const found = nodes ? findBlockNode(nodes, name) : undefined;
                    (this as any)._node = found ?? {
                        kind: 'define',
                        path: [],
                        rawText: '',
                        line: this.line,
                        col: this.col,
                        blockName: name,
                    };
                }
                return (this as any)._node;
            }
        };
    }

    getGraph(): KnowledgeGraph {
        return this.graph;
    }

    private scanTemplateDirectoryForNamedBlocks(
        templateBase: string,
        namedBlocks: NamedBlockRegistry
    ): void {
        if (!fs.existsSync(templateBase)) return;

        const declarationRe = /\{\{-?\s*(?:define|block)\s+"([^"]+)"/g;

        const walk = (dir: string): void => {
            let entries: fs.Dirent[];
            try {
                entries = fs.readdirSync(dir, { withFileTypes: true });
            } catch {
                return;
            }

            for (const entry of entries) {
                const fullPath = path.join(dir, entry.name);
                if (entry.isDirectory()) {
                    walk(fullPath);
                    continue;
                }
                if (!entry.isFile() || !/\.(html|gohtml|tmpl|tpl)$/.test(entry.name)) {
                    continue;
                }

                let content: string;
                try {
                    const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === fullPath);
                    content = openDoc ? openDoc.getText() : fs.readFileSync(fullPath, 'utf8');
                } catch {
                    continue;
                }

                const templatePath = path.relative(templateBase, fullPath).replace(/\\/g, '/');
                this.outputChannel.appendLine(`[KnowledgeGraph] Scanning for named blocks: ${templatePath}`);
                declarationRe.lastIndex = 0;

                let match: RegExpExecArray | null;
                while ((match = declarationRe.exec(content)) !== null) {
                    const name = match[1];
                    if (namedBlocks.has(name)) continue;

                    const upTo = content.slice(0, match.index);
                    const line = (upTo.match(/\n/g) ?? []).length + 1;
                    const lastNl = upTo.lastIndexOf('\n');
                    const col = match.index - (lastNl === -1 ? 0 : lastNl + 1);

                    const blockEntry: NamedBlockEntry = this.createNamedBlockEntry(
                        { templatePath, absolutePath: fullPath, line, col },
                        name
                    );

                    namedBlocks.set(name, [blockEntry]);
                    this.outputChannel.appendLine(
                        `[KnowledgeGraph] Discovered unreferenced named block "${name}" in ${templatePath}`
                    );
                }
            }
        };

        walk(templateBase);
    }

    lookupNamedBlock(name: string): NamedBlockEntry | undefined {
        const entries = this.graph.namedBlocks.get(name);
        if (!entries || entries.length === 0) return undefined;
        return entries[0];
    }

    getDuplicateErrorsForBlock(name: string): NamedBlockDuplicateError[] {
        return this.graph.namedBlockErrors.filter(e => e.name === name);
    }

    getAllDuplicateErrors(): NamedBlockDuplicateError[] {
        return this.graph.namedBlockErrors;
    }

    findContextForFile(absolutePath: string): TemplateContext | undefined {
        const templateBase = this.getTemplateBase();
        let rel = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

        if (this.graph.templates.has(rel)) {
            return this.graph.templates.get(rel);
        }

        const normalizedAbsPath = path.normalize(absolutePath).toLowerCase();
        for (const [tplPath, ctx] of this.graph.templates) {
            if (ctx.absolutePath && path.normalize(ctx.absolutePath).toLowerCase() === normalizedAbsPath) {
                return ctx;
            }
            if (rel.endsWith(tplPath) || tplPath.endsWith(rel)) {
                return ctx;
            }
        }

        const base = path.basename(absolutePath);
        for (const [, ctx] of this.graph.templates) {
            if (path.basename(ctx.templatePath) === base) {
                return ctx;
            }
        }

        return undefined;
    }

    findPartialContext(partialName: string, currentFile: string): TemplateContext | undefined {
        for (const [tplPath, ctx] of this.graph.templates) {
            if (
                tplPath === partialName ||
                tplPath.endsWith('/' + partialName) ||
                path.basename(tplPath) === partialName
            ) {
                return ctx;
            }
        }

        const templateBase = this.getTemplateBase();
        const config = vscode.workspace.getConfiguration('rex');
        const sourceDir: string = config.get('sourceDir') ?? '.';

        const candidates = [
            path.join(path.dirname(currentFile), partialName),
            path.join(templateBase, partialName),
            path.join(this.workspaceRoot, partialName),
        ];

        for (const candidate of candidates) {
            if (fs.existsSync(candidate)) {
                return {
                    templatePath: path.relative(templateBase, candidate).replace(/\\/g, '/'),
                    absolutePath: candidate,
                    vars: new Map(),
                    renderCalls: [],
                };
            }
        }

        return undefined;
    }

    findContextForFileAsPartial(absolutePath: string): TemplateContext | undefined {
        if (this.partialContextCache.has(absolutePath)) {
            return this.partialContextCache.get(absolutePath) ?? undefined;
        }

        // Note: callers expecting a synchronous return should await the async version instead.
        // This sync shim still works for existing callers via the cache once primed.
        throw new Error(
            `findContextForFileAsPartial: call the async variant findContextForFileAsPartialAsync for "${absolutePath}"`
        );
    }

    /**
     * Async version of findContextForFileAsPartial.
     * Parses all parent templates in parallel with Promise.allSettled,
     * and updates the status bar as each template is validated.
     */
    async findContextForFileAsPartialAsync(absolutePath: string): Promise<TemplateContext | undefined> {
        if (this.partialContextCache.has(absolutePath)) {
            return this.partialContextCache.get(absolutePath) ?? undefined;
        }

        const result = await this._findContextForFileAsPartialUncached(absolutePath);
        this.partialContextCache.set(absolutePath, result ?? null);
        return result;
    }

    private async _findContextForFileAsPartialUncached(absolutePath: string): Promise<TemplateContext | undefined> {
        const templateBase = this.getTemplateBase();
        const partialRelPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');
        const partialBasename = path.basename(absolutePath);

        this.outputChannel.appendLine(
            `[KnowledgeGraph] Looking for partial: ${partialRelPath} (basename: ${partialBasename})`
        );

        // Priority 1: named block with context-file vars
        for (const [blockName, entries] of this.graph.namedBlocks) {
            for (const entry of entries) {
                if (path.normalize(entry.absolutePath).toLowerCase() === path.normalize(absolutePath).toLowerCase()) {
                    const blockCtx = this.graph.templates.get(blockName);
                    if (blockCtx && blockCtx.renderCalls.some(rc => rc.file === 'context-file')) {
                        this.outputChannel.appendLine(
                            `[KnowledgeGraph] Found named block "${blockName}" with context-file vars`
                        );
                        return {
                            templatePath: partialRelPath,
                            absolutePath: absolutePath,
                            vars: blockCtx.vars,
                            renderCalls: blockCtx.renderCalls,
                            isMap: false,
                            isSlice: false,
                        };
                    }
                }
            }
        }

        // Priority 2: Scan all parent templates for {{ template }} calls — in parallel
        const normalizedTargetAbsPath = path.normalize(absolutePath).toLowerCase();

        const definedBlocksInFile = new Set<string>();
        for (const [blockName, entries] of this.graph.namedBlocks) {
            for (const entry of entries) {
                if (path.normalize(entry.absolutePath).toLowerCase() === normalizedTargetAbsPath) {
                    definedBlocksInFile.add(blockName);
                }
            }
        }

        const parentEntries = [...this.graph.templates.entries()].filter(
            ([, ctx]) => ctx.absolutePath && fs.existsSync(ctx.absolutePath)
        );
        const total = parentEntries.length;

        this.updateStatus(`Validating partial ${partialBasename} (0/${total})...`);

        type PartialMatch = {
            vars: Map<string, TemplateVar>;
            renderCalls: RenderCall[];
            partialSourceVar: TemplateVar | undefined;
            meta: Record<string, unknown>;
        };

        /**
         * Process one parent template: parse its AST, look for a call to our
         * partial, and if found resolve the vars that would be passed to it.
         * Returns null when the parent does not call this partial.
         */
        const processParent = async (
            parentTplPath: string,
            parentCtx: TemplateContext,
            index: number
        ): Promise<PartialMatch | null> => {
            this.updateStatus(`Parsing template ${parentTplPath} (${index + 1}/${total})...`);
            this.outputChannel.appendLine(
                `[KnowledgeGraph] Validating parent template ${index + 1}/${total}: ${parentTplPath}`
            );

            const nodes = this.getParsedNodes(parentCtx.absolutePath!);
            if (!nodes) return null;

            const partialCall = this.findPartialCall(nodes, partialRelPath, partialBasename, definedBlocksInFile);
            if (!partialCall) return null;

            this.outputChannel.appendLine(
                `[KnowledgeGraph] Found partial call in ${parentTplPath}: template "${partialCall.partialName}" ${partialCall.partialContext}`
            );

            const resolved = this.resolvePartialVars(
                partialCall.partialContext ?? '.',
                parentCtx.vars
            );

            const partialSourceVar = this.findPartialSourceVar(
                partialCall.partialContext ?? '.',
                parentCtx.vars
            );

            return {
                vars: resolved.vars,
                renderCalls: [...parentCtx.renderCalls],
                partialSourceVar,
                meta: {
                    isMap: resolved.isMap,
                    keyType: resolved.keyType,
                    elemType: resolved.elemType,
                    isSlice: resolved.isSlice,
                    rootTypeStr: resolved.rootTypeStr,
                },
            };
        };

        // Fire all parent-template checks in parallel
        const results = await Promise.allSettled(
            parentEntries.map(([tplPath, ctx], i) => processParent(tplPath, ctx, i))
        );

        this.clearStatus();

        let foundAny = false;
        const mergedVars = new Map<string, TemplateVar>();
        const mergedRenderCalls: RenderCall[] = [];
        let lastPartialSourceVar: TemplateVar | undefined;
        let lastResolvedMeta: Record<string, unknown> = {};

        for (const result of results) {
            if (result.status === 'rejected') {
                this.outputChannel.appendLine(
                    `[KnowledgeGraph] Error while processing parent template: ${result.reason}`
                );
                continue;
            }

            const match = result.value;
            if (!match) continue;

            foundAny = true;

            for (const [k, v] of match.vars) {
                const existing = mergedVars.get(k);
                if (!existing || isMoreComplete(v, existing, this.graph.typeRegistry)) {
                    mergedVars.set(k, v);
                }
            }

            mergedRenderCalls.push(...match.renderCalls);
            lastPartialSourceVar = match.partialSourceVar;
            lastResolvedMeta = match.meta;
        }

        if (foundAny) {
            this.outputChannel.appendLine(
                `[KnowledgeGraph] Resolved merged partial vars: ${[...mergedVars.keys()].join(', ')}`
            );

            return {
                templatePath: partialRelPath,
                absolutePath: absolutePath,
                vars: mergedVars,
                renderCalls: mergedRenderCalls,
                partialSourceVar: lastPartialSourceVar,
                ...lastResolvedMeta,
            };
        }

        this.outputChannel.appendLine(
            `[KnowledgeGraph] No partial call found for ${partialRelPath}`
        );
        return undefined;
    }

    private findPartialCall(
        nodes: TemplateNode[],
        partialRelPath: string,
        partialBasename: string,
        definedBlocksInFile?: Set<string>
    ): TemplateNode | undefined {
        for (const node of nodes) {
            if (node.kind === 'partial' && node.partialName) {
                const name = node.partialName;
                if (
                    name === partialRelPath ||
                    name === partialBasename ||
                    partialRelPath.endsWith('/' + name) ||
                    partialRelPath.endsWith(name) ||
                    (definedBlocksInFile && definedBlocksInFile.has(name))
                ) {
                    return node;
                }
            }

            if (node.children) {
                const found = this.findPartialCall(node.children, partialRelPath, partialBasename, definedBlocksInFile);
                if (found) return found;
            }
        }
        return undefined;
    }

    private resolvePartialVars(
        contextArg: string,
        vars: Map<string, TemplateVar>
    ): { vars: Map<string, TemplateVar>; isMap?: boolean; keyType?: string; elemType?: string; isSlice?: boolean; rootTypeStr?: string } {
        if (contextArg === '.' || contextArg === '$') {
            return { vars: new Map(vars) };
        }

        const normalizedCtx = normalizeDictArg(contextArg);

        if (normalizedCtx.startsWith('dict ')) {
            const fieldResolver = this.buildLocalFieldResolver(vars);
            const dictType = inferExpressionType(normalizedCtx, vars, [], undefined, undefined, fieldResolver);
            if (dictType && dictType.fields) {
                const partialVars = new Map<string, TemplateVar>();
                for (const f of dictType.fields) {
                    partialVars.set(f.name, {
                        name: f.name,
                        type: f.type,
                        fields: f.fields,
                        isSlice: f.isSlice ?? false,
                        isMap: f.isMap,
                        elemType: f.elemType,
                        keyType: f.keyType,
                    });
                }
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

        const parser = new TemplateParser();
        const parsedPath = parser.parseDotPath(normalizedCtx);
        const result = resolvePath(parsedPath, vars, [], undefined, this.buildLocalFieldResolver(vars));

        if (!result.found) {
            return { vars: new Map() };
        }

        const partialVars = new Map<string, TemplateVar>();
        if (result.fields) {
            for (const f of result.fields) {
                partialVars.set(f.name, fieldInfoToTemplateVar(f));
            }
        }

        return {
            vars: partialVars,
            isMap: result.isMap,
            keyType: result.keyType,
            elemType: result.elemType,
            isSlice: result.isSlice,
            rootTypeStr: result.typeStr
        };
    }

    private buildLocalFieldResolver(
        vars: Map<string, TemplateVar>
    ): (typeStr: string) => FieldInfo[] | undefined {
        const typeIndex = new Map<string, FieldInfo[]>();

        const indexVar = (v: TemplateVar | FieldInfo) => {
            const bare = extractBareType(v.type);
            if (bare && v.fields && v.fields.length > 0 && !typeIndex.has(bare)) {
                typeIndex.set(bare, v.fields);
                for (const f of v.fields) indexVar(f);
            }
        };
        for (const v of vars.values()) indexVar(v);

        return (t: string) => {
            const bare = extractBareType(t);
            return typeIndex.get(bare) ?? this.graph.typeRegistry.get(bare);
        };
    }

    private findPartialSourceVar(
        contextArg: string,
        vars: Map<string, TemplateVar>
    ): TemplateVar | undefined {
        if (contextArg === '.' || contextArg === '') {
            return undefined;
        }

        const parser = new TemplateParser();
        const parsedPath = parser.parseDotPath(normalizeDictArg(contextArg));
        if (parsedPath.length === 0 || parsedPath[0] === '.') {
            return undefined;
        }

        const result = resolvePath(parsedPath, vars, [], undefined, this.buildLocalFieldResolver(vars));
        if (!result.found) return undefined;

        const topVar = vars.get(parsedPath[0]);
        if (parsedPath.length === 1 && topVar) {
            return topVar;
        }

        return {
            name: parsedPath[parsedPath.length - 1],
            type: result.typeStr,
            fields: result.fields,
            isSlice: result.isSlice ?? false,
        };
    }

    resolveGoFilePath(relativeFile: string): string | null {
        const config = vscode.workspace.getConfiguration('rex');
        const sourceDir: string = config.get('sourceDir') ?? '.';
        const sourceDirAbs = path.resolve(this.workspaceRoot, sourceDir);
        const abs = path.join(sourceDirAbs, relativeFile);
        return fs.existsSync(abs) ? abs : null;
    }

    resolveTemplatePath(templatePath: string): string | null {
        const ctx = this.graph.templates.get(templatePath);
        if (ctx?.absolutePath && fs.existsSync(ctx.absolutePath)) {
            return ctx.absolutePath;
        }

        for (const [tplPath, tplCtx] of this.graph.templates) {
            if (tplPath.endsWith(templatePath) || templatePath.endsWith(tplPath)) {
                if (tplCtx.absolutePath && fs.existsSync(tplCtx.absolutePath)) {
                    return tplCtx.absolutePath;
                }
            }
        }

        const templateBase = this.getTemplateBase();
        const config = vscode.workspace.getConfiguration('rex');
        const sourceDir: string = config.get('sourceDir') ?? '.';

        const candidates = [
            path.join(templateBase, templatePath),
            path.join(this.workspaceRoot, templatePath),
            path.join(path.resolve(this.workspaceRoot, sourceDir), templatePath),
        ];

        for (const candidate of candidates) {
            if (fs.existsSync(candidate)) {
                return candidate;
            }
        }

        return null;
    }

    toJSON(): object {
        const obj: Record<string, unknown> = {};
        for (const [key, ctx] of this.graph.templates) {
            obj[key] = {
                vars: Object.fromEntries(
                    [...ctx.vars.entries()].map(([k, v]) => [k, { type: v.type, fields: v.fields }])
                ),
                renderCalls: ctx.renderCalls.map(r => ({ file: r.file, line: r.line })),
            };
        }
        return obj;
    }

    updateTemplateFile(absolutePath: string, content: string) {
        this.invalidateAstCache(absolutePath);
        this.partialContextCache.clear();

        // Remove old named blocks from this file
        for (const [name, entries] of this.graph.namedBlocks.entries()) {
            const filtered = entries.filter(e => e.absolutePath !== absolutePath);
            if (filtered.length === 0) {
                this.graph.namedBlocks.delete(name);
            } else {
                this.graph.namedBlocks.set(name, filtered);
            }
        }

        // Re-parse new content for named blocks
        const templateBase = this.getTemplateBase();
        const templatePath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

        this.outputChannel.appendLine(`[KnowledgeGraph] Re-parsing named blocks for updated file: ${templatePath}`);

        const declarationRe = /\{\{-?\s*(?:define|block)\s+"([^"]+)"/g;
        let match: RegExpExecArray | null;

        while ((match = declarationRe.exec(content)) !== null) {
            const name = match[1];
            const upTo = content.slice(0, match.index);
            const line = (upTo.match(/\n/g) ?? []).length + 1;
            const lastNl = upTo.lastIndexOf('\n');
            const col = match.index - (lastNl === -1 ? 0 : lastNl + 1);

            const entry = this.createNamedBlockEntry(
                { templatePath, absolutePath, line, col },
                name
            );

            const existing = this.graph.namedBlocks.get(name) ?? [];
            existing.push(entry);
            this.graph.namedBlocks.set(name, existing);

            this.outputChannel.appendLine(
                `[KnowledgeGraph] Re-registered named block "${name}" at ${templatePath}:${line}`
            );
        }

        this.recalculateDuplicateErrors();
    }

    private recalculateDuplicateErrors() {
        this.graph.namedBlockErrors = [];
        for (const [name, entries] of this.graph.namedBlocks.entries()) {
            if (entries.length > 1) {
                this.graph.namedBlockErrors.push({
                    name,
                    entries,
                    message: `Duplicate named block "${name}" found`,
                });
            }
        }
    }

}

// ── Helpers ───────────────────────────────────────────────────────────────────

function fieldInfoToTemplateVar(f: FieldInfo): TemplateVar {
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

function hydrateVar(v: TemplateVar, typeRegistry: Map<string, FieldInfo[]>): TemplateVar {
    if (v.fields && v.fields.length > 0) return v;
    const bare = extractBareType(v.type);
    const fields = typeRegistry.get(bare);
    if (fields && fields.length > 0) {
        return { ...v, fields };
    }
    return v;
}

function isMoreComplete(
    a: TemplateVar,
    b: TemplateVar,
    typeRegistry: Map<string, FieldInfo[]>
): boolean {
    const depthA = maxFieldDepth(a.fields ?? [], typeRegistry);
    const depthB = maxFieldDepth(b.fields ?? [], typeRegistry);
    return depthA > depthB;
}

function maxFieldDepth(
    fields: FieldInfo[],
    typeRegistry?: Map<string, FieldInfo[]>,
    visited: Set<string> = new Set()
): number {
    if (fields.length === 0) return 0;
    let max = 0;
    for (const f of fields) {
        const bare = extractBareType(f.type);
        if (bare && visited.has(bare)) continue;
        const childVisited = bare ? new Set([...visited, bare]) : visited;
        const childFields = f.fields && f.fields.length > 0
            ? f.fields
            : typeRegistry?.get(bare) ?? [];
        const d = 1 + maxFieldDepth(childFields, typeRegistry, childVisited);
        if (d > max) max = d;
    }
    return max;
}

function findBlockNode(nodes: TemplateNode[], name: string): TemplateNode | undefined {
    for (const n of nodes) {
        if ((n.kind === 'define' || n.kind === 'block') && n.blockName === name) {
            return n;
        }
        if (n.children) {
            const found = findBlockNode(n.children, name);
            if (found) return found;
        }
    }
    return undefined;
}

// Temporary bridge until you can inject the builder instance properly
// In production code you should avoid this global — pass builder to entries instead
let knowledgeGraphBuilder: KnowledgeGraphBuilder;

export function setKnowledgeGraphBuilder(builder: KnowledgeGraphBuilder) {
    knowledgeGraphBuilder = builder;
}
