import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as path from 'path';
import * as fs from 'fs';
import * as util from 'util';

const exec = util.promisify(cp.exec);
const MODULE_PATH = 'analyzer@latest';
const BINARY_NAME = process.platform === 'win32' ? 'gotpl-analyzer.exe' : 'gotpl-analyzer';

export class AnalyzerInstaller {
    /**
     * Finds the GOPATH/bin directory using the `go env` command.
     */
    static async getGoBinPath(): Promise<string | null> {
        try {
            const { stdout } = await exec('go env GOPATH');
            const firstGoPath = stdout.trim().split(path.delimiter)[0];
            return path.join(firstGoPath, 'bin');
        } catch (err) {
            return null;
        }
    }

    /**
     * Builds the analyzer locally when running in Extension Development Mode.
     */
    static async buildLocalAnalyzer(context: vscode.ExtensionContext, outputChannel: vscode.OutputChannel): Promise<string | null> {
        // workspace structure: /repo/extension and /repo/analyzer
        const analyzerSourceDir = path.join(context.extensionPath, '..', 'analyzer');
        const outputBinary = path.join(analyzerSourceDir, BINARY_NAME);

        if (!fs.existsSync(analyzerSourceDir)) {
            outputChannel.appendLine(`[Installer] Local analyzer source not found at ${analyzerSourceDir}`);
            return null;
        }

        outputChannel.appendLine('[Installer] Development mode detected. Building local analyzer...');
        try {
            await exec(`go build -o ${BINARY_NAME} .`, { cwd: analyzerSourceDir });
            outputChannel.appendLine('[Installer] Local build successful.');
            return outputBinary;
        } catch (err) {
            outputChannel.appendLine(`[Installer] Local build failed: ${err}`);
            vscode.window.showErrorMessage('Failed to build local analyzer. Check output channel.');
            return null;
        }
    }

    static async getAnalyzerPath(): Promise<string | null> {
        const goBin = await this.getGoBinPath();
        if (!goBin) return null;

        const analyzerPath = path.join(goBin, BINARY_NAME);
        if (fs.existsSync(analyzerPath)) return analyzerPath;

        try {
            const command = process.platform === 'win32' ? 'where' : 'which';
            const { stdout } = await exec(`${command} ${BINARY_NAME}`);
            if (stdout.trim()) return stdout.trim().split('\n')[0];
        } catch (e) { }

        return null;
    }

    /**
     * Resolves the path to the analyzer, building or installing it if necessary.
     */
    static async ensureInstalled(context: vscode.ExtensionContext, outputChannel: vscode.OutputChannel): Promise<string | null> {
        // 1. Always respect user's manual configuration override first
        const configPath = vscode.workspace.getConfiguration('gotpl').get<string>('goAnalyzerPath');
        if (configPath && fs.existsSync(configPath)) {
            return configPath;
        }

        // 2. If running via F5 (Development), build from local source automatically
        if (context.extensionMode === vscode.ExtensionMode.Development) {
            const localBin = await this.buildLocalAnalyzer(context, outputChannel);
            if (localBin) return localBin;
            // Fallback to normal behavior if local build fails
        }

        // 3. Production mode: check if already installed via `go install`
        let analyzerPath = await this.getAnalyzerPath();
        if (analyzerPath) return analyzerPath;

        // 4. Prompt user to install
        const installItem = 'Install';
        const selection = await vscode.window.showInformationMessage(
            `The Go Template LSP requires the '${BINARY_NAME}' tool. Would you like to install it now?`,
            installItem
        );

        if (selection !== installItem) {
            vscode.window.showWarningMessage('Go Template LSP features will be disabled until the analyzer is installed.');
            return null;
        }

        // 5. Run `go install`
        return await vscode.window.withProgress({
            location: vscode.ProgressLocation.Notification,
            title: 'Installing gotpl-analyzer...',
            cancellable: false
        }, async () => {
            try {
                outputChannel.appendLine(`[Installer] Running: go install ${MODULE_PATH}`);
                await exec(`go install ${MODULE_PATH}`);

                analyzerPath = await this.getAnalyzerPath();
                if (!analyzerPath) throw new Error('Binary not found in GOPATH/bin.');

                vscode.window.showInformationMessage('Go Template LSP analyzer installed successfully!');
                return analyzerPath;
            } catch (err) {
                outputChannel.appendLine(`[Installer] Failed to install: ${err}`);
                vscode.window.showErrorMessage(`Failed to install gotpl-analyzer. Try running 'go install ${MODULE_PATH}' manually.`);
                return null;
            }
        });
    }
}
