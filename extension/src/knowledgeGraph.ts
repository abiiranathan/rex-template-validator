import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import {
  AnalysisResult,
  KnowledgeGraph,
  RenderCall,
  TemplateContext,
  TemplateVar,
  TemplateNode,
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

      // Absolute path on disk: workspaceRoot / sourceDir / templateRoot / template
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
        if (!existing || (v.fields && v.fields.length > 0)) {
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

    // Compute path relative to: workspaceRoot / sourceDir / templateRoot
    const templateBase = path.join(this.workspaceRoot, sourceDir, templateRoot);
    let rel = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

    // Direct match
    if (this.graph.templates.has(rel)) {
      return this.graph.templates.get(rel);
    }

    // Suffix match — the render call path may be a suffix of the relative path
    for (const [tplPath, ctx] of this.graph.templates) {
      if (rel.endsWith(tplPath) || tplPath.endsWith(rel)) {
        return ctx;
      }
    }

    // Basename match as last resort
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
    // 1. Check graph first
    for (const [tplPath, ctx] of this.graph.templates) {
      if (
        tplPath === partialName ||
        tplPath.endsWith('/' + partialName) ||
        path.basename(tplPath) === partialName
      ) {
        return ctx;
      }
    }

    // 2. Search filesystem
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
   * This walks the graph to find which parent templates include this partial
   * and what context they pass to it, then resolves the correct vars.
   */
  findContextForFileAsPartial(absolutePath: string): TemplateContext | undefined {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    // Compute the relative path for the partial file
    const templateBase = path.join(this.workspaceRoot, sourceDir, templateRoot);
    let partialRelPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

    // Also check basename for matching
    const partialBasename = path.basename(absolutePath);

    this.outputChannel.appendLine(
      `[KnowledgeGraph] Looking for partial: ${partialRelPath} (basename: ${partialBasename})`
    );
    this.outputChannel.appendLine(
      `[KnowledgeGraph] Template base: ${templateBase}, Graph has ${this.graph.templates.size} templates`
    );

    const parser = new TemplateParser();

    // Search through all templates in the graph to find calls to this partial
    for (const [parentTplPath, parentCtx] of this.graph.templates) {
      this.outputChannel.appendLine(
        `[KnowledgeGraph] Checking parent template: ${parentTplPath} at ${parentCtx.absolutePath}`
      );
      
      // Read and parse the parent template
      if (!parentCtx.absolutePath || !fs.existsSync(parentCtx.absolutePath)) {
        this.outputChannel.appendLine(
          `[KnowledgeGraph] Skipping ${parentTplPath}: file not found at ${parentCtx.absolutePath}`
        );
        continue;
      }

      try {
        const content = fs.readFileSync(parentCtx.absolutePath, 'utf8');
        const nodes = parser.parse(content);

        // Search for partial nodes that reference this file
        const partialCall = this.findPartialCall(nodes, partialRelPath, partialBasename);
        if (partialCall) {
          this.outputChannel.appendLine(
            `[KnowledgeGraph] Found partial call in ${parentTplPath}: template "${partialCall.partialName}" ${partialCall.partialContext}`
          );
          // Found a call to this partial - resolve the context argument
          const partialVars = this.resolvePartialVars(
            partialCall.partialContext ?? '.',
            parentCtx.vars,
            [],
            parentCtx
          );

          this.outputChannel.appendLine(
            `[KnowledgeGraph] Resolved partial vars: ${[...partialVars.keys()].join(', ')}`
          );

          // Track which parent variable was passed to this partial for go-to-definition
          const partialSourceVar = this.findPartialSourceVar(
            partialCall.partialContext ?? '.',
            parentCtx.vars
          );

          return {
            templatePath: partialRelPath,
            absolutePath: absolutePath,
            vars: partialVars,
            renderCalls: parentCtx.renderCalls, // Inherit parent's render calls for go-to-def
            partialSourceVar, // Track the source variable passed to this partial
          };
        }
      } catch {
        // Ignore read/parse errors
        continue;
      }
    }

    this.outputChannel.appendLine(
      `[KnowledgeGraph] No partial call found for ${partialRelPath}`
    );
    return undefined;
  }

  /**
   * Recursively search for a partial call in the AST that matches the given partial path.
   */
  private findPartialCall(
    nodes: TemplateNode[],
    partialRelPath: string,
    partialBasename: string
  ): TemplateNode | undefined {
    for (const node of nodes) {
      if (node.kind === 'partial' && node.partialName) {
        // Check if this partial call matches our target
        const name = node.partialName;
        this.outputChannel.appendLine(
          `[KnowledgeGraph] Found partial call: "${name}" (looking for: ${partialRelPath} or ${partialBasename})`
        );
        if (
          name === partialRelPath ||
          name === partialBasename ||
          partialRelPath.endsWith('/' + name) ||
          partialRelPath.endsWith(name)
        ) {
          return node;
        }
      }

      // Recurse into children
      if (node.children) {
        const found = this.findPartialCall(node.children, partialRelPath, partialBasename);
        if (found) return found;
      }
    }
    return undefined;
  }

  /**
   * Given the context arg passed to a partial (e.g. ".", ".User", ".User.Address"),
   * build the vars map that the partial will see as its root scope.
   */
  private resolvePartialVars(
    contextArg: string,
    vars: Map<string, TemplateVar>,
    scopeStack: { key: string; typeStr: string; fields?: { name: string; type: string; fields?: any[]; isSlice: boolean; defFile?: string; defLine?: number; defCol?: number; doc?: string }[] }[],
    ctx: TemplateContext
  ): Map<string, TemplateVar> {
    // "." → pass through all current vars + current dot scope
    if (contextArg === '.') {
      // If we're in a scoped block, expose the dot frame's fields as top-level vars
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame?.fields) {
        const result = new Map<string, TemplateVar>();
        for (const f of dotFrame.fields) {
          result.set(f.name, {
            name: f.name,
            type: f.type,
            fields: f.fields,
            isSlice: f.isSlice ?? false,
            defFile: f.defFile,
            defLine: f.defLine,
            defCol: f.defCol,
            doc: f.doc,
          });
        }
        return result;
      }
      // Root scope: pass through all vars (preserve all metadata)
      return new Map(vars);
    }

    // ".SomePath" → resolve that path and expose its fields
    const parser = new TemplateParser();
    const path = parser.parseDotPath(contextArg);
    const result = resolvePath(path, vars, scopeStack);

    if (!result.found || !result.fields) {
      return new Map();
    }

    const partialVars = new Map<string, TemplateVar>();
    for (const f of result.fields) {
      partialVars.set(f.name, {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
        defFile: f.defFile,
        defLine: f.defLine,
        defCol: f.defCol,
        doc: f.doc,
      });
    }
    return partialVars;
  }

  /**
   * Find the source variable that was passed to a partial.
   * For {{ template "partial" .User }}, returns the "User" TemplateVar.
   */
  private findPartialSourceVar(
    contextArg: string,
    vars: Map<string, TemplateVar>
  ): TemplateVar | undefined {
    // "." means all vars passed - no single source
    if (contextArg === '.' || contextArg === '') {
      return undefined;
    }

    // Parse path like ".User" or ".User.Address"
    const parser = new TemplateParser();
    const path = parser.parseDotPath(contextArg);
    if (path.length === 0) {
      return undefined;
    }

    // Find the top-level variable
    const topVar = vars.get(path[0]);
    if (!topVar) {
      return undefined;
    }

    // If it's just a single path component, return that var
    if (path.length === 1) {
      return topVar;
    }

    // Navigate through fields to find the nested type
    let currentVar = topVar;
    for (let i = 1; i < path.length; i++) {
      const field = currentVar.fields?.find(f => f.name === path[i]);
      if (!field) {
        return undefined;
      }
      // Create a synthetic TemplateVar from the field
      currentVar = {
        name: field.name,
        type: field.type,
        fields: field.fields,
        isSlice: field.isSlice ?? false,
        defFile: field.defFile,
        defLine: field.defLine,
        defCol: field.defCol,
        doc: field.doc,
      };
    }

    return currentVar;
  }

  /**
   * Resolve a Go source file path (relative to sourceDir) to an absolute path.
   * Used for go-to-definition.
   */
  resolveGoFilePath(relativeFile: string): string | null {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';

    const abs = path.join(this.workspaceRoot, sourceDir, relativeFile);
    return fs.existsSync(abs) ? abs : null;
  }

  /**
   * Resolve a template path (e.g., "views/partial.html") to an absolute file path.
   * Searches the graph and filesystem for the template.
   */
  resolveTemplatePath(templatePath: string): string | null {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    // 1. Check if it's already in the graph
    const ctx = this.graph.templates.get(templatePath);
    if (ctx?.absolutePath && fs.existsSync(ctx.absolutePath)) {
      return ctx.absolutePath;
    }

    // 2. Search by suffix match in the graph
    for (const [tplPath, tplCtx] of this.graph.templates) {
      if (tplPath.endsWith(templatePath) || templatePath.endsWith(tplPath)) {
        if (tplCtx.absolutePath && fs.existsSync(tplCtx.absolutePath)) {
          return tplCtx.absolutePath;
        }
      }
    }

    // 3. Search filesystem at common locations
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
