import * as fs from 'fs';
import * as path from 'path';
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

  async validateDocument(document: vscode.TextDocument, providedCtx?: TemplateContext): Promise<vscode.Diagnostic[]> {
    const ctx = providedCtx || this.graphBuilder.findContextForFile(document.uri.fsPath);

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
            const elemScope = this.buildRangeScope(node.path, vars, scopeStack, ctx);
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

      case 'block':
      case 'define': {
        // We do NOT validate the default body of a block or define during initial 
        // per-file pass. They are meant to be executed and evaluated when explicitly 
        // called via `template`, potentially with a different scope context!
        // Returning here prevents `this.validateNodes` from recursing into their children.
        return;
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
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): ScopeFrame | null {
    // The range target should be a slice; its element type becomes the new dot
    const topVar = vars.get(path[0]);
    if (topVar?.isSlice) {
      return {
        key: '.',
        typeStr: topVar.elemType ?? topVar.type,
        fields: topVar.fields,
        isRange: true,
        sourceVar: topVar,
      };
    }

    // Try resolving through scope
    const result = resolvePath(path, vars, scopeStack);
    if (result.found && result.isSlice && result.fields) {
      // Find the source variable from render calls to track definition
      let sourceVar: TemplateVar | undefined;
      for (const rc of ctx.renderCalls) {
        const v = rc.vars.find(v => v.name === path[0]);
        if (v?.isSlice) {
          sourceVar = v;
          break;
        }
      }
      return {
        key: '.',
        typeStr: result.typeStr,
        fields: result.fields,
        isRange: true,
        sourceVar,
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
      let content = '';
      const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === partialCtx.absolutePath);
      if (openDoc) {
        content = openDoc.getText();
      } else {
        content = fs.readFileSync(partialCtx.absolutePath, 'utf8');
      }
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

  async getHoverInfo(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): Promise<vscode.Hover | null> {
    const content = document.getText();
    const nodes = this.parser.parse(content);

    // Check if hovering over a block/define/partial name
    const blockHover = this.findTemplateNameHover(nodes, position);
    if (blockHover) {
      const md = new vscode.MarkdownString();
      md.appendMarkdown(`**Template Block:** \`${blockHover}\`\n`);
      return new vscode.Hover(md);
    }

    const hit = this.findNodeAtPosition(nodes, position, ctx.vars, []);
    if (!hit) return null;

    const { node, stack } = hit;
    let result = resolvePath(node.path, ctx.vars, stack);
    let resolvedStack = stack;
    let varInfo = this.findVariableInfo(node.path, ctx.vars, stack);

    if (!result.found) {
      // If we couldn't resolve it, check if we are inside a define or block and try to infer 
      // the context from a template call in the same file!
      const fallback = this.tryResolveFromTemplateCalls(node, nodes, position, ctx);
      if (fallback) {
        result = fallback.result;
        resolvedStack = fallback.stack;
      } else {
        return null;
      }
    }

    const varName = node.path[0] === '.' ? '.' : '.' + node.path.join('.');
    const md = new vscode.MarkdownString();
    md.isTrusted = true;

    // Build hover content similar to gopls
    md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');

    // Add documentation if available
    if (varInfo?.doc) {
      md.appendMarkdown('\n\n---\n\n');
      md.appendMarkdown(varInfo.doc);
    }

    // Add field information with documentation
    if (result.fields && result.fields.length > 0) {
      md.appendMarkdown('\n\n---\n\n');
      md.appendMarkdown('**Fields:**\n\n');

      for (const f of result.fields.slice(0, 30)) {
        const typeLabel = f.isSlice ? `[]${f.type}` : f.type;

        // Field signature
        md.appendMarkdown(`**${f.name}** \`${typeLabel}\`\n`);

        // Field documentation if available
        if (f.doc) {
          md.appendMarkdown(`\n${f.doc}\n`);
        }

        md.appendMarkdown('\n');
      }
    }

    return new vscode.Hover(md);
  }

  /**
   * Find variable information including documentation for a path
   */
  private findVariableInfo(
    path: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[]
  ): { typeStr: string; doc?: string } | null {
    if (path.length === 0) return null;

    const topVarName = path[0] === '.' ? null : path[0];
    if (!topVarName) return null;

    // Check top-level vars
    const topVar = vars.get(topVarName);
    if (!topVar) {
      // Check scope stack
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame?.fields) {
        const field = dotFrame.fields.find(f => f.name === topVarName);
        if (field) {
          return { typeStr: field.type, doc: field.doc };
        }
      }
      return null;
    }

    // If just the top-level var
    if (path.length === 1) {
      return { typeStr: topVar.type, doc: topVar.doc };
    }

    // Navigate through fields
    let fields = topVar.fields ?? [];
    for (let i = 1; i < path.length; i++) {
      const field = fields.find(f => f.name === path[i]);
      if (!field) return null;

      if (i === path.length - 1) {
        return { typeStr: field.type, doc: field.doc };
      }

      fields = field.fields ?? [];
    }

    return null;
  }

  // ── Definition ─────────────────────────────────────────────────────────────

  /**
   * Returns the definition location for the symbol at the given position.
   * Handles:
   * - Template variables → jumps to c.Render() call in Go code
   * - Partial template names → jumps to the template file
   */
  async getDefinitionLocation(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): Promise<vscode.Location | null> {
    const content = document.getText();
    const nodes = this.parser.parse(content);

    // First, check if cursor is on a partial/template name
    const partialLocation = await this.findPartialDefinitionAtPosition(
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

    const { node, stack } = hit;
    const topVarName = node.path[0] === '.' ? null : node.path[0];
    if (!topVarName) return null;

    // Check for explicitly declared variables first (e.g., $name := .DrugName)
    if (topVarName.startsWith('$')) {
      // Search in all nodes but also check if it's accessing a range element
      const declaredVar = this.findDeclaredVariableDefinition(node, nodes, position, ctx);
      if (declaredVar) {
        return declaredVar;
      }
      // For $ variables inside ranges, they might be assigned from range elements
      // Try to resolve through the scope stack
      const rangeVarResult = this.findRangeAssignedVariable(node, stack, ctx);
      if (rangeVarResult) {
        return rangeVarResult;
      }
    }

    // For partials: check if the variable is a field of the partial source variable
    // e.g., partial called with {{ template "partial" .User }}, and we're clicking on .Name
    // or clicking on .Patient.Name - need to navigate through the path
    if (ctx.partialSourceVar) {
      // Navigate through the path to find the final field
      let currentFields = ctx.partialSourceVar.fields;
      let currentVar: TemplateVar | FieldInfo | undefined = ctx.partialSourceVar;

      for (const pathPart of node.path) {
        if (pathPart === '.') continue;

        // Find the field matching this path component
        const field = currentFields?.find(f => f.name === pathPart);
        if (!field) {
          currentVar = undefined;
          break;
        }

        currentVar = field;
        currentFields = field.fields;
      }

      // If we found the final field/variable, go to its definition
      if (currentVar) {
        const target = currentVar as FieldInfo;
        if (target.defFile && target.defLine) {
          let absGoFile: string | null = target.defFile;
          if (!path.isAbsolute(absGoFile)) {
            absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
          } else if (!fs.existsSync(absGoFile)) {
            absGoFile = null;
          }
          if (absGoFile) {
            return new vscode.Location(
              vscode.Uri.file(absGoFile),
              new vscode.Position(Math.max(0, target.defLine - 1), (target.defCol ?? 1) - 1)
            );
          }
        }
      }

      // Fallback: use the source variable's location
      if (ctx.partialSourceVar.defFile && ctx.partialSourceVar.defLine) {
        let absGoFile: string | null = ctx.partialSourceVar.defFile;
        if (!path.isAbsolute(absGoFile)) {
          absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
        } else if (!fs.existsSync(absGoFile)) {
          absGoFile = null;
        }
        if (absGoFile) {
          return new vscode.Location(
            vscode.Uri.file(absGoFile),
            new vscode.Position(Math.max(0, ctx.partialSourceVar.defLine - 1), (ctx.partialSourceVar.defCol ?? 1) - 1)
          );
        }
      }
    }

    // Find the variable definition and go to its source location
    for (const rc of ctx.renderCalls) {
      const passedVar = rc.vars.find(v => v.name === topVarName);
      if (passedVar) {
        // Use the definition location if available
        if (passedVar.defFile && passedVar.defLine) {
          // defFile may be absolute or relative - handle both
          let absGoFile: string | null = passedVar.defFile;
          if (!path.isAbsolute(absGoFile)) {
            absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
          } else if (!fs.existsSync(absGoFile)) {
            absGoFile = null;
          }
          if (absGoFile) {
            return new vscode.Location(
              vscode.Uri.file(absGoFile),
              new vscode.Position(Math.max(0, passedVar.defLine - 1), (passedVar.defCol ?? 1) - 1)
            );
          }
        }
        // Fallback to render call location if no definition location available
        if (rc.file) {
          const absGoFile = this.graphBuilder.resolveGoFilePath(rc.file);
          if (absGoFile) {
            return new vscode.Location(
              vscode.Uri.file(absGoFile),
              new vscode.Position(Math.max(0, rc.line - 1), 0)
            );
          }
        }
      }
    }

    // Handle variables inside range blocks (where path starts from element type)
    // e.g., {{ range .prescriptions }} ... {{ .DrugName }}
    const rangeScopeResult = this.findRangeVariableDefinition(node, stack, ctx);
    if (rangeScopeResult) {
      return rangeScopeResult;
    }

    return null;
  }

  /**
   * Find definition for a variable declared inside the template (e.g., $name := .DrugName)
   */
  private findDeclaredVariableDefinition(
    targetNode: TemplateNode,
    nodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.Location | null {
    // Search for the assignment that defines this variable
    const varName = targetNode.path[0];
    if (!varName?.startsWith('$')) return null;

    for (const node of nodes) {
      const result = this.findVariableAssignment(node, varName, position, ctx);
      if (result) return result;
    }

    return null;
  }

  /**
   * Recursively search for a variable assignment (e.g., $name := .DrugName)
   */
  private findVariableAssignment(
    node: TemplateNode,
    varName: string,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.Location | null {
    // Check if this node is a variable assignment
    if (node.kind === 'variable' && node.rawText.includes(':=')) {
      const assignMatch = node.rawText.match(/\{\{\s*\$([\w]+)\s*:=\s*(.+?)\s*\}\}/);
      if (assignMatch && '$' + assignMatch[1] === varName) {
        // This is the assignment - go to the right-hand side definition
        const rhs = assignMatch[2].trim();
        // Create a temporary node to resolve the RHS
        const rhsNode: TemplateNode = {
          kind: 'variable',
          path: this.parser.parseDotPath(rhs),
          rawText: rhs,
          line: node.line,
          col: node.col,
        };
        // Try to find definition for the RHS
        return this.findVariableDefinition(rhsNode, ctx);
      }
    }

    // Recurse into children
    if (node.children) {
      for (const child of node.children) {
        const result = this.findVariableAssignment(child, varName, position, ctx);
        if (result) return result;
      }
    }

    return null;
  }

  /**
   * Find definition for a variable, checking render calls
   */
  private findVariableDefinition(
    node: TemplateNode,
    ctx: TemplateContext
  ): vscode.Location | null {
    const topVarName = node.path[0] === '.' ? null : node.path[0];
    if (!topVarName) return null;

    for (const rc of ctx.renderCalls) {
      const passedVar = rc.vars.find(v => v.name === topVarName);
      if (passedVar?.defFile && passedVar.defLine) {
        let absGoFile: string | null = passedVar.defFile;
        if (!path.isAbsolute(absGoFile)) {
          absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
        } else if (!fs.existsSync(absGoFile)) {
          absGoFile = null;
        }
        if (absGoFile) {
          return new vscode.Location(
            vscode.Uri.file(absGoFile),
            new vscode.Position(Math.max(0, passedVar.defLine - 1), (passedVar.defCol ?? 1) - 1)
          );
        }
      }
    }

    return null;
  }

  /**
   * Find definition for a variable assigned from a range element
   * e.g., {{ $i, $v := range .Items }} ... {{ $v.Name }}
   */
  private findRangeAssignedVariable(
    node: TemplateNode,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): vscode.Location | null {
    const varName = node.path[0];
    if (!varName?.startsWith('$')) return null;

    // Look through the scope stack for range contexts
    // Check if this $variable is an iteration variable from a range
    for (const frame of scopeStack) {
      if (frame.isRange && frame.sourceVar) {
        // This might be an iteration variable
        // Go to the slice/array definition
        if (frame.sourceVar.defFile && frame.sourceVar.defLine) {
          let absGoFile: string | null = frame.sourceVar.defFile;
          if (!path.isAbsolute(absGoFile)) {
            absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
          } else if (!fs.existsSync(absGoFile)) {
            absGoFile = null;
          }
          if (absGoFile) {
            return new vscode.Location(
              vscode.Uri.file(absGoFile),
              new vscode.Position(Math.max(0, frame.sourceVar.defLine - 1), (frame.sourceVar.defCol ?? 1) - 1)
            );
          }
        }
      }
    }

    return null;
  }

  /**
   * Find definition for variables accessed inside range blocks
   * e.g., {{ range .prescriptions }} ... {{ .DrugName }}
   * When clicking on a field inside a range, goes to the specific struct field definition
   */
  private findRangeVariableDefinition(
    node: TemplateNode,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): vscode.Location | null {
    // If path doesn't start with a top-level var, it might be accessing range element
    if (node.path.length === 0 || node.path[0] === '.') return null;

    const firstFieldName = node.path[0];

    // Look through scope stack for range contexts
    for (let i = scopeStack.length - 1; i >= 0; i--) {
      const frame = scopeStack[i];
      if (frame.isRange && frame.fields) {
        // Check if the first path element is a field of this range element
        const field = frame.fields.find(f => f.name === firstFieldName);
        if (field) {
          // Try to go to the specific field definition if available
          if (field.defFile && field.defLine) {
            let absGoFile: string | null = field.defFile;
            if (!path.isAbsolute(absGoFile)) {
              absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
            } else if (!fs.existsSync(absGoFile)) {
              absGoFile = null;
            }
            if (absGoFile) {
              return new vscode.Location(
                vscode.Uri.file(absGoFile),
                new vscode.Position(Math.max(0, field.defLine - 1), (field.defCol ?? 1) - 1)
              );
            }
          }

          // Fallback: go to the slice/array definition
          if (frame.sourceVar?.defFile && frame.sourceVar.defLine) {
            let absGoFile: string | null = frame.sourceVar.defFile;
            if (!path.isAbsolute(absGoFile)) {
              absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
            } else if (!fs.existsSync(absGoFile)) {
              absGoFile = null;
            }
            if (absGoFile) {
              return new vscode.Location(
                vscode.Uri.file(absGoFile),
                new vscode.Position(Math.max(0, frame.sourceVar.defLine - 1), (frame.sourceVar.defCol ?? 1) - 1)
              );
            }
          }
        }
      }
    }

    return null;
  }

  private findTemplateNameHover(nodes: TemplateNode[], position: vscode.Position): string | null {
    for (const node of nodes) {
      if ((node.kind === 'partial' && node.partialName) || 
          (node.kind === 'block' && node.blockName) || 
          (node.kind === 'define' && node.blockName)) {
        
        const name = node.partialName || node.blockName!;
        const startLine = node.line - 1;
        const startCol = node.col - 1;

        const keywordMatch = node.rawText.match(/\{\{\s*(template|block|define)\s+/);
        if (keywordMatch) {
          const nameMatch = node.rawText.match(new RegExp(`\\{\\{\\s*(template|block|define)\\s+"([^"]+)"`));
          if (nameMatch && nameMatch[2] === name) {
            const nameStartOffset = node.rawText.indexOf('"' + name + '"') + 1;
            const nameStartCol = startCol + nameStartOffset;
            const nameEndCol = nameStartCol + name.length;

            if (
              position.line === startLine &&
              position.character >= nameStartCol &&
              position.character <= nameEndCol
            ) {
              return name;
            }
          }
        }
      }

      if (node.children) {
        const found = this.findTemplateNameHover(node.children, position);
        if (found) return found;
      }
    }
    return null;
  }

  /**
   * Check if cursor is on a partial/template/block name in {{ template "name" . }}
   * and return the location of the template file or define block.
   */
  private async findPartialDefinitionAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext
  ): Promise<vscode.Location | null> {
    for (const node of nodes) {
      // Check if this is a partial, block, or define node
      if ((node.kind === 'partial' && node.partialName) || 
          (node.kind === 'block' && node.blockName) || 
          (node.kind === 'define' && node.blockName)) {
        
        const name = node.partialName || node.blockName!;
        const startLine = node.line - 1;
        const startCol = node.col - 1;

        const keywordMatch = node.rawText.match(/\{\{\s*(template|block|define)\s+/);
        if (keywordMatch) {
          const nameMatch = node.rawText.match(new RegExp(`\\{\\{\\s*(template|block|define)\\s+"([^"]+)"`));
          if (nameMatch && nameMatch[2] === name) {
            const nameStartOffset = node.rawText.indexOf('"' + name + '"') + 1;
            const nameStartCol = startCol + nameStartOffset;
            const nameEndCol = nameStartCol + name.length;

            // Check if cursor is on the template name
            if (
              position.line === startLine &&
              position.character >= nameStartCol &&
              position.character <= nameEndCol
            ) {
              if (isFileBasedPartial(name)) {
                const templatePath = this.graphBuilder.resolveTemplatePath(name);
                if (templatePath) {
                  return new vscode.Location(
                    vscode.Uri.file(templatePath),
                    new vscode.Position(0, 0)
                  );
                }
              } else {
                return await this.findNamedBlockDefinition(name, ctx);
              }
            }
          }
        }
      }

      // Recurse into children
      if (node.children) {
        const found = await this.findPartialDefinitionAtPosition(
          node.children,
          position,
          ctx
        );
        if (found) return found;
      }
    }

    return null;
  }

  private async findNamedBlockDefinition(name: string, ctx: TemplateContext): Promise<vscode.Location | null> {
    const graph = this.graphBuilder.getGraph();
    if (!graph) return null;

    const defineRegex = new RegExp(`\\{\\{\\s*(?:define|block)\\s+"${name}"`);

    // Prioritize current file
    const filesToSearch = [ctx.absolutePath];
    for (const [_, tctx] of graph.templates) {
      if (tctx.absolutePath !== ctx.absolutePath) {
        filesToSearch.push(tctx.absolutePath);
      }
    }

    for (const filePath of filesToSearch) {
      try {
        let content = '';
        const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath);
        if (openDoc) {
          content = openDoc.getText();
        } else {
          if (!fs.existsSync(filePath)) continue;
          content = await fs.promises.readFile(filePath, 'utf-8');
        }
        
        if (!defineRegex.test(content)) continue;

        const nodes = this.parser.parse(content);
        const defNode = this.findDefineNodeInAST(nodes, name);
        if (defNode) {
          return new vscode.Location(
            vscode.Uri.file(filePath),
            new vscode.Position(defNode.line - 1, defNode.col - 1)
          );
        }
      } catch (err) {
        // ignore
      }
    }
    return null;
  }

  private findDefineNodeInAST(nodes: TemplateNode[], name: string): TemplateNode | null {
    for (const node of nodes) {
      if ((node.kind === 'define' || node.kind === 'block') && node.blockName === name) {
        return node;
      }
      if (node.children) {
        const found = this.findDefineNodeInAST(node.children, name);
        if (found) return found;
      }
    }
    return null;
  }

  // ── Block/Define Context Inference ──────────────────────────────────────────

  private findEnclosingBlockOrDefine(nodes: TemplateNode[], position: vscode.Position): TemplateNode | null {
    for (const node of nodes) {
      if (node.endLine !== undefined) {
        const startLine = node.line - 1;
        const startCol = node.col - 1;
        const endLine = node.endLine - 1;

        const afterStart = position.line > startLine ||
          (position.line === startLine && position.character >= startCol);
        const beforeEnd = position.line < endLine ||
          (position.line === endLine && position.character <= (node.endCol ?? 0) - 1);

        if (afterStart && beforeEnd) {
          if (node.kind === 'block' || node.kind === 'define') {
            const deeper = this.findEnclosingBlockOrDefine(node.children ?? [], position);
            return deeper || node;
          } else if (node.children) {
            const found = this.findEnclosingBlockOrDefine(node.children, position);
            if (found) return found;
          }
        }
      }
    }
    return null;
  }

  private findTemplateCallSite(nodes: TemplateNode[], partialName: string): TemplateNode | null {
    for (const node of nodes) {
      if (node.kind === 'partial' && node.partialName === partialName) {
        return node;
      }
      if (node.children) {
        const found = this.findTemplateCallSite(node.children, partialName);
        if (found) return found;
      }
    }
    return null;
  }

  private tryResolveFromTemplateCalls(
    varNode: TemplateNode,
    allNodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext
  ): { result: ResolveResult; stack: ScopeFrame[]; varInfo?: FieldInfo | TemplateVar } | null {
    const enclosingBlock = this.findEnclosingBlockOrDefine(allNodes, position);
    if (!enclosingBlock || !enclosingBlock.blockName) return null;

    const callSite = this.findTemplateCallSite(allNodes, enclosingBlock.blockName);
    if (!callSite) return null;

    // Mock position to be directly on the template call node
    const callPos = new vscode.Position(callSite.line - 1, callSite.col);
    const callStackHit = this.findNodeAtPosition(allNodes, callPos, ctx.vars, []);
    if (!callStackHit) return null;

    const callResult = resolvePath(callSite.path, ctx.vars, callStackHit.stack);
    if (!callResult.found) return null;

    // Use the call context to evaluate the hovered variable!
    const syntheticFrame: ScopeFrame = {
      key: '.',
      typeStr: callResult.typeStr,
      fields: callResult.fields,
    };

    const syntheticStack = [...callStackHit.stack, syntheticFrame];
    const result = resolvePath(varNode.path, ctx.vars, syntheticStack);

    if (result.found) {
      const varInfo = this.findVariableInfo(varNode.path, ctx.vars, syntheticStack);
      return { result, stack: syntheticStack };
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
            // Find the source variable for ranges (to enable go-to-definition)
            let sourceVar: TemplateVar | undefined;
            if (node.kind === 'range') {
              // Try to find the source variable from top-level vars or scope
              sourceVar = vars.get(node.path[0]);
              if (!sourceVar && scopeStack.length > 0) {
                // Check scope stack for the variable
                const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
                if (dotFrame?.fields) {
                  const field = dotFrame.fields.find(f => f.name === node.path[0]);
                  if (field) {
                    // Create a synthetic TemplateVar from field info
                    sourceVar = {
                      name: node.path[0],
                      type: field.type,
                      fields: field.fields,
                      isSlice: field.isSlice,
                    };
                  }
                }
              }
            }
            const frame: ScopeFrame = {
              key: '.',
              typeStr: result.typeStr,
              fields: result.fields,
              isRange: node.kind === 'range',
              sourceVar,
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
