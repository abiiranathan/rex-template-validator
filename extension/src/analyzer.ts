import * as cp from 'child_process';
import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import { AnalysisResult } from './types';

export class GoAnalyzer {
  private analyzerPath: string;
  private outputChannel: vscode.OutputChannel;

  constructor(context: vscode.ExtensionContext, outputChannel: vscode.OutputChannel) {
    this.outputChannel = outputChannel;
    this.analyzerPath = this.resolveAnalyzerPath(context);
  }

  private resolveAnalyzerPath(context: vscode.ExtensionContext): string {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
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
   *   -template-root <templateRoot>     (relative to -dir)
   *   -validate
   *
   * cwd is set to workspaceRoot so relative paths in output stay predictable.
   */
  async analyzeWorkspace(workspaceRoot: string): Promise<AnalysisResult> {
    const config = vscode.workspace.getConfiguration('rexTemplateValidator');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';

    // Resolve the Go source directory to an absolute path
    const absSourceDir = path.resolve(workspaceRoot, sourceDir);

    if (!fs.existsSync(absSourceDir)) {
      this.outputChannel.appendLine(`[Analyzer] Source dir does not exist: ${absSourceDir}`);
      return { renderCalls: [], errors: [`Source directory not found: ${absSourceDir}`] };
    }

    const args = ['-dir', absSourceDir, '-validate'];
    if (templateRoot) {
      args.push('-template-root', templateRoot);
    }

    this.outputChannel.appendLine(`[Analyzer] Running: ${this.analyzerPath} ${args.join(' ')}`);

    return new Promise((resolve) => {
      let stdout = '';
      let stderr = '';

      const proc = cp.spawn(this.analyzerPath, args, {
        // cwd is the workspace root so relative file paths in output are stable
        cwd: workspaceRoot,
        env: process.env,
      });

      proc.stdout.on('data', (data: Buffer) => { stdout += data.toString(); });
      proc.stderr.on('data', (data: Buffer) => { stderr += data.toString(); });

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
}
