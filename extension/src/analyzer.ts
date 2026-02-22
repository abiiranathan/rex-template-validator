import * as cp from 'child_process';
import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import { createGunzip } from 'zlib';

import { AnalysisResult } from './types';

export class GoAnalyzer {
  private analyzerPath: string;
  private outputChannel: vscode.OutputChannel;

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
    const config = vscode.workspace.getConfiguration('rex-analyzer');
    const sourceDir: string = config.get('sourceDir') ?? '.';
    const templateRoot: string = config.get('templateRoot') ?? '';
    const templateBaseDir: string = config.get('templateBaseDir') ?? '';
    const contextFile: string = config.get('contextFile') ?? '';
    const enableGZIPCompression = config.get("compress") ?? false;

    this.outputChannel.appendLine(`SourceDir: ${sourceDir}`)
    this.outputChannel.appendLine(`templateRoot: ${templateRoot}`)
    this.outputChannel.appendLine(`templateBaseDir: ${templateBaseDir}`)
    this.outputChannel.appendLine(`contextFile: ${contextFile}`)

    // Resolve the Go source directory to an absolute path
    const absSourceDir = path.resolve(workspaceRoot, sourceDir);

    if (!fs.existsSync(absSourceDir)) {
      this.outputChannel.appendLine(`[Analyzer] Source dir does not exist: ${absSourceDir}`);
      return { renderCalls: [], errors: [`Source directory not found: ${absSourceDir}`] };
    }

    const args = ['-dir', absSourceDir, '-validate'];

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
}
