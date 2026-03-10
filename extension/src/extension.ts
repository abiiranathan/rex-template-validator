import * as vscode from 'vscode';
import * as path from 'path';
import * as fs from 'fs';
import { GoAnalyzer } from './analyzer';
import { KnowledgeGraphBuilder, setKnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { KnowledgeGraph, GoValidationError, NamedBlockDuplicateError } from './types';

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

  outputChannel = vscode.window.createOutputChannel('Rex Template Validator');
  analyzerCollection = vscode.languages.createDiagnosticCollection('rex-analyzer');
  editorCollection = vscode.languages.createDiagnosticCollection('rex-editor');
  namedBlockCollection = vscode.languages.createDiagnosticCollection('rex-named-blocks');

  context.subscriptions.push(outputChannel, analyzerCollection, editorCollection, namedBlockCollection);
  outputChannel.appendLine('[Rex] Extension activated');

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[Rex] No workspace folder found');
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
    vscode.commands.registerCommand('rexAnalyzer.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc && isTemplate(doc)) {
        await validateDocument(doc);
      }
    }),

    vscode.commands.registerCommand('rexAnalyzer.rebuildIndex', async () => {
      await rebuildIndex(workspaceRoot);
    }),

    vscode.commands.registerCommand('rexAnalyzer.showKnowledgeGraph', () => {
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
            outputChannel.appendLine(`[Rex] Failed to sync open template ${doc.uri.fsPath}: ${err}`);
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
            outputChannel.appendLine(`[Rex] Incremental graph update failed for ${doc.uri.fsPath}: ${err}`);
          }
        }
        latestValidationVersions.set(doc.uri.toString(), doc.version);
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[Rex] Failed to sync template ${doc.uri.fsPath}: ${err}`);
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
          outputChannel.appendLine(`[Rex] Failed to clear template sync ${doc.uri.fsPath}: ${err}`);
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
      if (e.affectsConfiguration('rex-analyzer')) {
        outputChannel.appendLine('[Rex] Configuration changed, rebuilding index...');
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
  outputChannel.appendLine('[Rex] Ready');
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

  statusBarItem.text = '$(sync~spin) Rex: Analyzing Go sources...';
  statusBarItem.show();

  const config = vscode.workspace.getConfiguration('rex-analyzer');
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

    statusBarItem.text = `$(check) Rex: ${count} template${count === 1 ? '' : 's'} indexed`;
    statusBarItem.show();
    setTimeout(() => statusBarItem.hide(), 5000);
  } catch (err) {
    outputChannel.appendLine(`[Rex] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) Rex: Analysis failed';
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
      diag.source = 'Rex';
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
    `[Rex] Applied ${duplicateErrors.length} duplicate named-block diagnostic(s)`
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
  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const contextFile: string = config.get('contextFile') ?? '';

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
    outputChannel.appendLine(
      `[Rex] Live validation ${doc.uri.fsPath}: hasContext=${result.hasContext} errors=${result.validationErrors.length}`
    );
    if (!result.hasContext) {
      editorCollection.delete(doc.uri);
      return;
    }
    editorCollection.set(doc.uri, diagnosticsFromValidationErrors(result.validationErrors));
    analyzerCollection.delete(doc.uri);
  } catch (err) {
    outputChannel.appendLine(`[Rex] Live validation failed for ${doc.uri.fsPath}: ${err}`);
  }
}

/**
 * Validates a template file that is NOT currently open in a VS Code editor.
 *
 * Uses fs.readFileSync directly so we never call vscode.workspace.openTextDocument,
 * which would fire onDidOpenTextDocument and cascade into another full revalidation.
 */
async function validateFileDirect(filePath: string): Promise<void> {
  if (!analyzer) return;

  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const validationEnabled: boolean = config.get('validate') ?? true;
  if (!validationEnabled) return;

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) return;

  let content: string;
  try {
    content = fs.readFileSync(filePath, 'utf8');
  } catch {
    return;
  }

  try {
    const result = await analyzer.validateTemplate(workspaceRoot, filePath, content);
    const uri = vscode.Uri.file(filePath);
    outputChannel.appendLine(
      `[Rex] Live validation ${filePath}: hasContext=${result.hasContext} errors=${result.validationErrors.length}`
    );
    if (!result.hasContext) {
      editorCollection.delete(uri);
      return;
    }
    editorCollection.set(uri, diagnosticsFromValidationErrors(result.validationErrors));
    analyzerCollection.delete(uri);
  } catch (err) {
    outputChannel.appendLine(`[Rex] Live validation failed for ${filePath}: ${err}`);
  }
}

// ── Debounce helpers ───────────────────────────────────────────────────────────

function scheduleRebuild(workspaceRoot: string) {
  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const debounceMs = config.get<number>('debounceMs') ?? 1000;
  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(() => rebuildIndex(workspaceRoot), debounceMs);
}

function scheduleValidateDocument(doc: vscode.TextDocument) {
  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const debounceMs = config.get<number>('debounceMs') ?? 1000;
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
  const config = vscode.workspace.getConfiguration('rex-analyzer');
  const debounceMs = config.get<number>('debounceMs') ?? 1000;

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
  return errors.map(err => {
    const line = Math.max(0, err.line - 1);
    const col = Math.max(0, err.column - 1);
    const range = new vscode.Range(line, col, line, col + (err.variable?.length || 1));
    const diagnostic = new vscode.Diagnostic(
      range,
      err.message,
      err.severity === 'warning' ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Error
    );
    diagnostic.source = 'Rex';
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
