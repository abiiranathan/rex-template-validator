import * as vscode from 'vscode';
import * as path from 'path';
import { GoAnalyzer } from './analyzer';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { KnowledgeGraph } from './types';

const TEMPLATE_SELECTOR: vscode.DocumentSelector = [
  { language: 'html', scheme: 'file' },
  { language: 'go-template', scheme: 'file' },
  { pattern: '**/*.tmpl' },
  { pattern: '**/*.html' },
];

let diagnosticCollection: vscode.DiagnosticCollection;
let outputChannel: vscode.OutputChannel;
let graphBuilder: KnowledgeGraphBuilder | undefined;
let validator: TemplateValidator | undefined;
let currentGraph: KnowledgeGraph | undefined;
let analyzer: GoAnalyzer | undefined;
let debounceTimer: NodeJS.Timeout | undefined;

export async function activate(context: vscode.ExtensionContext) {
  outputChannel = vscode.window.createOutputChannel('Rex Template Validator');
  diagnosticCollection = vscode.languages.createDiagnosticCollection('rex-templates');

  context.subscriptions.push(outputChannel, diagnosticCollection);

  outputChannel.appendLine('[Rex] Extension activated');

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[Rex] No workspace folder found');
    return;
  }

  analyzer = new GoAnalyzer(context, outputChannel);
  graphBuilder = new KnowledgeGraphBuilder(workspaceRoot, outputChannel);
  validator = new TemplateValidator(outputChannel, graphBuilder);

  // Register commands
  context.subscriptions.push(
    vscode.commands.registerCommand('rexTemplateValidator.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc) {
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

  // Hover provider
  context.subscriptions.push(
    vscode.languages.registerHoverProvider(TEMPLATE_SELECTOR, {
      provideHover(document, position) {
        if (!validator || !graphBuilder) return;
        const ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        if (!ctx) return;
        return validator.getHoverInfo(document, position, ctx);
      },
    })
  );

  // Completion provider
  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider(
      TEMPLATE_SELECTOR,
      {
        provideCompletionItems(document, position) {
          if (!validator || !graphBuilder) return;
          const ctx = graphBuilder.findContextForFile(document.uri.fsPath);
          if (!ctx) return [];
          return validator.getCompletions(document, position, ctx);
        },
      },
      '.'
    )
  );

  // File watcher for Go files → rebuild index
  const goWatcher = vscode.workspace.createFileSystemWatcher('**/*.go');
  context.subscriptions.push(
    goWatcher,
    goWatcher.onDidChange(() => scheduleRebuild(workspaceRoot)),
    goWatcher.onDidCreate(() => scheduleRebuild(workspaceRoot)),
    goWatcher.onDidDelete(() => scheduleRebuild(workspaceRoot))
  );

  // File watcher for templates → validate
  const tplWatcher = vscode.workspace.createFileSystemWatcher('**/*.{html,tmpl}');
  context.subscriptions.push(
    tplWatcher,
    tplWatcher.onDidChange(async (uri) => {
      const doc = await vscode.workspace.openTextDocument(uri);
      scheduleValidate(doc);
    })
  );

  // Validate on open
  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((doc) => {
      if (isTemplate(doc)) {
        scheduleValidate(doc);
      }
    }),
    vscode.workspace.onDidSaveTextDocument((doc) => {
      if (isTemplate(doc)) {
        scheduleValidate(doc);
      }
    })
  );

  // Initial index build
  await rebuildIndex(workspaceRoot);

  // Validate any already-open template docs
  for (const doc of vscode.workspace.textDocuments) {
    if (isTemplate(doc)) {
      await validateDocument(doc);
    }
  }

  outputChannel.appendLine('[Rex] Ready');
  vscode.window.setStatusBarMessage('$(check) Rex templates indexed', 3000);
}

function isTemplate(doc: vscode.TextDocument): boolean {
  return (
    doc.uri.scheme === 'file' &&
    (doc.fileName.endsWith('.html') || doc.fileName.endsWith('.tmpl'))
  );
}

async function rebuildIndex(workspaceRoot: string) {
  if (!analyzer || !graphBuilder) return;

  const statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left);
  statusBarItem.text = '$(sync~spin) Rex: Analyzing...';
  statusBarItem.show();

  // Read config — must match the CLI flags -dir and -template-root
  const config = vscode.workspace.getConfiguration('rexTemplateValidator');
  const sourceDir: string = config.get('sourceDir') ?? '.';
  const templateRoot: string = config.get('templateRoot') ?? '';

  try {
    const result = await analyzer.analyzeWorkspace(workspaceRoot);
    currentGraph = graphBuilder.build(result);

    if (result.errors?.length) {
      outputChannel.appendLine('[Rex] Analysis warnings:');
      result.errors.slice(0, 10).forEach((e) => outputChannel.appendLine(`  ${e}`));
    }

    const count = currentGraph.templates.size;

    if (count === 0) {
      outputChannel.appendLine('[Rex] No templates found in analysis result.');
      if (result.renderCalls.length === 0) {
        outputChannel.appendLine('[Rex] No render calls found. Check if your Go code calls c.Render().');
      }
    }

    statusBarItem.text = `$(check) Rex: ${count} templates indexed`;

    // Apply diagnostics from Go analyzer
    diagnosticCollection.clear();

    if (result.validationErrors) {
      const issuesByFile = new Map<string, vscode.Diagnostic[]>();

      for (const err of result.validationErrors) {
        // Full absolute path: workspaceRoot / sourceDir / templateRoot / err.template
        // err.template is the logical relative path from the analyzer,
        // e.g. "views/inpatient/treatment-chart.html"
        const absPath = path.join(workspaceRoot, sourceDir, templateRoot, err.template);

        const range = new vscode.Range(
          Math.max(0, err.line - 1),
          Math.max(0, err.column - 1),
          Math.max(0, err.line - 1),
          Math.max(0, err.column - 1 + (err.variable?.length || 1))
        );

        const diag = new vscode.Diagnostic(
          range,
          err.message,
          err.severity === 'warning' ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Error
        );
        diag.source = 'Rex';

        // Attach the Go source file as related information so the user can
        // jump from the template diagnostic back to the c.Render() call site.
        if (err.goFile) {
          const goFileAbs = path.join(workspaceRoot, sourceDir, err.goFile);
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

        const list = issuesByFile.get(absPath) || [];
        list.push(diag);
        issuesByFile.set(absPath, list);
      }

      for (const [filePath, issues] of issuesByFile) {
        diagnosticCollection.set(vscode.Uri.file(filePath), issues);
      }

      outputChannel.appendLine(`[Rex] Applied ${result.validationErrors.length} diagnostics from analyzer`);
    }
  } catch (err) {
    outputChannel.appendLine(`[Rex] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) Rex: Index failed';
  } finally {
    setTimeout(() => statusBarItem.dispose(), 4000);
  }
}

async function validateDocument(doc: vscode.TextDocument) {
  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (workspaceRoot) {
    scheduleRebuild(workspaceRoot);
  }
}

function scheduleRebuild(workspaceRoot: string) {
  const config = vscode.workspace.getConfiguration('rexTemplateValidator');
  const debounceMs = config.get<number>('debounceMs') ?? 1500;

  if (debounceTimer) clearTimeout(debounceTimer);
  debounceTimer = setTimeout(() => rebuildIndex(workspaceRoot), debounceMs * 2);
}

function scheduleValidate(doc: vscode.TextDocument) {
  const config = vscode.workspace.getConfiguration('rexTemplateValidator');
  const debounceMs = config.get<number>('debounceMs') ?? 1500;

  if (debounceTimer) clearTimeout(debounceTimer);
  debounceTimer = setTimeout(() => validateDocument(doc), debounceMs);
}

export function deactivate() {
  diagnosticCollection?.dispose();
  outputChannel?.dispose();
}
