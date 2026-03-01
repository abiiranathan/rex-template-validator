/**
 * Package validator provides the public TemplateValidator façade for the Rex Template Validator.
 * It assembles the role-based providers and delegates every public method to them.
 *
 * Named blocks ({{ define "name" }} / {{ block "name" ... }}) are resolved
 * from a cross-file NamedBlockRegistry built by KnowledgeGraphBuilder. This means
 * intellisense (hover, completion, validation) works correctly inside a named
 * block even when it lives in a different file from the template that calls it.
 *
 * Duplicate block-name detection: if the same name is declared in more than one
 * file, a diagnostic error is surfaced on every call-site that references it.
 */

import * as vscode from 'vscode';
import {
    TemplateContext,
    ValidationError,
} from './types';
import { TemplateParser } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { ScopeUtils } from './scopeUtils';
import { ValidatorCore } from './validatorCore';
import { HoverProvider } from './hoverProvider';
import { DefinitionProvider } from './definitionProvider';
import { CompletionProvider } from './completionProvider';

export class TemplateValidator {
    private readonly parser: TemplateParser;
    private readonly graphBuilder: KnowledgeGraphBuilder;
    private readonly outputChannel: vscode.OutputChannel;

    // Role-based providers assembled once at construction.
    private readonly scope: ScopeUtils;
    private readonly core: ValidatorCore;
    private readonly hover: HoverProvider;
    private readonly definition: DefinitionProvider;
    private readonly completion: CompletionProvider;

    constructor(
        outputChannel: vscode.OutputChannel,
        graphBuilder: KnowledgeGraphBuilder
    ) {
        this.outputChannel = outputChannel;
        this.graphBuilder = graphBuilder;
        this.parser = new TemplateParser();

        this.scope = new ScopeUtils(this.parser, graphBuilder);
        this.core = new ValidatorCore(outputChannel, graphBuilder, this.scope);
        this.hover = new HoverProvider(graphBuilder, this.scope);
        this.definition = new DefinitionProvider(graphBuilder, this.scope, this.hover);
        this.completion = new CompletionProvider(graphBuilder, this.scope);
    }

    // ── Public API ─────────────────────────────────────────────────────────────

    /**
     * Validates a VS Code TextDocument against the Rex render context and returns
     * an array of Diagnostic objects suitable for pushing to a DiagnosticCollection.
     * When no context is found a single Hint diagnostic is returned.
     */
    async validateDocument(
        document: vscode.TextDocument,
        providedCtx?: TemplateContext
    ): Promise<vscode.Diagnostic[]> {
        const ctx =
            providedCtx ||
            this.graphBuilder.findContextForFile(document.uri.fsPath);

        if (!ctx) {
            return [
                new vscode.Diagnostic(
                    new vscode.Range(0, 0, 0, 0),
                    'No rex.Render() call found for this template. Run "Rex: Rebuild Template Index" if you just added it.',
                    vscode.DiagnosticSeverity.Hint
                ),
            ];
        }

        const content = document.getText();
        const errors = this.validate(content, ctx, document.uri.fsPath);

        return errors.map((e) => {
            const line = Math.max(0, e.line - 1);
            const col = Math.max(0, e.col - 1);
            const range = new vscode.Range(
                line, col, line, col + (e.variable?.length ?? 10)
            );
            const diag = new vscode.Diagnostic(
                range,
                e.message,
                e.severity === 'error'
                    ? vscode.DiagnosticSeverity.Error
                    : e.severity === 'warning'
                        ? vscode.DiagnosticSeverity.Warning
                        : vscode.DiagnosticSeverity.Information
            );
            diag.source = 'Rex';
            return diag;
        });
    }

    /**
     * Validates the raw template content string against the provided context.
     * Returns a list of ValidationError values; does not interact with VS Code APIs.
     */
    validate(
        content: string,
        ctx: TemplateContext,
        filePath: string
    ): ValidationError[] {
        return this.core.validate(content, ctx, filePath);
    }

    /**
     * Returns hover information for the element at position, or null when nothing
     * actionable is under the cursor.
     */
    async getHoverInfo(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.Hover | null> {
        return this.hover.getHoverInfo(document, position, ctx);
    }

    /**
     * Returns the definition location for the template element at position,
     * or null when no definition can be found.
     */
    async getDefinitionLocation(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.Location | null> {
        return this.definition.getDefinitionLocation(document, position, ctx);
    }

    /**
     * Returns completion items for the cursor position (async variant).
     */
    async getCompletionItems(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.CompletionItem[]> {
        return this.completion.getCompletionItems(document, position, ctx);
    }

    /**
     * Returns completion items for the cursor position (synchronous variant).
     */
    getCompletions(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): vscode.CompletionItem[] {
        return this.completion.getCompletions(document, position, ctx);
    }

    /**
     * Returns the definition location when the cursor is on a rex.Render() template
     * path string inside a Go source file.  Returns null otherwise.
     */
    getTemplateDefinitionFromGo(
        document: vscode.TextDocument,
        position: vscode.Position
    ): vscode.Location | null {
        return this.definition.getTemplateDefinitionFromGo(document, position);
    }
}
