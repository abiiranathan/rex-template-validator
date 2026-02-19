import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import {
  AnalysisResult,
  KnowledgeGraph,
  RenderCall,
  TemplateContext,
  TemplateVar,
} from './types';

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
   * Resolve a Go source file path (relative to sourceDir) to an absolute path.
   * Used for go-to-definition.
   */
  resolveGoFilePath(relativeFile: string): string | null {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';

    const abs = path.join(this.workspaceRoot, sourceDir, relativeFile);
    return fs.existsSync(abs) ? abs : null;
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
