/**
 * lspClient.ts — JSON-RPC 2.0 client that talks to the rex-analyzer LSP daemon.
 *
 * The daemon is launched with the `--lsp` flag and communicates over stdin/stdout
 * using the standard Content-Length framing from the Language Server Protocol.
 * All custom methods live under the `rex/` namespace.
 */

import * as cp from 'child_process';
import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';

// ── Protocol mirror types (matches Go lsp/protocol.go) ───────────────────────

export interface FieldInfoJSON {
    name: string;
    type: string;
    fields?: FieldInfoJSON[];
    isSlice: boolean;
    isMap?: boolean;
    keyType?: string;
    elemType?: string;
    params?: ParamInfoJSON[];
    returns?: ParamInfoJSON[];
    defFile?: string;
    defLine?: number;
    defCol?: number;
    doc?: string;
}

export interface ParamInfoJSON {
    name?: string;
    type: string;
    fields?: FieldInfoJSON[];
}

export interface TemplateVarJSON {
    name: string;
    type: string;
    fields?: FieldInfoJSON[];
    isSlice: boolean;
    isMap?: boolean;
    keyType?: string;
    elemType?: string;
    defFile?: string;
    defLine?: number;
    defCol?: number;
    doc?: string;
}

export interface FuncMapInfoJSON {
    name: string;
    params?: ParamInfoJSON[];
    returns?: ParamInfoJSON[];
    doc?: string;
    defFile?: string;
    defLine?: number;
    defCol?: number;
    returnTypeFields?: FieldInfoJSON[];
}

export interface ValidationResultJSON {
    template: string;
    line: number;
    column: number;
    variable: string;
    message: string;
    severity: string;
    goFile?: string;
    goLine?: number;
    templateNameStartCol?: number;
    templateNameEndCol?: number;
}

export interface NamedBlockEntryJSON {
    name: string;
    absolutePath: string;
    templatePath: string;
    line: number;
    col: number;
}

export interface NamedBlockDuplicateErrorJSON {
    name: string;
    entries: NamedBlockEntryJSON[];
    message: string;
}

// ── Request / response shapes ─────────────────────────────────────────────────

export interface GetTemplateContextParams {
    dir: string;
    templateName: string;
    templateRoot?: string;
    contextFile?: string;
}
export interface GetTemplateContextResult {
    vars: TemplateVarJSON[];
    errors?: string[];
}

export interface ValidateParams {
    dir: string;
    templateName: string;
    templateRoot?: string;
    contextFile?: string;
}
export interface ValidateResult {
    errors: ValidationResultJSON[];
}

export interface GetFuncMapsParams {
    dir: string;
    contextFile?: string;
}
export interface GetFuncMapsResult {
    funcMaps: FuncMapInfoJSON[];
    errors?: string[];
}

export interface GetNamedBlocksParams {
    dir: string;
    templateRoot?: string;
}
export interface GetNamedBlocksResult {
    namedBlocks: Record<string, NamedBlockEntryJSON[]>;
    duplicateErrors?: NamedBlockDuplicateErrorJSON[];
}

// ── Client ────────────────────────────────────────────────────────────────────

const REQUEST_TIMEOUT_MS = 15_000;

export class LspClient {
    private proc?: cp.ChildProcess;
    private readBuffer: Buffer = Buffer.alloc(0);
    private pending = new Map<
        number,
        { resolve: (v: unknown) => void; reject: (e: Error) => void; timer: NodeJS.Timeout }
    >();
    private nextId = 1;
    private _started = false;

    constructor(
        private readonly binaryPath: string,
        private readonly outputChannel: vscode.OutputChannel
    ) { }

    get started(): boolean { return this._started; }

    // ── Lifecycle ───────────────────────────────────────────────────────────────

    async start(): Promise<void> {
        if (!fs.existsSync(this.binaryPath)) {
            throw new Error(`rex-analyzer binary not found: ${this.binaryPath}`);
        }

        this.proc = cp.spawn(this.binaryPath, ['--lsp'], { env: process.env });

        this.proc.stdout!.on('data', (chunk: Buffer) => {
            this.readBuffer = Buffer.concat([this.readBuffer, chunk]);
            this.drainMessages();
        });

        this.proc.stderr!.on('data', (data: Buffer) => {
            this.outputChannel.appendLine(`[LSP] ${data.toString().trimEnd()}`);
        });

        this.proc.on('error', (err) => {
            this.outputChannel.appendLine(`[LSP] Process error: ${err.message}`);
            this.rejectAll(err);
        });

        this.proc.on('close', (code) => {
            if (this._started) {
                this.outputChannel.appendLine(`[LSP] Process exited (code ${code})`);
            }
            this._started = false;
            this.rejectAll(new Error(`LSP process exited with code ${code}`));
        });

        // Handshake
        await this.request<unknown>('initialize', { rootUri: null });
        this.notify('initialized', {});
        this._started = true;
        this.outputChannel.appendLine('[LSP] Daemon started');
    }

    dispose(): void {
        if (!this.proc) return;
        try {
            this.notify('shutdown', null);
            this.notify('exit', null);
        } catch { /* ignore */ }
        this._started = false;
        this.proc.kill();
        this.proc = undefined;
    }

    // ── Transport ────────────────────────────────────────────────────────────────

    private drainMessages(): void {
        while (true) {
            const sep = this.readBuffer.indexOf('\r\n\r\n');
            if (sep === -1) break;

            const header = this.readBuffer.subarray(0, sep).toString('ascii');
            const m = header.match(/Content-Length:\s*(\d+)/i);
            if (!m) {
                // Malformed header; skip past separator and continue.
                this.readBuffer = this.readBuffer.subarray(sep + 4);
                continue;
            }

            const contentLen = parseInt(m[1], 10);
            const bodyStart = sep + 4;
            if (this.readBuffer.length < bodyStart + contentLen) break; // wait for more data

            const body = this.readBuffer.subarray(bodyStart, bodyStart + contentLen).toString('utf8');
            this.readBuffer = this.readBuffer.subarray(bodyStart + contentLen);

            try {
                this.handleMessage(JSON.parse(body));
            } catch (e) {
                this.outputChannel.appendLine(`[LSP] Message parse error: ${e}`);
            }
        }
    }

    private handleMessage(msg: Record<string, unknown>): void {
        // Only handle responses (notifications have no id or null id)
        if (msg.id === undefined || msg.id === null) return;
        const id = msg.id as number;
        const entry = this.pending.get(id);
        if (!entry) return;

        clearTimeout(entry.timer);
        this.pending.delete(id);

        if (msg.error) {
            const err = msg.error as { message: string };
            entry.reject(new Error(err.message));
        } else {
            entry.resolve(msg.result);
        }
    }

    private writeFrame(obj: unknown): void {
        if (!this.proc?.stdin) throw new Error('LSP process not running');
        const body = JSON.stringify(obj);
        const frame = `Content-Length: ${Buffer.byteLength(body, 'utf8')}\r\n\r\n${body}`;
        this.proc.stdin.write(frame);
    }

    async request<T>(method: string, params: unknown): Promise<T> {
        const id = this.nextId++;
        return new Promise<T>((resolve, reject) => {
            const timer = setTimeout(() => {
                this.pending.delete(id);
                reject(new Error(`LSP request "${method}" timed out`));
            }, REQUEST_TIMEOUT_MS);

            this.pending.set(id, {
                resolve: (v) => resolve(v as T),
                reject,
                timer,
            });

            try {
                this.writeFrame({ jsonrpc: '2.0', id, method, params });
            } catch (e) {
                clearTimeout(timer);
                this.pending.delete(id);
                reject(e);
            }
        });
    }

    notify(method: string, params: unknown): void {
        try {
            this.writeFrame({ jsonrpc: '2.0', method, params });
        } catch { /* ignore if process died */ }
    }

    private rejectAll(err: Error): void {
        for (const [id, entry] of this.pending) {
            clearTimeout(entry.timer);
            entry.reject(err);
        }
        this.pending.clear();
    }

    // ── High-level API ────────────────────────────────────────────────────────────

    async getTemplateContext(p: GetTemplateContextParams): Promise<GetTemplateContextResult> {
        return this.request<GetTemplateContextResult>('rex/getTemplateContext', p);
    }

    async validate(p: ValidateParams): Promise<ValidateResult> {
        return this.request<ValidateResult>('rex/validate', p);
    }

    async getFuncMaps(p: GetFuncMapsParams): Promise<GetFuncMapsResult> {
        return this.request<GetFuncMapsResult>('rex/getFuncMaps', p);
    }

    async getNamedBlocks(p: GetNamedBlocksParams): Promise<GetNamedBlocksResult> {
        return this.request<GetNamedBlocksResult>('rex/getNamedBlocks', p);
    }

    /** Notify the daemon that Go source files have changed so it can invalidate caches. */
    notifyFileChanges(absolutePaths: string[]): void {
        const changes = absolutePaths.map(p => ({
            uri: pathToFileUri(p),
            type: 2, // changed
        }));
        this.notify('workspace/didChangeWatchedFiles', { changes });
    }

    /** Explicitly invalidate the server-side cache for a directory. */
    invalidateDir(dir: string): void {
        this.notify('rex/invalidateCache', { dir });
    }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

export function pathToFileUri(p: string): string {
    const normalised = p.replace(/\\/g, '/');
    return normalised.startsWith('/')
        ? `file://${normalised}`
        : `file:///${normalised}`;
}
