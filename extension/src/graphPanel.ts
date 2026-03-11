import * as vscode from 'vscode';
import { KnowledgeGraph } from './types';

export class KnowledgeGraphPanel {
  private static currentPanel: KnowledgeGraphPanel | undefined;
  private readonly panel: vscode.WebviewPanel;
  private disposables: vscode.Disposable[] = [];

  static show(context: vscode.ExtensionContext, graph: KnowledgeGraph) {
    if (KnowledgeGraphPanel.currentPanel) {
      KnowledgeGraphPanel.currentPanel.panel.reveal();
      KnowledgeGraphPanel.currentPanel.update(graph);
      return;
    }

    const panel = vscode.window.createWebviewPanel(
      'rexKnowledgeGraph',
      'Rex Template Knowledge Graph',
      vscode.ViewColumn.Beside,
      { enableScripts: true }
    );

    KnowledgeGraphPanel.currentPanel = new KnowledgeGraphPanel(panel, graph, context);
  }

  private constructor(
    panel: vscode.WebviewPanel,
    graph: KnowledgeGraph,
    context: vscode.ExtensionContext
  ) {
    this.panel = panel;
    this.panel.webview.html = this.buildHTML(graph);

    this.panel.onDidDispose(
      () => {
        KnowledgeGraphPanel.currentPanel = undefined;
        this.dispose();
      },
      null,
      this.disposables
    );
  }

  update(graph: KnowledgeGraph) {
    this.panel.webview.html = this.buildHTML(graph);
  }

  private dispose() {
    this.disposables.forEach((d) => d.dispose());
  }

  private shortType(typeStr: string): string {
    const prefix = typeStr.startsWith('*') ? '*' : '';
    const base = typeStr.replace(/^\*/, '');
    const parts = base.split('.');
    return prefix + parts[parts.length - 1];
  }

  private buildHTML(graph: KnowledgeGraph): string {
    const analyzedAt = graph.analyzedAt.toLocaleTimeString();
    const stats = {
      templates: graph.templates.size,
      handlers: new Set(Array.from(graph.templates.values()).flatMap(ctx => ctx.renderCalls.map(rc => rc.file))).size,
      variables: Array.from(graph.templates.values()).reduce((sum, ctx) => sum + ctx.vars.size, 0),
    };

    // Helper to render variable fields recursively
    function renderFields(fields: any[] | undefined, depth = 0): string {
      if (!fields || fields.length === 0) return '';
      return `<details style="margin-left:${depth * 16}px;">\n` +
        `<summary>Fields (${fields.length})</summary>` +
        `<ul style="margin:0; padding-left:16px;">` +
        fields.map(f => `<li><b>${f.name}</b>: <span>${f.type}</span>${renderFields(f.fields, depth + 1)}</li>`).join('') +
        `</ul></details>`;
    }

    // Helper to render variables
    function renderVars(vars: Map<string, any>): string {
      if (!vars || vars.size === 0) return '<em>No variables</em>';
      return `<table class="vars-table">\n        <thead><tr><th>Name</th><th>Type</th><th>Fields</th></tr></thead>\n        <tbody>\n        ${Array.from(vars.values()).map(v => `\n          <tr>\n            <td>${v.name}</td>\n            <td>${v.type}</td>\n            <td>${v.fields && v.fields.length > 0 ? renderFields(v.fields) : '<em>None</em>'}</td>\n          </tr>\n        `).join('')}\n        </tbody>\n      </table>`;
    }

    // Helper to render renderCalls
    function renderRenderCalls(renderCalls: any[]): string {
      if (!renderCalls || renderCalls.length === 0) return '<em>No render calls</em>';
      return `<ul>
        ${renderCalls.map(rc => `<li><b>${rc.file}:${rc.line}</b> (vars: ${rc.vars.map((v: any) => v.name).join(', ')})</li>`).join('')}
      </ul>`;
    }

    // Main HTML
    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Templates Table</title>
  <style>
    body {
      font-family: var(--vscode-font-family);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      margin: 0;
      padding: 0;
    }
    .header {
      padding: 16px 20px;
      background: var(--vscode-sideBar-background);
      border-bottom: 1px solid var(--vscode-panel-border);
    }
    .header h1 {
      font-size: 16px;
      font-weight: 600;
      margin-bottom: 8px;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .header h1::before {
      content: '📄';
      color: var(--vscode-textLink-foreground);
      font-size: 20px;
    }
    .stats {
      display: flex;
      gap: 20px;
      font-size: 12px;
      color: var(--vscode-descriptionForeground);
      margin-bottom: 8px;
    }
    .stat-item {
      display: flex;
      align-items: center;
      gap: 6px;
    }
    .stat-value {
      font-weight: 600;
      color: var(--vscode-foreground);
    }
    .templates-table {
      width: 100%;
      border-collapse: collapse;
      margin: 24px 0;
    }
    .templates-table th, .templates-table td {
      border: 1px solid var(--vscode-panel-border);
      padding: 8px 12px;
      text-align: left;
      vertical-align: top;
    }
    .templates-table th {
      background: var(--vscode-sideBar-background);
    }
    details {
      margin-bottom: 8px;
      background: var(--vscode-editorWidget-background);
      border-radius: 4px;
      padding: 4px 8px;
    }
    summary {
      font-weight: 600;
      cursor: pointer;
    }
    .vars-table {
      width: 100%;
      border-collapse: collapse;
      margin: 8px 0;
    }
    .vars-table th, .vars-table td {
      border: 1px solid var(--vscode-panel-border);
      padding: 4px 8px;
      text-align: left;
      vertical-align: top;
      font-size: 12px;
    }
    .vars-table th {
      background: var(--vscode-sideBar-background);
    }
  </style>
</head>
<body>
  <div class="header">
    <h1>Rex Templates</h1>
    <div class="stats">
      <div class="stat-item">
        <span class="stat-value">${stats.templates}</span>
        <span>templates</span>
      </div>
      <div class="stat-item">
        <span class="stat-value">${stats.handlers}</span>
        <span>handlers</span>
      </div>
      <div class="stat-item">
        <span class="stat-value">${stats.variables}</span>
        <span>variables</span>
      </div>
      <div class="stat-item">
        <span>•</span>
        <span>Updated ${analyzedAt}</span>
      </div>
    </div>
  </div>
  <div style="padding: 20px;">
    <table class="templates-table">
      <thead>
        <tr>
          <th>Template</th>
          <th>Context</th>
        </tr>
      </thead>
      <tbody>
        ${Array.from(graph.templates.values()).map(ctx => `
          <tr>
            <td><b>${ctx.templatePath}</b></td>
            <td>
              <details>
                <summary>Show Context</summary>
                <div><b>Variables:</b>${renderVars(ctx.vars)}</div>
                <div style="margin-top:8px;"><b>Render Calls:</b>${renderRenderCalls(ctx.renderCalls)}</div>
              </details>
            </td>
          </tr>
        `).join('')}
      </tbody>
    </table>
  </div>
</body>
</html>`;
  }
}
