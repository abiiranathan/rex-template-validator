import * as vscode from 'vscode';

// Centralized configuration management for the extension
// Namespace: gotpl
const extensionNamespace = 'gotpl';

class Config {
    private config: vscode.WorkspaceConfiguration;
    constructor() {
        this.config = vscode.workspace.getConfiguration(extensionNamespace);
    }

    private get<T>(name: string): T | undefined {
        return this.config.get<T>(name);
    }

    analyzerPath(): string {
        return this.get<string>('goAnalyzerPath') ?? '';
    }

    sourceDir(): string {
        return this.get<string>('sourceDir') ?? '.';
    }

    templateRoot(): string {
        return this.get<string>('templateRoot') ?? '';
    }

    templateBaseDir(): string {
        return this.get<string>('templateBaseDir') ?? '';
    }

    contextFile(): string {
        return this.get<string>('contextFile') ?? '';
    }

    debounceMs(): number {
        return this.get<number>('debounceMs') ?? 1500;
    }

    compress(): boolean {
        return this.get<boolean>('compress') ?? false;
    }

    validate(): boolean {
        return this.get<boolean>('validate') ?? true;
    }
}

export const config = new Config();
