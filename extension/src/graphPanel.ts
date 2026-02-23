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
    const nodes: { id: string; label: string; group: string; level?: number }[] = [];
    const edges: { from: string; to: string; label?: string; arrows?: string }[] = [];

    for (const [tplPath, ctx] of graph.templates) {
      const tplId = `tpl:${tplPath}`;
      const tplName = tplPath.split('/').slice(-2).join('/');

      nodes.push({
        id: tplId,
        label: tplName,
        group: 'template',
        level: 1,
      });

      // Add Go source files
      for (const rc of ctx.renderCalls) {
        const srcId = `src:${rc.file}:${rc.line}`;
        const existingNode = nodes.find((n) => n.id === srcId);

        if (!existingNode) {
          const shortFile = rc.file.split('/').slice(-2).join('/');
          nodes.push({
            id: srcId,
            label: `${shortFile}:${rc.line}`,
            group: 'gofile',
            level: 0,
          });
        }

        edges.push({
          from: srcId,
          to: tplId,
          label: 'renders',
          arrows: 'to'
        });
      }

      // Add variables
      for (const [varName, v] of ctx.vars) {
        const varId = `var:${tplPath}:${varName}`;
        nodes.push({
          id: varId,
          label: `${varName}: ${this.shortType(v.type)}`,
          group: 'variable',
          level: 2,
        });

        edges.push({
          from: tplId,
          to: varId,
          arrows: 'to'
        });

        // Add fields (limit to prevent overcrowding)
        if (v.fields && v.fields.length > 0) {
          const fieldsToShow = v.fields.slice(0, 6);

          for (const f of fieldsToShow) {
            const fId = `field:${tplPath}:${varName}:${f.name}`;
            nodes.push({
              id: fId,
              label: `${f.name}: ${this.shortType(f.type)}`,
              group: 'field',
              level: 3,
            });

            edges.push({
              from: varId,
              to: fId,
              arrows: 'to'
            });
          }

          // Add indicator for more fields
          if (v.fields.length > 6) {
            const moreId = `more:${tplPath}:${varName}`;
            nodes.push({
              id: moreId,
              label: `+${v.fields.length - 6} more fields`,
              group: 'more',
              level: 3,
            });
            edges.push({
              from: varId,
              to: moreId,
              arrows: 'to'
            });
          }
        }
      }
    }

    const nodesJson = JSON.stringify(nodes);
    const edgesJson = JSON.stringify(edges);
    const analyzedAt = graph.analyzedAt.toLocaleTimeString();

    const stats = {
      templates: graph.templates.size,
      handlers: new Set(Array.from(graph.templates.values()).flatMap(ctx => ctx.renderCalls.map(rc => rc.file))).size,
      variables: Array.from(graph.templates.values()).reduce((sum, ctx) => sum + ctx.vars.size, 0),
    };

    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Knowledge Graph</title>
  <script src="https://unpkg.com/vis-network@9.1.2/dist/vis-network.min.js"></script>
  <style>
    * {
      margin: 0;
      padding: 0;
      box-sizing: border-box;
    }
    
    body {
      font-family: var(--vscode-font-family);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      height: 100vh;
      display: flex;
      flex-direction: column;
    }
    
    .header {
      padding: 16px 20px;
      background: var(--vscode-sideBar-background);
      border-bottom: 1px solid var(--vscode-panel-border);
      flex-shrink: 0;
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
      content: '⬡';
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
    
    .controls {
      display: flex;
      gap: 12px;
      flex-wrap: wrap;
      align-items: center;
    }
    
    .control-group {
      display: flex;
      gap: 8px;
      align-items: center;
    }
    
    .control-group label {
      font-size: 11px;
      color: var(--vscode-descriptionForeground);
      text-transform: uppercase;
      letter-spacing: 0.5px;
    }
    
    select, button {
      background: var(--vscode-dropdown-background);
      color: var(--vscode-dropdown-foreground);
      border: 1px solid var(--vscode-dropdown-border);
      padding: 4px 8px;
      font-size: 12px;
      border-radius: 2px;
      cursor: pointer;
    }
    
    button:hover {
      background: var(--vscode-button-hoverBackground);
    }
    
    #graph-container {
      flex: 1;
      position: relative;
      overflow: hidden;
    }
    
    #network {
      width: 100%;
      height: 100%;
    }
    
    .empty-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      height: 100%;
      color: var(--vscode-descriptionForeground);
      text-align: center;
      padding: 40px;
    }
    
    .empty-state-icon {
      font-size: 48px;
      margin-bottom: 16px;
      opacity: 0.5;
    }
    
    .empty-state h2 {
      font-size: 14px;
      font-weight: 600;
      margin-bottom: 8px;
    }
    
    .empty-state p {
      font-size: 12px;
      max-width: 300px;
    }
    
    .legend {
      position: absolute;
      bottom: 16px;
      right: 16px;
      background: var(--vscode-sideBar-background);
      border: 1px solid var(--vscode-panel-border);
      border-radius: 4px;
      padding: 12px;
      font-size: 11px;
      box-shadow: 0 2px 8px rgba(0,0,0,0.15);
    }
    
    .legend-title {
      font-weight: 600;
      margin-bottom: 8px;
      color: var(--vscode-foreground);
    }
    
    .legend-item {
      display: flex;
      align-items: center;
      gap: 8px;
      margin-bottom: 4px;
    }
    
    .legend-color {
      width: 16px;
      height: 16px;
      border-radius: 50%;
      flex-shrink: 0;
    }
    
    .info-panel {
      position: absolute;
      top: 16px;
      left: 16px;
      background: var(--vscode-sideBar-background);
      border: 1px solid var(--vscode-panel-border);
      border-radius: 4px;
      padding: 12px;
      max-width: 300px;
      display: none;
      box-shadow: 0 2px 8px rgba(0,0,0,0.15);
    }
    
    .info-panel.visible {
      display: block;
    }
    
    .info-panel h3 {
      font-size: 12px;
      font-weight: 600;
      margin-bottom: 8px;
    }
    
    .info-panel p {
      font-size: 11px;
      color: var(--vscode-descriptionForeground);
      margin-bottom: 4px;
    }
  </style>
</head>
<body>
  <div class="header">
    <h1>Rex Template Knowledge Graph</h1>
    
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
    
    <div class="controls">
      <div class="control-group">
        <label>Layout:</label>
        <select id="layout-select">
          <option value="hierarchical">Hierarchical</option>
          <option value="force">Force-directed</option>
          <option value="circular">Circular</option>
        </select>
      </div>
      
      <div class="control-group">
        <label>Direction:</label>
        <select id="direction-select">
          <option value="UD">Top → Down</option>
          <option value="LR">Left → Right</option>
          <option value="DU">Bottom → Up</option>
          <option value="RL">Right → Left</option>
        </select>
      </div>
      
      <button id="fit-btn">Fit to Screen</button>
      <button id="reset-btn">Reset View</button>
    </div>
  </div>
  
  <div id="graph-container">
    ${nodes.length === 0 ? `
      <div class="empty-state">
        <div class="empty-state-icon">🔍</div>
        <h2>No render calls found</h2>
        <p>Rebuild the index to analyze your templates and handler relationships.</p>
      </div>
    ` : `
      <div id="network"></div>
      
      <div id="info-panel" class="info-panel">
        <h3 id="info-title">Select a node</h3>
        <div id="info-content"></div>
      </div>
      
      <div class="legend">
        <div class="legend-title">Legend</div>
        <div class="legend-item">
          <div class="legend-color" style="background: #4CAF50;"></div>
          <span>Go Handler</span>
        </div>
        <div class="legend-item">
          <div class="legend-color" style="background: #2196F3;"></div>
          <span>Template</span>
        </div>
        <div class="legend-item">
          <div class="legend-color" style="background: #FF9800;"></div>
          <span>Variable</span>
        </div>
        <div class="legend-item">
          <div class="legend-color" style="background: #9C27B0;"></div>
          <span>Field</span>
        </div>
      </div>
    `}
  </div>

  <script>
    (function() {
      const nodes = ${nodesJson};
      const edges = ${edgesJson};
      
      if (nodes.length === 0) return;
      
      const container = document.getElementById('network');
      const infoPanel = document.getElementById('info-panel');
      const infoTitle = document.getElementById('info-title');
      const infoContent = document.getElementById('info-content');
      
      const colorMap = {
        gofile: '#4CAF50',
        template: '#2196F3',
        variable: '#FF9800',
        field: '#9C27B0',
        more: '#757575'
      };
      
      const shapeMap = {
        gofile: 'box',
        template: 'diamond',
        variable: 'ellipse',
        field: 'dot',
        more: 'text'
      };
      
      const nodesDataset = new vis.DataSet(
        nodes.map(n => ({
          id: n.id,
          label: n.label,
          group: n.group,
          level: n.level,
          color: {
            background: colorMap[n.group],
            border: colorMap[n.group],
            highlight: {
              background: colorMap[n.group],
              border: '#FFF'
            }
          },
          shape: shapeMap[n.group],
          font: {
            color: '#FFF',
            size: n.group === 'more' ? 10 : 12,
            face: 'monospace'
          },
          size: n.group === 'template' ? 20 : n.group === 'variable' ? 15 : 10
        }))
      );
      
      const edgesDataset = new vis.DataSet(
        edges.map(e => ({
          from: e.from,
          to: e.to,
          label: e.label,
          arrows: e.arrows || 'to',
          color: { color: '#666', highlight: '#FFF' },
          smooth: { type: 'cubicBezier' },
          font: { size: 10, color: '#999', strokeWidth: 0 }
        }))
      );
      
      let currentLayout = 'hierarchical';
      let currentDirection = 'UD';
      
      function getOptions() {
        const baseOptions = {
          nodes: {
            borderWidth: 2,
            borderWidthSelected: 3
          },
          edges: {
            width: 1.5,
            selectionWidth: 3
          },
          physics: {
            enabled: currentLayout !== 'hierarchical',
            stabilization: {
              iterations: 200
            }
          },
          interaction: {
            hover: true,
            navigationButtons: true,
            keyboard: true
          }
        };
        
        if (currentLayout === 'hierarchical') {
          baseOptions.layout = {
            hierarchical: {
              enabled: true,
              direction: currentDirection,
              sortMethod: 'directed',
              nodeSpacing: 150,
              levelSeparation: 200,
              treeSpacing: 200
            }
          };
        } else if (currentLayout === 'force') {
          baseOptions.layout = {
            randomSeed: 42
          };
          baseOptions.physics = {
            enabled: true,
            barnesHut: {
              gravitationalConstant: -2000,
              springConstant: 0.001,
              springLength: 200
            },
            stabilization: {
              iterations: 300
            }
          };
        } else if (currentLayout === 'circular') {
          baseOptions.layout = {
            randomSeed: undefined
          };
          baseOptions.physics = {
            enabled: false
          };
        }
        
        return baseOptions;
      }
      
      let network = new vis.Network(container, {
        nodes: nodesDataset,
        edges: edgesDataset
      }, getOptions());
      
      // Event handlers
      network.on('click', function(params) {
        if (params.nodes.length > 0) {
          const nodeId = params.nodes[0];
          const node = nodesDataset.get(nodeId);
          
          infoTitle.textContent = node.label;
          
          let content = '';
          content += '<p><strong>Type:</strong> ' + node.group + '</p>';
          
          const connected = network.getConnectedNodes(nodeId);
          if (connected.length > 0) {
            content += '<p><strong>Connected:</strong> ' + connected.length + ' nodes</p>';
          }
          
          infoContent.innerHTML = content;
          infoPanel.classList.add('visible');
        } else {
          infoPanel.classList.remove('visible');
        }
      });
      
      // Controls
      document.getElementById('layout-select').addEventListener('change', function(e) {
        currentLayout = e.target.value;
        network.setOptions(getOptions());
        network.fit();
      });
      
      document.getElementById('direction-select').addEventListener('change', function(e) {
        currentDirection = e.target.value;
        if (currentLayout === 'hierarchical') {
          network.setOptions(getOptions());
          network.fit();
        }
      });
      
      document.getElementById('fit-btn').addEventListener('click', function() {
        network.fit({ animation: true });
      });
      
      document.getElementById('reset-btn').addEventListener('click', function() {
        network.moveTo({
          position: {x: 0, y: 0},
          scale: 1,
          animation: true
        });
      });
      
      // Initial fit
      setTimeout(() => network.fit(), 100);
    })();
  </script>
</body>
</html>`;
  }
}
