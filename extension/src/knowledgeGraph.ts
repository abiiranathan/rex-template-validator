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

/**
 * Builds and maintains a knowledge graph mapping template paths to their
 * available variables (from Go render calls).
 */
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

    for (const rc of analysisResult.renderCalls ?? []) {
      const resolved = this.resolveTemplatePath(rc);
      if (!resolved) continue;

      let ctx = templates.get(resolved);
      if (!ctx) {
        ctx = {
          templatePath: resolved,
          vars: new Map(),
          renderCalls: [],
        };
        templates.set(resolved, ctx);
      }

      ctx.renderCalls.push(rc);

      // Merge vars (last one wins for type info)
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
        `  ${tpl}: ${[...ctx.vars.keys()].join(', ')} (from ${ctx.renderCalls.length} call(s))`
      );
    }

    return this.graph;
  }

  getGraph(): KnowledgeGraph {
    return this.graph;
  }

  /**
   * Find the template context for a given absolute file path.
   */
  findContextForFile(absolutePath: string): TemplateContext | undefined {
    // Try to match relative paths
    const rel = path.relative(this.workspaceRoot, absolutePath).replace(/\\/g, '/');

    // Direct match
    if (this.graph.templates.has(rel)) {
      return this.graph.templates.get(rel);
    }

    // Suffix match (template path may be partial)
    for (const [tplPath, ctx] of this.graph.templates) {
      if (rel.endsWith(tplPath) || tplPath.endsWith(rel)) {
        return ctx;
      }
      // Match on filename portion
      if (path.basename(rel) === path.basename(tplPath)) {
        return ctx;
      }
    }

    return undefined;
  }

  /**
   * Try to find a partial template in the workspace.
   */
  findPartialContext(partialName: string, currentFile: string): TemplateContext | undefined {
    // partialName might be relative or just a basename
    // Try all template contexts
    for (const [tplPath, ctx] of this.graph.templates) {
      if (
        tplPath.endsWith(partialName) ||
        path.basename(tplPath) === partialName ||
        path.basename(tplPath, '.html') === partialName
      ) {
        return ctx;
      }
    }

    // Also try to find the file in the workspace even without a render call
    const dir = path.dirname(currentFile);
    const candidates = [
      path.join(dir, partialName),
      path.join(this.workspaceRoot, partialName),
    ];
    for (const c of candidates) {
      if (fs.existsSync(c)) {
        return {
          templatePath: c,
          vars: new Map(),
          renderCalls: [],
        };
      }
    }

    return undefined;
  }

  /**
   * Convert render call template path to a workspace-relative path.
   */
  private resolveTemplatePath(rc: RenderCall): string | null {
    let tplPath = rc.template;

    // Handle glob/pattern (e.g. "views/*.html") - just use as-is
    // Remove leading "./"
    tplPath = tplPath.replace(/^\.\//, '');

    return tplPath;
  }

  toJSON(): object {
    const obj: Record<string, unknown> = {};
    for (const [key, ctx] of this.graph.templates) {
      obj[key] = {
        vars: Object.fromEntries(
          [...ctx.vars.entries()].map(([k, v]) => [k, { type: v.type, fields: v.fields }])
        ),
        renderCalls: ctx.renderCalls.map((r) => ({
          file: r.file,
          line: r.line,
        })),
      };
    }
    return obj;
  }
}
