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
} from './types';
import { TemplateParser, resolvePath } from './templateParser';

export class KnowledgeGraphBuilder {
  private graph: KnowledgeGraph = {
    templates: new Map(),
    namedBlocks: new Map(),
    namedBlockErrors: [],
    analyzedAt: new Date(),
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

    // Build the cross-file named block registry
    const { namedBlocks, namedBlockErrors } = this.buildNamedBlockRegistry(templates, templateBase);

    this.graph = { templates, namedBlocks, namedBlockErrors, analyzedAt: new Date() };

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Built graph with ${templates.size} templates, ` +
      `${namedBlocks.size} named block(s)`
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

  /**
   * Walk all template files on disk (from templateBase) and collect every
   * {{ define "name" }} and {{ block "name" ... }} declaration into a
   * cross-file registry.  Also detects duplicates.
   */
  private buildNamedBlockRegistry(
    templates: Map<string, TemplateContext>,
    templateBase: string
  ): { namedBlocks: NamedBlockRegistry; namedBlockErrors: NamedBlockDuplicateError[] } {
    const namedBlocks: NamedBlockRegistry = new Map();
    const parser = new TemplateParser();

    // Gather all absolute paths to scan — include both known render-call targets
    // and any additional template files discovered on disk.
    const toScan = new Set<string>();

    for (const ctx of templates.values()) {
      if (ctx.absolutePath) toScan.add(path.normalize(ctx.absolutePath));
    }

    // Also walk the template base directory for any files not in the render graph
    // (e.g. pure partial files that are only {{ define }}'d).
    this.walkTemplateDir(templateBase, toScan);

    for (const absPath of toScan) {
      if (!fs.existsSync(absPath)) continue;

      let content: string;
      try {
        // Prefer the in-editor version if open.
        const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === absPath);
        content = openDoc ? openDoc.getText() : fs.readFileSync(absPath, 'utf8');
      } catch {
        continue;
      }

      const logicalPath = path.relative(templateBase, absPath).replace(/\\/g, '/');
      const nodes = parser.parse(content);
      this.collectNamedBlocksFromNodes(nodes, absPath, logicalPath, namedBlocks);
    }

    // Detect duplicates
    const namedBlockErrors: NamedBlockDuplicateError[] = [];
    for (const [name, entries] of namedBlocks) {
      if (entries.length > 1) {
        const locations = entries
          .map(e => `${e.templatePath}:${e.line}`)
          .join(', ');
        namedBlockErrors.push({
          name,
          entries,
          message: `Duplicate named block "${name}" found in: ${locations}`,
        });
        this.outputChannel.appendLine(
          `[KnowledgeGraph] ERROR: Duplicate named block "${name}" declared in: ${locations}`
        );
      }
    }

    return { namedBlocks, namedBlockErrors };
  }

  /**
   * Recursively walk a directory and add all template files (.html, .tmpl,
   * .gohtml, .tpl, .htm) to the toScan set.
   */
  private walkTemplateDir(dir: string, toScan: Set<string>) {
    if (!fs.existsSync(dir)) return;
    try {
      const entries = fs.readdirSync(dir, { withFileTypes: true });
      for (const entry of entries) {
        const full = path.normalize(path.join(dir, entry.name));
        if (entry.isDirectory()) {
          this.walkTemplateDir(full, toScan);
        } else if (isTemplateFile(entry.name)) {
          toScan.add(full);
        }
      }
    } catch {
      // permission errors etc — skip silently
    }
  }

  /**
   * Walk an AST and register every `define` and `block` node.
   * We descend into children so nested defines are found too (though unusual).
   */
  private collectNamedBlocksFromNodes(
    nodes: TemplateNode[],
    absPath: string,
    logicalPath: string,
    registry: NamedBlockRegistry
  ) {
    for (const node of nodes) {
      if ((node.kind === 'define' || node.kind === 'block') && node.blockName) {
        const entry: NamedBlockEntry = {
          name: node.blockName,
          absolutePath: absPath,
          templatePath: logicalPath,
          line: node.line,
          col: node.col,
          node,
        };

        const existing = registry.get(node.blockName) ?? [];
        existing.push(entry);
        registry.set(node.blockName, existing);
      }

      // Recurse into children (handles nested blocks / defines inside range etc.)
      if (node.children) {
        this.collectNamedBlocksFromNodes(node.children, absPath, logicalPath, registry);
      }
    }
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
   * Rebuild only the named block registry (fast path: called when a template
   * file changes without needing a full Go re-analysis).
   */
  rebuildNamedBlocks() {
    const templateBase = this.getTemplateBase();
    const { namedBlocks, namedBlockErrors } = this.buildNamedBlockRegistry(
      this.graph.templates,
      templateBase
    );
    this.graph = { ...this.graph, namedBlocks, namedBlockErrors };
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

function isTemplateFile(name: string): boolean {
  return ['.html', '.tmpl', '.gohtml', '.tpl', '.htm'].some(ext =>
    name.toLowerCase().endsWith(ext)
  );
}
