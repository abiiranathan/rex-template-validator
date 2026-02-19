import * as fs from 'fs';
import * as vscode from 'vscode';
import { TemplateContext, TemplateNode, TemplateVar, ValidationError, FieldInfo } from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';

interface ScopeFrame {
  key: string;
  typeStr: string;
  fields?: FieldInfo[];
  isRange?: boolean;
}

export class TemplateValidator {
  private parser = new TemplateParser();
  private outputChannel: vscode.OutputChannel;
  private graphBuilder: KnowledgeGraphBuilder;

  constructor(outputChannel: vscode.OutputChannel, graphBuilder: KnowledgeGraphBuilder) {
    this.outputChannel = outputChannel;
    this.graphBuilder = graphBuilder;
  }

  /**
   * Validate a template document and return VSCode diagnostics.
   */
  async validateDocument(document: vscode.TextDocument): Promise<vscode.Diagnostic[]> {
    const ctx = this.graphBuilder.findContextForFile(document.uri.fsPath);

    if (!ctx) {
      // No Go render call found for this template - warn softly
      return [
        new vscode.Diagnostic(
          new vscode.Range(0, 0, 0, 0),
          'No rex.Render() call found for this template. Run "Rex: Rebuild Template Index" if you just added it.',
          vscode.DiagnosticSeverity.Information
        ),
      ];
    }

    const content = document.getText();
    const errors = this.validate(content, ctx, document.uri.fsPath);

    return errors.map((e) => {
      const line = Math.max(0, e.line - 1);
      const col = Math.max(0, e.col - 1);
      const range = new vscode.Range(line, col, line, col + (e.variable?.length ?? 10));
      return new vscode.Diagnostic(
        range,
        e.message,
        e.severity === 'error'
          ? vscode.DiagnosticSeverity.Error
          : e.severity === 'warning'
            ? vscode.DiagnosticSeverity.Warning
            : vscode.DiagnosticSeverity.Information
      );
    });
  }

  /**
   * Core validation logic. Returns errors.
   */
  validate(
    content: string,
    ctx: TemplateContext,
    filePath: string
  ): ValidationError[] {
    const errors: ValidationError[] = [];
    const nodes = this.parser.parse(content);

    // Build a scope-aware traversal
    this.validateNodes(nodes, ctx.vars, [], errors, ctx, filePath, content);

    return errors;
  }

  private validateNodes(
    nodes: TemplateNode[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string,
    fullContent: string
  ) {
    for (const node of nodes) {
      this.validateNode(node, vars, scopeStack, errors, ctx, filePath, fullContent);
    }
  }

  private validateNode(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string,
    fullContent: string
  ) {
    switch (node.kind) {
      case 'variable': {
        const result = resolvePath(node.path, vars, scopeStack);
        if (!result.found && node.path.length > 0 && node.path[0] !== '.') {
          errors.push({
            message: `Template variable ".${node.path.join('.')}" is not defined in the render context`,
            line: node.line,
            col: node.col,
            severity: 'error',
            variable: node.path[0],
          });
        }
        break;
      }

      case 'range': {
        // Check the range variable exists and is a slice
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (!result.found) {
            errors.push({
              message: `Range target ".${node.path.join('.')}" is not defined in the render context`,
              line: node.line,
              col: node.col,
              severity: 'error',
              variable: node.path[0],
            });
          } else {
            // Push elem type into scope as "."
            const topVar = vars.get(node.path[0]);
            if (topVar && topVar.isSlice) {
              scopeStack.push({
                key: '.',
                typeStr: topVar.elemType ?? 'unknown',
                fields: topVar.fields,
                isRange: true,
              });
            }
          }
        }
        break;
      }

      case 'with':
      case 'if': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack);
          if (!result.found) {
            errors.push({
              message: `Condition ".${node.path.join('.')}" is not defined in the render context`,
              line: node.line,
              col: node.col,
              severity: 'warning',
              variable: node.path[0],
            });
          }
        }
        break;
      }

      case 'partial': {
        this.validatePartial(node, vars, scopeStack, errors, ctx, filePath, fullContent);
        break;
      }
    }

    // Recurse children
    if (node.children) {
      this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath, fullContent);
    }
  }

  private validatePartial(
    node: TemplateNode,
    _vars: Map<string, TemplateVar>,
    _scopeStack: ScopeFrame[],
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string,
    _fullContent: string
  ) {
    if (!node.partialName) return;

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

    // Read and validate the partial with the passed context
    const partialPath = partialCtx.templatePath;
    if (!fs.existsSync(partialPath)) return;

    try {
      const partialContent = fs.readFileSync(partialPath, 'utf8');
      // Validate partial using the same vars (context is passed down)
      const partialErrors = this.validate(partialContent, ctx, partialPath);
      for (const e of partialErrors) {
        errors.push({
          ...e,
          message: `[in partial "${node.partialName}"] ${e.message}`,
        });
      }
    } catch {
      // ignore read errors
    }
  }

  /**
   * Get hover information for a variable at a given position.
   */
  getHoverInfo(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.Hover | null {
    const content = document.getText();
    const nodes = this.parser.parse(content);
    
    const hit = this.findNodeAndScope(nodes, position, ctx.vars, []);
    if (!hit) {
        return null;
    }
    
    const { node, stack } = hit;
    
    // Resolve the variable using the found scope stack
    const result = resolvePath(node.path, ctx.vars, stack);

    if (result.found) {
      // Reconstruct variable name from path
      // If path starts with valid char, assume it's root or field.
      // We usually prefix with . for display unless it's a root var?
      // Standardize on dot notation for display.
      const varName = '.' + node.path.join('.');
      
      const md = new vscode.MarkdownString();
      md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');

      if (result.fields && result.fields.length > 0) {
        md.appendMarkdown('\n\n**Available fields:**\n');
        for (const f of result.fields.slice(0, 15)) {
          md.appendMarkdown(`- \`.${f.name}\`: \`${f.type}\`\n`);
        }
      }

      return new vscode.Hover(md);
    }

    return null;
  }

  private findNodeAndScope(
    nodes: TemplateNode[],
    position: vscode.Position,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[]
  ): { node: TemplateNode; stack: ScopeFrame[] } | null {
    for (const node of nodes) {
      const startLine = node.line - 1;
      const startCol = node.col - 1;

      // Check if position is after start of node
      if (position.line < startLine || (position.line === startLine && position.character < startCol)) {
        continue;
      }

      // Variable node (leaf)
      if (node.kind === 'variable') {
        // Approximate end
        const endCol = startCol + node.rawText.length;
        if (position.line === startLine && position.character <= endCol) {
          return { node, stack: scopeStack };
        }
        continue;
      }

      // Block node
      if (node.endLine !== undefined && node.endCol !== undefined) {
        const endLine = node.endLine - 1;
        const endCol = node.endCol - 1;

        // Check if cursor is on the OPENING tag (to hover the condition/range var)
        const openingEndCol = startCol + node.rawText.length;
        if (position.line === startLine && position.character >= startCol && position.character <= openingEndCol) {
             return { node, stack: scopeStack }; // Use current stack
        }

        const beforeEnd =
          position.line < endLine || (position.line === endLine && position.character <= endCol);

        if (beforeEnd) {
          // Inside block content
          let nextStack = scopeStack;

          if (node.kind === 'range' || node.kind === 'with') {
            const result = resolvePath(node.path, vars, scopeStack);
            if (result.found && result.fields) {
                // Determine implicit type for "."
                // For range, result.fields are element fields (if slice)
                // For with, result.fields are struct fields
                
                const newFrame: ScopeFrame = {
                    key: '.',
                    typeStr: result.typeStr, 
                    fields: result.fields,
                    isRange: node.kind === 'range'
                };
                nextStack = [...scopeStack, newFrame];
            }
          }

          if (node.children) {
            const found = this.findNodeAndScope(node.children, position, vars, nextStack);
            if (found) return found;
          }
          
          // Cursor is inside block but not on a child variable?
          // Return null as we only hover variables
        }
      }
    }
    return null;
  }


  /**
   * Get completion items for dot expressions.
   */
  getCompletions(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.CompletionItem[] {
    const line = document.lineAt(position.line).text.slice(0, position.character);

    // Find what's before the cursor
    const dotMatch = line.match(/\{\{.*?\.([\w.]*)$/);
    if (!dotMatch) return [];

    const partial = dotMatch[1];
    const parts = partial.split('.');

    // If one part, suggest top-level vars
    if (parts.length <= 1) {
      const prefix = parts[0] ?? '';
      return [...ctx.vars.values()]
        .filter((v) => v.name.toLowerCase().startsWith(prefix.toLowerCase()))
        .map((v) => {
          const item = new vscode.CompletionItem(v.name, vscode.CompletionItemKind.Variable);
          item.detail = v.type;
          item.documentation = new vscode.MarkdownString(`**Type:** \`${v.type}\``);
          return item;
        });
    }

    // Resolve parent path and suggest fields
    const parentPath = parts.slice(0, -1);
    const prefix = parts[parts.length - 1];
    const topVar = ctx.vars.get(parentPath[0]);

    if (!topVar || !topVar.fields) return [];

    let fields = topVar.fields;
    for (let i = 1; i < parentPath.length; i++) {
      const field = fields.find((f) => f.name === parentPath[i]);
      if (!field) return [];
      fields = field.fields ?? [];
    }

    return fields
      .filter((f) => f.name.toLowerCase().startsWith(prefix.toLowerCase()))
      .map((f) => {
        const kind =
          f.type === 'method'
            ? vscode.CompletionItemKind.Method
            : vscode.CompletionItemKind.Field;
        const item = new vscode.CompletionItem(f.name, kind);
        item.detail = f.type;
        return item;
      });
  }
}
