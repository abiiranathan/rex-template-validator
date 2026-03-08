/**
 * extension.ts — Main VS Code extension entry point.
 *
 * Architecture change (LSP migration):
 *   OLD: activate() → GoAnalyzer.analyzeWorkspace() (spawns rex-analyzer once,
 *        loads full graph from stdout JSON) → KnowledgeGraphBuilder.build()
 *
 *   NEW: activate() → LspClient.start() (keeps rex-analyzer --lsp running) →
 *        KnowledgeGraphBuilder.initialize() (fetches funcMaps + namedBlocks via LSP) →
 *        per-file context and validation are fetched on-demand via rex/* methods.
 *
 * Three diagnostic collections remain unchanged:
 *   analyzerCollection  — Go-side per-template validation (rex/validate results)
 *   editorCollection    — TypeScript-side in-editor validation
 *   namedBlockCollection — duplicate named-block errors (cross-file)
 */

import * as vscode from 'vscode';
import * as path from 'path';
import * as fs from 'fs';
import { LspClient } from './lspClient';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { NamedBlockDuplicateError } from './types';

// ── Document selector ──────────────────────────────────────────────────────────

const TEMPLATE_SELECTOR: vscode.DocumentSelector = [
  { language: 'html', scheme: 'file' },
  { language: 'go-template', scheme: 'file' },
  { pattern: '**/*.tmpl' },
  { pattern: '**/*.html' },
];

const GO_SELECTOR: vscode.DocumentSelector = [{ language: 'go', scheme: 'file' }];

// ── Module-level state ─────────────────────────────────────────────────────────

let lspClient: LspClient | undefined;
let graphBuilder: KnowledgeGraphBuilder | undefined;
let validator: TemplateValidator | undefined;
let outputChannel: vscode.OutputChannel;
let statusBarItem: vscode.StatusBarItem;

let analyzerCollection: vscode.DiagnosticCollection;
let editorCollection: vscode.DiagnosticCollection;
let namedBlockCollection: vscode.DiagnosticCollection;

let rebuildTimer: NodeJS.Timeout | undefined;
let validateAllTimer: NodeJS.Timeout | undefined;

// ── Activation ─────────────────────────────────────────────────────────────────

export async function activate(context: vscode.ExtensionContext) {
  statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left);
  outputChannel = vscode.window.createOutputChannel('Rex Template Validator');
  analyzerCollection = vscode.languages.createDiagnosticCollection('rex-analyzer');
  editorCollection = vscode.languages.createDiagnosticCollection('rex-editor');
  namedBlockCollection = vscode.languages.createDiagnosticCollection('rex-named-blocks');

  context.subscriptions.push(
    statusBarItem, outputChannel,
    analyzerCollection, editorCollection, namedBlockCollection
  );
  outputChannel.appendLine('[Rex] Extension activated');

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[Rex] No workspace folder found');
    return;
  }

  // ── LSP client + graph builder ───────────────────────────────────────────────

  const binaryPath = resolveBinaryPath(context);
  lspClient = new LspClient(binaryPath, outputChannel);
  graphBuilder = new KnowledgeGraphBuilder(workspaceRoot, outputChannel, lspClient);
  validator = new TemplateValidator(outputChannel, graphBuilder);

  // ── Commands ─────────────────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.commands.registerCommand('rexAnalyzer.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc && isTemplate(doc)) await validateDocument(doc);
    }),

    vscode.commands.registerCommand('rexAnalyzer.rebuildIndex', async () => {
      await rebuildIndex(workspaceRoot);
    }),

    vscode.commands.registerCommand('rexAnalyzer.showKnowledgeGraph', () => {
      const graph = graphBuilder?.getGraph();
      if (graph && graph.templates.size > 0) {
        KnowledgeGraphPanel.show(context, graph);
      } else {
        vscode.window.showInformationMessage(
          'No template index yet. Run "Rex: Rebuild Template Index" first.'
        );
      }
    })
  );

  // ── Language feature providers ────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.languages.registerHoverProvider(TEMPLATE_SELECTOR, {
      async provideHover(document, position) {
        const ctx = await resolveContext(document.uri.fsPath);
        if (!ctx) return;
        return validator?.getHoverInfo(document, position, ctx);
      },
    }),

    vscode.languages.registerCompletionItemProvider(
      TEMPLATE_SELECTOR,
      {
        async provideCompletionItems(document, position) {
          const ctx = await resolveContext(document.uri.fsPath);
          if (!ctx) return [];
          return validator?.getCompletionItems(document, position, ctx) ?? [];
        },
      },
      '.', '$', '"'
    ),

    vscode.languages.registerDefinitionProvider(TEMPLATE_SELECTOR, {
      async provideDefinition(document, position) {
        const ctx = await resolveContext(document.uri.fsPath);
        if (!ctx) return;
        return validator?.getDefinitionLocation(document, position, ctx);
      },
    }),

    vscode.languages.registerDefinitionProvider(GO_SELECTOR, {
      provideDefinition(document, position) {
        return validator?.getTemplateDefinitionFromGo(document, position);
      },
    }),

    vscode.languages.registerReferenceProvider(TEMPLATE_SELECTOR, {
      async provideReferences(document, position, refCtx) {
        return validator?.getReferences(document, position, refCtx.includeDeclaration) ?? [];
      },
    })
  );

  // ── Workspace watchers ────────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((doc) => {
      if (isTemplate(doc)) scheduleValidateAllOpenTemplates();
    }),

    vscode.workspace.onDidChangeTextDocument((e) => {
      const doc = e.document;
      if (isTemplate(doc)) {
        graphBuilder?.updateTemplateFile(doc.uri.fsPath, doc.getText());
        applyNamedBlockDiagnostics();
        scheduleValidateAllOpenTemplates();
      }
    }),

    vscode.workspace.onDidSaveTextDocument((doc) => {
      const name = doc.fileName;
      if (name.endsWith('.go') || name.endsWith('go.mod') || name.endsWith('.json')) {
        // Tell the daemon about the change so it invalidates its cache.
        lspClient?.notifyFileChanges([doc.uri.fsPath]);
        scheduleRebuild(workspaceRoot);
      }
    }),

    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('rex-analyzer')) {
        outputChannel.appendLine('[Rex] Configuration changed, rebuilding index...');
        scheduleRebuild(workspaceRoot);
      }
    })
  );

  // ── Initial build ─────────────────────────────────────────────────────────────

  await rebuildIndex(workspaceRoot);
  outputChannel.appendLine('[Rex] Ready');
}

// ── Deactivation ──────────────────────────────────────────────────────────────

export function deactivate() {
  if (rebuildTimer) clearTimeout(rebuildTimer);
  if (validateAllTimer) clearTimeout(validateAllTimer);
  lspClient?.dispose();
  analyzerCollection?.dispose();
  editorCollection?.dispose();
  namedBlockCollection?.dispose();
  outputChannel?.dispose();
}

// ── Rebuild (start/restart daemon + re-fetch metadata) ────────────────────────

async function rebuildIndex(workspaceRoot: string) {
  if (!lspClient || !graphBuilder) return;

  statusBarItem.text = '$(sync~spin) Rex: Analyzing...';
  statusBarItem.show();

  try {
    // (Re-)start the LSP daemon if it isn't running yet.
    if (!lspClient.started) {
      await lspClient.start();
    }

    // Fetch funcMaps + namedBlocks eagerly; everything else is on-demand.
    await graphBuilder.initialize();

    const graph = graphBuilder.getGraph();
    const count = graph.namedBlocks.size + graph.funcMaps.size;

    statusBarItem.text = `$(check) Rex: ${graph.funcMaps.size} func(s), ${graph.namedBlocks.size} block(s) indexed`;

    applyNamedBlockDiagnostics();
    await validateAllKnownTemplates();
  } catch (err) {
    outputChannel.appendLine(`[Rex] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) Rex: Analysis failed';
  } finally {
    setTimeout(() => statusBarItem.hide(), 5000);
  }
}

// ── Per-document validation ───────────────────────────────────────────────────

/**
 * Validates a single template document:
 *   1. TypeScript-side structural validation  → editorCollection
 *   2. Go-side semantic validation (rex/validate) → analyzerCollection
 */
async function validateDocument(doc: vscode.TextDocument) {
  if (!validator || !graphBuilder || !lspClient?.started) return;

  const ctx = await resolveContext(doc.uri.fsPath);

  if (!ctx) {
    editorCollection.delete(doc.uri);
    return;
  }

  // TypeScript-side diagnostics.
  const tsDiags = await validator.validateDocument(doc, ctx);
  editorCollection.set(doc.uri, tsDiags);

  // Go-side diagnostics via rex/validate.
  await applyGoValidationDiagnostics(doc.uri.fsPath, ctx.templatePath);
}

/**
 * Calls rex/validate for a single template file and pushes the results into
 * analyzerCollection.
 */
async function applyGoValidationDiagnostics(absolutePath: string, templateName: string) {
  if (!lspClient?.started || !graphBuilder) return;

  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const validationEnabled: boolean = config.get('validate') ?? true;
  if (!validationEnabled) return;

  try {
    const result = await lspClient.validate({
      dir: graphBuilder.getSourceDir(),
      templateName,
      templateRoot: (config.get<string>('templateRoot') || undefined),
      contextFile: resolveContextFile() || undefined,
    });

    const diagnostics: vscode.Diagnostic[] = [];
    const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? '';
    const sourceDir = config.get<string>('sourceDir') ?? '.';

    for (const err of result.errors ?? []) {
      const line = Math.max(0, err.line - 1);
      const col = Math.max(0, err.column - 1);
      const endCol = col + (err.variable?.length ?? 1);

      const diag = new vscode.Diagnostic(
        new vscode.Range(line, col, line, endCol),
        err.message,
        err.severity === 'warning'
          ? vscode.DiagnosticSeverity.Warning
          : vscode.DiagnosticSeverity.Error
      );
      diag.source = 'Rex (Go)';

      if (err.goFile) {
        const goFileAbs = path.join(path.resolve(workspaceRoot, sourceDir), err.goFile);
        diag.relatedInformation = [
          new vscode.DiagnosticRelatedInformation(
            new vscode.Location(
              vscode.Uri.file(goFileAbs),
              new vscode.Position(Math.max(0, (err.goLine ?? 1) - 1), 0)
            ),
            'Variable passed from here'
          ),
        ];
      }

      diagnostics.push(diag);
    }

    // Merge into analyzerCollection (keyed by absolute file path).
    const fileUri = vscode.Uri.file(absolutePath);
    const existing = analyzerCollection.get(fileUri) ?? [];
    // Keep any diagnostics for *other* templates; replace only for this file.
    analyzerCollection.set(fileUri, diagnostics);
  } catch (e) {
    outputChannel.appendLine(`[Rex] rex/validate failed for ${templateName}: ${e}`);
  }
}

// ── Context resolution helper ─────────────────────────────────────────────────

async function resolveContext(absolutePath: string) {
  if (!graphBuilder) return undefined;

  let ctx = await graphBuilder.findContextForFile(absolutePath);
  if (!ctx || ctx.renderCalls.length === 0) {
    const partialCtx = await graphBuilder.findContextForFileAsPartial(absolutePath);
    if (partialCtx) ctx = partialCtx;
  }
  return ctx || undefined;
}

// ── Named-block duplicate diagnostics ────────────────────────────────────────

function applyNamedBlockDiagnostics() {
  if (!graphBuilder) return;
  namedBlockCollection.clear();

  const duplicateErrors: NamedBlockDuplicateError[] = graphBuilder.getAllDuplicateErrors();
  if (duplicateErrors.length === 0) return;

  const issuesByFile = new Map<string, vscode.Diagnostic[]>();

  for (const err of duplicateErrors) {
    for (const entry of err.entries) {
      const locs = err.entries
        .filter(e => e.absolutePath !== entry.absolutePath || e.line !== entry.line)
        .map(e => `${e.templatePath}:${e.line}`)
        .join(', ');

      const msg =
        `Duplicate named block "${err.name}". ` +
        `Also declared at: ${locs}. Only one declaration is allowed project-wide.`;

      const range = new vscode.Range(
        Math.max(0, entry.line - 1), Math.max(0, entry.col - 1),
        Math.max(0, entry.line - 1), Math.max(0, entry.col - 1) + entry.name.length + 2
      );

      const diag = new vscode.Diagnostic(range, msg, vscode.DiagnosticSeverity.Error);
      diag.source = 'Rex';
      diag.code = 'duplicate-named-block';
      diag.relatedInformation = err.entries
        .filter(e => e !== entry)
        .map(e => new vscode.DiagnosticRelatedInformation(
          new vscode.Location(
            vscode.Uri.file(e.absolutePath),
            new vscode.Position(Math.max(0, e.line - 1), Math.max(0, e.col - 1))
          ),
          `Also declared here as "${e.name}"`
        ));

      const list = issuesByFile.get(entry.absolutePath) ?? [];
      list.push(diag);
      issuesByFile.set(entry.absolutePath, list);
    }
  }

  for (const [filePath, issues] of issuesByFile) {
    namedBlockCollection.set(vscode.Uri.file(filePath), issues);
  }

  outputChannel.appendLine(
    `[Rex] Applied ${duplicateErrors.length} duplicate named-block diagnostic(s)`
  );
}

// ── Validate all known templates ──────────────────────────────────────────────

async function validateAllKnownTemplates() {
  if (!validator || !graphBuilder) return;

  // Collect template absolute paths from namedBlocks (since we no longer have a
  // large templates map pre-populated; context is fetched on demand).
  const allPaths = new Set<string>();
  const graph = graphBuilder.getGraph();
  for (const [, entries] of graph.namedBlocks) {
    for (const e of entries) {
      if (e.absolutePath && fs.existsSync(e.absolutePath)) {
        allPaths.add(e.absolutePath);
      }
    }
  }
  // Also validate any currently-open template documents.
  for (const doc of vscode.workspace.textDocuments) {
    if (isTemplate(doc)) allPaths.add(doc.uri.fsPath);
  }

  outputChannel.appendLine(`[Rex] Validating ${allPaths.size} template(s)...`);

  for (const filePath of allPaths) {
    try {
      const doc =
        vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath) ??
        await vscode.workspace.openTextDocument(filePath);
      await validateDocument(doc);
    } catch { /* file may have been deleted */ }
  }
}

// ── Debounce helpers ──────────────────────────────────────────────────────────

function scheduleRebuild(workspaceRoot: string) {
  const debounceMs = vscode.workspace.getConfiguration('rex-analyzer').get<number>('debounceMs') ?? 1000;
  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(async () => {
    // Invalidate graph cache (tells daemon to clear its cache too, via the LSP
    // workspace/didChangeWatchedFiles notification already sent in the watcher).
    await graphBuilder?.invalidateGraphCache();
    await validateAllKnownTemplates();
    applyNamedBlockDiagnostics();
  }, debounceMs);
}

function scheduleValidateAllOpenTemplates() {
  const debounceMs = vscode.workspace.getConfiguration('rex-analyzer').get<number>('debounceMs') ?? 1000;
  if (validateAllTimer) clearTimeout(validateAllTimer);
  validateAllTimer = setTimeout(() => validateAllKnownTemplates(), debounceMs);
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function isTemplate(doc: vscode.TextDocument): boolean {
  return (
    doc.uri.scheme === 'file' &&
    (doc.fileName.endsWith('.html') || doc.fileName.endsWith('.tmpl'))
  );
}

function resolveBinaryPath(context: vscode.ExtensionContext): string {
  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const configPath = config.get<string>('goAnalyzerPath');
  if (configPath && fs.existsSync(configPath)) return configPath;

  const ext = process.platform === 'win32' ? '.exe' : '';
  const bundled = path.join(context.extensionPath, 'out', `rex-analyzer${ext}`);
  if (fs.existsSync(bundled)) return bundled;

  return 'rex-analyzer';
}

function resolveContextFile(): string {
  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? '';
  const contextFile = vscode.workspace.getConfiguration('rex-analyzer').get<string>('contextFile') ?? '';
  if (!contextFile) return '';
  const abs = path.resolve(workspaceRoot, contextFile);
  return fs.existsSync(abs) ? abs : '';
}
