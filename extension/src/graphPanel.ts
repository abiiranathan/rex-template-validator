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
    // "github.com/example/pkg.TypeName" -> "TypeName"
    // "*github.com/example/pkg.TypeName" -> "*TypeName"
    const prefix = typeStr.startsWith('*') ? '*' : '';
    const base = typeStr.replace(/^\*/, '');
    const parts = base.split('.');
    return prefix + parts[parts.length - 1];
  }

  private buildHTML(graph: KnowledgeGraph): string {
    const nodes: { id: string; label: string; group: string }[] = [];
    const edges: { from: string; to: string; label?: string }[] = [];

    for (const [tplPath, ctx] of graph.templates) {
      const tplId = `tpl:${tplPath}`;
      nodes.push({
        id: tplId,
        label: tplPath.split('/').slice(-2).join('/'),
        group: 'template',
      });

      for (const [varName, v] of ctx.vars) {
        const varId = `var:${tplPath}:${varName}`;
        nodes.push({
          id: varId,
          label: `${varName}\n(${this.shortType(v.type)})`,
          group: 'variable',
        });
        edges.push({ from: tplId, to: varId });

        // Show fields
        if (v.fields) {
          for (const f of v.fields.slice(0, 8)) {
            const fId = `field:${tplPath}:${varName}:${f.name}`;
            nodes.push({
              id: fId,
              label: `${f.name}\n(${this.shortType(f.type)})`,
              group: 'field',
            });
            edges.push({ from: varId, to: fId, label: '' });
          }
        }
      }

      // Go source -> template edges
      for (const rc of ctx.renderCalls) {
        const srcId = `src:${rc.file}:${rc.line}`;
        const existingNode = nodes.find((n) => n.id === srcId);
        if (!existingNode) {
          const shortFile = rc.file.split('/').slice(-2).join('/');
          nodes.push({
            id: srcId,
            label: `${shortFile}\nL${rc.line}`,
            group: 'gofile',
          });
        }
        edges.push({ from: srcId, to: tplId, label: 'Render()' });
      }
    }

    const nodesJson = JSON.stringify(nodes);
    const edgesJson = JSON.stringify(edges);
    const analyzedAt = graph.analyzedAt.toLocaleTimeString();

    return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Rex Template Knowledge Graph</title>
<style>
  :root {
    --bg: #0f1117;
    --surface: #1a1d27;
    --border: #2a2d3a;
    --accent: #6c63ff;
    --green: #00d9a3;
    --yellow: #ffd166;
    --red: #ef476f;
    --text: #e2e8f0;
    --muted: #64748b;
    --font: 'Fira Code', 'Cascadia Code', monospace;
  }

  * { box-sizing: border-box; margin: 0; padding: 0; }

  body {
    background: var(--bg);
    color: var(--text);
    font-family: var(--font);
    height: 100vh;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  header {
    padding: 12px 20px;
    background: var(--surface);
    border-bottom: 1px solid var(--border);
    display: flex;
    align-items: center;
    gap: 16px;
  }

  header h1 {
    font-size: 13px;
    font-weight: 600;
    color: var(--accent);
    letter-spacing: 0.05em;
    text-transform: uppercase;
  }

  .badge {
    font-size: 11px;
    padding: 2px 8px;
    border-radius: 4px;
    background: rgba(108,99,255,0.15);
    color: var(--accent);
    border: 1px solid rgba(108,99,255,0.3);
  }

  .time {
    margin-left: auto;
    font-size: 11px;
    color: var(--muted);
  }

  .legend {
    display: flex;
    gap: 16px;
    padding: 8px 20px;
    background: var(--surface);
    border-bottom: 1px solid var(--border);
    font-size: 11px;
  }

  .legend-item {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--muted);
  }

  .dot {
    width: 10px; height: 10px;
    border-radius: 50%;
  }

  .dot.template { background: var(--accent); }
  .dot.variable { background: var(--green); }
  .dot.field    { background: var(--yellow); }
  .dot.gofile   { background: #ff6b9d; }

  #graph-container {
    flex: 1;
    position: relative;
    overflow: hidden;
  }

  canvas {
    width: 100%;
    height: 100%;
  }
</style>
</head>
<body>
<header>
  <h1>⬡ Rex Template Knowledge Graph</h1>
  <span class="badge">${nodes.length} nodes · ${edges.length} edges</span>
  <span class="time">Analyzed at ${analyzedAt}</span>
</header>
<div class="legend">
  <div class="legend-item"><div class="dot gofile"></div> Go Handler</div>
  <div class="legend-item"><div class="dot template"></div> Template</div>
  <div class="legend-item"><div class="dot variable"></div> Variable</div>
  <div class="legend-item"><div class="dot field"></div> Field</div>
</div>
<div id="graph-container">
  ${nodes.length === 0 ? `
    <div class="no-data">
      <div class="icon">🔍</div>
      <div>No render calls found. Rebuild the index first.</div>
    </div>` : '<canvas id="canvas"></canvas>'}
  <div class="tooltip" id="tooltip"></div>
</div>

<script>
const RAW_NODES = ${nodesJson};
const RAW_EDGES = ${edgesJson};

function log(msg) {
    console.log(msg);
}

if (RAW_NODES.length > 0) {
  const canvas = document.getElementById('canvas');
  const tooltip = document.getElementById('tooltip');
  const container = document.getElementById('graph-container');
  const ctx = canvas.getContext('2d');

  const COLORS = {
    template: '#6c63ff',
    variable: '#00d9a3',
    field: '#ffd166',
    gofile: '#ff6b9d',
  };

  let width = 0, height = 0;
  let nodes = [], edges = [];
  let transform = { x: 0, y: 0, scale: 1 };
  let dragging = null, panning = false, lastMouse = null;
  let animFrame;

  function resize() {
    if (!container) return;
    width = canvas.width = container.clientWidth;
    height = canvas.height = container.clientHeight;
    log(\`Size: \${width}x\${height} | Nodes: \${nodes.length}\`);
    
    // Center the graph if offset
    transform.x = width / 2;
    transform.y = height / 2;
    // We render relative to center (0,0 is center)
  }

  function initNodes() {
    // Need dimensions to place initially
    if (width === 0 || height === 0) return;

    const angleStep = (2 * Math.PI) / Math.max(RAW_NODES.length, 1);
    const radius = Math.min(width, height) * 0.35;
    
    nodes = RAW_NODES.map((n, i) => ({
      ...n,
      // Initialize around (0,0) which we will center with transform
      x: radius * Math.cos(i * angleStep),
      y: radius * Math.sin(i * angleStep),
      vx: 0, vy: 0,
    }));
    
    edges = RAW_EDGES.map(e => ({
      ...e,
      source: nodes.find(n => n.id === e.from),
      target: nodes.find(n => n.id === e.to),
    })).filter(e => e.source && e.target);
    
    log(\`Initialized \${nodes.length} nodes\`);
  }

  // Force-directed layout
  function tick() {
    const repulse = 5000; // Stronger repulsion
    const attract = 0.05;

    // Repulsion
    for (let i = 0; i < nodes.length; i++) {
      for (let j = i + 1; j < nodes.length; j++) {
        const a = nodes[i], b = nodes[j];
        let dx = b.x - a.x;
        let dy = b.y - a.y;
        let dist = Math.sqrt(dx*dx + dy*dy);
        
        // Jitter if overlapping
        if (dist < 0.1) {
            dx = (Math.random() - 0.5);
            dy = (Math.random() - 0.5);
            dist = Math.sqrt(dx*dx + dy*dy) || 1;
        }
        
        const force = repulse / (dist * dist);
        a.vx -= force * dx / dist;
        a.vy -= force * dy / dist;
        b.vx += force * dx / dist;
        b.vy += force * dy / dist;
      }
    }

    // Attraction along edges
    for (const e of edges) {
      if (!e.source || !e.target) continue;
      const dx = e.target.x - e.source.x;
      const dy = e.target.y - e.source.y;
      const dist = Math.sqrt(dx*dx + dy*dy) || 1;
      const force = attract * (dist - 100);
      e.source.vx += force * dx / dist;
      e.source.vy += force * dy / dist;
      e.target.vx -= force * dx / dist;
      e.target.vy -= force * dy / dist;
    }
    
    // Center gravity (pull to 0,0)
    for (const n of nodes) {
        const dist = Math.sqrt(n.x*n.x + n.y*n.y) || 1;
        const force = 0.01 * dist; // Weak pull to center
        n.vx -= force * n.x / dist;
        n.vy -= force * n.y / dist;
    }

    // Damping + integrate
    for (const n of nodes) {
      n.vx *= 0.85; n.vy *= 0.85;
      n.x += n.vx; n.y += n.vy;
    }
  }

  function draw() {
    try {
        ctx.clearRect(0, 0, width, height);
        
        // Debug border
        ctx.strokeStyle = '#333';
        ctx.lineWidth = 1;
        ctx.strokeRect(0, 0, width, height);

        ctx.save();
        // Move origin to center + transform offset
        ctx.translate(transform.x, transform.y);
        ctx.scale(transform.scale, transform.scale);

        // Edges
        ctx.strokeStyle = 'rgba(100,116,139,0.35)';
        ctx.lineWidth = 1;
        for (const e of edges) {
          if (!e.source || !e.target) continue;
          ctx.beginPath();
          ctx.moveTo(e.source.x, e.source.y);
          ctx.lineTo(e.target.x, e.target.y);
          ctx.stroke();
        }

        // Nodes
        for (const n of nodes) {
          const r = n.group === 'template' ? 28 : n.group === 'gofile' ? 22 : 18;
          const color = COLORS[n.group] || '#888';

          // Glow
          const grd = ctx.createRadialGradient(n.x, n.y, 0, n.x, n.y, r + 8);
          grd.addColorStop(0, color + '33');
          grd.addColorStop(1, 'transparent');
          ctx.beginPath();
          ctx.arc(n.x, n.y, r + 8, 0, Math.PI * 2);
          ctx.fillStyle = grd;
          ctx.fill();

          // Circle
          ctx.beginPath();
          ctx.arc(n.x, n.y, r, 0, Math.PI * 2);
          ctx.fillStyle = '#1a1d27';
          ctx.fill();
          ctx.strokeStyle = color;
          ctx.lineWidth = 2;
          ctx.stroke();

          // Label
          ctx.fillStyle = color;
          ctx.font = '9px Fira Code, monospace';
          ctx.textAlign = 'center';
          ctx.textBaseline = 'middle';
          const lines = n.label.split('\\n');
          lines.forEach((line, i) => {
            ctx.fillText(line, n.x, n.y + (i - (lines.length-1)/2) * 11);
          });
        }

        ctx.restore();
    } catch(e) {
        log('Draw error: ' + e);
    }
  }

  function loop() {
    tick();
    draw();
    animFrame = requestAnimationFrame(loop);
  }

  // Mouse interaction
  function screenToWorld(x, y) {
    return {
      x: (x - transform.x) / transform.scale,
      y: (y - transform.y) / transform.scale,
    };
  }

  function nodeAt(wx, wy) {
    for (const n of nodes) {
      const r = n.group === 'template' ? 28 : 20;
      const dx = n.x - wx, dy = n.y - wy;
      if (dx*dx + dy*dy <= r*r) return n;
    }
    return null;
  }

  canvas.addEventListener('mousedown', e => {
    const w = screenToWorld(e.offsetX, e.offsetY);
    const n = nodeAt(w.x, w.y);
    if (n) { dragging = n; }
    else { panning = true; lastMouse = { x: e.offsetX, y: e.offsetY }; }
  });

  canvas.addEventListener('mousemove', e => {
    if (dragging) {
      const w = screenToWorld(e.offsetX, e.offsetY);
      dragging.x = w.x; dragging.y = w.y;
      dragging.vx = 0; dragging.vy = 0;
    } else if (panning && lastMouse) {
      transform.x += e.offsetX - lastMouse.x;
      transform.y += e.offsetY - lastMouse.y;
      lastMouse = { x: e.offsetX, y: e.offsetY };
    } else {
      const w = screenToWorld(e.offsetX, e.offsetY);
      const n = nodeAt(w.x, w.y);
      if (n) {
        tooltip.style.display = 'block';
        tooltip.style.left = (e.offsetX + 12) + 'px';
        tooltip.style.top = (e.offsetY - 8) + 'px';
        tooltip.textContent = n.id;
      } else {
        tooltip.style.display = 'none';
      }
    }
  });

  canvas.addEventListener('mouseup', () => { dragging = null; panning = false; lastMouse = null; });
  canvas.addEventListener('mouseleave', () => { dragging = null; panning = false; tooltip.style.display = 'none'; });

  canvas.addEventListener('wheel', e => {
    e.preventDefault();
    const factor = e.deltaY > 0 ? 0.9 : 1.1;
    // Zoom towards cursor (cx, cy)
    // transform.x and y are translation.
    // screenX = worldX * scale + transX
    // newScale = scale * factor
    // transX' = cx - (cx - transX) * factor
    const cx = e.offsetX, cy = e.offsetY;
    transform.x = cx - factor * (cx - transform.x);
    transform.y = cy - factor * (cy - transform.y);
    transform.scale *= factor;
  }, { passive: false });

  window.addEventListener('resize', () => {
      resize();
      // If we haven't initialized nodes yet (because width was 0), do it now
      if (nodes.length === 0 && width > 0) {
          initNodes();
      }
  });
  
  if (window.ResizeObserver) {
    const ro = new ResizeObserver(() => {
        resize();
        if (nodes.length === 0 && width > 0) {
             initNodes();
        }
    });
    ro.observe(container);
  }

  resize();
  initNodes();
  loop();
}
</script>
</body>
</html>`;
  }
}
