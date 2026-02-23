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
} from './types';
import { TemplateParser, resolvePath } from './templateParser';

export class KnowledgeGraphBuilder {
  private graph: KnowledgeGraph = {
    templates: new Map(),
    namedBlocks: new Map(),
    namedBlockErrors: [],
    analyzedAt: new Date(),
    funcMaps: new Map(),
  };

  private workspaceRoot: string;
  private outputChannel: vscode.OutputChannel;

  constructor(workspaceRoot: string, outputChannel: vscode.OutputChannel) {
    this.workspaceRoot = workspaceRoot;
    this.outputChannel = outputChannel;
  }

  /**
   * Resolves the base directory for templates by combining workspaceRoot,
   * sourceDir, templateBaseDir, and templateRoot correctly.
   */
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

  build(analysisResult: AnalysisResult): KnowledgeGraph {
    const templates = new Map<string, TemplateContext>();
    const templateBase = this.getTemplateBase();

    for (const rc of analysisResult.renderCalls ?? []) {
      const logicalPath = rc.template.replace(/^\.\//, '');
      const absPath = path.join(templateBase, logicalPath);

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
        if (!existing || isMoreComplete(v, existing)) {
          ctx.vars.set(v.name, v);
        }
      }
    }

    // Build the cross-file named block registry directly from the Go analyzer's output
    const namedBlocks: NamedBlockRegistry = new Map();
    if (analysisResult.namedBlocks) {
      for (const [name, entries] of Object.entries(analysisResult.namedBlocks)) {
        const fullEntries: NamedBlockEntry[] = entries.map(e => ({
          ...e,
          get node() {
            // Lazy-loading mechanism to read AST of the define block only on hover queries
            if (!(this as any)._node) {
              try {
                const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === e.absolutePath);
                const content = openDoc ? openDoc.getText() : fs.readFileSync(e.absolutePath, 'utf8');
                const parser = new TemplateParser();
                const nodes = parser.parse(content);
                const findNode = (ns: TemplateNode[]): TemplateNode | undefined => {
                  for (const n of ns) {
                    if ((n.kind === 'define' || n.kind === 'block') && n.blockName === name) return n;
                    if (n.children) {
                      const f = findNode(n.children);
                      if (f) return f;
                    }
                  }
                  return undefined;
                };
                (this as any)._node = findNode(nodes) || { kind: 'define', path: [], rawText: '', line: e.line, col: e.col, blockName: name };
              } catch {
                (this as any)._node = { kind: 'define', path: [], rawText: '', line: e.line, col: e.col, blockName: name };
              }
            }
            return (this as any)._node;
          }
        }));
        namedBlocks.set(name, fullEntries);
      }
    }

    const namedBlockErrors = analysisResult.namedBlockErrors ?? [];

    const funcMaps = new Map<string, FuncMapInfo>();
    for (const fm of analysisResult.funcMaps ?? []) {
      funcMaps.set(fm.name, fm);
    }

    this.graph = { templates, namedBlocks, namedBlockErrors, analyzedAt: new Date(), funcMaps };

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Built graph with ${templates.size} templates, ` +
      `${namedBlocks.size} named block(s), ` +
      `${funcMaps.size} template functions`
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

  getGraph(): KnowledgeGraph {
    return this.graph;
  }

  /**
   * Look up a named block by name from the cross-file registry.
   * Returns the single entry, or undefined if not found.
   * If duplicates exist, returns the first entry (the caller will also see the
   * duplicate error surfaced separately).
   */
  lookupNamedBlock(name: string): NamedBlockEntry | undefined {
    const entries = this.graph.namedBlocks.get(name);
    if (!entries || entries.length === 0) return undefined;
    return entries[0];
  }

  /**
   * Get any duplicate-block errors for a specific block name.
   */
  getDuplicateErrorsForBlock(name: string): NamedBlockDuplicateError[] {
    return this.graph.namedBlockErrors.filter(e => e.name === name);
  }

  /**
   * Get all duplicate block errors, for surfacing as diagnostics.
   */
  getAllDuplicateErrors(): NamedBlockDuplicateError[] {
    return this.graph.namedBlockErrors;
  }

  /**
   * Find the TemplateContext for a given absolute file path.
   */
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

  /**
   * Find a partial template context by name.
   */
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

  /**
   * Find the context for a file when it's being used as a partial.
   */
  findContextForFileAsPartial(absolutePath: string): TemplateContext | undefined {
    const templateBase = this.getTemplateBase();
    let partialRelPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');
    const partialBasename = path.basename(absolutePath);

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Looking for partial: ${partialRelPath} (basename: ${partialBasename})`
    );

    const parser = new TemplateParser();
    const normalizedTargetAbsPath = path.normalize(absolutePath).toLowerCase();

    const definedBlocksInFile = new Set<string>();
    for (const [blockName, entries] of this.graph.namedBlocks) {
      for (const entry of entries) {
        if (path.normalize(entry.absolutePath).toLowerCase() === normalizedTargetAbsPath) {
          definedBlocksInFile.add(blockName);
        }
      }
    }

    for (const [parentTplPath, parentCtx] of this.graph.templates) {
      if (!parentCtx.absolutePath || !fs.existsSync(parentCtx.absolutePath)) {
        continue;
      }

      try {
        const normalizedParentAbsPath = path.normalize(parentCtx.absolutePath).toLowerCase();
        const openDoc = vscode.workspace.textDocuments.find(
          d => path.normalize(d.uri.fsPath).toLowerCase() === normalizedParentAbsPath
        );
        const content = openDoc ? openDoc.getText() : fs.readFileSync(parentCtx.absolutePath, 'utf8');
        const nodes = parser.parse(content);

        const partialCall = this.findPartialCall(nodes, partialRelPath, partialBasename, definedBlocksInFile);
        if (partialCall) {
          this.outputChannel.appendLine(
            `[KnowledgeGraph] Found partial call in ${parentTplPath}: template "${partialCall.partialName}" ${partialCall.partialContext}`
          );

          const resolved = this.resolvePartialVars(
            partialCall.partialContext ?? '.',
            parentCtx.vars
          );

          this.outputChannel.appendLine(
            `[KnowledgeGraph] Resolved partial vars: ${[...resolved.vars.keys()].join(', ')}`
          );

          const partialSourceVar = this.findPartialSourceVar(
            partialCall.partialContext ?? '.',
            parentCtx.vars
          );

          return {
            templatePath: partialRelPath,
            absolutePath: absolutePath,
            vars: resolved.vars,
            renderCalls: parentCtx.renderCalls,
            partialSourceVar,
            isMap: resolved.isMap,
            keyType: resolved.keyType,
            elemType: resolved.elemType,
            isSlice: resolved.isSlice,
            rootTypeStr: resolved.rootTypeStr
          };
        }
      } catch {
        continue;
      }
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

    const parser = new TemplateParser();
    const parsedPath = parser.parseDotPath(contextArg);
    const result = resolvePath(parsedPath, vars, []);

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

  private findPartialSourceVar(
    contextArg: string,
    vars: Map<string, TemplateVar>
  ): TemplateVar | undefined {
    if (contextArg === '.' || contextArg === '') {
      return undefined;
    }

    const parser = new TemplateParser();
    const parsedPath = parser.parseDotPath(contextArg);
    if (parsedPath.length === 0 || parsedPath[0] === '.') {
      return undefined;
    }

    const result = resolvePath(parsedPath, vars, []);
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
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const abs = path.join(this.workspaceRoot, sourceDir, relativeFile);
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
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';

    const candidates = [
      path.join(templateBase, templatePath),
      path.join(this.workspaceRoot, templatePath),
      path.join(this.workspaceRoot, sourceDir, templatePath),
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

function isMoreComplete(a: TemplateVar, b: TemplateVar): boolean {
  const depthA = maxFieldDepth(a.fields ?? []);
  const depthB = maxFieldDepth(b.fields ?? []);
  return depthA > depthB;
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
