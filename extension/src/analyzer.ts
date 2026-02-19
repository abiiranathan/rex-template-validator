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

    // Bundled binary
    const ext = process.platform === 'win32' ? '.exe' : '';
    const bundled = path.join(context.extensionPath, 'out', `rex-analyzer${ext}`);
    if (fs.existsSync(bundled)) {
      return bundled;
    }

    // Try system PATH
    return 'rex-analyzer';
  }

  /**
   * Analyze a Go source directory. Returns parsed render calls with type info.
   */
  async analyzeDirectory(dir: string): Promise<AnalysisResult> {
    return new Promise((resolve) => {
      const config = vscode.workspace.getConfiguration('rexTemplateValidator');
      const templateRoot = config.get<string>('templateRoot') || '';
      
      const args = ['-dir', dir, '-template-root', templateRoot, '-validate'];
      this.outputChannel.appendLine(`[Analyzer] Running: ${this.analyzerPath} ${args.join(' ')}`);

      let stdout = '';
      let stderr = '';

      const proc = cp.spawn(this.analyzerPath, args, {
        cwd: dir,
        env: process.env,
      });

      proc.stdout.on('data', (data: Buffer) => {
        stdout += data.toString();
      });

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
            `[Analyzer] Found ${result.renderCalls?.length ?? 0} render calls`
          );
          resolve(result);
        } catch (e) {
          this.outputChannel.appendLine(`[Analyzer] JSON parse error: ${e}`);
          resolve({
            renderCalls: [],
            errors: [`Failed to parse analyzer output: ${e}`],
          });
        }
      });
    });
  }

  /**
   * Run the analyzer across multiple directories (e.g. all packages in workspace)
   */
  async analyzeWorkspace(workspaceRoot: string): Promise<AnalysisResult> {
    const goPackageDirs = await this.findGoPackageDirs(workspaceRoot);
    this.outputChannel.appendLine(`[Analyzer] Scanning ${goPackageDirs.length} Go package dirs`);

    const results = await Promise.all(goPackageDirs.map((d) => this.analyzeDirectory(d)));

    // Merge
    const merged: AnalysisResult = { renderCalls: [], errors: [], validationErrors: [] };
    for (const r of results) {
      merged.renderCalls.push(...(r.renderCalls ?? []));
      merged.errors.push(...(r.errors ?? []));
      merged.validationErrors?.push(...(r.validationErrors ?? []));
    }

    return merged;
  }

  private async findGoPackageDirs(root: string): Promise<string[]> {
    return new Promise((resolve) => {
      const dirs = new Set<string>();

      const walk = (dir: string) => {
        try {
          const entries = fs.readdirSync(dir, { withFileTypes: true });
          let hasGo = false;

          for (const e of entries) {
            if (e.isDirectory()) {
              if (!['vendor', 'node_modules', '.git', 'testdata'].includes(e.name)) {
                walk(path.join(dir, e.name));
              }
            } else if (e.name.endsWith('.go') && !e.name.endsWith('_test.go')) {
              hasGo = true;
            }
          }

          if (hasGo) {
            dirs.add(dir);
          }
        } catch {
          // Skip unreadable dirs
        }
      };

      walk(root);
      resolve(Array.from(dirs));
    });
  }
}
