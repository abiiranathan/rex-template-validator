import * as fs from 'fs';
import * as vscode from 'vscode';
import { FieldInfo, ScopeFrame, TemplateContext, TemplateNode, TemplateVar, ValidationError } from './types';
import { TemplateParser, resolvePath, ResolveResult } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';

export class TemplateValidator {
  private parser = new TemplateParser();
  private outputChannel: vscode.OutputChannel;
  private graphBuilder: KnowledgeGraphBuilder;

  constructor(outputChannel: vscode.OutputChannel, graphBuilder: KnowledgeGraphBuilder) {
    this.outputChannel = outputChannel;
    this.graphBuilder = graphBuilder;
  }

  // ── Public API ─────────────────────────────────────────────────────────────

  async validateDocument(document: vscode.TextDocument): Promise<vscode.Diagnostic[]> {
    const ctx = this.graphBuilder.findContextForFile(document.uri.fsPath);

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
      const range = new vscode.Range(line, col, line, col + (e.variable?.length ?? 10));
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

  validate(content: string, ctx: TemplateContext, filePath: string): ValidationError[] {
    const errors: ValidationError[] = [];
    const nodes = this.parser.parse(content);
    this.validateNodes(nodes, ctx.vars, [], errors, ctx, filePath);
    return errors;
  }

  // ── Validation ─────────────────────────────────────────────────────────────

  private validateNodes(
    nodes: TemplateNode[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    for (const node of nodes) {
      this.validateNode(node, vars, scopeStack, errors, ctx, filePath);
    }
  }

  private validateNode(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],  // never mutated; always copy before pushing
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    switch (node.kind) {
      case 'variable': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (!result.found) {
            errors.push({
              message: `Template variable ".${node.path.join('.')}" is not defined in the render context`,
              line: node.line,
              col: node.col,
              severity: 'error',
              variable: node.rawText,
            });
          }
        }
        // Recurse children (shouldn't exist for variables, but safe)
        break;
      }

      case 'range': {
        // Validate the range expression itself
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (!result.found) {
            errors.push({
              message: `Range target ".${node.path.join('.')}" is not defined`,
              line: node.line,
              col: node.col,
              severity: 'error',
              variable: node.path[0],
            });
          } else {
            // Build child scope from element type
            const elemScope = this.buildRangeScope(node.path, vars, scopeStack);
            const childStack = elemScope ? [...scopeStack, elemScope] : scopeStack;
            if (node.children) {
              this.validateNodes(node.children, vars, childStack, errors, ctx, filePath);
            }
            return; // children already handled
          }
        }
        break;
      }

      case 'with': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (!result.found) {
            errors.push({
              message: `".${node.path.join('.')}" is not defined`,
              line: node.line,
              col: node.col,
              severity: 'warning',
              variable: node.path[0],
            });
          } else if (result.fields) {
            // Push the resolved type as the new dot
            const childStack: ScopeFrame[] = [...scopeStack, {
              key: '.',
              typeStr: result.typeStr,
              fields: result.fields,
            }];
            if (node.children) {
              this.validateNodes(node.children, vars, childStack, errors, ctx, filePath);
            }
            return;
          }
        }
        break;
      }

      case 'if': {
        // if doesn't change scope; just validate condition references
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (!result.found) {
            errors.push({
              message: `".${node.path.join('.')}" is not defined`,
              line: node.line,
              col: node.col,
              severity: 'warning',
              variable: node.path[0],
            });
          }
        }
        // Children inherit same scope
        break;
      }

      case 'partial': {
        this.validatePartial(node, vars, scopeStack, errors, ctx, filePath);
        return; // partial handler recurses children itself
      }

      case 'block': {
        // block doesn't change scope
        break;
      }
    }

    // Default: recurse children with unchanged scope
    if (node.children) {
      this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath);
    }
  }

  private buildRangeScope(
    path: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[]
  ): ScopeFrame | null {
    // The range target should be a slice; its element type becomes the new dot
    const topVar = vars.get(path[0]);
    if (topVar?.isSlice) {
      return {
        key: '.',
        typeStr: topVar.elemType ?? topVar.type,
        fields: topVar.fields,
        isRange: true,
      };
    }

    // Try resolving through scope
    const result = resolvePath(path, vars, scopeStack);
    if (result.found && result.isSlice && result.fields) {
      return {
        key: '.',
        typeStr: result.typeStr,
        fields: result.fields,
        isRange: true,
      };
    }

    return null;
  }

  private validatePartial(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    if (!node.partialName) return;

    // Named blocks (no file extension) are validated by Go itself
    if (!isFileBasedPartial(node.partialName)) return;

    const partialCtx = this.graphBuilder.findPartialContext(node.partialName, filePath);
    if (!partialCtx) {
      errors.push({
        message: `Partial template "${node.partialName}" could not be found`,
        line: node.line,
        col: node.col,
        severity: 'warning',
        variable: node.partialName,
      });
      return;
    }

    if (!fs.existsSync(partialCtx.absolutePath)) return;

    // Resolve the vars the partial receives based on its context arg
    const partialVars = this.resolvePartialVars(
      node.partialContext ?? '.',
      vars,
      scopeStack,
      ctx
    );

    try {
      const content = fs.readFileSync(partialCtx.absolutePath, 'utf8');
      const partialErrors = this.validate(content, { ...partialCtx, vars: partialVars }, partialCtx.absolutePath);
      for (const e of partialErrors) {
        errors.push({
          ...e,
          message: `[in partial "${node.partialName}"] ${e.message}`,
          line: node.line,  // Report at the call site line in parent
          col: node.col,
        });
      }
    } catch {
      // ignore read errors
    }
  }

  /**
   * Given the context arg passed to a partial (e.g. ".", ".User", ".User.Address"),
   * build the vars map that the partial will see as its root scope.
   */
  private resolvePartialVars(
    contextArg: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): Map<string, TemplateVar> {
    // "." → pass through all current vars + current dot scope
    if (contextArg === '.') {
      // If we're in a scoped block, expose the dot frame's fields as top-level vars
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame?.fields) {
        const result = new Map<string, TemplateVar>();
        for (const f of dotFrame.fields) {
          result.set(f.name, {
            name: f.name,
            type: f.type,
            fields: f.fields,
            isSlice: f.isSlice,
          });
        }
        return result;
      }
      // Root scope: pass through all vars
      return new Map(vars);
    }

    // ".SomePath" → resolve that path and expose its fields
    const parser = new TemplateParser();
    const path = parser.parseDotPath(contextArg);
    const result = resolvePath(path, vars, scopeStack);

    if (!result.found || !result.fields) {
      return new Map();
    }

    const partialVars = new Map<string, TemplateVar>();
    for (const f of result.fields) {
      partialVars.set(f.name, {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
      });
    }
    return partialVars;
  }

  // ── Hover ──────────────────────────────────────────────────────────────────

  getHoverInfo(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.Hover | null {
    const content = document.getText();
    const nodes = this.parser.parse(content);

    const hit = this.findNodeAtPosition(nodes, position, ctx.vars, []);
    if (!hit) return null;

    const { node, stack } = hit;
    const result = resolvePath(node.path, ctx.vars, stack);

    if (!result.found) return null;

    const varName = node.path[0] === '.' ? '.' : '.' + node.path.join('.');
    const md = new vscode.MarkdownString();
    md.isTrusted = true;
    md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');

    if (result.fields && result.fields.length > 0) {
      md.appendMarkdown('\n\n**Available fields:**\n');
      for (const f of result.fields.slice(0, 20)) {
        const typeLabel = f.isSlice ? `[]${f.type}` : f.type;
        md.appendMarkdown(`- \`.${f.name}\`: \`${typeLabel}\`\n`);
      }
    }

    return new vscode.Hover(md);
  }

  // ── Definition ─────────────────────────────────────────────────────────────

  /**
   * Returns the definition location for the symbol at the given position.
   * Handles:
   * - Template variables → jumps to c.Render() call in Go code
   * - Partial template names → jumps to the template file
   */
  getDefinitionLocation(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.Location | null {
    const content = document.getText();
    const nodes = this.parser.parse(content);

    // First, check if cursor is on a partial/template name
    const partialLocation = this.findPartialDefinitionAtPosition(
      nodes,
      position,
      ctx
    );
    if (partialLocation) {
      return partialLocation;
    }

    // Otherwise, handle variable definitions
    const hit = this.findNodeAtPosition(nodes, position, ctx.vars, []);
    if (!hit) return null;

    const { node } = hit;
    const topVarName = node.path[0] === '.' ? null : node.path[0];
    if (!topVarName) return null;

    // Find the render call that passes this variable
    for (const rc of ctx.renderCalls) {
      const passeVar = rc.vars.find(v => v.name === topVarName);
      if (passeVar && rc.file) {
        const absGoFile = this.graphBuilder.resolveGoFilePath(rc.file);
        if (absGoFile) {
          return new vscode.Location(
            vscode.Uri.file(absGoFile),
            new vscode.Position(Math.max(0, rc.line - 1), 0)
          );
        }
      }
    }

    return null;
  }

  /**
   * Check if cursor is on a partial/template name in {{ template "name" . }}
   * and return the location of the template file.
   */
  private findPartialDefinitionAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.Location | null {
    for (const node of nodes) {
      // Check if this is a partial node
      if (node.kind === 'partial' && node.partialName) {
        const startLine = node.line - 1;
        const startCol = node.col - 1;

        // Calculate where the template name appears in the raw text
        // {{ template "name" . }}
        // The name is inside quotes after "template "
        const templateKeywordMatch = node.rawText.match(/\{\{\s*template\s+/);
        if (!templateKeywordMatch) continue;

        const nameMatch = node.rawText.match(/\{\{\s*template\s+"([^"]+)"/);
        if (!nameMatch) continue;

        const nameStartOffset = node.rawText.indexOf('"' + nameMatch[1] + '"') + 1;
        const nameStartCol = startCol + nameStartOffset;
        const nameEndCol = nameStartCol + nameMatch[1].length;

        // Check if cursor is on the template name
        if (
          position.line === startLine &&
          position.character >= nameStartCol &&
          position.character <= nameEndCol
        ) {
          // Resolve the template name to a file
          const templatePath = this.graphBuilder.resolveTemplatePath(node.partialName);
          if (templatePath) {
            return new vscode.Location(
              vscode.Uri.file(templatePath),
              new vscode.Position(0, 0)
            );
          }
        }
      }

      // Recurse into children
      if (node.children) {
        const found = this.findPartialDefinitionAtPosition(
          node.children,
          position,
          ctx
        );
        if (found) return found;
      }
    }

    return null;
  }

  // ── Node search ────────────────────────────────────────────────────────────

  private findNodeAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[]
  ): { node: TemplateNode; stack: ScopeFrame[] } | null {
    for (const node of nodes) {
      const startLine = node.line - 1;
      const startCol = node.col - 1;

      // Variable node — check if cursor is over it
      if (node.kind === 'variable') {
        const endCol = startCol + node.rawText.length;
        if (
          position.line === startLine &&
          position.character >= startCol &&
          position.character <= endCol
        ) {
          return { node, stack: scopeStack };
        }
        continue;
      }

      // Block node — check if cursor is inside
      if (node.endLine !== undefined) {
        const endLine = node.endLine - 1;

        const afterStart = position.line > startLine ||
          (position.line === startLine && position.character >= startCol);
        const beforeEnd = position.line < endLine ||
          (position.line === endLine && position.character <= (node.endCol ?? 0) - 1);

        if (!afterStart || !beforeEnd) continue;

        // Cursor on opening tag → hover the condition variable
        const openEndCol = startCol + node.rawText.length;
        if (position.line === startLine && position.character <= openEndCol) {
          return { node, stack: scopeStack };
        }

        // Inside block body → push scope and recurse
        let childStack = scopeStack;

        if ((node.kind === 'range' || node.kind === 'with') && node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (result.found && result.fields) {
            const frame: ScopeFrame = {
              key: '.',
              typeStr: result.typeStr,
              fields: result.fields,
              isRange: node.kind === 'range',
            };
            childStack = [...scopeStack, frame];
          }
        }

        if (node.children) {
          const found = this.findNodeAtPosition(node.children, position, vars, childStack);
          if (found) return found;
        }
      }
    }

    return null;
  }

  /**
   * Get definition location for a template path string in Go code.
   * Used when clicking on c.Render("template.html", data).
   */
  getTemplateDefinitionFromGo(
    document: vscode.TextDocument,
    position: vscode.Position
  ): vscode.Location | null {
    const line = document.lineAt(position.line).text;

    // Look for c.Render("template-path", ...) pattern
    // Match the template string literal at cursor position
    const renderRegex = /\.Render\s*\(\s*"([^"]+)"/g;
    let match: RegExpExecArray | null;

    while ((match = renderRegex.exec(line)) !== null) {
      const templatePath = match[1];
      const matchStart = match.index + '.Render("'.length;
      const matchEnd = matchStart + templatePath.length;

      // Check if cursor is within the template path string
      if (position.character >= matchStart && position.character <= matchEnd) {
        const absPath = this.graphBuilder.resolveTemplatePath(templatePath);
        if (absPath) {
          return new vscode.Location(
            vscode.Uri.file(absPath),
            new vscode.Position(0, 0)
          );
        }
      }
    }

    return null;
  }

  // ── Completions ────────────────────────────────────────────────────────────

  getCompletions(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.CompletionItem[] {
    const line = document.lineAt(position.line).text.slice(0, position.character);

    // Match dot expression inside a template action: {{ .Foo.Bar| }}
    const dotMatch = line.match(/\{\{.*?\.([\w.]*)$/);
    if (!dotMatch) return [];

    const partial = dotMatch[1];
    const parts = partial.split('.');

    // Single segment → suggest top-level vars
    if (parts.length <= 1) {
      const prefix = parts[0] ?? '';
      return [...ctx.vars.values()]
        .filter(v => v.name.toLowerCase().startsWith(prefix.toLowerCase()))
        .map(v => {
          const item = new vscode.CompletionItem(v.name, vscode.CompletionItemKind.Variable);
          item.detail = v.type;
          item.documentation = new vscode.MarkdownString(`**Type:** \`${v.type}\``);
          return item;
        });
    }

    // Multi-segment → resolve parent path and suggest its fields
    const parentPath = parts.slice(0, -1);
    const prefix = parts[parts.length - 1];
    const topVar = ctx.vars.get(parentPath[0]);
    if (!topVar?.fields) return [];

    let fields = topVar.fields;
    for (let i = 1; i < parentPath.length; i++) {
      const field = fields.find(f => f.name === parentPath[i]);
      if (!field) return [];
      fields = field.fields ?? [];
    }

    return fields
      .filter(f => f.name.toLowerCase().startsWith(prefix.toLowerCase()))
      .map(f => {
        const kind = f.type === 'method'
          ? vscode.CompletionItemKind.Method
          : vscode.CompletionItemKind.Field;
        const item = new vscode.CompletionItem(f.name, kind);
        item.detail = f.isSlice ? `[]${f.type}` : f.type;
        return item;
      });
  }
}

/**
 * Returns true if the template name looks like a file path
 * (has a file extension or path separator) rather than a named block.
 */
function isFileBasedPartial(name: string): boolean {
  if (name.includes('/') || name.includes('\\')) return true;
  const ext = name.slice(name.lastIndexOf('.')).toLowerCase();
  return ['.html', '.tmpl', '.gohtml', '.tpl', '.htm'].includes(ext);
}
