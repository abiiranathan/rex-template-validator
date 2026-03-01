/**
 * Package definitionProvider implements the VS Code go-to-definition provider
 * for Rex templates.  It resolves template variables, fields, funcMap functions,
 * partial templates, and named blocks to their declaration sites in Go source files
 * or template files.
 */

import * as fs from 'fs';
import * as vscode from 'vscode';
import {
    FieldInfo,
    TemplateContext,
    TemplateNode,
    TemplateVar,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { ScopeUtils, isFileBasedPartial } from './scopeUtils';
import { HoverProvider } from './hoverProvider';

/**
 * DefinitionProvider resolves go-to-definition requests for template and Go source files.
 */
export class DefinitionProvider {
    private readonly parser: TemplateParser;
    private readonly graphBuilder: KnowledgeGraphBuilder;
    private readonly scope: ScopeUtils;
    private readonly hover: HoverProvider;

    constructor(
        graphBuilder: KnowledgeGraphBuilder,
        scope: ScopeUtils,
        hover: HoverProvider
    ) {
        this.graphBuilder = graphBuilder;
        this.parser = scope.parser;
        this.scope = scope;
        this.hover = hover;
    }

    // ── Public API ─────────────────────────────────────────────────────────────

    /**
     * Returns the definition location for the template element at position,
     * or null when no definition can be found.
     */
    async getDefinitionLocation(
        document: vscode.TextDocument,
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.Location | null> {
        const content = document.getText();
        const nodes = this.parser.parse(content);

        const partialLocation = await this.findPartialDefinitionAtPosition(
            nodes, position, ctx
        );
        if (partialLocation) return partialLocation;

        const hit = this.scope.findNodeAtPosition(
            nodes, position, ctx.vars, [], nodes
        );
        if (!hit) return null;

        const { node, stack, vars: hitVars, locals: hitLocals } = hit;

        let targetPath: string[] = [];

        if (node.rawText) {
            const cursorOffset = position.character - (node.col - 1);
            const subPathStr = this.hover.extractPathAtCursor(node.rawText, cursorOffset);
            if (subPathStr) {
                const funcMaps = this.graphBuilder.getGraph().funcMaps;
                if (funcMaps && funcMaps.has(subPathStr)) {
                    const fn = funcMaps.get(subPathStr)!;
                    if (fn.defFile && fn.defLine) {
                        const abs = this.resolveGoFile(fn.defFile);
                        if (abs) {
                            return new vscode.Location(
                                vscode.Uri.file(abs),
                                new vscode.Position(
                                    Math.max(0, fn.defLine - 1),
                                    (fn.defCol ?? 1) - 1
                                )
                            );
                        }
                    }
                }
                targetPath = this.parser.parseDotPath(subPathStr);
            }
        }

        if (targetPath.length === 0) {
            if (node.path.length > 0 &&
                (node.path[0] === '.' || node.path[0].startsWith('$'))) {
                targetPath = node.path;
            } else {
                return null;
            }
        }

        if (targetPath.length > 1) {
            const subResult = resolvePath(
                targetPath, hitVars, stack, hitLocals,
                this.scope.buildFieldResolver(hitVars, stack)
            );
            if (subResult.found && subResult.defFile && subResult.defLine) {
                const abs = this.resolveGoFile(subResult.defFile);
                if (abs) {
                    return new vscode.Location(
                        vscode.Uri.file(abs),
                        new vscode.Position(
                            Math.max(0, subResult.defLine - 1),
                            (subResult.defCol ?? 1) - 1
                        )
                    );
                }
            }
        }

        let pathForDef = targetPath;
        let topVarName = pathForDef[0];
        if (topVarName === '$' && pathForDef.length > 1) {
            topVarName = pathForDef[1];
            pathForDef = pathForDef.slice(1);
        }
        if (!topVarName || topVarName === '.' || topVarName === '$') return null;

        if (topVarName.startsWith('$') && topVarName !== '$') {
            const declaredVar = this.findDeclaredVariableDefinition(
                { ...node, path: targetPath },
                nodes, position, ctx, hit.stack, hit.locals
            );
            if (declaredVar) return declaredVar;
            const rangeVar = this.findRangeAssignedVariable(
                { ...node, path: targetPath }, stack, ctx
            );
            if (rangeVar) return rangeVar;
        }

        if (ctx.partialSourceVar) {
            const loc = this.findDefinitionInVar(pathForDef, ctx.partialSourceVar, ctx);
            if (loc) return loc;
        }

        const stackDefLoc = this.findDefinitionInScope(pathForDef, hitVars, stack, ctx);
        if (stackDefLoc) return stackDefLoc;

        for (const rc of ctx.renderCalls) {
            const passedVar = rc.vars.find(v => v.name === topVarName);
            if (passedVar) {
                const fieldLoc = this.navigateToFieldDefinition(
                    pathForDef.slice(1), passedVar.fields ?? [], ctx
                );
                if (fieldLoc) return fieldLoc;
                if (passedVar.defFile && passedVar.defLine) {
                    const abs = this.resolveGoFile(passedVar.defFile);
                    if (abs) {
                        return new vscode.Location(
                            vscode.Uri.file(abs),
                            new vscode.Position(
                                Math.max(0, passedVar.defLine - 1),
                                (passedVar.defCol ?? 1) - 1
                            )
                        );
                    }
                }
                if (rc.file) {
                    const abs = this.graphBuilder.resolveGoFilePath(rc.file);
                    if (abs) {
                        return new vscode.Location(
                            vscode.Uri.file(abs),
                            new vscode.Position(Math.max(0, rc.line - 1), 0)
                        );
                    }
                }
            }
        }

        return this.findRangeVariableDefinition(pathForDef, stack, ctx);
    }

    /**
     * Returns the definition location when the cursor is on a rex.Render() template
     * path string inside a Go source file.  Returns null otherwise.
     */
    getTemplateDefinitionFromGo(
        document: vscode.TextDocument,
        position: vscode.Position
    ): vscode.Location | null {
        const line = document.lineAt(position.line).text;
        const renderRegex = /\.Render\s*\(\s*"([^"]+)"/g;
        let match: RegExpExecArray | null;
        while ((match = renderRegex.exec(line)) !== null) {
            const templatePath = match[1];
            const matchStart = match.index + '.Render("'.length;
            if (
                position.character >= matchStart &&
                position.character <= matchStart + templatePath.length
            ) {
                const absPath = this.graphBuilder.resolveTemplatePath(templatePath);
                if (absPath) {
                    return new vscode.Location(
                        vscode.Uri.file(absPath), new vscode.Position(0, 0)
                    );
                }
            }
        }
        return null;
    }

    // ── Template name / partial navigation ───────────────────────────────────

    async findPartialDefinitionAtPosition(
        nodes: TemplateNode[],
        position: vscode.Position,
        ctx: TemplateContext
    ): Promise<vscode.Location | null> {
        for (const node of nodes) {
            if (
                (node.kind === 'partial' || node.kind === 'block' || node.kind === 'define') &&
                (node.partialName || node.blockName)
            ) {
                const name = node.partialName || node.blockName!;
                const nameMatch = node.rawText.match(
                    new RegExp(`\\{\\{\\s*(template|block|define)\\s+"([^"]+)"`)
                );
                if (nameMatch && nameMatch[2] === name) {
                    const nameStartCol =
                        (node.col - 1) + node.rawText.indexOf('"' + name + '"') + 1;
                    if (
                        position.line === node.line - 1 &&
                        position.character >= nameStartCol &&
                        position.character <= nameStartCol + name.length
                    ) {
                        if (isFileBasedPartial(name)) {
                            const templatePath = this.graphBuilder.resolveTemplatePath(name);
                            if (templatePath) {
                                return new vscode.Location(
                                    vscode.Uri.file(templatePath), new vscode.Position(0, 0)
                                );
                            }
                        } else {
                            return await this.findNamedBlockDefinitionLocation(name, ctx);
                        }
                    }
                }
            }
            if (node.children) {
                const found = await this.findPartialDefinitionAtPosition(
                    node.children, position, ctx
                );
                if (found) return found;
            }
        }
        return null;
    }

    private async findNamedBlockDefinitionLocation(
        name: string,
        ctx: TemplateContext
    ): Promise<vscode.Location | null> {
        const { entry } = this.scope.resolveNamedBlock(name);
        if (entry) {
            return new vscode.Location(
                vscode.Uri.file(entry.absolutePath),
                new vscode.Position(entry.line - 1, entry.col - 1)
            );
        }

        const graph = this.graphBuilder.getGraph();
        const defineRegex = new RegExp(`\\{\\{\\s*(?:define|block)\\s+"${name}"`);
        const filesToSearch = [
            ctx.absolutePath,
            ...[...graph.templates.values()]
                .map(t => t.absolutePath)
                .filter(p => p !== ctx.absolutePath),
        ];

        for (const filePath of filesToSearch) {
            try {
                const openDoc = vscode.workspace.textDocuments.find(
                    d => d.uri.fsPath === filePath
                );
                let content = openDoc ? openDoc.getText() : '';
                if (!content) {
                    if (!fs.existsSync(filePath)) continue;
                    content = await fs.promises.readFile(filePath, 'utf-8');
                }
                if (!defineRegex.test(content)) continue;
                const defNode = this.scope.findDefineNodeInAST(
                    this.parser.parse(content), name
                );
                if (defNode) {
                    return new vscode.Location(
                        vscode.Uri.file(filePath),
                        new vscode.Position(defNode.line - 1, defNode.col - 1)
                    );
                }
            } catch { /* ignore */ }
        }
        return null;
    }

    // ── Go source field/variable navigation ──────────────────────────────────

    private navigateToFieldDefinition(
        fieldPath: string[],
        fields: FieldInfo[],
        ctx: TemplateContext
    ): vscode.Location | null {
        if (fieldPath.length === 0) return null;
        let current = fields;
        for (let i = 0; i < fieldPath.length; i++) {
            if (fieldPath[i] === '[]') continue;
            const field = current.find(f => f.name === fieldPath[i]);
            if (!field) return null;
            if (i === fieldPath.length - 1) {
                if (field.defFile && field.defLine) {
                    const abs = this.resolveGoFile(field.defFile);
                    if (abs) {
                        return new vscode.Location(
                            vscode.Uri.file(abs),
                            new vscode.Position(
                                Math.max(0, field.defLine - 1),
                                (field.defCol ?? 1) - 1
                            )
                        );
                    }
                }
                return null;
            }
            current = field.fields ?? [];
        }
        return null;
    }

    private findDefinitionInVar(
        path: string[],
        sourceVar: TemplateVar,
        ctx: TemplateContext
    ): vscode.Location | null {
        let currentFields = sourceVar.fields ?? [];
        let currentTarget: TemplateVar | FieldInfo = sourceVar;
        for (const part of path) {
            if (part === '.' || part === '$' || part === '[]') continue;
            const field = currentFields.find(f => f.name === part);
            if (!field) {
                currentTarget = sourceVar;
                break;
            }
            currentTarget = field;
            currentFields = field.fields ?? [];
        }
        const t = currentTarget as TemplateVar & FieldInfo;
        if (t.defFile && t.defLine) {
            const abs = this.resolveGoFile(t.defFile);
            if (abs) {
                return new vscode.Location(
                    vscode.Uri.file(abs),
                    new vscode.Position(Math.max(0, t.defLine - 1), (t.defCol ?? 1) - 1)
                );
            }
        }
        return null;
    }

    private resolveGoFile(filePath: string): string | null {
        if (fs.existsSync(filePath) && require('path').isAbsolute(filePath)) return filePath;
        return this.graphBuilder.resolveGoFilePath(filePath);
    }

    private findDefinitionInScope(
        targetPath: string[],
        vars: Map<string, TemplateVar>,
        scopeStack: import('./types').ScopeFrame[],
        ctx: TemplateContext
    ): vscode.Location | null {
        let topVarName = targetPath[0];
        let searchPath = targetPath;
        if (topVarName === '$' && targetPath.length > 1) {
            topVarName = targetPath[1];
            searchPath = targetPath.slice(1);
        }
        if (!topVarName || topVarName === '.' || topVarName === '$') return null;

        const topVar = vars.get(topVarName);
        if (topVar) {
            if (searchPath.length > 1) {
                const loc = this.navigateToFieldDefinition(
                    searchPath.slice(1), topVar.fields ?? [], ctx
                );
                if (loc) return loc;
            }
            if (topVar.defFile && topVar.defLine) {
                const abs = this.resolveGoFile(topVar.defFile);
                if (abs) {
                    return new vscode.Location(
                        vscode.Uri.file(abs),
                        new vscode.Position(
                            Math.max(0, topVar.defLine - 1),
                            (topVar.defCol ?? 1) - 1
                        )
                    );
                }
            }
        }

        const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
        if (dotFrame?.fields) {
            return this.navigateToFieldDefinition(searchPath, dotFrame.fields, ctx);
        }
        return null;
    }

    private findDeclaredVariableDefinition(
        targetNode: TemplateNode,
        nodes: TemplateNode[],
        position: vscode.Position,
        ctx: TemplateContext,
        scopeStack: import('./types').ScopeFrame[],
        blockLocals: Map<string, TemplateVar>
    ): vscode.Location | null {
        const varName = targetNode.path[0];
        if (!varName?.startsWith('$')) return null;
        for (const node of nodes) {
            const result = this.findVariableAssignment(
                node, varName, position, ctx, scopeStack, blockLocals
            );
            if (result) return result;
        }
        return null;
    }

    private findVariableAssignment(
        node: TemplateNode,
        varName: string,
        position: vscode.Position,
        ctx: TemplateContext,
        scopeStack: import('./types').ScopeFrame[],
        blockLocals: Map<string, TemplateVar>
    ): vscode.Location | null {
        if (node.kind === 'assignment' && node.assignVars?.includes(varName)) {
            return new vscode.Location(
                vscode.Uri.file(ctx.absolutePath),
                new vscode.Position(node.line - 1, node.col - 1)
            );
        }
        if (node.children) {
            for (const child of node.children) {
                const result = this.findVariableAssignment(
                    child, varName, position, ctx, scopeStack, blockLocals
                );
                if (result) return result;
            }
        }
        return null;
    }

    private findRangeAssignedVariable(
        node: TemplateNode,
        scopeStack: import('./types').ScopeFrame[],
        ctx: TemplateContext
    ): vscode.Location | null {
        if (!node.path[0]?.startsWith('$')) return null;
        for (const frame of scopeStack) {
            if (frame.isRange && frame.sourceVar?.defFile && frame.sourceVar.defLine) {
                const abs = this.resolveGoFile(frame.sourceVar.defFile);
                if (abs) {
                    return new vscode.Location(
                        vscode.Uri.file(abs),
                        new vscode.Position(
                            Math.max(0, frame.sourceVar.defLine - 1),
                            (frame.sourceVar.defCol ?? 1) - 1
                        )
                    );
                }
            }
        }
        return null;
    }

    private findRangeVariableDefinition(
        targetPath: string[],
        scopeStack: import('./types').ScopeFrame[],
        ctx: TemplateContext
    ): vscode.Location | null {
        if (!targetPath.length || targetPath[0] === '.' || targetPath[0] === '$') return null;
        const firstName = targetPath[0];
        for (let i = scopeStack.length - 1; i >= 0; i--) {
            const frame = scopeStack[i];
            if (!frame.isRange || !frame.fields) continue;
            const field = frame.fields.find(f => f.name === firstName);
            if (!field) continue;
            if (targetPath.length > 1) {
                const loc = this.navigateToFieldDefinition(
                    targetPath.slice(1), field.fields ?? [], ctx
                );
                if (loc) return loc;
            }
            if (field.defFile && field.defLine) {
                const abs = this.resolveGoFile(field.defFile);
                if (abs) {
                    return new vscode.Location(
                        vscode.Uri.file(abs),
                        new vscode.Position(
                            Math.max(0, field.defLine - 1),
                            (field.defCol ?? 1) - 1
                        )
                    );
                }
            }
            if (frame.sourceVar?.defFile && frame.sourceVar.defLine) {
                const abs = this.resolveGoFile(frame.sourceVar.defFile);
                if (abs) {
                    return new vscode.Location(
                        vscode.Uri.file(abs),
                        new vscode.Position(
                            Math.max(0, frame.sourceVar.defLine - 1),
                            (frame.sourceVar.defCol ?? 1) - 1
                        )
                    );
                }
            }
        }
        return null;
    }
}
