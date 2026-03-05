/**
 * Package referenceProvider implements the VS Code find-references provider for
 * Rex templates.  When the cursor is on a block name inside any of:
 *
 *   {{ template "block_name" . }}   ← call site
 *   {{ block   "block_name" . }}   ← declaration + call site
 *   {{ define  "block_name" }}     ← declaration
 *
 * it returns every {{ template "block_name" … }} call site across the whole
 * workspace, optionally including the declaration(s) themselves.
 */

import * as fs from 'fs';
import * as vscode from 'vscode';
import { TemplateNode, TemplateContext } from './types';
import { TemplateParser } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';

export class ReferenceProvider {
    private readonly parser: TemplateParser;
    private readonly graphBuilder: KnowledgeGraphBuilder;

    constructor(graphBuilder: KnowledgeGraphBuilder) {
        this.graphBuilder = graphBuilder;
        this.parser = new TemplateParser();
    }

    // ── Public API ─────────────────────────────────────────────────────────────

    /**
     * Returns all reference locations for the named block under the cursor.
     * Returns null when the cursor is not on a block name.
     *
     * @param includeDeclaration - mirrors the VS Code ReferenceContext flag; when
     *   true the {{ define }} / {{ block }} declaration(s) are included in results.
     */
    async getReferences(
        document: vscode.TextDocument,
        position: vscode.Position,
        includeDeclaration: boolean
    ): Promise<vscode.Location[] | null> {
        const content = document.getText();
        const nodes = this.parser.parse(content);

        const blockName = this.findBlockNameAtPosition(nodes, position);
        if (!blockName) return null;

        return this.findAllReferences(blockName, includeDeclaration);
    }

    // ── Block-name detection ───────────────────────────────────────────────────

    /**
     * Walks the AST looking for a template/block/define node whose quoted name
     * spans the cursor position.  Returns the name string or null.
     *
     * This mirrors the logic already used by HoverProvider.findTemplateNameHover
     * and DefinitionProvider.findPartialDefinitionAtPosition.
     */
    private findBlockNameAtPosition(
        nodes: TemplateNode[],
        position: vscode.Position
    ): string | null {
        for (const node of nodes) {
            if (
                (node.kind === 'partial' || node.kind === 'block' || node.kind === 'define') &&
                (node.partialName || node.blockName)
            ) {
                const name = (node.partialName || node.blockName)!;

                // Locate the quoted name inside the raw tag text, e.g.
                //   {{ template "my-block" . }}
                //            ^^^^^^^^^^^
                const quoteIndex = node.rawText.indexOf(`"${name}"`);
                if (quoteIndex === -1) continue;

                // quoteIndex is relative to the start of rawText.
                // node.col is 1-based, so the absolute start column of the opening
                // quote is:  (node.col - 1) + quoteIndex
                // The name itself starts one character later (after the opening quote).
                const nameStartCol = (node.col - 1) + quoteIndex + 1;
                const nameEndCol = nameStartCol + name.length;
                const nodeLine = node.line - 1; // convert to 0-based

                if (
                    position.line === nodeLine &&
                    position.character >= nameStartCol &&
                    position.character <= nameEndCol
                ) {
                    return name;
                }
            }

            if (node.children) {
                const found = this.findBlockNameAtPosition(node.children, position);
                if (found) return found;
            }
            if (node.elseChildren) {
                const found = this.findBlockNameAtPosition(node.elseChildren, position);
                if (found) return found;
            }
        }
        return null;
    }

    // ── Reference search ──────────────────────────────────────────────────────

    /**
     * Scans every known template file (from the knowledge graph) for call sites
     * of `blockName`, then optionally appends declaration locations.
     */
    private async findAllReferences(
        blockName: string,
        includeDeclaration: boolean
    ): Promise<vscode.Location[]> {
        const graph = this.graphBuilder.getGraph();
        const locations: vscode.Location[] = [];

        // Collect the absolute paths of every template file we know about.
        const filePaths = new Set<string>();
        for (const [, ctx] of graph.templates) {
            if (ctx.absolutePath) filePaths.add(ctx.absolutePath);
        }
        // Also include files that only contain named blocks (no Go render call).
        for (const [, entries] of graph.namedBlocks) {
            for (const entry of entries) {
                if (entry.absolutePath) filePaths.add(entry.absolutePath);
            }
        }

        for (const filePath of filePaths) {
            if (!fs.existsSync(filePath)) continue;

            let content: string;
            try {
                const openDoc = vscode.workspace.textDocuments.find(
                    d => d.uri.fsPath === filePath
                );
                content = openDoc ? openDoc.getText() : fs.readFileSync(filePath, 'utf8');
            } catch {
                continue;
            }

            const nodes = this.parser.parse(content);
            this.collectLocations(nodes, blockName, filePath, includeDeclaration, locations);
        }

        return locations;
    }

    /**
     * Recursively walks `nodes`, appending a Location for every node whose name
     * matches `blockName`.
     *
     * - `partial` nodes  → call sites  (always included)
     * - `block`/`define` → declarations (included only when includeDeclaration)
     */
    private collectLocations(
        nodes: TemplateNode[],
        blockName: string,
        filePath: string,
        includeDeclaration: boolean,
        out: vscode.Location[]
    ): void {
        for (const node of nodes) {
            const isCallSite = node.kind === 'partial' && node.partialName === blockName;
            const isDeclaration =
                (node.kind === 'block' || node.kind === 'define') &&
                node.blockName === blockName;

            if (isCallSite || (isDeclaration && includeDeclaration)) {
                const loc = this.locationForNode(node, blockName, filePath);
                if (loc) out.push(loc);
            }

            if (node.children) {
                this.collectLocations(
                    node.children, blockName, filePath, includeDeclaration, out
                );
            }
            if (node.elseChildren) {
                this.collectLocations(
                    node.elseChildren, blockName, filePath, includeDeclaration, out
                );
            }
        }
    }

    /**
     * Builds a vscode.Location that covers only the quoted name portion of the
     * raw tag text, e.g. the `"my-block"` part of `{{ template "my-block" . }}`.
     * Falls back to the opening character of the whole tag when the quote cannot
     * be located.
     */
    private locationForNode(
        node: TemplateNode,
        blockName: string,
        filePath: string
    ): vscode.Location | null {
        const line = node.line - 1; // 0-based

        const quoteIndex = node.rawText.indexOf(`"${blockName}"`);
        if (quoteIndex !== -1) {
            // Point at just the name (inside the quotes).
            const nameStartCol = (node.col - 1) + quoteIndex + 1;
            const nameEndCol = nameStartCol + blockName.length;
            return new vscode.Location(
                vscode.Uri.file(filePath),
                new vscode.Range(line, nameStartCol, line, nameEndCol)
            );
        }

        // Fallback: point at the start of the whole tag.
        return new vscode.Location(
            vscode.Uri.file(filePath),
            new vscode.Position(line, Math.max(0, node.col - 1))
        );
    }
}
