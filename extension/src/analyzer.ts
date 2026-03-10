import * as cp from 'child_process';
import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import * as readline from 'readline';
import { createGunzip } from 'zlib';

import { AnalysisResult, ExpressionTypeResult, GoTemplateValidationResult, ScopeFrame, TemplateVar } from './types';

interface AnalyzerRequest {
  jsonrpc: '2.0';
  id: number;
  method: string;
  params?: Record<string, unknown>;
}

interface AnalyzerResponse<T> {
  jsonrpc: '2.0';
  id: number;
  result?: T;
  error?: {
    code: number;
    message: string;
  };
}

export class GoAnalyzer {
  private analyzerPath: string;
  private outputChannel: vscode.OutputChannel;
  private daemonProcess: cp.ChildProcessWithoutNullStreams | undefined;
  private daemonReader: readline.Interface | undefined;
  private requestId = 0;
  private pendingRequests = new Map<number, {
    resolve: (value: unknown) => void;
    reject: (reason?: unknown) => void;
  }>();

  constructor(context: vscode.ExtensionContext, outputChannel: vscode.OutputChannel) {
    this.outputChannel = outputChannel;
    this.analyzerPath = this.resolveAnalyzerPath(context);
  }

  private resolveAnalyzerPath(context: vscode.ExtensionContext): string {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const configPath = config.get<string>('goAnalyzerPath');

    if (configPath && fs.existsSync(configPath)) {
      return configPath;
    }

    const ext = process.platform === 'win32' ? '.exe' : '';
    const bundled = path.join(context.extensionPath, 'out', `rex-analyzer${ext}`);
    if (fs.existsSync(bundled)) {
      return bundled;
    }

    return 'rex-analyzer';
  }

  /**
   * Analyze the workspace by invoking the Go analyzer once.
   *
   * The analyzer is invoked with:
   *   -dir <workspaceRoot/sourceDir>   (absolute path to Go source)
   *   -template-root <templateRoot>     (relative to -dir or -template-base-dir)
   *   -validate
   *
   * cwd is set to workspaceRoot so relative paths in output stay predictable.
   */
  async analyzeWorkspace(workspaceRoot: string): Promise<AnalysisResult> {
    try {
      const result = await this.sendDaemonRequest<AnalysisResult>(workspaceRoot, 'analyze', this.buildAnalyzeParams(workspaceRoot));
      this.outputChannel.appendLine(
        `[Analyzer] Daemon returned ${result.renderCalls?.length ?? 0} render calls, ` +
        `${result.validationErrors?.length ?? 0} validation errors`
      );
      return result;
    } catch (err) {
      this.outputChannel.appendLine(`[Analyzer] Daemon analyze failed, falling back to CLI: ${err}`);
      return this.analyzeWorkspaceViaCli(workspaceRoot);
    }
  }

  async validateTemplate(workspaceRoot: string, absolutePath: string, content: string): Promise<GoTemplateValidationResult> {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const validationEnabled: boolean = config.get('validate') ?? true;
    if (!validationEnabled) {
      return { validationErrors: [], hasContext: false };
    }

    return this.sendDaemonRequest<GoTemplateValidationResult>(workspaceRoot, 'validateTemplate', {
      absolutePath,
      content,
    });
  }

  async updateTemplate(workspaceRoot: string, absolutePath: string, content: string): Promise<void> {
    await this.sendDaemonRequest(workspaceRoot, 'updateTemplate', {
      absolutePath,
      content,
    });
  }

  async clearTemplate(workspaceRoot: string, absolutePath: string): Promise<void> {
    await this.sendDaemonRequest(workspaceRoot, 'clearTemplate', {
      absolutePath,
    });
  }

  async inferExpressionType(
    workspaceRoot: string,
    expression: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals?: Map<string, TemplateVar>,
  ): Promise<ExpressionTypeResult | null> {
    return this.sendDaemonRequest<ExpressionTypeResult | null>(workspaceRoot, 'inferExpressionType', {
      expression,
      vars: serializeTemplateVarMap(vars),
      scopeStack: serializeScopeStack(scopeStack),
      blockLocals: serializeTemplateVarMap(blockLocals),
    });
  }

  async getHoverInfo(
    workspaceRoot: string,
    absolutePath: string,
    line: number,
    col: number,
    content: string,
  ): Promise<ExpressionTypeResult | null> {
    return this.sendDaemonRequest<ExpressionTypeResult | null>(workspaceRoot, 'getHoverInfo', {
      absolutePath,
      line,
      col,
      content,
    });
  }

  dispose(): void {
    if (this.daemonProcess && !this.daemonProcess.killed) {
      const shutdownId = ++this.requestId;
      const request: AnalyzerRequest = { jsonrpc: '2.0', id: shutdownId, method: 'shutdown' };
      try {
        this.daemonProcess.stdin.write(`${JSON.stringify(request)}\n`);
      } catch {
        // Ignore shutdown errors and terminate below.
      }
      this.daemonProcess.kill();
    }

    this.daemonReader?.close();
    this.daemonReader = undefined;
    this.daemonProcess = undefined;

    for (const pending of this.pendingRequests.values()) {
      pending.reject(new Error('Analyzer daemon disposed'));
    }
    this.pendingRequests.clear();
  }

  private buildAnalyzeParams(workspaceRoot: string): Record<string, unknown> {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';
    const templateBaseDir: string = config.get('templateBaseDir') ?? '';
    const contextFile: string = config.get('contextFile') ?? '';
    const validate: boolean = config.get("validate") ?? true;

    // Resolve the Go source directory to an absolute path
    const absSourceDir = path.resolve(workspaceRoot, sourceDir);

    return {
      dir: absSourceDir,
      templateRoot,
      templateBaseDir: templateBaseDir ? path.resolve(workspaceRoot, templateBaseDir) : '',
      contextFile: contextFile ? path.resolve(workspaceRoot, contextFile) : '',
      validate,
    };
  }

  private async analyzeWorkspaceViaCli(workspaceRoot: string): Promise<AnalysisResult> {
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';
    const templateBaseDir: string = config.get('templateBaseDir') ?? '';
    const contextFile: string = config.get('contextFile') ?? '';
    const enableGZIPCompression = config.get("compress") ?? false;
    const validate: boolean = config.get("validate") ?? true;

    this.outputChannel.appendLine(`SourceDir: ${sourceDir}`)
    this.outputChannel.appendLine(`templateRoot: ${templateRoot}`)
    this.outputChannel.appendLine(`templateBaseDir: ${templateBaseDir}`)
    this.outputChannel.appendLine(`contextFile: ${contextFile}`)

    const absSourceDir = path.resolve(workspaceRoot, sourceDir);

    if (!fs.existsSync(absSourceDir)) {
      this.outputChannel.appendLine(`[Analyzer] Source dir does not exist: ${absSourceDir}`);
      return { renderCalls: [], errors: [`Source directory not found: ${absSourceDir}`] };
    }

    const args = ['-dir', absSourceDir];

    if (validate) {
      args.push('-validate')
    }

    // Only pass template-base-dir if explicitly set, otherwise let Go analyzer default to sourceDir
    if (templateBaseDir) {
      args.push('-template-base-dir', templateBaseDir || workspaceRoot);
    }

    if (enableGZIPCompression) {
      args.push("-compress");
    }

    if (templateRoot) {
      args.push('-template-root', templateRoot);
    }

    if (contextFile) {
      const absContextFile = path.resolve(workspaceRoot, contextFile);
      if (fs.existsSync(absContextFile)) {
        args.push('-context-file', absContextFile);
      } else {
        this.outputChannel.appendLine(`[Analyzer] Context file not found: ${absContextFile}`);
      }
    }

    this.outputChannel.appendLine(`[Analyzer] Running: ${this.analyzerPath} ${args.join(' ')}`);

    return new Promise((resolve) => {
      let stdout = '';
      let stderr = '';

      const proc = cp.spawn(this.analyzerPath, args, {
        cwd: workspaceRoot,
        env: process.env,
      });

      // If compression is enabled, decompress first
      if (enableGZIPCompression) {
        const gunzip = createGunzip();

        proc.stdout.pipe(gunzip);

        gunzip.on('data', (chunk: Buffer) => {
          stdout += chunk.toString();
        });

        gunzip.on('error', (err) => {
          this.outputChannel.appendLine(`[Analyzer] Gunzip error: ${err.message}`);
          resolve({
            renderCalls: [],
            errors: [`Failed to decompress analyzer output: ${err.message}`],
          });
        });

      } else {
        // Normal (uncompressed) stdout
        proc.stdout.on('data', (data: Buffer) => {
          stdout += data.toString();
        });
      }

      proc.stderr.on('data', (data: Buffer) => {
        stderr += data.toString();
      });

      proc.on('error', (err) => {
        this.outputChannel.appendLine(`[Analyzer] Failed to spawn: ${err.message}`);
        resolve({
          renderCalls: [],
          errors: [`Failed to run analyzer: ${err.message}. Make sure rex-analyzer is built.`],
        });
      });

      proc.on('close', (code) => {
        if (stderr) {
          this.outputChannel.appendLine(`[Analyzer] stderr: ${stderr}`);
        }

        if (code !== 0 && !stdout) {
          resolve({
            renderCalls: [],
            errors: [`Analyzer exited with code ${code}: ${stderr}`],
          });
          return;
        }

        try {
          const result: AnalysisResult = JSON.parse(stdout);

          this.outputChannel.appendLine(
            `[Analyzer] Found ${result.renderCalls?.length ?? 0} render calls, ` +
            `${result.validationErrors?.length ?? 0} validation errors`
          );

          resolve(result);

        } catch (e) {
          this.outputChannel.appendLine(`[Analyzer] JSON parse error: ${e}`);
          this.outputChannel.appendLine(`[Analyzer] Raw output: ${stdout.slice(0, 500)}`);

          resolve({
            renderCalls: [],
            errors: [`Failed to parse analyzer output: ${e}`],
          });
        }
      });
    });
  }

  private async sendDaemonRequest<T>(workspaceRoot: string, method: string, params?: Record<string, unknown>): Promise<T> {
    await this.ensureDaemonStarted(workspaceRoot);

    if (!this.daemonProcess) {
      throw new Error('Analyzer daemon is not running');
    }

    const id = ++this.requestId;
    const request: AnalyzerRequest = {
      jsonrpc: '2.0',
      id,
      method,
      params,
    };

    return new Promise<T>((resolve, reject) => {
      this.pendingRequests.set(id, {
        resolve: (value) => resolve(value as T),
        reject,
      });
      this.daemonProcess!.stdin.write(`${JSON.stringify(request)}\n`, (err) => {
        if (err) {
          this.pendingRequests.delete(id);
          reject(err);
        }
      });
    });
  }

  private async ensureDaemonStarted(workspaceRoot: string): Promise<void> {
    if (this.daemonProcess && !this.daemonProcess.killed) {
      return;
    }

    this.outputChannel.appendLine('[Analyzer] Starting daemon...');
    const proc = cp.spawn(this.analyzerPath, ['-daemon'], {
      cwd: workspaceRoot,
      env: process.env,
      stdio: 'pipe',
    });

    proc.stderr.on('data', (data: Buffer) => {
      this.outputChannel.appendLine(`[Analyzer] daemon stderr: ${data.toString()}`);
    });

    proc.on('error', (err) => {
      this.outputChannel.appendLine(`[Analyzer] Daemon failed to start: ${err.message}`);
      for (const pending of this.pendingRequests.values()) {
        pending.reject(err);
      }
      this.pendingRequests.clear();
    });

    proc.on('exit', (code, signal) => {
      this.outputChannel.appendLine(`[Analyzer] Daemon exited (code=${code}, signal=${signal ?? 'none'})`);
      if (this.daemonReader) {
        this.daemonReader.close();
        this.daemonReader = undefined;
      }
      this.daemonProcess = undefined;
      for (const pending of this.pendingRequests.values()) {
        pending.reject(new Error(`Analyzer daemon exited before responding`));
      }
      this.pendingRequests.clear();
    });

    const reader = readline.createInterface({ input: proc.stdout, crlfDelay: Infinity });
    reader.on('line', (line) => {
      if (!line.trim()) return;

      let response: AnalyzerResponse<unknown>;
      try {
        response = JSON.parse(line) as AnalyzerResponse<unknown>;
      } catch (err) {
        this.outputChannel.appendLine(`[Analyzer] Invalid daemon response: ${err}`);
        return;
      }

      const pending = this.pendingRequests.get(response.id);
      if (!pending) {
        return;
      }

      this.pendingRequests.delete(response.id);
      if (response.error) {
        pending.reject(new Error(response.error.message));
        return;
      }

      pending.resolve(response.result as unknown);
    });

    this.daemonProcess = proc;
    this.daemonReader = reader;
  }
}

function serializeTemplateVarMap(vars?: Map<string, TemplateVar>): Record<string, TemplateVar> {
  if (!vars) return {};
  return Object.fromEntries(vars.entries());
}

function serializeScopeStack(scopeStack: ScopeFrame[]): Array<Omit<ScopeFrame, 'locals'> & { locals?: Record<string, TemplateVar> }> {
  return scopeStack.map(frame => ({
    ...frame,
    locals: frame.locals ? Object.fromEntries(frame.locals.entries()) : undefined,
  }));
}
