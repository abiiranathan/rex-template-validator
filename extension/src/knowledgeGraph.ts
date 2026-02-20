import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import {
  AnalysisResult,
  KnowledgeGraph,
  TemplateContext,
  TemplateVar,
  TemplateNode,
  FieldInfo,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';

export class KnowledgeGraphBuilder {
  private graph: KnowledgeGraph = {
    templates: new Map(),
    analyzedAt: new Date(),
  };

  private workspaceRoot: string;
  private outputChannel: vscode.OutputChannel;

  constructor(workspaceRoot: string, outputChannel: vscode.OutputChannel) {
    this.workspaceRoot = workspaceRoot;
    this.outputChannel = outputChannel;
  }

  build(analysisResult: AnalysisResult): KnowledgeGraph {
    const templates = new Map<string, TemplateContext>();
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    for (const rc of analysisResult.renderCalls ?? []) {
      const logicalPath = rc.template.replace(/^\.\//, '');

      const absPath = path.join(this.workspaceRoot, sourceDir, templateRoot, logicalPath);

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
        // Prefer the entry with the most complete field information (deepest nesting).
        // This ensures that if the same template is rendered from multiple call sites,
        // we keep whichever has richer type information.
        if (!existing || isMoreComplete(v, existing)) {
          ctx.vars.set(v.name, v);
        }
      }
    }

    this.graph = { templates, analyzedAt: new Date() };

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Built graph with ${templates.size} templates`
    );
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
   * Find the TemplateContext for a given absolute file path.
   * Handles templateRoot stripping and fuzzy suffix matching.
   */
  findContextForFile(absolutePath: string): TemplateContext | undefined {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    const templateBase = path.join(this.workspaceRoot, sourceDir, templateRoot);
    let rel = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

    if (this.graph.templates.has(rel)) {
      return this.graph.templates.get(rel);
    }

    for (const [tplPath, ctx] of this.graph.templates) {
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
   * Find a partial template context by name, searching the graph and filesystem.
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

    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    const templateBase = path.join(this.workspaceRoot, sourceDir, templateRoot);
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
   * Walks the graph to find which parent templates include this partial
   * and what context they pass to it, then resolves the correct vars.
   */
  findContextForFileAsPartial(absolutePath: string): TemplateContext | undefined {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    const templateBase = path.join(this.workspaceRoot, sourceDir, templateRoot);
    let partialRelPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');
    const partialBasename = path.basename(absolutePath);

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Looking for partial: ${partialRelPath} (basename: ${partialBasename})`
    );

    const parser = new TemplateParser();

    for (const [parentTplPath, parentCtx] of this.graph.templates) {
      if (!parentCtx.absolutePath || !fs.existsSync(parentCtx.absolutePath)) {
        continue;
      }

      try {
        const content = fs.readFileSync(parentCtx.absolutePath, 'utf8');
        const nodes = parser.parse(content);

        const partialCall = this.findPartialCall(nodes, partialRelPath, partialBasename);
        if (partialCall) {
          this.outputChannel.appendLine(
            `[KnowledgeGraph] Found partial call in ${parentTplPath}: template "${partialCall.partialName}" ${partialCall.partialContext}`
          );

          const partialVars = this.resolvePartialVars(
            partialCall.partialContext ?? '.',
            parentCtx.vars
          );

          this.outputChannel.appendLine(
            `[KnowledgeGraph] Resolved partial vars: ${[...partialVars.keys()].join(', ')}`
          );

          const partialSourceVar = this.findPartialSourceVar(
            partialCall.partialContext ?? '.',
            parentCtx.vars
          );

          return {
            templatePath: partialRelPath,
            absolutePath: absolutePath,
            vars: partialVars,
            renderCalls: parentCtx.renderCalls,
            partialSourceVar,
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
    partialBasename: string
  ): TemplateNode | undefined {
    for (const node of nodes) {
      if (node.kind === 'partial' && node.partialName) {
        const name = node.partialName;
        if (
          name === partialRelPath ||
          name === partialBasename ||
          partialRelPath.endsWith('/' + name) ||
          partialRelPath.endsWith(name)
        ) {
          return node;
        }
      }

      if (node.children) {
        const found = this.findPartialCall(node.children, partialRelPath, partialBasename);
        if (found) return found;
      }
    }
    return undefined;
  }

  /**
   * Given the context arg passed to a partial (e.g. ".", ".User", ".User.Profile.Address"),
   * build the vars map that the partial will see as its root scope.
   *
   * Supports unlimited nesting depth. For ".User.Profile.Address", the partial
   * will see Address's fields (Street, City, Zip) as its top-level variables.
   */
  private resolvePartialVars(
    contextArg: string,
    vars: Map<string, TemplateVar>
  ): Map<string, TemplateVar> {
    if (contextArg === '.' || contextArg === '$') {
      return new Map(vars);
    }

    const parser = new TemplateParser();
    const parsedPath = parser.parseDotPath(contextArg);

    // resolvePath with empty scopeStack → resolves against top-level vars
    const result = resolvePath(parsedPath, vars, []);

    if (!result.found || !result.fields) {
      return new Map();
    }

    const partialVars = new Map<string, TemplateVar>();
    for (const f of result.fields) {
      partialVars.set(f.name, fieldInfoToTemplateVar(f));
    }
    return partialVars;
  }

  /**
   * Find the source variable that was passed to a partial.
   * For {{ template "partial" .User }}, returns the "User" TemplateVar.
   * For {{ template "partial" .User.Profile }}, returns a synthetic TemplateVar
   * for Profile with its fields.
   */
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

    // Resolve the full path to get type info for the passed context
    const result = resolvePath(parsedPath, vars, []);
    if (!result.found) return undefined;

    // Return a synthetic TemplateVar representing the resolved context
    const topVar = vars.get(parsedPath[0]);
    if (parsedPath.length === 1 && topVar) {
      return topVar;
    }

    // Navigate to nested field and return as synthetic var
    return {
      name: parsedPath[parsedPath.length - 1],
      type: result.typeStr,
      fields: result.fields,
      isSlice: result.isSlice ?? false,
    };
  }

  /**
   * Resolve a Go source file path (relative to sourceDir) to an absolute path.
   */
  resolveGoFilePath(relativeFile: string): string | null {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';

    const abs = path.join(this.workspaceRoot, sourceDir, relativeFile);
    return fs.existsSync(abs) ? abs : null;
  }

  /**
   * Resolve a template path to an absolute file path.
   */
  resolveTemplatePath(templatePath: string): string | null {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

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

    const templateBase = path.join(this.workspaceRoot, sourceDir, templateRoot);
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

/**
 * Convert a FieldInfo to a TemplateVar, preserving all nested fields recursively.
 */
function fieldInfoToTemplateVar(f: FieldInfo): TemplateVar {
  return {
    name: f.name,
    type: f.type,
    fields: f.fields, // preserve full nested field tree
    isSlice: f.isSlice,
    defFile: f.defFile,
    defLine: f.defLine,
    defCol: f.defCol,
    doc: f.doc,
  };
}

/**
 * Returns true if `a` has more complete field information than `b`.
 * Used to prefer richer type data when merging vars from multiple render calls.
 */
function isMoreComplete(a: TemplateVar, b: TemplateVar): boolean {
  const depthA = maxFieldDepth(a.fields ?? []);
  const depthB = maxFieldDepth(b.fields ?? []);
  return depthA > depthB;
}

/**
 * Compute the maximum nesting depth of a FieldInfo array.
 */
function maxFieldDepth(fields: FieldInfo[]): number {
  if (fields.length === 0) return 0;
  let max = 0;
  for (const f of fields) {
    const d = 1 + maxFieldDepth(f.fields ?? []);
    if (d > max) max = d;
  }
  return max;
}
