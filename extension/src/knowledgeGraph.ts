/**
 * knowledgeGraph.ts — On-demand knowledge graph backed by the rex LSP daemon.
 *
 * Architecture change from the previous version:
 *   OLD: build() ran the full Go binary, loaded everything into memory upfront.
 *   NEW: initialize() eagerly fetches only funcMaps + namedBlocks (fast, small).
 *        findContextForFile() calls rex/getTemplateContext on demand and caches
 *        the result so subsequent calls (in the same session) are instant.
 *        Go file changes send workspace/didChangeWatchedFiles to the daemon and
 *        invalidate the local cache so the next request re-fetches fresh data.
 */

import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import {
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
  ParamInfo,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { normalizeDictArg } from './scopeUtils';
import { inferExpressionType } from './compiler/expressionParser';
import {
  LspClient,
  TemplateVarJSON,
  FieldInfoJSON,
  FuncMapInfoJSON,
  ParamInfoJSON,
  NamedBlockEntryJSON,
  NamedBlockDuplicateErrorJSON,
} from './lspClient';

// ── JSON → TypeScript type converters ─────────────────────────────────────────

function jsonToFieldInfo(f: FieldInfoJSON): FieldInfo {
  return {
    name: f.name,
    type: f.type,
    fields: f.fields?.map(jsonToFieldInfo),
    isSlice: f.isSlice,
    isMap: f.isMap,
    keyType: f.keyType,
    elemType: f.elemType,
    params: f.params?.map(jsonToParamInfo),
    returns: f.returns?.map(jsonToParamInfo),
    defFile: f.defFile,
    defLine: f.defLine,
    defCol: f.defCol,
    doc: f.doc,
  };
}

function jsonToParamInfo(p: ParamInfoJSON): ParamInfo {
  return {
    name: p.name,
    type: p.type,
    fields: p.fields?.map(jsonToFieldInfo),
  };
}

function jsonToTemplateVar(v: TemplateVarJSON): TemplateVar {
  return {
    name: v.name,
    type: v.type,
    fields: v.fields?.map(jsonToFieldInfo),
    isSlice: v.isSlice,
    isMap: v.isMap,
    keyType: v.keyType,
    elemType: v.elemType,
    defFile: v.defFile,
    defLine: v.defLine,
    defCol: v.defCol,
    doc: v.doc,
  };
}

function jsonToFuncMapInfo(f: FuncMapInfoJSON): FuncMapInfo {
  return {
    name: f.name,
    params: f.params?.map(jsonToParamInfo),
    returns: f.returns?.map(jsonToParamInfo),
    doc: f.doc,
    defFile: f.defFile,
    defLine: f.defLine,
    defCol: f.defCol,
    returnTypeFields: f.returnTypeFields?.map(jsonToFieldInfo),
  };
}

function jsonToNamedBlockEntry(e: NamedBlockEntryJSON): NamedBlockEntry {
  return {
    name: e.name,
    absolutePath: e.absolutePath,
    templatePath: e.templatePath,
    line: e.line,
    col: e.col,
  };
}

// ── Synthetic RenderCall used to satisfy ctx.renderCalls.length > 0 ───────────

function makeSyntheticRenderCall(template: string, vars: TemplateVar[]): RenderCall {
  return { file: 'lsp', line: 0, template, templateNameStartCol: 0, templateNameEndCol: 0, vars };
}

// ── KnowledgeGraphBuilder ─────────────────────────────────────────────────────

export class KnowledgeGraphBuilder {
  private graph: KnowledgeGraph = {
    templates: new Map(),
    namedBlocks: new Map(),
    namedBlockErrors: [],
    analyzedAt: new Date(),
    funcMaps: new Map(),
  };

  // Absolute paths of all known template files (populated at init + from updateTemplateFile).
  private knownTemplatePaths = new Set<string>();

  private readonly parser: TemplateParser;

  constructor(
    private readonly workspaceRoot: string,
    private readonly outputChannel: vscode.OutputChannel,
    private readonly lspClient: LspClient,
  ) {
    this.parser = new TemplateParser();
  }

  // ── Initialisation (called once from extension activate) ─────────────────────

  /**
   * Eagerly fetches funcMaps + namedBlocks from the LSP daemon and scans the
   * template directory so partial-context resolution has a file list to work from.
   * This is fast (< 1 s on typical projects) and is the only upfront work.
   */
  async initialize(): Promise<void> {
    const dir = this.getSourceDir();
    const templateRoot = this.getTemplateRoot();
    const contextFile = this.getAbsContextFile();

    this.outputChannel.appendLine('[KnowledgeGraph] Fetching funcMaps + namedBlocks from LSP…');

    const [fmResult, nbResult] = await Promise.all([
      this.lspClient.getFuncMaps({ dir, contextFile: contextFile || undefined }).catch(e => {
        this.outputChannel.appendLine(`[KnowledgeGraph] getFuncMaps error: ${e}`);
        return { funcMaps: [], errors: [] };
      }),
      this.lspClient.getNamedBlocks({ dir, templateRoot: templateRoot || undefined }).catch(e => {
        this.outputChannel.appendLine(`[KnowledgeGraph] getNamedBlocks error: ${e}`);
        return { namedBlocks: {}, duplicateErrors: [] };
      }),
    ]);

    // Populate funcMaps
    const funcMaps = new Map<string, FuncMapInfo>();
    for (const f of fmResult.funcMaps) {
      funcMaps.set(f.name, jsonToFuncMapInfo(f));
    }
    this.graph.funcMaps = funcMaps;

    // Populate namedBlocks
    const namedBlocks: NamedBlockRegistry = new Map();
    for (const [name, entries] of Object.entries(nbResult.namedBlocks ?? {})) {
      namedBlocks.set(name, entries.map(jsonToNamedBlockEntry));
    }
    this.graph.namedBlocks = namedBlocks;

    // Populate duplicate errors
    this.graph.namedBlockErrors = (nbResult.duplicateErrors ?? []).map(e => ({
      name: e.name,
      entries: e.entries.map(jsonToNamedBlockEntry),
      message: e.message,
    }));

    this.graph.analyzedAt = new Date();

    // Scan template directory so we know which files exist (for partial resolution).
    this.scanTemplateDirectory();

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Ready — ${funcMaps.size} func(s), ${namedBlocks.size} named block(s), ` +
      `${this.knownTemplatePaths.size} template file(s)`
    );
  }

  /**
   * Invalidates the local template-context cache and re-fetches funcMaps +
   * namedBlocks.  Called after Go source files change.
   */
  async invalidateGraphCache(): Promise<void> {
    this.outputChannel.appendLine('[KnowledgeGraph] Invalidating cache…');
    // Clear the per-template context cache so stale data is not served.
    this.graph.templates = new Map();
    // Re-fetch top-level metadata.
    await this.initialize();
  }

  // ── Context lookup (async, LSP-backed) ───────────────────────────────────────

  /**
   * Returns the template context for a given template file by calling
   * rex/getTemplateContext on the LSP daemon.  Results are cached so
   * subsequent calls for the same file are instant.
   *
   * Returns undefined when no Go render call targets this template.
   */
  async findContextForFile(absolutePath: string): Promise<TemplateContext | undefined> {
    // Fast path: already cached.
    const cached = this.lookupCachedContext(absolutePath);
    if (cached) return cached;

    const templateBase = this.getTemplateBase();
    const templateName = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

    const ctx = await this.fetchContextFromLSP(templateName, absolutePath);
    if (ctx) {
      this.graph.templates.set(templateName, ctx);
    }
    return ctx;
  }

  /**
   * Tries to find the context for a file that is included as a partial
   * ({{template "x" .field}}) rather than rendered directly by Go code.
   * Falls back to TypeScript-level parent-template scanning.
   *
   * Returns undefined when no calling context can be determined.
   */
  async findContextForFileAsPartial(absolutePath: string): Promise<TemplateContext | undefined> {
    const templateBase = this.getTemplateBase();
    const partialRelPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');
    const partialBasename = path.basename(absolutePath);

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Looking for partial: ${partialRelPath}`
    );

    // Priority 1: named block with LSP context.
    for (const [blockName, entries] of this.graph.namedBlocks) {
      for (const entry of entries) {
        if (path.normalize(entry.absolutePath).toLowerCase() === path.normalize(absolutePath).toLowerCase()) {
          // Try fetching context for the named block from LSP.
          const blockCtx = await this.fetchContextFromLSP(blockName, absolutePath);
          if (blockCtx) {
            this.graph.templates.set(blockName, blockCtx);
            return blockCtx;
          }
        }
      }
    }

    // Priority 2: scan known parent template files for {{ template "x" ctx }} calls.
    const definedBlocksInFile = new Set<string>();
    const normAbsPath = path.normalize(absolutePath).toLowerCase();
    for (const [blockName, entries] of this.graph.namedBlocks) {
      for (const entry of entries) {
        if (path.normalize(entry.absolutePath).toLowerCase() === normAbsPath) {
          definedBlocksInFile.add(blockName);
        }
      }
    }

    let foundAny = false;
    const mergedVars = new Map<string, TemplateVar>();
    const mergedRenderCalls: RenderCall[] = [];
    let lastPartialSourceVar: TemplateVar | undefined;
    let lastResolvedMeta: Partial<TemplateContext> = {};

    for (const parentAbsPath of this.knownTemplatePaths) {
      if (path.normalize(parentAbsPath).toLowerCase() === normAbsPath) continue;
      if (!fs.existsSync(parentAbsPath)) continue;

      try {
        const openDoc = vscode.workspace.textDocuments.find(
          d => path.normalize(d.uri.fsPath).toLowerCase() === path.normalize(parentAbsPath).toLowerCase()
        );
        const content = openDoc ? openDoc.getText() : fs.readFileSync(parentAbsPath, 'utf8');
        const nodes = this.parser.parse(content);

        const partialCall = this.findPartialCall(nodes, partialRelPath, partialBasename, definedBlocksInFile);
        if (!partialCall) continue;

        foundAny = true;
        this.outputChannel.appendLine(
          `[KnowledgeGraph] Found partial call in ${parentAbsPath}: "${partialCall.partialName}" ${partialCall.partialContext}`
        );

        // Get parent context (from cache or LSP).
        const parentCtx = await this.findContextForFile(parentAbsPath);
        const parentVars = parentCtx?.vars ?? new Map<string, TemplateVar>();

        const resolved = this.resolvePartialVars(partialCall.partialContext ?? '.', parentVars);
        for (const [k, v] of resolved.vars) {
          const existing = mergedVars.get(k);
          if (!existing || isMoreComplete(v, existing)) mergedVars.set(k, v);
        }

        if (parentCtx) mergedRenderCalls.push(...parentCtx.renderCalls);
        lastPartialSourceVar = this.findPartialSourceVar(partialCall.partialContext ?? '.', parentVars);
        lastResolvedMeta = {
          isMap: resolved.isMap,
          keyType: resolved.keyType,
          elemType: resolved.elemType,
          isSlice: resolved.isSlice,
          rootTypeStr: resolved.rootTypeStr,
        };
      } catch { /* ignore per-file errors */ }
    }

    if (!foundAny) {
      this.outputChannel.appendLine(`[KnowledgeGraph] No partial call found for ${partialRelPath}`);
      return undefined;
    }

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Merged partial vars: ${[...mergedVars.keys()].join(', ')}`
    );

    return {
      templatePath: partialRelPath,
      absolutePath,
      vars: mergedVars,
      renderCalls: mergedRenderCalls,
      partialSourceVar: lastPartialSourceVar,
      ...lastResolvedMeta,
    };
  }

  // ── Graph accessor (synchronous — uses local cache) ───────────────────────────

  getGraph(): KnowledgeGraph {
    return this.graph;
  }

  // ── Named-block helpers (synchronous) ────────────────────────────────────────

  lookupNamedBlock(name: string): NamedBlockEntry | undefined {
    const entries = this.graph.namedBlocks.get(name);
    return entries?.[0];
  }

  getDuplicateErrorsForBlock(name: string): NamedBlockDuplicateError[] {
    return this.graph.namedBlockErrors.filter(e => e.name === name);
  }

  getAllDuplicateErrors(): NamedBlockDuplicateError[] {
    return this.graph.namedBlockErrors;
  }

  // ── Incremental template-file update (TypeScript-side, no LSP needed) ────────

  /**
   * Incrementally updates the named block registry for a single template file
   * when its content changes in the editor.  Does NOT require a Go re-analysis.
   */
  updateTemplateFile(absolutePath: string, content: string): void {
    // Add to known paths.
    this.knownTemplatePaths.add(absolutePath);

    // Remove existing entries from this file.
    for (const [name, entries] of this.graph.namedBlocks.entries()) {
      const filtered = entries.filter(e => e.absolutePath !== absolutePath);
      if (filtered.length === 0) this.graph.namedBlocks.delete(name);
      else this.graph.namedBlocks.set(name, filtered);
    }

    // Also drop any cached template context for this file (content changed).
    const templateBase = this.getTemplateBase();
    const relPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');
    this.graph.templates.delete(relPath);

    // Re-scan for named blocks.
    const declarationRe = /\{\{-?\s*(?:define|block)\s+"([^"]+)"/g;
    let match: RegExpExecArray | null;
    while ((match = declarationRe.exec(content)) !== null) {
      const name = match[1];
      const upTo = content.slice(0, match.index);
      const line = (upTo.match(/\n/g) ?? []).length + 1;
      const lastNl = upTo.lastIndexOf('\n');
      const col = match.index - (lastNl === -1 ? 0 : lastNl + 1);
      const entry: NamedBlockEntry = { name, templatePath: relPath, absolutePath, line, col };
      const existing = this.graph.namedBlocks.get(name) ?? [];
      existing.push(entry);
      this.graph.namedBlocks.set(name, existing);
    }

    this.recalculateDuplicateErrors();
  }

  // ── File-path resolution helpers ──────────────────────────────────────────────

  findPartialContext(partialName: string, _currentFile: string): TemplateContext | undefined {
    // Synchronous fast-path: try the in-memory cache.
    for (const [tplPath, ctx] of this.graph.templates) {
      if (
        tplPath === partialName ||
        tplPath.endsWith('/' + partialName) ||
        path.basename(tplPath) === partialName
      ) {
        return ctx;
      }
    }

    // Resolve to an absolute path for the validator to read.
    const absPath = this.resolveTemplatePath(partialName);
    if (absPath) {
      const templateBase = this.getTemplateBase();
      return {
        templatePath: path.relative(templateBase, absPath).replace(/\\/g, '/'),
        absolutePath: absPath,
        vars: new Map(),
        renderCalls: [],
      };
    }
    return undefined;
  }

  resolveGoFilePath(relativeFile: string): string | null {
    const abs = path.join(this.getSourceDir(), relativeFile);
    return fs.existsSync(abs) ? abs : null;
  }

  resolveTemplatePath(templatePath: string): string | null {
    const ctx = this.graph.templates.get(templatePath);
    if (ctx?.absolutePath && fs.existsSync(ctx.absolutePath)) return ctx.absolutePath;

    for (const [tplPath, tplCtx] of this.graph.templates) {
      if (tplPath.endsWith(templatePath) || templatePath.endsWith(tplPath)) {
        if (tplCtx.absolutePath && fs.existsSync(tplCtx.absolutePath)) return tplCtx.absolutePath;
      }
    }

    const templateBase = this.getTemplateBase();
    const candidates = [
      path.join(templateBase, templatePath),
      path.join(this.workspaceRoot, templatePath),
      path.join(this.getSourceDir(), templatePath),
    ];
    for (const c of candidates) {
      if (fs.existsSync(c)) return c;
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

  // ── Config accessors ─────────────────────────────────────────────────────────

  getSourceDir(): string {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    return path.resolve(this.workspaceRoot, sourceDir);
  }

  private getTemplateRoot(): string {
    return vscode.workspace.getConfiguration('rex-analyzer').get<string>('templateRoot') ?? '';
  }

  private getTemplateBase(): string {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateBaseDir: string = config.get('templateBaseDir') ?? '';
    const templateRoot: string = config.get('templateRoot') ?? '';
    const baseDir = templateBaseDir
      ? path.resolve(this.workspaceRoot, templateBaseDir)
      : path.resolve(this.workspaceRoot, sourceDir);
    return path.join(baseDir, templateRoot);
  }

  private getAbsContextFile(): string {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const contextFile: string = config.get('contextFile') ?? '';
    if (!contextFile) return '';
    const abs = path.resolve(this.workspaceRoot, contextFile);
    return fs.existsSync(abs) ? abs : '';
  }

  // ── Private: LSP fetch + caching ─────────────────────────────────────────────

  private lookupCachedContext(absolutePath: string): TemplateContext | undefined {
    const normalized = path.normalize(absolutePath).toLowerCase();
    for (const [, ctx] of this.graph.templates) {
      if (ctx.absolutePath && path.normalize(ctx.absolutePath).toLowerCase() === normalized) {
        return ctx;
      }
    }
    return undefined;
  }

  private async fetchContextFromLSP(
    templateName: string,
    absolutePath: string
  ): Promise<TemplateContext | undefined> {
    if (!this.lspClient.started) return undefined;

    try {
      const result = await this.lspClient.getTemplateContext({
        dir: this.getSourceDir(),
        templateName,
        templateRoot: this.getTemplateRoot() || undefined,
        contextFile: this.getAbsContextFile() || undefined,
      });

      if (!result.vars || result.vars.length === 0) return undefined;

      const vars = new Map(result.vars.map(v => [v.name, jsonToTemplateVar(v)]));
      const varArr = result.vars.map(jsonToTemplateVar);

      return {
        templatePath: templateName,
        absolutePath,
        vars,
        renderCalls: [makeSyntheticRenderCall(templateName, varArr)],
      };
    } catch (e) {
      this.outputChannel.appendLine(`[KnowledgeGraph] getTemplateContext error for "${templateName}": ${e}`);
      return undefined;
    }
  }

  // ── Private: template directory scan ────────────────────────────────────────

  private scanTemplateDirectory(): void {
    const templateBase = this.getTemplateBase();
    if (!fs.existsSync(templateBase)) return;

    const walk = (dir: string) => {
      let entries: fs.Dirent[];
      try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
      for (const entry of entries) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) walk(full);
        else if (entry.isFile() && /\.(html|gohtml|tmpl|tpl|htm)$/i.test(entry.name)) {
          this.knownTemplatePaths.add(full);
        }
      }
    };

    walk(templateBase);
    this.outputChannel.appendLine(
      `[KnowledgeGraph] Scanned ${this.knownTemplatePaths.size} template file(s) in ${templateBase}`
    );
  }

  // ── Private: partial helpers (unchanged from original) ───────────────────────

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
      const typeIndex = new Map<string, import('./types').FieldInfo[]>();
      const indexVar = (v: TemplateVar | import('./types').FieldInfo) => {
        const bare = v.type.replace(/^\*/, '').replace(/^\[\]/, '').replace(/^map\[.*?\]/, '').trim();
        if (bare && v.fields && v.fields.length > 0 && !typeIndex.has(bare)) {
          typeIndex.set(bare, v.fields);
          for (const f of v.fields) indexVar(f);
        }
      };
      for (const v of vars.values()) indexVar(v);
      const fieldResolver = (t: string) => {
        const bare = t.replace(/^\*/, '').replace(/^\[\]/, '').replace(/^map\[.*?\]/, '').trim();
        return typeIndex.get(bare);
      };

      const dictType = inferExpressionType(normalizedCtx, vars, [], undefined, undefined, fieldResolver);
      if (dictType && dictType.fields) {
        const partialVars = new Map<string, TemplateVar>();
        for (const f of dictType.fields) {
          partialVars.set(f.name, {
            name: f.name, type: f.type, fields: f.fields,
            isSlice: f.isSlice ?? false, isMap: f.isMap,
            elemType: f.elemType, keyType: f.keyType,
          });
        }
        return { vars: partialVars, isMap: dictType.isMap, keyType: dictType.keyType, elemType: dictType.elemType, isSlice: dictType.isSlice, rootTypeStr: dictType.typeStr };
      }
    }

    const parsedPath = this.parser.parseDotPath(normalizedCtx);
    const result = resolvePath(parsedPath, vars, []);
    if (!result.found) return { vars: new Map() };

    const partialVars = new Map<string, TemplateVar>();
    if (result.fields) {
      for (const f of result.fields) {
        partialVars.set(f.name, { name: f.name, type: f.type, fields: f.fields, isSlice: f.isSlice });
      }
    }
    return { vars: partialVars, isMap: result.isMap, keyType: result.keyType, elemType: result.elemType, isSlice: result.isSlice, rootTypeStr: result.typeStr };
  }

  private findPartialSourceVar(contextArg: string, vars: Map<string, TemplateVar>): TemplateVar | undefined {
    if (contextArg === '.' || contextArg === '') return undefined;
    const parsedPath = this.parser.parseDotPath(normalizeDictArg(contextArg));
    if (parsedPath.length === 0 || parsedPath[0] === '.') return undefined;
    const result = resolvePath(parsedPath, vars, []);
    if (!result.found) return undefined;
    const topVar = vars.get(parsedPath[0]);
    if (parsedPath.length === 1 && topVar) return topVar;
    return { name: parsedPath[parsedPath.length - 1], type: result.typeStr, fields: result.fields, isSlice: result.isSlice ?? false };
  }

  private recalculateDuplicateErrors(): void {
    this.graph.namedBlockErrors = [];
    for (const [name, entries] of this.graph.namedBlocks.entries()) {
      if (entries.length > 1) {
        this.graph.namedBlockErrors.push({ name, entries, message: `Duplicate named block "${name}" found` });
      }
    }
  }

  // Keep original parser reference accessible for partial scanning
  private get parser_(): TemplateParser { return this.parser; }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function isMoreComplete(a: TemplateVar, b: TemplateVar): boolean {
  return maxFieldDepth(a.fields ?? []) > maxFieldDepth(b.fields ?? []);
}

function maxFieldDepth(fields: FieldInfo[]): number {
  if (fields.length === 0) return 0;
  let max = 0;
  for (const f of fields) {
    const d = 1 + maxFieldDepth(f.fields ?? []);
    if (d > max) max = d;
  }
  return max;
}
