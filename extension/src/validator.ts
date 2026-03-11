/**
 * Package validator provides the editor-facing TemplateValidator façade.
 * Diagnostics are produced by the Go analyzer daemon; this class keeps the
 * TypeScript-side language features for hover, completion, definitions, and
 * references.
 *
 * Named blocks ({{ define "name" }} / {{ block "name" ... }}) are resolved
 * from a cross-file NamedBlockRegistry built by KnowledgeGraphBuilder. This means
 * intellisense works correctly inside a named
 * block even when it lives in a different file from the template that calls it.
 *
 * Duplicate block-name detection: if the same name is declared in more than one
 * file, a diagnostic error is surfaced on every call-site that references it.
 */

import * as vscode from 'vscode';
import { TemplateContext } from './types';
import { TemplateParser } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { ScopeUtils } from './scopeUtils';
import { HoverProvider } from './hoverProvider';
import { DefinitionProvider } from './definitionProvider';
import { CompletionProvider } from './completionProvider';
import { ReferenceProvider } from './referenceProvider';
import { GoAnalyzer } from './analyzer';

export class TemplateValidator {
    private readonly parser: TemplateParser;
    private readonly graphBuilder: KnowledgeGraphBuilder;
    private readonly outputChannel: vscode.OutputChannel;
    private readonly analyzer: GoAnalyzer;

    // Role-based providers assembled once at construction.
    private readonly scope: ScopeUtils;
    private readonly hover: HoverProvider;
    private readonly definition: DefinitionProvider;
    private readonly completion: CompletionProvider;
    private readonly reference: ReferenceProvider;

    constructor(
        outputChannel: vscode.OutputChannel,
        graphBuilder: KnowledgeGraphBuilder,
        analyzer: GoAnalyzer,
    ) {
        this.outputChannel = outputChannel;
        this.graphBuilder = graphBuilder;
        this.analyzer = analyzer;
        this.parser = new TemplateParser();

        this.scope = new ScopeUtils(this.parser, graphBuilder);
        this.hover = new HoverProvider(graphBuilder, this.scope, analyzer);
        this.definition = new DefinitionProvider(graphBuilder, this.scope, this.hover);
        this.completion = new CompletionProvider(graphBuilder, this.scope, analyzer);
        this.reference = new ReferenceProvider(graphBuilder);
    }

    // ── Public API ─────────────────────────────────────────────────────────────

    /**
 * Returns all reference locations for the named block under the cursor,
 * or null when the cursor is not on a block name.
 */
    async getReferences(
        document: vscode.TextDocument,
        position: vscode.Position,
        includeDeclaration: boolean
    ): Promise<vscode.Location[] | null> {
        return this.reference.getReferences(document, position, includeDeclaration);
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
     * Returns the definition location when the cursor is on a Render() template
     * path string inside a Go source file.  Returns null otherwise.
     */
    getTemplateDefinitionFromGo(
        document: vscode.TextDocument,
        position: vscode.Position
    ): vscode.Location | null {
        return this.definition.getTemplateDefinitionFromGo(document, position);
    }
}
