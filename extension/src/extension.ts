import * as vscode from 'vscode';
import * as path from 'path';
import { GoAnalyzer } from './analyzer';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { AnalysisResult, KnowledgeGraph, GoValidationError } from './types';
import * as fs from 'fs';

const TEMPLATE_SELECTOR: vscode.DocumentSelector = [
  { language: 'html', scheme: 'file' },
  { language: 'go-template', scheme: 'file' },
  { pattern: '**/*.tmpl' },
  { pattern: '**/*.html' },
];

// Two separate collections so they never interfere with each other:
// - analyzerCollection: diagnostics from the Go binary (persists across template edits)
// - editorCollection:   diagnostics from the in-editor TypeScript validator (per-document)
let analyzerCollection: vscode.DiagnosticCollection;
let editorCollection: vscode.DiagnosticCollection;
let outputChannel: vscode.OutputChannel;
let graphBuilder: KnowledgeGraphBuilder | undefined;
let validator: TemplateValidator | undefined;
let currentGraph: KnowledgeGraph | undefined;
let analyzer: GoAnalyzer | undefined;

// Separate timers for rebuild (Go changes) vs validate (template changes)
// so a template edit doesn't trigger a full Go re-analysis.
let rebuildTimer: NodeJS.Timeout | undefined;
let validateTimer: NodeJS.Timeout | undefined;

export async function activate(context: vscode.ExtensionContext) {
  outputChannel = vscode.window.createOutputChannel('Rex Template Validator');
  analyzerCollection = vscode.languages.createDiagnosticCollection('rex-analyzer');
  editorCollection = vscode.languages.createDiagnosticCollection('rex-editor');

  context.subscriptions.push(outputChannel, analyzerCollection, editorCollection);
  outputChannel.appendLine('[Rex] Extension activated');

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[Rex] No workspace folder found');
    return;
  }

  analyzer = new GoAnalyzer(context, outputChannel);
  graphBuilder = new KnowledgeGraphBuilder(workspaceRoot, outputChannel);
  validator = new TemplateValidator(outputChannel, graphBuilder);

  // ── Commands ───────────────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.commands.registerCommand('rexTemplateValidator.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc && isTemplate(doc)) {
        await validateDocument(doc);
      }
    }),

    vscode.commands.registerCommand('rexTemplateValidator.rebuildIndex', async () => {
      await rebuildIndex(workspaceRoot);
    }),

    vscode.commands.registerCommand('rexTemplateValidator.showKnowledgeGraph', () => {
      if (currentGraph) {
        KnowledgeGraphPanel.show(context, currentGraph);
      } else {
        vscode.window.showInformationMessage(
          'No template index yet. Run "Rex: Rebuild Template Index" first.'
        );
      }
    })
  );

  // ── Language features ──────────────────────────────────────────────────────

  // Hover
  context.subscriptions.push(
    vscode.languages.registerHoverProvider(TEMPLATE_SELECTOR, {
      async provideHover(document, position) {
        if (!validator || !graphBuilder) return;
        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        // If this file has no render calls or wasn't found, it might be a partial used by other templates
        // Try to find the context from a parent template's partial call
        if (!ctx || ctx.renderCalls.length === 0) {
          const partialCtx = graphBuilder.findContextForFileAsPartial(document.uri.fsPath);
          if (partialCtx) {
            ctx = partialCtx;
          }
        }
        if (!ctx) return;
        return await validator.getHoverInfo(document, position, ctx);
      },
    })
  );

  // Completion
  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider(
      TEMPLATE_SELECTOR,
      {
        provideCompletionItems(document, position) {
          if (!validator || !graphBuilder) return;
          let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
          // If this file has no render calls or wasn't found, it might be a partial used by other templates
          // Try to find the context from a parent template's partial call
          if (!ctx || ctx.renderCalls.length === 0) {
            const partialCtx = graphBuilder.findContextForFileAsPartial(document.uri.fsPath);
            if (partialCtx) {
              ctx = partialCtx;
            }
          }
          if (!ctx) return [];
          return validator.getCompletions(document, position, ctx);
        },
      },
      '.'
    )
  );

  // Go to Definition — jumps from template variable to the c.Render() call in Go
  context.subscriptions.push(
    vscode.languages.registerDefinitionProvider(TEMPLATE_SELECTOR, {
      async provideDefinition(document, position) {
        if (!validator || !graphBuilder) return;
        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        // If this file has no render calls or wasn't found, it might be a partial used by other templates
        // Try to find the context from a parent template's partial call
        if (!ctx || ctx.renderCalls.length === 0) {
          const partialCtx = graphBuilder.findContextForFileAsPartial(document.uri.fsPath);
          if (partialCtx) {
            ctx = partialCtx;
          }
        }
        if (!ctx) return;
        return await validator.getDefinitionLocation(document, position, ctx);
      },
    })
  );

  // Go to Definition for Go files — jumps from c.Render("template.html", ...) to the template file
  const GO_SELECTOR: vscode.DocumentSelector = [{ language: 'go', scheme: 'file' }];
  context.subscriptions.push(
    vscode.languages.registerDefinitionProvider(GO_SELECTOR, {
      provideDefinition(document, position) {
        if (!validator) return;
        return validator.getTemplateDefinitionFromGo(document, position);
      },
    })
  );

  // ── File watchers ──────────────────────────────────────────────────────────

  // Go source changes → full re-analysis
  const goWatcher = vscode.workspace.createFileSystemWatcher('**/*.go');
  context.subscriptions.push(
    goWatcher,
    goWatcher.onDidChange(() => scheduleRebuild(workspaceRoot)),
    goWatcher.onDidCreate(() => scheduleRebuild(workspaceRoot)),
    goWatcher.onDidDelete(() => scheduleRebuild(workspaceRoot))
  );

  // Template changes → re-validate only (no Go analysis needed)
  const tplWatcher = vscode.workspace.createFileSystemWatcher('**/*.{html,tmpl}');
  context.subscriptions.push(
    tplWatcher,
    tplWatcher.onDidChange(async (uri) => {
      const doc = await vscode.workspace.openTextDocument(uri);
      if (isTemplate(doc)) scheduleValidate(doc);
    }),
    tplWatcher.onDidCreate(async (uri) => {
      // New template file — rebuild so it can be indexed
      scheduleRebuild(workspaceRoot);
    })
  );

  // Validate on open/change/save
  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((doc) => {
      if (isTemplate(doc)) scheduleValidate(doc);
    }),

    vscode.workspace.onDidChangeTextDocument((e) => {
      if (isTemplate(e.document)) scheduleValidate(e.document);
    }),

    vscode.workspace.onDidSaveTextDocument((doc) => {
      if (doc.fileName.endsWith('.go')) {
        scheduleRebuild(workspaceRoot);      // re-run Go analyzer
      } else if (isTemplate(doc)) {
        scheduleValidate(doc);               // re-validate template against existing graph
      } else {
        scheduleRebuild(workspaceRoot); // default
      }
    })
  );

  // Config changes → rebuild so new sourceDir/templateRoot take effect
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('rexTemplateValidator')) {
        outputChannel.appendLine('[Rex] Configuration changed, rebuilding index...');
        scheduleRebuild(workspaceRoot);
      }
    })
  );

  // ── Initial build ──────────────────────────────────────────────────────────

  await rebuildIndex(workspaceRoot);

  // Validate already-open templates
  for (const doc of vscode.workspace.textDocuments) {
    if (isTemplate(doc)) {
      await validateDocument(doc);
    }
  }

  outputChannel.appendLine('[Rex] Ready');
  vscode.window.setStatusBarMessage('$(check) Rex templates indexed', 3000);
}

// ── Template detection ─────────────────────────────────────────────────────────

function isTemplate(doc: vscode.TextDocument): boolean {
  return (
    doc.uri.scheme === 'file' &&
    (doc.fileName.endsWith('.html') || doc.fileName.endsWith('.tmpl'))
  );
}

// ── Rebuild (full Go analysis) ─────────────────────────────────────────────────

async function rebuildIndex(workspaceRoot: string) {
  if (!analyzer || !graphBuilder) return;

  const statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left);
  statusBarItem.text = '$(sync~spin) Rex: Analyzing...';
  statusBarItem.show();

  const config = vscode.workspace.getConfiguration('rexTemplateValidator');
  const sourceDir: string = config.get('sourceDir') ?? '.';
  const templateRoot: string = config.get('templateRoot') ?? '';
  const templateBaseDir: string = config.get('templateBaseDir') ?? '';

  try {
    const result = await analyzer.analyzeWorkspace(workspaceRoot);
    currentGraph = graphBuilder.build(result);

    if (result.errors?.length) {
      outputChannel.appendLine('[Rex] Analysis warnings:');
      result.errors.slice(0, 10).forEach(e => outputChannel.appendLine(`  ${e}`));
    }

    const count = currentGraph.templates.size;
    if (count === 0) {
      outputChannel.appendLine('[Rex] No templates found.');
      if (!result.renderCalls.length) {
        outputChannel.appendLine('[Rex] No render calls found. Check your Go code calls c.Render().');
      }
    }

    statusBarItem.text = `$(check) Rex: ${count} template${count === 1 ? '' : 's'} indexed`;

    // Apply diagnostics from Go analyzer
    const initialValidationErrors = result.validationErrors ?? [];
    const extensionMissingTemplateLogicalPaths = new Set<string>();
    const extensionGeneratedErrors: GoValidationError[] = [];

    // Check for missing templates
    for (const [logicalPath, ctx] of currentGraph.templates) {
      if (!fs.existsSync(ctx.absolutePath)) {
        for (const rc of ctx.renderCalls) {
          extensionGeneratedErrors.push({
            template: logicalPath,
            line: rc.line,
            column: rc.templateNameStartCol,
            variable: logicalPath,
            message: `Template file not found: ${logicalPath}`,
            severity: 'error',
            goFile: rc.file,
            goLine: rc.line,
            templateNameStartCol: rc.templateNameStartCol,
            templateNameEndCol: rc.templateNameEndCol,
          });
          extensionMissingTemplateLogicalPaths.add(logicalPath);
        }
      }
    }

    const finalValidationErrors: GoValidationError[] = [];

    // Filter out analyzer errors if a more precise extension error exists
    for (const analyzerErr of initialValidationErrors) {
      if (
        analyzerErr.message.includes('Could not read template file:') &&
        extensionMissingTemplateLogicalPaths.has(analyzerErr.template)
      ) {
        // Skip this analyzer error as the extension provides a better one
        continue;
      }
      finalValidationErrors.push(analyzerErr);
    }

    // Add all extension-generated errors
    finalValidationErrors.push(...extensionGeneratedErrors);

    await applyAnalyzerDiagnostics(finalValidationErrors, workspaceRoot, sourceDir, templateRoot, templateBaseDir);

    // Re-validate open template docs with fresh graph (editor diagnostics only)
    for (const doc of vscode.workspace.textDocuments) {
      if (isTemplate(doc)) {
        await validateDocument(doc);
      }
    }
  } catch (err) {
    outputChannel.appendLine(`[Rex] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) Rex: Analysis failed';
  } finally {
    setTimeout(() => statusBarItem.dispose(), 5000);
  }
}

async function applyAnalyzerDiagnostics(
  validationErrors: import('./types').GoValidationError[],
  workspaceRoot: string,
  sourceDir: string,
  templateRoot: string,
  templateBaseDir: string
) {
  analyzerCollection.clear();

  const issuesByFile = new Map<string, vscode.Diagnostic[]>();

  for (const err of validationErrors) {
    let diagnosticFilePath: string;
    let diagnosticLine: number;
    let diagnosticCol: number;
    let diagnosticEndCol: number;
    let relatedInfo: vscode.DiagnosticRelatedInformation[] | undefined;

    // If it's a template not found error, point to the Go call site
    if (err.message.includes('Template file not found') && err.goFile && err.goLine !== undefined) {
      diagnosticFilePath = path.join(workspaceRoot, sourceDir, err.goFile);
      diagnosticLine = Math.max(0, err.goLine - 1);

      // Use the precise column info from the error object
      diagnosticCol = Math.max(0, (err.templateNameStartCol ?? 1) - 1);
      diagnosticEndCol = Math.max(0, (err.templateNameEndCol ?? (err.templateNameStartCol ?? 1) + err.template.length) - 1); // Fallback if end col is missing

      // No related info needed as the diagnostic itself is on the Go file
      relatedInfo = undefined;

    } else {
      // For other validation errors, point to the template file itself
      const baseDir = templateBaseDir === '' ? workspaceRoot : path.join(workspaceRoot, templateBaseDir);
      diagnosticFilePath = path.join(baseDir, templateRoot, err.template);
      diagnosticLine = Math.max(0, err.line - 1);
      diagnosticCol = Math.max(0, err.column - 1);
      diagnosticEndCol = diagnosticCol + (err.variable?.length || 1);

      if (err.goFile) {
        const goFileAbs = path.join(workspaceRoot, sourceDir, err.goFile);
        relatedInfo = [
          new vscode.DiagnosticRelatedInformation(
            new vscode.Location(
              vscode.Uri.file(goFileAbs),
              new vscode.Position(Math.max(0, (err.goLine ?? 1) - 1), 0)
            ),
            'Variable passed from here'
          ),
        ];
      }
    }

    const range = new vscode.Range(diagnosticLine, diagnosticCol, diagnosticLine, diagnosticEndCol);
    const diag = new vscode.Diagnostic(
      range,
      err.message,
      err.severity === 'warning' ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Error
    );
    diag.source = 'Rex';
    if (relatedInfo) {
      diag.relatedInformation = relatedInfo;
    }

    const list = issuesByFile.get(diagnosticFilePath) ?? [];
    list.push(diag);
    issuesByFile.set(diagnosticFilePath, list);
  }

  for (const [filePath, issues] of issuesByFile) {
    analyzerCollection.set(vscode.Uri.file(filePath), issues);
  }

  outputChannel.appendLine(`[Rex] Applied ${validationErrors.length} analyzer diagnostics`);
}

// ── Per-document validation ────────────────────────────────────────────────────

async function validateDocument(doc: vscode.TextDocument) {
  if (!validator || !graphBuilder) return;

  let ctx = graphBuilder.findContextForFile(doc.uri.fsPath);

  // If this file has no render calls or wasn't found, it might be a partial used by other templates
  // Try to find the context from a parent template's partial call
  if (!ctx || ctx.renderCalls.length === 0) {
    const partialCtx = graphBuilder.findContextForFileAsPartial(doc.uri.fsPath);
    if (partialCtx) {
      ctx = partialCtx;
    }
  }

  if (!ctx) {
    editorCollection.delete(doc.uri);
    return;
  }

  const diagnostics = await validator.validateDocument(doc, ctx);
  editorCollection.set(doc.uri, diagnostics);
}

// ── Debounce helpers ───────────────────────────────────────────────────────────

function scheduleRebuild(workspaceRoot: string) {
  const config = vscode.workspace.getConfiguration('rexTemplateValidator');
  const debounceMs = config.get<number>('debounceMs') ?? 1500;

  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(() => rebuildIndex(workspaceRoot), debounceMs * 2);
}

function scheduleValidate(doc: vscode.TextDocument) {
  const config = vscode.workspace.getConfiguration('rexTemplateValidator');
  const debounceMs = config.get<number>('debounceMs') ?? 1500;

  if (validateTimer) clearTimeout(validateTimer);
  validateTimer = setTimeout(() => validateDocument(doc), debounceMs);
}

// ── Deactivation ───────────────────────────────────────────────────────────────

export function deactivate() {
  if (rebuildTimer) clearTimeout(rebuildTimer);
  if (validateTimer) clearTimeout(validateTimer);
  analyzerCollection?.dispose();
  editorCollection?.dispose();
  outputChannel?.dispose();
}
