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
        // Both block and define declare a named template body. The actual execution
        // context is determined by the {{ template "name" .Ctx }} call site, not by
        // the declaration. We skip direct body validation here to avoid false positives
        // from using the wrong scope. validateNamedBlock (called from validatePartial
        // when the {{ template }} call is encountered) handles body validation with
        // the correct call-site scope.
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
        typeStr: topVar.elemType ?? (topVar.isSlice && topVar.type.startsWith('[]') ? topVar.type.slice(2) : topVar.type),
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
        typeStr: result.isSlice && result.typeStr.startsWith('[]') ? result.typeStr.slice(2) : result.typeStr,
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

    // Always validate the context argument against the current scope first.
    // e.g. {{ template "foo" .NotExist }} should error if .NotExist is undefined.
    // "." means "pass the current dot through" — always valid, skip check.
    const contextArg = node.partialContext ?? '.';
    if (contextArg !== '.') {
      const contextPath = this.parser.parseDotPath(contextArg);
      if (contextPath.length > 0) {
        const result = resolvePath(contextPath, vars, scopeStack);
        if (!result.found) {
          let errCol = node.col;
          if (node.rawText) {
            // Find start of contextArg in raw text (it comes after the template name)
            const nameIdx = node.partialName ? node.rawText.indexOf(`"${node.partialName}"`) : -1;
            const searchStart = nameIdx !== -1 ? nameIdx + node.partialName!.length + 2 : 0;
            const ctxIdx = node.rawText.indexOf(contextArg, searchStart);

            if (ctxIdx !== -1) {
              // Now refine to the actual variable path start
              let varOffset = 0;
              // If path is just ".", use index of "."
              if (contextPath.length === 1 && contextPath[0] === '.') {
                varOffset = contextArg.indexOf('.');
              } else {
                // Otherwise find ".Path" inside the context arg
                const p = '.' + contextPath.join('.');
                const pIdx = contextArg.indexOf(p);
                if (pIdx !== -1) varOffset = pIdx;
              }
              errCol = node.col + ctxIdx + varOffset;
            }
          }

          errors.push({
            message: `Template variable "${contextArg}" is not defined in the render context`,
            line: node.line,
            col: errCol,
            severity: 'error',
            variable: contextArg,
          });
          return;
        }
      }
    }

    // Named blocks (no file extension) — validate their body with the resolved context scope.
    // We look up the define/block node in the current file's AST and re-validate it.
    if (!isFileBasedPartial(node.partialName)) {
      this.validateNamedBlock(node, vars, scopeStack, errors, ctx, filePath);
      return;
    }

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
        // Keep the original line/col so the error points inside the partial file,
        // not at the {{ template "..." }} call site in the parent.
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
   * Validate a named block (define/block) body when it is called via {{ template "name" .Ctx }}.
   * We find the define/block node in the current document and validate its children
   * with the scope that the template call passes.
   */
  private validateNamedBlock(
    callNode: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    if (!callNode.partialName) return;

    // Build the scope the named block body will see
    const partialVars = this.resolvePartialVars(
      callNode.partialContext ?? '.',
      vars,
      scopeStack,
      ctx
    );

    // Try to find and validate the named block body from the current file
    try {
      let content = '';
      const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath);
      if (openDoc) {
        content = openDoc.getText();
      } else if (fs.existsSync(filePath)) {
        content = fs.readFileSync(filePath, 'utf8');
      } else {
        return;
      }

      const nodes = this.parser.parse(content);
      const blockNode = this.findDefineNodeInAST(nodes, callNode.partialName);
      if (!blockNode || !blockNode.children) return;

      const contextArg = callNode.partialContext ?? '.';

      let blockVars: Map<string, TemplateVar>;
      let childStack: ScopeFrame[];

      if (contextArg === '.') {
        // "." — pass the current scope through unchanged
        blockVars = partialVars;
        childStack = scopeStack;
      } else {
        // ".Expr" — the block's dot IS the resolved value of .Expr.
        // Build a completely fresh scope so the outer scope doesn't leak in.
        const result = resolvePath(
          this.parser.parseDotPath(contextArg),
          vars,
          scopeStack
        );
        if (!result.found) {
          // Context arg already validated above; this shouldn't happen
          return;
        }
        // New dot frame with the resolved type (may have no fields for scalar types)
        childStack = [{
          key: '.',
          typeStr: result.typeStr,
          fields: result.fields ?? [],
        }];
        blockVars = partialVars; // already built from result.fields by resolvePartialVars
      }

      const blockErrors = this.validateBlockChildren(
        blockNode.children,
        blockVars,
        childStack,
        ctx,
        filePath
      );

      for (const e of blockErrors) {
        // Keep the error's original line/col so it points inside the block body,
        // not at the {{ template "..." }} call site.
        errors.push({
          ...e,
          message: `[in block "${callNode.partialName}"] ${e.message}`,
        });
      }
    } catch {
      // ignore
    }
  }

  /**
   * Validate block children using the provided vars map and scope stack.
   */
  private validateBlockChildren(
    children: TemplateNode[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext,
    filePath: string
  ): ValidationError[] {
    const errors: ValidationError[] = [];
    this.validateNodes(children, vars, scopeStack, errors, ctx, filePath);
    return errors;
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

    const { node, stack, vars: hitVars } = hit;

    // ── Bare dot hover ────────────────────────────────────────────────────────
    // Handles two cases:
    //   (a) {{ . }} — variable node with path ["."]
    //   (b) {{ template "name" . }} — cursor over the "." context arg in the tag
    //       (partial node, opening tag position)
    // Bare dot: {{ . }} or {{ template "name" . }} — show current dot's type
    const isBareVarDot = node.kind === 'variable' && node.path.length === 1 && node.path[0] === '.';
    const isPartialDotCtx = node.kind === 'partial' && (node.partialContext ?? '.') === '.';

    if (isBareVarDot || isPartialDotCtx) {
      return this.buildDotHover(stack, hitVars);
    }

    // ── Normal variable hover ─────────────────────────────────────────────────
    // hitVars and stack are scoped correctly for define/block bodies —
    // use them instead of ctx.vars so single-segment paths like .Name resolve.
    const result = resolvePath(node.path, hitVars, stack);
    const varInfo = this.findVariableInfo(node.path, hitVars, stack);

    if (!result.found) {
      return null;
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
   * Build hover content for bare "." — shows the type and fields of the current dot.
   * Used for {{ . }}, {{ range .Items }}{{ . }}{{ end }}, {{ template "x" . }}, etc.
   */
  private buildDotHover(
    scopeStack: ScopeFrame[],
    vars: Map<string, TemplateVar>
  ): vscode.Hover | null {
    const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');

    let typeStr: string;
    let fields: FieldInfo[] | undefined;

    if (dotFrame) {
      typeStr = dotFrame.typeStr ?? 'unknown';
      fields = dotFrame.fields;
    } else {
      // Root scope — dot is the render context itself
      const allVars = [...vars.values()];
      typeStr = 'RenderContext';
      fields = allVars.map(v => ({
        name: v.name,
        type: v.type,
        fields: v.fields,
        isSlice: v.isSlice ?? false,
        doc: v.doc,
      } as FieldInfo));
    }

    const md = new vscode.MarkdownString();
    md.isTrusted = true;
    md.appendCodeblock(`. : ${typeStr}`, 'go');

    if (fields && fields.length > 0) {
      md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
      for (const f of fields.slice(0, 30)) {
        const typeLabel = f.isSlice ? `[]${f.type}` : f.type;
        md.appendMarkdown(`**${f.name}** \`${typeLabel}\`\n`);
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

    const { node, stack, vars: hitVars } = hit;
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
      const rangeVarResult = this.findRangeAssignedVariable(node, stack, ctx);
      if (rangeVarResult) {
        return rangeVarResult;
      }
    }

    // For partials: check if the variable is a field of the partial source variable
    if (ctx.partialSourceVar) {
      // Navigate through the path to find the final field
      let currentFields = ctx.partialSourceVar.fields;
      let currentVar: TemplateVar | FieldInfo | undefined = ctx.partialSourceVar;

      for (const pathPart of node.path) {
        if (pathPart === '.') continue;

        const field = currentFields?.find(f => f.name === pathPart);
        if (!field) {
          currentVar = undefined;
          break;
        }

        currentVar = field;
        currentFields = field.fields;
      }

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

    // Use the scope stack returned by findNodeAtPosition — for define/block bodies
    // it now carries the inferred dot frame from the call site, so field definitions
    // can be resolved directly from the stack without a separate fallback.
    const stackDefLoc = this.findDefinitionInScope(node, hitVars, stack, ctx);
    if (stackDefLoc) return stackDefLoc;

    // Find the variable definition and go to its source location
    for (const rc of ctx.renderCalls) {
      const passedVar = rc.vars.find(v => v.name === topVarName);
      if (passedVar) {
        if (passedVar.defFile && passedVar.defLine) {
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
        // Fallback to render call location
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

    // Handle variables inside range blocks
    const rangeScopeResult = this.findRangeVariableDefinition(node, stack, ctx);
    if (rangeScopeResult) {
      return rangeScopeResult;
    }

    return null;
  }

  /**
   * Resolve the call-site context for a variable inside a define/block body.
   * Returns the vars map and scope stack that the block body would see when called
   * via {{ template "blockName" .Ctx }}.
   */
  private resolveBlockCallSiteContext(
    varNode: TemplateNode,
    allNodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext
  ): { vars: Map<string, TemplateVar>; scopeStack: ScopeFrame[] } | null {
    const enclosingBlock = this.findEnclosingBlockOrDefine(allNodes, position);
    if (!enclosingBlock || !enclosingBlock.blockName) return null;

    const callSite = this.findTemplateCallSite(allNodes, enclosingBlock.blockName);
    if (!callSite) return null;

    // Resolve the context arg at the call site
    const contextArg = callSite.partialContext ?? '.';
    const resolvedPath = this.parser.parseDotPath(contextArg);

    if (contextArg === '.') {
      // Passes the whole root scope through
      return { vars: ctx.vars, scopeStack: [] };
    }

    const result = resolvePath(resolvedPath, ctx.vars, []);
    if (!result.found || !result.fields) return null;

    const syntheticFrame: ScopeFrame = {
      key: '.',
      typeStr: result.typeStr,
      fields: result.fields,
    };

    // Build a vars map from the fields so resolvePath can find them
    const partialVars = new Map<string, TemplateVar>();
    for (const f of result.fields) {
      partialVars.set(f.name, {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
        defFile: f.defFile,
        defLine: f.defLine,
        defCol: f.defCol,
        doc: f.doc,
      });
    }

    return { vars: partialVars, scopeStack: [syntheticFrame] };
  }

  /**
   * Find the Go definition location for a variable node using the given vars/scope.
   */
  private findDefinitionInScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): vscode.Location | null {
    const topVarName = node.path[0] === '.' ? null : node.path[0];
    if (!topVarName) return null;

    // Check top-level vars
    const topVar = vars.get(topVarName);
    if (topVar) {
      // Navigate to the specific field if path has more parts
      if (node.path.length > 1) {
        let fields = topVar.fields ?? [];
        for (let i = 1; i < node.path.length; i++) {
          const field = fields.find(f => f.name === node.path[i]);
          if (!field) break;
          if (i === node.path.length - 1 && field.defFile && field.defLine) {
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
          fields = field.fields ?? [];
        }
      }

      if (topVar.defFile && topVar.defLine) {
        let absGoFile: string | null = topVar.defFile;
        if (!path.isAbsolute(absGoFile)) {
          absGoFile = this.graphBuilder.resolveGoFilePath(absGoFile);
        } else if (!fs.existsSync(absGoFile)) {
          absGoFile = null;
        }
        if (absGoFile) {
          return new vscode.Location(
            vscode.Uri.file(absGoFile),
            new vscode.Position(Math.max(0, topVar.defLine - 1), (topVar.defCol ?? 1) - 1)
          );
        }
      }
    }

    // Check scope stack dot frame
    const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
    if (dotFrame?.fields) {
      const field = dotFrame.fields.find(f => f.name === topVarName);
      if (field?.defFile && field.defLine) {
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
    if (node.kind === 'variable' && node.rawText.includes(':=')) {
      const assignMatch = node.rawText.match(/\{\{\s*\$([\w]+)\s*:=\s*(.+?)\s*\}\}/);
      if (assignMatch && '$' + assignMatch[1] === varName) {
        const rhs = assignMatch[2].trim();
        const rhsNode: TemplateNode = {
          kind: 'variable',
          path: this.parser.parseDotPath(rhs),
          rawText: rhs,
          line: node.line,
          col: node.col,
        };
        return this.findVariableDefinition(rhsNode, ctx);
      }
    }

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
   */
  private findRangeAssignedVariable(
    node: TemplateNode,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): vscode.Location | null {
    const varName = node.path[0];
    if (!varName?.startsWith('$')) return null;

    for (const frame of scopeStack) {
      if (frame.isRange && frame.sourceVar) {
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
   */
  private findRangeVariableDefinition(
    node: TemplateNode,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): vscode.Location | null {
    if (node.path.length === 0 || node.path[0] === '.') return null;

    const firstFieldName = node.path[0];

    for (let i = scopeStack.length - 1; i >= 0; i--) {
      const frame = scopeStack[i];
      if (frame.isRange && frame.fields) {
        const field = frame.fields.find(f => f.name === firstFieldName);
        if (field) {
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

      if (node.children) {
        const found = this.findTemplateNameHover(node.children, position);
        if (found) return found;
      }
    }
    return null;
  }

  /**
   * Check if cursor is on a partial/template/block name and return the definition location.
   */
  private async findPartialDefinitionAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext
  ): Promise<vscode.Location | null> {
    for (const node of nodes) {
      if ((node.kind === 'partial' && node.partialName) ||
        (node.kind === 'block' && node.blockName) ||
        (node.kind === 'define' && node.blockName)) {

        const name = node.partialName || node.blockName!;
        const startLine = node.line - 1;
        const startCol = node.col - 1;

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

    // Build scope from call site context arg
    const contextArg = callSite.partialContext ?? '.';
    const resolvedPath = this.parser.parseDotPath(contextArg);

    let syntheticStack: ScopeFrame[] = [];

    if (contextArg === '.') {
      // Whole root scope — use ctx.vars directly, no synthetic frame needed
      const result = resolvePath(varNode.path, ctx.vars, []);
      if (result.found) {
        return { result, stack: [] };
      }
      return null;
    }

    const callResult = resolvePath(resolvedPath, ctx.vars, []);
    if (!callResult.found) return null;

    const syntheticFrame: ScopeFrame = {
      key: '.',
      typeStr: callResult.typeStr,
      fields: callResult.fields,
    };
    syntheticStack = [syntheticFrame];

    // Build vars from the fields of the context
    const partialVars = new Map<string, TemplateVar>();
    for (const f of callResult.fields ?? []) {
      partialVars.set(f.name, {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
        defFile: f.defFile,
        defLine: f.defLine,
        defCol: f.defCol,
        doc: f.doc,
      });
    }

    const result = resolvePath(varNode.path, partialVars, syntheticStack);
    if (result.found) {
      return { result, stack: syntheticStack };
    }

    return null;
  }

  /**
   * Traverse from root to find a specific call site by name and resolve its context
   */
  private findCallSiteContext(
    nodes: TemplateNode[],
    blockName: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[]
  ): { typeStr: string, fields?: FieldInfo[] } | null {
    for (const node of nodes) {
      if (node.kind === 'partial' && node.partialName === blockName) {
        // Found call site
        const contextArg = node.partialContext ?? '.';
        if (contextArg === '.') {
            const frame = scopeStack.slice().reverse().find(f => f.key === '.');
            if (frame) {
                return { typeStr: frame.typeStr, fields: frame.fields };
            }
            // If no dot frame, it's root context
            return { typeStr: 'context', fields: [...vars.values()] as any };
        }
        
        const path = this.parser.parseDotPath(contextArg);
        const result = resolvePath(path, vars, scopeStack);
        if (result.found) {
            return { typeStr: result.typeStr, fields: result.fields };
        }
        return null;
      }

      // Recurse with scope updates
      if (node.children) {
        let childStack = scopeStack;

        if (node.kind === 'range') {
             const elemScope = this.buildRangeScope(node.path, vars, scopeStack, { vars, renderCalls: [], absolutePath: '', templatePath: '' });
             if (elemScope) childStack = [...scopeStack, elemScope];
        } else if (node.kind === 'with') {
            if (node.path.length > 0 && node.path[0] !== '.') {
                const result = resolvePath(node.path, vars, scopeStack);
                if (result.found && result.fields) {
                    childStack = [...scopeStack, {
                        key: '.', typeStr: result.typeStr, fields: result.fields,
                    }];
                }
            }
        } else if (node.kind === 'block') {
             if (node.path.length > 0 && node.path[0] !== '.') {
                const result = resolvePath(node.path, vars, scopeStack);
                if (result.found && result.fields) {
                    childStack = [...scopeStack, {
                        key: '.', typeStr: result.typeStr, fields: result.fields,
                    }];
                }
             }
        }

        const found = this.findCallSiteContext(node.children, blockName, vars, childStack);
        if (found) return found;
      }
    }
    return null;
  }


  /**
   * Find the AST node at the given cursor position, returning it along with
   * the scope stack active at that point. The scope-building logic mirrors
   * validateNode exactly — this is intentional so hover and go-to-def see
   * the same types as validation.
   */
  private findNodeAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    rootNodes?: TemplateNode[] // Pass root nodes to search for define/block call sites
  ): { node: TemplateNode; stack: ScopeFrame[]; vars: Map<string, TemplateVar> } | null {
    if (!rootNodes) rootNodes = nodes;

    for (const node of nodes) {
      const startLine = node.line - 1;
      const startCol = node.col - 1;

      // ── Variable leaf ──────────────────────────────────────────────────────
      if (node.kind === 'variable') {
        const endCol = startCol + node.rawText.length;
        if (
          position.line === startLine &&
          position.character >= startCol &&
          position.character <= endCol
        ) {
          return { node, stack: scopeStack, vars };
        }
        continue;
      }

      // ── Single-line nodes (partial / template call) ──────────────────────
      // These have no endLine — match by tag span on the same line.
      if (node.endLine === undefined) {
        const endCol = startCol + (node.rawText?.length ?? 0);
        if (
          position.line === startLine &&
          position.character >= startCol &&
          position.character <= endCol
        ) {
          return { node, stack: scopeStack, vars };
        }
        continue;
      }

      // ── Block nodes ────────────────────────────────────────────────────────

      const endLine = node.endLine - 1;
      const afterStart = position.line > startLine ||
        (position.line === startLine && position.character >= startCol);
      const beforeEnd = position.line < endLine ||
        (position.line === endLine && position.character <= (node.endCol ?? 0) - 1);

      if (!afterStart || !beforeEnd) continue;

      // Cursor on the opening tag itself → return with current scope
      const openEndCol = startCol + node.rawText.length;
      if (position.line === startLine && position.character <= openEndCol) {
        return { node, stack: scopeStack, vars };
      }

      // Cursor is inside the body — build child scope mirroring validateNode ─

      switch (node.kind) {

        case 'range': {
          if (node.path.length > 0 && node.path[0] !== '.') {
            const result = resolvePath(node.path, vars, scopeStack);
            if (result.found && result.fields) {
              let sourceVar: TemplateVar | undefined = vars.get(node.path[0]);
              if (!sourceVar) {
                const df = scopeStack.slice().reverse().find(f => f.key === '.');
                const field = df?.fields?.find(f => f.name === node.path[0]);
                if (field) {
                  sourceVar = { name: field.name, type: field.type, fields: field.fields, isSlice: field.isSlice };
                }
              }
              const childStack: ScopeFrame[] = [...scopeStack, {
                key: '.',
                typeStr: result.isSlice && result.typeStr.startsWith('[]') ? result.typeStr.slice(2) : result.typeStr,
                fields: result.fields,
                isRange: true,
                sourceVar,
              }];
            if (node.children) {
              const found = this.findNodeAtPosition(node.children, position, vars, childStack, rootNodes);
              if (found) return found;
            }
            }
          }
          break;
        }

        case 'with': {
          if (node.path.length > 0 && node.path[0] !== '.') {
            const result = resolvePath(node.path, vars, scopeStack);
            if (result.found && result.fields) {
              const childStack: ScopeFrame[] = [...scopeStack, {
                key: '.', typeStr: result.typeStr, fields: result.fields,
              }];
            if (node.children) {
              const found = this.findNodeAtPosition(node.children, position, vars, childStack, rootNodes);
              if (found) return found;
            }
            }
          }
          break;
        }

        case 'if': {
            if (node.children) {
              const found = this.findNodeAtPosition(node.children, position, vars, scopeStack, rootNodes);
              if (found) return found;
            }
          break;
        }

        case 'block': {
          // {{ block "name" .Expr }} — body scope is .Expr as the new dot, like `with`.
          // Pass fields-as-vars so single-segment paths like .Name resolve correctly.
          if (node.path.length > 0 && node.path[0] !== '.') {
            const result = resolvePath(node.path, vars, scopeStack);
            if (result.found && result.fields) {
              const childStack: ScopeFrame[] = [...scopeStack, {
                key: '.', typeStr: result.typeStr, fields: result.fields,
              }];
              const childVars = this.fieldsToVarMap(result.fields);
            if (node.children) {
              const found = this.findNodeAtPosition(node.children, position, childVars, childStack, rootNodes);
              if (found) return found;
            }
              break;
            }
          }
          // "." context — pass through unchanged
            if (node.children) {
              const found = this.findNodeAtPosition(node.children, position, vars, scopeStack, rootNodes);
              if (found) return found;
            }
          break;
        }

        case 'define': {
          // {{ define "name" }} body scope = whatever .Ctx the matching
          // {{ template "name" .Ctx }} call passes. Find that call in the same file.
        if (node.blockName) {
            // Find call site context by traversing from root
            const callContext = this.findCallSiteContext(rootNodes!, node.blockName, vars, []);

            if (callContext) {
                // If found, use that context
                const childVars = this.fieldsToVarMap(callContext.fields ?? []);
                const childStack: ScopeFrame[] = [{
                    key: '.', typeStr: callContext.typeStr, fields: callContext.fields
                }];
                if (node.children) {
                    const found = this.findNodeAtPosition(node.children, position, childVars, childStack, rootNodes);
                    if (found) return found;
                }
            } else {
                // Fallback to previous logic if no call site found (or simpler case)
                const callSite = this.findTemplateCallSite(nodes, node.blockName);
                if (callSite) {
                    const contextArg = callSite.partialContext ?? '.';
                    if (contextArg === '.') {
                        // Caller passes root scope — vars and stack are already correct
                        if (node.children) {
                            const found = this.findNodeAtPosition(node.children, position, vars, scopeStack, rootNodes);
                            if (found) return found;
                        }
                    } else {
                        const resolvedPath = this.parser.parseDotPath(contextArg);
                        const callResult = resolvePath(resolvedPath, vars, scopeStack);
                        if (callResult.found && callResult.fields) {
                            const childVars = this.fieldsToVarMap(callResult.fields);
                            const childStack: ScopeFrame[] = [...scopeStack, {
                                key: '.', typeStr: callResult.typeStr, fields: callResult.fields,
                            }];
                            if (node.children) {
                                const found = this.findNodeAtPosition(node.children, position, childVars, childStack, rootNodes);
                                if (found) return found;
                            }
                        }
                    }
                }
            }
        }

          break;
        }

        default: {
            if (node.children) {
              const found = this.findNodeAtPosition(node.children, position, vars, scopeStack, rootNodes);
              if (found) return found;
            }
        }
      }
    }

    return null;
  }

  /**
   * Convert a FieldInfo array into a TemplateVar map so that single-segment dot
   * paths like .Name can be resolved by resolvePath when inside a scoped block.
   */
  private fieldsToVarMap(fields: FieldInfo[]): Map<string, TemplateVar> {
    const m = new Map<string, TemplateVar>();
    for (const f of fields) {
      m.set(f.name, {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
        defFile: f.defFile,
        defLine: f.defLine,
        defCol: f.defCol,
        doc: f.doc,
      });
    }
    return m;
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

    const renderRegex = /\.Render\s*\(\s*"([^"]+)"/g;
    let match: RegExpExecArray | null;

    while ((match = renderRegex.exec(line)) !== null) {
      const templatePath = match[1];
      const matchStart = match.index + '.Render("'.length;
      const matchEnd = matchStart + templatePath.length;

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
    const content = document.getText();
    const nodes = this.parser.parse(content);
    
    // 1. Determine the active scope at the cursor position
    const { stack } = this.findScopeAtPosition(nodes, position, ctx.vars, [], nodes, ctx) || { stack: [] };

    // 2. Analyze text to find the partial path being typed
    const lineText = document.lineAt(position.line).text;
    const linePrefix = lineText.slice(0, position.character);

    // Match a potential variable path ending at cursor
    // e.g. "{{ .Use", "{{ $v.Nam", "{{ .User.Addr"
    // We look for a sequence of dots and word chars immediately preceding the cursor
    const match = linePrefix.match(/(?:\$|\.)[\w.]*$/);
    
    if (!match) {
        // Not typing a variable path?
        return [];
    }

    const rawPath = match[0]; // e.g. ".Use" or ".User.Ad"
    const parts = rawPath.split('.');
    
    // If ending with dot (e.g. ".User."), we want fields of ".User"
    // If ending with word (e.g. ".User.Na"), we want fields of ".User" filtered by "Na"
    
    let lookupPath: string[] = [];
    let filterPrefix = '';

    if (rawPath.endsWith('.')) {
        // e.g. "." or ".User."
        // Resolve the whole path (excluding empty trailing part)
        lookupPath = this.parser.parseDotPath(rawPath);
    } else {
        // e.g. ".User.Na"
        filterPrefix = parts[parts.length - 1];
        // Resolve the path minus the last segment
        const parentPathStr = rawPath.slice(0, rawPath.length - filterPrefix.length);
        lookupPath = this.parser.parseDotPath(parentPathStr);
    }

    // 3. Resolve the lookup path against the scope
    let fields: FieldInfo[] = [];

    // Special case: just "." or "$" or empty lookup path -> suggests variables/current dot fields
    if (lookupPath.length === 1 && lookupPath[0] === '.') {
         // Resolve "." (current scope)
         const res = resolvePath(['.'], ctx.vars, stack);
         if (res.found && res.fields) {
             fields = res.fields;
         } else {
             // Fallback to all top-level vars if "." isn't resolved (shouldn't happen often)
             fields = [...ctx.vars.values()] as any; 
         }
         
         // Also include "$" vars from scope stack? 
         // For now, let's stick to fields of dot.
    } else {
        const res = resolvePath(lookupPath, ctx.vars, stack);
        if (res.found && res.fields) {
            fields = res.fields;
        }
    }

    // 4. Map to CompletionItems
    return fields
      .filter(f => f.name.toLowerCase().startsWith(filterPrefix.toLowerCase()))
      .map(f => {
        const kind = f.type === 'method'
          ? vscode.CompletionItemKind.Method
          : vscode.CompletionItemKind.Field;
        const item = new vscode.CompletionItem(f.name, kind);
        item.detail = f.isSlice ? `[]${f.type}` : f.type;
        if (f.doc) {
            item.documentation = new vscode.MarkdownString(f.doc);
        }
        return item;
      });
  }

  /**
   * Find the active scope stack at the given position.
   * Traverses the AST and returns the stack of the deepest block node containing the position.
   */
  private findScopeAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    rootNodes: TemplateNode[],
    ctx: TemplateContext
  ): { stack: ScopeFrame[] } | null {
    for (const node of nodes) {
      const startLine = node.line - 1;
      const startCol = node.col - 1;

      // Check if we are "inside" this node's block structure
      // For leaf nodes (variable), we don't really "enter" a scope, but for blocks we do.
      
      if (node.endLine === undefined) continue; // Not a block

      const endLine = node.endLine - 1;
      // We consider "inside" to be strictly after start tag and before end tag?
      // Or just strictly contained in the range?
      // findNodeAtPosition uses strict containment.
      
      const afterStart = position.line > startLine ||
        (position.line === startLine && position.character >= startCol);
      const beforeEnd = position.line < endLine ||
        (position.line === endLine && position.character <= (node.endCol ?? 0));

      if (afterStart && beforeEnd) {
         // We are inside this block. Calculate its scope.
         let childStack = scopeStack;
         let childVars = vars; // Only modified for block calls passing fields-as-vars

         switch (node.kind) {
            case 'range': {
                const elemScope = this.buildRangeScope(node.path, vars, scopeStack, ctx);
                if (elemScope) {
                     childStack = [...scopeStack, elemScope];
                }
                break;
            }
            case 'with': {
                if (node.path.length > 0 && node.path[0] !== '.') {
                    const result = resolvePath(node.path, vars, scopeStack);
                    if (result.found && result.fields) {
                        childStack = [...scopeStack, {
                            key: '.', typeStr: result.typeStr, fields: result.fields,
                        }];
                    }
                }
                break;
            }
            case 'block': {
                 // Similar to with/range
                 if (node.path.length > 0 && node.path[0] !== '.') {
                    const result = resolvePath(node.path, vars, scopeStack);
                    if (result.found && result.fields) {
                        childStack = [...scopeStack, {
                            key: '.', typeStr: result.typeStr, fields: result.fields,
                        }];
                        childVars = this.fieldsToVarMap(result.fields);
                    }
                 }
                 break;
            }
            case 'define': {
                if (node.blockName) {
                    const callContext = this.findCallSiteContext(rootNodes, node.blockName, vars, []);
                    if (callContext) {
                        childVars = this.fieldsToVarMap(callContext.fields ?? []);
                        childStack = [{
                            key: '.', typeStr: callContext.typeStr, fields: callContext.fields
                        }];
                    }
                }
                break;
            }
         }

         // Try to recurse deeper
         if (node.children) {
             const inner = this.findScopeAtPosition(node.children, position, childVars, childStack, rootNodes, ctx);
             if (inner) return inner;
         }
         
         // If no child contains the position, we are directly in this block's scope
         return { stack: childStack };
      }
    }

    return null;
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
