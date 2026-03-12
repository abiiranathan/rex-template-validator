import * as vscode from 'vscode';
import * as path from 'path';
import * as fs from 'fs';
import { GoAnalyzer } from './analyzer';
import { KnowledgeGraphBuilder, setKnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { KnowledgeGraph, GoValidationError, NamedBlockDuplicateError } from './types';
import { config } from './config';

const TEMPLATE_SELECTOR: vscode.DocumentSelector = [
  { language: 'html', scheme: 'file' },
  { language: 'go-template', scheme: 'file' },
  { pattern: '**/*.tmpl' },
  { pattern: '**/*.html' },
];

// Three separate collections so they never interfere with each other:
// - analyzerCollection:   diagnostics from the Go binary (persists across template edits)
// - editorCollection:     live diagnostics from the Go daemon for open documents
// - namedBlockCollection: duplicate named-block errors (cross-file, rebuilt with index)
let analyzerCollection: vscode.DiagnosticCollection;
let editorCollection: vscode.DiagnosticCollection;
let namedBlockCollection: vscode.DiagnosticCollection;
let outputChannel: vscode.OutputChannel;
let graphBuilder: KnowledgeGraphBuilder | undefined;
let validator: TemplateValidator | undefined;
let currentGraph: KnowledgeGraph | undefined;
let analyzer: GoAnalyzer | undefined;

/**
 * Single status bar item shared across the extension and KnowledgeGraphBuilder.
 * extension.ts owns its lifetime (created here, disposed in deactivate).
 * KnowledgeGraphBuilder receives it by reference and only mutates .text / .show() / .hide().
 */
let statusBarItem: vscode.StatusBarItem;

let rebuildTimer: NodeJS.Timeout | undefined;
let validateOpenTemplatesTimer: NodeJS.Timeout | undefined;
const validateTimers = new Map<string, NodeJS.Timeout>();
const latestValidationVersions = new Map<string, number>();

export async function activate(context: vscode.ExtensionContext) {
  // Create the single shared status bar item.
  statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left);
  context.subscriptions.push(statusBarItem);

  outputChannel = vscode.window.createOutputChannel('GoTpl LSP');
  analyzerCollection = vscode.languages.createDiagnosticCollection('gotpl-analyzer');
  editorCollection = vscode.languages.createDiagnosticCollection('gotpl-editor');
  namedBlockCollection = vscode.languages.createDiagnosticCollection('gotpl-named-blocks');

  context.subscriptions.push(outputChannel, analyzerCollection, editorCollection, namedBlockCollection);
  outputChannel.appendLine('[GoTpl] Extension activated');

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[GoTpl] No workspace folder found');
    return;
  }

  analyzer = new GoAnalyzer(context, outputChannel);

  // Pass the shared statusBarItem into the builder so it can show progress
  // messages without creating a second item.
  graphBuilder = new KnowledgeGraphBuilder(workspaceRoot, outputChannel, statusBarItem);
  setKnowledgeGraphBuilder(graphBuilder);
  validator = new TemplateValidator(outputChannel, graphBuilder, analyzer);

  // ── Commands ───────────────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.commands.registerCommand('gotpl.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc && isTemplate(doc)) {
        await validateDocument(doc);
      }
    }),

    vscode.commands.registerCommand('gotpl.rebuildIndex', async () => {
      await rebuildIndex(workspaceRoot);
    }),

    vscode.commands.registerCommand('gotpl.showKnowledgeGraph', () => {
      if (currentGraph) {
        KnowledgeGraphPanel.show(context, currentGraph);
      } else {
        vscode.window.showInformationMessage(
          'No template index yet. Run "GoTpl: Rebuild Template Index" first.'
        );
      }
    }),

    vscode.commands.registerCommand('gotpl.goToRenderCall', async () => {
      const editor = vscode.window.activeTextEditor;
      if (!editor || !isTemplate(editor.document)) return;

      if (!graphBuilder) {
        vscode.window.showErrorMessage('Template index is not ready.');
        return;
      }

      let ctx = graphBuilder.findContextForFile(editor.document.uri.fsPath);

      // If no direct render calls, see if it's used as a partial (inherits calls from parent)
      if (!ctx || ctx.renderCalls.length === 0) {
        const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(editor.document.uri.fsPath);
        if (partialCtx) ctx = partialCtx;
      }

      if (!ctx) {
        vscode.window.showInformationMessage('No Go render calls found for this template.');
        return;
      }

      // Filter out synthetic context file calls
      const realCalls = ctx.renderCalls.filter(rc => rc.file !== 'context-file');

      if (realCalls.length === 0) {
        vscode.window.showInformationMessage('No Go render calls found for this template (only synthetic context).');
        return;
      }

      const jumpTo = async (rc: any) => {
        const absPath = graphBuilder!.resolveGoFilePath(rc.file);
        if (!absPath) {
          vscode.window.showErrorMessage(`Could not resolve Go file: ${rc.file}`);
          return;
        }
        const goDoc = await vscode.workspace.openTextDocument(absPath);
        const goEditor = await vscode.window.showTextDocument(goDoc);
        const line = Math.max(0, rc.line - 1);
        const col = Math.max(0, (rc.templateNameStartCol ?? 1) - 1);
        const pos = new vscode.Position(line, col);

        goEditor.selection = new vscode.Selection(pos, pos);
        goEditor.revealRange(new vscode.Range(pos, pos), vscode.TextEditorRevealType.InCenter);
      };

      if (realCalls.length === 1) {
        await jumpTo(realCalls[0]);
      } else {
        const items = realCalls.map(rc => ({
          label: `$(go) ${rc.file}:${rc.line}`,
          description: rc.vars && rc.vars.length > 0
            ? `Context vars: ${rc.vars.map((v: any) => v.name).join(', ')}`
            : 'No context vars',
          rc: rc
        }));

        const selected = await vscode.window.showQuickPick(items, {
          placeHolder: 'Select a Go render call to jump to',
          matchOnDescription: true
        });

        if (selected) {
          await jumpTo(selected.rc);
        }
      }
    })
  );

  // ── Language features ──────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.languages.registerHoverProvider(TEMPLATE_SELECTOR, {
      async provideHover(document, position, token) {
        if (!validator || !graphBuilder) return;

        // Resolve context (cheap — uses the in-memory cache on graphBuilder)
        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        if (!ctx || ctx.renderCalls.length === 0) {
          const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
          if (partialCtx) ctx = partialCtx;
        }
        if (!ctx) return;

        // Race the hover computation against:
        //   • the VS Code cancellation token (user moved cursor away), and
        //   • a hard 1 s wall-clock timeout (prevents the "Loading…" spinner
        //     from sticking when file I/O inside the hover provider is slow).
        const hoverPromise = validator.getHoverInfo(document, position, ctx).catch(() => undefined);

        const abortPromise = new Promise<undefined>(resolve => {
          const cancelDisposable = token.onCancellationRequested(() => {
            cancelDisposable.dispose();
            resolve(undefined);
          });
          const tid = setTimeout(() => {
            cancelDisposable.dispose();
            resolve(undefined);
          }, 1000);
          hoverPromise.finally(() => {
            clearTimeout(tid);
            cancelDisposable.dispose();
          });
        });

        return Promise.race([hoverPromise, abortPromise]);
      },
    })
  );

  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider(
      TEMPLATE_SELECTOR,
      {
        async provideCompletionItems(document, position) {
          if (!validator || !graphBuilder) return;
          let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
          if (!ctx || ctx.renderCalls.length === 0) {
            const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
            if (partialCtx) ctx = partialCtx;
          }
          if (!ctx) return [];
          return await validator.getCompletionItems(document, position, ctx);
        },
      },
      '.', '$', '"'
    )
  );

  context.subscriptions.push(
    vscode.languages.registerDefinitionProvider(TEMPLATE_SELECTOR, {
      async provideDefinition(document, position) {
        if (!validator || !graphBuilder) return;
        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        if (!ctx || ctx.renderCalls.length === 0) {
          const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
          if (partialCtx) ctx = partialCtx;
        }
        if (!ctx) return;
        return await validator.getDefinitionLocation(document, position, ctx);
      },
    })
  );

  const GO_SELECTOR: vscode.DocumentSelector = [{ language: 'go', scheme: 'file' }];
  context.subscriptions.push(
    vscode.languages.registerDefinitionProvider(GO_SELECTOR, {
      provideDefinition(document, position) {
        if (!validator) return;
        return validator.getTemplateDefinitionFromGo(document, position);
      },
    })
  );

  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((doc) => {
      if (isTemplate(doc)) {
        latestValidationVersions.set(doc.uri.toString(), doc.version);
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[GoTpl] Failed to sync open template ${doc.uri.fsPath}: ${err}`);
          });
        }
        validateDocument(doc, doc.version);
      }
    }),

    vscode.workspace.onDidChangeTextDocument((e) => {
      const doc = e.document;
      if (isTemplate(doc)) {
        if (graphBuilder) {
          try {
            graphBuilder.updateTemplateFile(doc.uri.fsPath, doc.getText());
            applyNamedBlockDiagnostics();
          } catch (err) {
            outputChannel.appendLine(`[GoTpl] Incremental graph update failed for ${doc.uri.fsPath}: ${err}`);
          }
        }
        latestValidationVersions.set(doc.uri.toString(), doc.version);
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[GoTpl] Failed to sync template ${doc.uri.fsPath}: ${err}`);
          });
        }
        scheduleValidateDocument(doc);
        scheduleValidateOpenTemplateDocuments(doc.uri.toString());
      }
    }),

    vscode.workspace.onDidCloseTextDocument((doc) => {
      if (!isTemplate(doc)) return;

      const key = doc.uri.toString();
      const timer = validateTimers.get(key);
      if (timer) {
        clearTimeout(timer);
        validateTimers.delete(key);
      }
      latestValidationVersions.delete(key);
      if (analyzer) {
        void analyzer.clearTemplate(workspaceRoot, doc.uri.fsPath).catch((err) => {
          outputChannel.appendLine(`[GoTpl] Failed to clear template sync ${doc.uri.fsPath}: ${err}`);
        });
      }
    }),

    vscode.workspace.onDidSaveTextDocument((doc) => {
      if (isTemplate(doc)) {
        scheduleRebuild(workspaceRoot);
        return;
      }

      if (doc.fileName.endsWith('.go') || doc.fileName.endsWith('go.mod') || doc.fileName.endsWith('.json')) {
        scheduleRebuild(workspaceRoot);
      }
    })
  );

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('gotpl-analyzer')) {
        outputChannel.appendLine('[GoTpl] Configuration changed, rebuilding index...');
        scheduleRebuild(workspaceRoot);
      }
    })
  );

  context.subscriptions.push(
    vscode.languages.registerReferenceProvider(TEMPLATE_SELECTOR, {
      async provideReferences(document, position, refCtx) {
        if (!validator) return [];
        const locs = await validator.getReferences(
          document,
          position,
          refCtx.includeDeclaration
        );
        return locs ?? [];
      },
    })
  );

  // ── Initial build ──────────────────────────────────────────────────────────

  await rebuildIndex(workspaceRoot);
  outputChannel.appendLine('[GoTpl] Ready');
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

  statusBarItem.text = '$(sync~spin) GoTpl: Analyzing Go sources...';
  statusBarItem.show();

  const sourceDir: string = config.sourceDir();
  const templateRoot: string = config.templateRoot();
  const templateBaseDir: string = config.templateBaseDir();

  try {
    const result = await analyzer.analyzeWorkspace(workspaceRoot);
    currentGraph = graphBuilder.build(result);

    if (result.errors?.length) {
      outputChannel.appendLine('[GoTpl] Analysis warnings:');
      result.errors.slice(0, 10).forEach(e => outputChannel.appendLine(`  ${e}`));
    }

    const count = currentGraph.templates.size;
    if (count === 0) {
      outputChannel.appendLine('[GoTpl] No templates found.');
      if (!result.renderCalls.length) {
        outputChannel.appendLine('[GoTpl] No render calls found. Check your Go code calls c.Render().');
      }
    }

    // Apply diagnostics from Go analyzer
    const initialValidationErrors = result.validationErrors ?? [];
    const extensionMissingTemplateLogicalPaths = new Set<string>();
    const extensionGeneratedErrors: GoValidationError[] = [];

    for (const [logicalPath, ctx] of currentGraph.templates) {
      const isNamedBlock = currentGraph.namedBlocks.has(logicalPath);
      if (!fs.existsSync(ctx.absolutePath) && !isNamedBlock) {
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
    for (const analyzerErr of initialValidationErrors) {
      const isNotFoundMsg = analyzerErr.message.includes('Could not read template file:') ||
        analyzerErr.message.includes('Template or named block not found');
      if (isNotFoundMsg && extensionMissingTemplateLogicalPaths.has(analyzerErr.template)) {
        continue;
      }
      finalValidationErrors.push(analyzerErr);
    }
    finalValidationErrors.push(...extensionGeneratedErrors);

    await applyAnalyzerDiagnostics(finalValidationErrors, workspaceRoot, sourceDir, templateRoot, templateBaseDir);
    applyNamedBlockDiagnostics();

    await validateOpenTemplateDocuments();

    statusBarItem.text = `$(check) GoTpl: ${count} template${count === 1 ? '' : 's'} indexed`;
    statusBarItem.show();
    setTimeout(() => statusBarItem.hide(), 5000);
  } catch (err) {
    outputChannel.appendLine(`[GoTpl] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) GoTpl: Analysis failed';
    statusBarItem.show();
  }
}

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
        Math.max(0, entry.line - 1),
        Math.max(0, entry.col - 1),
        Math.max(0, entry.line - 1),
        Math.max(0, entry.col - 1) + entry.name.length + 2
      );

      const diag = new vscode.Diagnostic(range, msg, vscode.DiagnosticSeverity.Error);
      diag.source = 'GoTpl';
      diag.code = 'duplicate-named-block';

      diag.relatedInformation = err.entries
        .filter(e => e !== entry)
        .map(
          e =>
            new vscode.DiagnosticRelatedInformation(
              new vscode.Location(
                vscode.Uri.file(e.absolutePath),
                new vscode.Position(Math.max(0, e.line - 1), Math.max(0, e.col - 1))
              ),
              `Also declared here as "${e.name}"`
            )
        );

      const list = issuesByFile.get(entry.absolutePath) ?? [];
      list.push(diag);
      issuesByFile.set(entry.absolutePath, list);
    }
  }

  for (const [filePath, issues] of issuesByFile) {
    namedBlockCollection.set(vscode.Uri.file(filePath), issues);
  }

  outputChannel.appendLine(
    `[GoTpl] Applied ${duplicateErrors.length} duplicate named-block diagnostic(s)`
  );
}

async function applyAnalyzerDiagnostics(
  validationErrors: GoValidationError[],
  workspaceRoot: string,
  sourceDir: string,
  templateRoot: string,
  templateBaseDir: string
) {
  analyzerCollection.clear();
  const contextFile: string = config.contextFile();
  const issuesByFile = new Map<string, vscode.Diagnostic[]>();

  for (const err of validationErrors) {
    let diagnosticFilePath: string;
    let diagnosticLine: number;
    let diagnosticCol: number;
    let diagnosticEndCol: number;
    let relatedInfo: vscode.DiagnosticRelatedInformation[] | undefined;

    const isNotFound = err.message.includes('not found') || err.message.includes('Could not read template file');

    if (err.goFile === "context-file") {
      if (isNotFound) {
        diagnosticFilePath = contextFile ? path.resolve(workspaceRoot, contextFile) : path.join(workspaceRoot, sourceDir, err.goFile);
        diagnosticLine = 0;
        diagnosticCol = 0;
        diagnosticEndCol = 100;
      } else {
        const baseDir = templateBaseDir ? path.resolve(workspaceRoot, templateBaseDir) : path.resolve(workspaceRoot, sourceDir);
        diagnosticFilePath = path.join(baseDir, templateRoot, err.template);
        diagnosticLine = Math.max(0, err.line - 1);
        diagnosticCol = Math.max(0, err.column - 1);
        diagnosticEndCol = diagnosticCol + (err.variable?.length || 1);

        if (contextFile) {
          relatedInfo = [
            new vscode.DiagnosticRelatedInformation(
              new vscode.Location(
                vscode.Uri.file(path.resolve(workspaceRoot, contextFile)),
                new vscode.Position(0, 0)
              ),
              'Context provided by context-file'
            )
          ];
        }
      }
    } else if (isNotFound && err.goFile && err.goLine !== undefined) {
      diagnosticFilePath = path.join(path.resolve(workspaceRoot, sourceDir), err.goFile);
      diagnosticLine = Math.max(0, err.goLine - 1);
      diagnosticCol = Math.max(0, (err.templateNameStartCol ?? 1) - 1);
      diagnosticEndCol = Math.max(
        0,
        (err.templateNameEndCol ?? (err.templateNameStartCol ?? 1) + err.template.length) - 1
      );
      relatedInfo = undefined;
    } else {
      const baseDir = templateBaseDir
        ? path.join(workspaceRoot, templateBaseDir)
        : path.join(workspaceRoot, sourceDir);
      diagnosticFilePath = path.join(baseDir, templateRoot, err.template);

      diagnosticLine = Math.max(0, err.line - 1);
      diagnosticCol = Math.max(0, err.column - 1);
      diagnosticEndCol = diagnosticCol + (err.variable?.length || 1);

      if (err.goFile) {
        const goFileAbs = path.join(path.resolve(workspaceRoot, sourceDir), err.goFile);
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
    diag.source = 'GoTpl';
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

  outputChannel.appendLine(`[GoTpl] Applied ${validationErrors.length} analyzer diagnostics`);
}

// ── Per-document validation ────────────────────────────────────────────────────

async function validateDocument(doc: vscode.TextDocument, requestedVersion = doc.version) {
  if (!analyzer) return;

  const docKey = doc.uri.toString();
  const latestVersion = latestValidationVersions.get(docKey);
  if (latestVersion !== undefined && latestVersion > requestedVersion) {
    return;
  }

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    editorCollection.delete(doc.uri);
    return;
  }

  try {
    const result = await analyzer.validateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText());
    if (latestValidationVersions.get(docKey) !== requestedVersion) {
      return;
    }

    const errors = result.validationErrors ?? [];

    outputChannel.appendLine(
      `[GoTpl] Live validation ${doc.uri.fsPath}: hasContext=${result.hasContext} errors=${errors.length}`
    );
    if (!result.hasContext) {
      editorCollection.delete(doc.uri);
      return;
    }
    editorCollection.set(doc.uri, diagnosticsFromValidationErrors(errors));
    analyzerCollection.delete(doc.uri);
  } catch (err) {
    outputChannel.appendLine(`[GoTpl] Live validation failed for ${doc.uri.fsPath}: ${err}`);
  }
}


function scheduleRebuild(workspaceRoot: string) {
  const debounceMs = config.debounceMs();
  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(() => rebuildIndex(workspaceRoot), debounceMs);
}

function scheduleValidateDocument(doc: vscode.TextDocument) {
  const debounceMs = config.debounceMs();
  const docKey = doc.uri.toString();
  latestValidationVersions.set(docKey, doc.version);

  const existingTimer = validateTimers.get(docKey);
  if (existingTimer) clearTimeout(existingTimer);

  const scheduledVersion = doc.version;
  const timer = setTimeout(async () => {
    validateTimers.delete(docKey);

    const currentDoc = vscode.workspace.textDocuments.find(openDoc => openDoc.uri.toString() === docKey);
    if (!currentDoc || !isTemplate(currentDoc)) {
      return;
    }

    await validateDocument(currentDoc, scheduledVersion);
  }, debounceMs);

  validateTimers.set(docKey, timer);
}

function scheduleValidateOpenTemplateDocuments(excludeDocKey?: string) {
  const debounceMs = config.debounceMs();

  if (validateOpenTemplatesTimer) clearTimeout(validateOpenTemplatesTimer);
  validateOpenTemplatesTimer = setTimeout(async () => {
    validateOpenTemplatesTimer = undefined;
    await validateOpenTemplateDocuments(excludeDocKey);
  }, debounceMs);
}

async function validateOpenTemplateDocuments(excludeDocKey?: string) {
  const openDocs = vscode.workspace.textDocuments.filter(isTemplate);
  for (const doc of openDocs) {
    if (excludeDocKey && doc.uri.toString() === excludeDocKey) {
      continue;
    }
    await validateDocument(doc);
  }
}

function diagnosticsFromValidationErrors(errors: GoValidationError[]): vscode.Diagnostic[] {
  if (!errors) return [];

  return errors.map(err => {
    const line = Math.max(0, err.line - 1);
    const col = Math.max(0, err.column - 1);
    const range = new vscode.Range(line, col, line, col + (err.variable?.length || 1));
    const diagnostic = new vscode.Diagnostic(
      range,
      err.message,
      err.severity === 'warning' ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Error
    );
    diagnostic.source = 'GoTpl';
    return diagnostic;
  });
}

// ── Deactivation ───────────────────────────────────────────────────────────────

export function deactivate() {
  if (rebuildTimer) clearTimeout(rebuildTimer);
  if (validateOpenTemplatesTimer) clearTimeout(validateOpenTemplatesTimer);
  for (const timer of validateTimers.values()) {
    clearTimeout(timer);
  }
  validateTimers.clear();
  latestValidationVersions.clear();
  analyzer?.dispose();
  analyzerCollection?.dispose();
  editorCollection?.dispose();
  namedBlockCollection?.dispose();
  outputChannel?.dispose();
  statusBarItem?.dispose();
}
