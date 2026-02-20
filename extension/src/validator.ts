/**
 * Package validator provides the core template validation logic for the Rex Template Validator.
 * It handles AST traversal, scope resolution, and diagnostic generation.
 */

import * as fs from 'fs';
import * as path from 'path';
import * as vscode from 'vscode';
import { FieldInfo, ScopeFrame, TemplateContext, TemplateNode, TemplateVar, ValidationError } from './types';
import { TemplateParser, resolvePath } from './templateParser';
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

  // ── Shared scope helpers ───────────────────────────────────────────────────

  /**
   * Build the child scope for a block-type node. Single shared implementation
   * used by both findNodeAtPosition and findScopeAtPosition.
   *
   * `block` and `define` both use call-site context so hover/completion inside
   * either declaration always uses the correct inferred scope.
   */
  private buildChildScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    rootNodes: TemplateNode[]
  ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
    switch (node.kind) {
      case 'range': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const elemScope = this.buildRangeElemScope(node, vars, scopeStack, blockLocals);
          if (elemScope) return { childVars: vars, childStack: [...scopeStack, elemScope] };
        }
        return null;
      }

      case 'with': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (result.found) {
            const childScope: ScopeFrame = { key: '.', typeStr: result.typeStr, fields: result.fields ?? [] };
            if (node.valVar) {
              childScope.locals = new Map();
              childScope.locals.set(node.valVar, {
                name: node.valVar,
                type: result.typeStr,
                fields: result.fields,
                isSlice: result.isSlice ?? false,
                isMap: result.isMap,
                elemType: result.elemType,
                keyType: result.keyType,
              });
            }
            return { childVars: vars, childStack: [...scopeStack, childScope] };
          }
        }
        return null;
      }

      // block and define: both use call-site context
      case 'block':
      case 'define': {
        return this.buildNamedBlockScope(node, vars, rootNodes);
      }

      default:
        return null;
    }
  }

  /**
   * For `block` and `define` declarations, find the {{ template "name" .Ctx }}
   * call site and use its resolved context as the body scope.
   */
  private buildNamedBlockScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    rootNodes: TemplateNode[]
  ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
    const name = node.blockName;
    if (!name) return null;

    const callCtx = this.findCallSiteContext(rootNodes, name, vars, []);
    if (callCtx) {
      return {
        childVars: this.fieldsToVarMap(callCtx.fields ?? []),
        childStack: [{ key: '.', typeStr: callCtx.typeStr, fields: callCtx.fields ?? [] }],
      };
    }

    // Fallback for `block`: its tag is also an implicit call site, so use its
    // own context argument if no external {{ template }} call was found.
    if (node.kind === 'block' && node.path.length > 0 && node.path[0] !== '.') {
      const result = resolvePath(node.path, vars, []);
      if (result.found) {
        return {
          childVars: this.fieldsToVarMap(result.fields ?? []),
          childStack: [{ key: '.', typeStr: result.typeStr, fields: result.fields ?? [] }],
        };
      }
    }

    return null;
  }

  /**
   * Build the element-level ScopeFrame for a range node, including key/val locals.
   */
  private buildRangeElemScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals?: Map<string, TemplateVar>
  ): ScopeFrame | null {
    const result = resolvePath(node.path, vars, scopeStack, blockLocals);
    if (!result.found) return null;

    let sourceVar: TemplateVar | undefined = vars.get(node.path[0]);
    if (!sourceVar) {
      const df = scopeStack.slice().reverse().find(f => f.key === '.');
      const field = df?.fields?.find(f => f.name === node.path[0]);
      if (field) sourceVar = fieldInfoToTemplateVar(field);
    }

    const elemTypeStr = result.isSlice && result.typeStr.startsWith('[]')
      ? result.typeStr.slice(2)
      : result.isMap && result.elemType ? result.elemType : result.typeStr;

    const elemScope: ScopeFrame = {
      key: '.',
      typeStr: elemTypeStr,
      fields: result.fields ?? [],
      isRange: true,
      sourceVar,
    };

    if (node.keyVar || node.valVar) {
      elemScope.locals = new Map();
      if (node.keyVar && node.valVar) {
        elemScope.locals.set(node.keyVar, {
          name: node.keyVar,
          type: result.isMap ? (result.keyType ?? 'unknown') : 'int',
          isSlice: false,
        });
        elemScope.locals.set(node.valVar, {
          name: node.valVar,
          type: elemScope.typeStr,
          fields: elemScope.fields,
          isSlice: false,
        });
      } else if (node.valVar) {
        elemScope.locals.set(node.valVar, {
          name: node.valVar,
          type: elemScope.typeStr,
          fields: elemScope.fields,
          isSlice: false,
        });
      }
    }

    return elemScope;
  }

  /**
   * Simpler range scope builder for callers that only need the frame (no node.key/valVar).
   */
  private buildRangeScope(
    path: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext,
    blockLocals?: Map<string, TemplateVar>
  ): ScopeFrame | null {
    const result = resolvePath(path, vars, scopeStack, blockLocals);
    if (!result.found) return null;

    let typeStr = result.typeStr;
    if (result.isSlice && typeStr.startsWith('[]')) typeStr = typeStr.slice(2);
    else if (result.isMap && result.elemType) typeStr = result.elemType;

    let sourceVar: TemplateVar | undefined;
    if (path[0].startsWith('$') && path[0] !== '$') {
      sourceVar = blockLocals?.get(path[0]) ||
        scopeStack.slice().reverse().find(f => f.locals?.has(path[0]))?.locals?.get(path[0]);
    } else {
      sourceVar = vars.get(path[0]);
    }

    return { key: '.', typeStr, fields: result.fields ?? [], isRange: true, sourceVar };
  }

  /**
   * Apply variable bindings from an assignment node into blockLocals.
   */
  private applyAssignmentLocals(
    assignVars: string[],
    result: ReturnType<typeof resolvePath>,
    blockLocals: Map<string, TemplateVar>
  ) {
    if (assignVars.length === 1) {
      blockLocals.set(assignVars[0], {
        name: assignVars[0],
        type: result.typeStr,
        fields: result.fields,
        isSlice: result.isSlice ?? false,
        isMap: result.isMap,
        elemType: result.elemType,
        keyType: result.keyType,
      });
    } else if (assignVars.length === 2 && result.isMap) {
      blockLocals.set(assignVars[0], { name: assignVars[0], type: result.keyType ?? 'unknown', isSlice: false });
      blockLocals.set(assignVars[1], { name: assignVars[1], type: result.elemType ?? 'unknown', fields: result.fields, isSlice: false });
    } else if (assignVars.length === 2 && result.isSlice) {
      blockLocals.set(assignVars[0], { name: assignVars[0], type: 'int', isSlice: false });
      blockLocals.set(assignVars[1], { name: assignVars[1], type: result.elemType ?? 'unknown', fields: result.fields, isSlice: false });
    }
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
    const blockLocals = new Map<string, TemplateVar>();
    for (const node of nodes) {
      this.validateNode(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
    }
  }

  private validateNode(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    switch (node.kind) {
      case 'assignment': {
        const result = resolvePath(node.path, vars, scopeStack, blockLocals);
        if (!result.found) {
          errors.push({ message: `Expression "${node.assignExpr}" is not defined`, line: node.line, col: node.col, severity: 'warning', variable: node.assignExpr });
        } else if (node.assignVars?.length) {
          this.applyAssignmentLocals(node.assignVars, result, blockLocals);
        }
        break;
      }

      case 'variable': {
        if (node.path.length === 0) break;
        if (node.path[0] === '.') break;
        if (node.path[0] === '$' && node.path.length === 1) break;

        if (!resolvePath(node.path, vars, scopeStack, blockLocals).found) {
          const displayPath = node.path[0] === '$'
            ? '$.' + node.path.slice(1).join('.')
            : node.path[0].startsWith('$') ? node.path.join('.') : '.' + node.path.join('.');
          errors.push({ message: `Template variable "${displayPath}" is not defined in the render context`, line: node.line, col: node.col, severity: 'error', variable: node.rawText });
        }
        break;
      }

      case 'range': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (!result.found) {
            errors.push({ message: `Range target ".${node.path.join('.')}" is not defined`, line: node.line, col: node.col, severity: 'error', variable: node.path[0] });
          } else {
            const elemScope = this.buildRangeElemScope(node, vars, scopeStack, blockLocals);
            if (elemScope && node.children) {
              this.validateNodes(node.children, vars, [...scopeStack, elemScope], errors, ctx, filePath);
            }
            return;
          }
        }
        break;
      }

      case 'with': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (!result.found) {
            errors.push({ message: `".${node.path.join('.')}" is not defined`, line: node.line, col: node.col, severity: 'warning', variable: node.path[0] });
          } else if (result.fields !== undefined) {
            const childScope: ScopeFrame = { key: '.', typeStr: result.typeStr, fields: result.fields };
            if (node.valVar) {
              childScope.locals = new Map();
              childScope.locals.set(node.valVar, { name: node.valVar, type: result.typeStr, fields: result.fields, isSlice: result.isSlice ?? false, isMap: result.isMap, elemType: result.elemType, keyType: result.keyType });
            }
            if (node.children) this.validateNodes(node.children, vars, [...scopeStack, childScope], errors, ctx, filePath);
            return;
          }
        }
        break;
      }

      case 'if': {
        if (node.path.length > 0 && node.path[0] !== '.') {
          if (!resolvePath(node.path, vars, scopeStack, blockLocals).found) {
            errors.push({ message: `".${node.path.join('.')}" is not defined`, line: node.line, col: node.col, severity: 'warning', variable: node.path[0] });
          }
        }
        break;
      }

      case 'partial': {
        this.validatePartial(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
        return;
      }

      // Both `block` and `define` declare a named template body. Validation of
      // their children is deferred to the call site (via validateNamedBlock /
      // validatePartial) so we always use the correct caller-provided scope.
      case 'block':
      case 'define':
        return;
    }

    if (node.children) {
      this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath);
    }
  }

  private validatePartial(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    if (!node.partialName) return;

    const contextArg = node.partialContext ?? '.';
    if (contextArg !== '.' && contextArg !== '$') {
      const contextPath = this.parser.parseDotPath(contextArg);
      if (contextPath.length > 0 && contextPath[0] !== '.') {
        const result = resolvePath(contextPath, vars, scopeStack, blockLocals);
        if (!result.found) {
          let errCol = node.col;
          if (node.rawText) {
            const nameIdx = node.rawText.indexOf(`"${node.partialName}"`);
            const searchStart = nameIdx !== -1 ? nameIdx + node.partialName!.length + 2 : 0;
            const ctxIdx = node.rawText.indexOf(contextArg, searchStart);
            if (ctxIdx !== -1) {
              const p = '.' + contextPath.join('.');
              const pIdx = contextArg.indexOf(p);
              errCol = node.col + ctxIdx + (pIdx !== -1 ? pIdx : 0);
            }
          }
          errors.push({ message: `Template variable "${contextArg}" is not defined in the render context`, line: node.line, col: errCol, severity: 'error', variable: contextArg });
          return;
        }
      }
    }

    if (!isFileBasedPartial(node.partialName)) {
      this.validateNamedBlock(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
      return;
    }

    const partialCtx = this.graphBuilder.findPartialContext(node.partialName, filePath);
    if (!partialCtx) {
      errors.push({ message: `Partial template "${node.partialName}" could not be found`, line: node.line, col: node.col, severity: 'warning', variable: node.partialName });
      return;
    }

    if (!fs.existsSync(partialCtx.absolutePath)) return;

    const partialVars = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
    try {
      const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === partialCtx.absolutePath);
      const content = openDoc ? openDoc.getText() : fs.readFileSync(partialCtx.absolutePath, 'utf8');
      const partialErrors = this.validate(content, { ...partialCtx, vars: partialVars }, partialCtx.absolutePath);
      for (const e of partialErrors) errors.push({ ...e, message: `[in partial "${node.partialName}"] ${e.message}` });
    } catch { /* ignore read errors */ }
  }

  private validateNamedBlock(
    callNode: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    if (!callNode.partialName) return;

    const contextArg = callNode.partialContext ?? '.';
    const partialVars = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);

    try {
      const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath);
      let content = '';
      if (openDoc) content = openDoc.getText();
      else if (fs.existsSync(filePath)) content = fs.readFileSync(filePath, 'utf8');
      else return;

      const blockNode = this.findDefineNodeInAST(this.parser.parse(content), callNode.partialName);
      if (!blockNode?.children) return;

      let childStack: ScopeFrame[];
      if (contextArg === '.' || contextArg === '$') {
        childStack = scopeStack;
      } else {
        const result = resolvePath(this.parser.parseDotPath(contextArg), vars, scopeStack, blockLocals);
        if (!result.found) return;
        childStack = [{ key: '.', typeStr: result.typeStr, fields: result.fields ?? [] }];
      }

      const blockErrors: ValidationError[] = [];
      this.validateNodes(blockNode.children, partialVars, childStack, blockErrors, ctx, filePath);
      for (const e of blockErrors) errors.push({ ...e, message: `[in block "${callNode.partialName}"] ${e.message}` });
    } catch { /* ignore */ }
  }

  private resolvePartialVars(
    contextArg: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>
  ): Map<string, TemplateVar> {
    if (contextArg === '.' || contextArg === '$') {
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame?.fields) {
        const result = new Map<string, TemplateVar>();
        for (const f of dotFrame.fields) result.set(f.name, fieldInfoToTemplateVar(f));
        return result;
      }
      return new Map(vars);
    }

    const result = resolvePath(this.parser.parseDotPath(contextArg), vars, scopeStack, blockLocals);
    if (!result.found || !result.fields) return new Map();

    const partialVars = new Map<string, TemplateVar>();
    for (const f of result.fields) partialVars.set(f.name, fieldInfoToTemplateVar(f));
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

    const blockHover = this.findTemplateNameHover(nodes, position);
    if (blockHover) {
      const md = new vscode.MarkdownString();
      md.appendMarkdown(`**Template Block:** \`${blockHover}\`\n`);
      return new vscode.Hover(md);
    }

    const hit = this.findNodeAtPosition(nodes, position, ctx.vars, [], nodes);
    if (!hit) {
      const enclosing = this.findEnclosingBlockOrDefine(nodes, position);
      if (enclosing?.blockName) {
        const callCtx = this.findCallSiteContext(nodes, enclosing.blockName, ctx.vars, []);
        if (callCtx) {
          const md = new vscode.MarkdownString();
          md.isTrusted = true;
          md.appendCodeblock(`. : ${callCtx.typeStr}`, 'go');
          if (callCtx.fields?.length) {
            md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
            for (const f of callCtx.fields.slice(0, 30)) {
              md.appendMarkdown(`**${f.name}** \`${(f as any).isSlice ? `[]${f.type}` : f.type}\`\n\n`);
            }
          }
          return new vscode.Hover(md);
        }
      }
      return null;
    }

    const { node, stack, vars: hitVars, locals: hitLocals } = hit;

    const isBareVarDot = node.kind === 'variable' && node.path.length === 1 && node.path[0] === '.';
    const isPartialDotCtx = node.kind === 'partial' && (node.partialContext ?? '.') === '.';
    if (isBareVarDot || isPartialDotCtx) return this.buildDotHover(stack, hitVars);

    const result = resolvePath(node.path, hitVars, stack, hitLocals);
    if (!result.found) return null;

    const varName = node.path[0] === '.' ? '.' :
      node.path[0] === '$' ? '$.' + node.path.slice(1).join('.') :
        node.path[0].startsWith('$') ? node.path.join('.') : '.' + node.path.join('.');

    const md = new vscode.MarkdownString();
    md.isTrusted = true;
    md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');

    const varInfo = this.findVariableInfo(node.path, hitVars, stack, hitLocals);
    if (varInfo?.doc) { md.appendMarkdown('\n\n---\n\n'); md.appendMarkdown(varInfo.doc); }

    if (result.fields?.length) {
      md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
      for (const f of result.fields.slice(0, 30)) {
        md.appendMarkdown(`**${f.name}** \`${f.isSlice ? `[]${f.type}` : f.type}\`\n`);
        if (f.doc) md.appendMarkdown(`\n${f.doc}\n`);
        md.appendMarkdown('\n');
      }
    }

    return new vscode.Hover(md);
  }

  private buildDotHover(scopeStack: ScopeFrame[], vars: Map<string, TemplateVar>): vscode.Hover | null {
    const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
    const typeStr = dotFrame ? (dotFrame.typeStr ?? 'unknown') : 'RenderContext';
    const fields: FieldInfo[] = dotFrame?.fields ?? [...vars.values()].map(v => ({
      name: v.name, type: v.type, fields: v.fields, isSlice: v.isSlice ?? false, doc: v.doc,
    } as FieldInfo));

    const md = new vscode.MarkdownString();
    md.isTrusted = true;
    md.appendCodeblock(`. : ${typeStr}`, 'go');

    if (fields.length > 0) {
      md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
      for (const f of fields.slice(0, 30)) {
        md.appendMarkdown(`**${f.name}** \`${f.isSlice ? `[]${f.type}` : f.type}\`\n`);
        if (f.doc) md.appendMarkdown(`\n${f.doc}\n`);
        md.appendMarkdown('\n');
      }
    }

    return new vscode.Hover(md);
  }

  private findVariableInfo(
    path: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals?: Map<string, TemplateVar>
  ): { typeStr: string; doc?: string } | null {
    if (path.length === 0) return null;

    let topVarName = path[0];
    let searchPath = path;
    if (topVarName === '$' && path.length > 1) { topVarName = path[1]; searchPath = path.slice(1); }
    if (topVarName === '.' || topVarName === '$') return null;

    let topVar: TemplateVar | undefined;
    if (topVarName.startsWith('$') && topVarName !== '$') {
      topVar = blockLocals?.get(topVarName);
      if (!topVar) for (let i = scopeStack.length - 1; i >= 0; i--) {
        if (scopeStack[i].locals?.has(topVarName)) { topVar = scopeStack[i].locals!.get(topVarName); break; }
      }
    } else {
      topVar = vars.get(topVarName);
      if (!topVar) {
        const field = scopeStack.slice().reverse().find(f => f.key === '.')?.fields?.find(f => f.name === topVarName);
        if (field) return { typeStr: field.type, doc: field.doc };
        return null;
      }
    }

    if (!topVar) return null;
    if (searchPath.length === 1) return { typeStr: topVar.type, doc: topVar.doc };

    let fields = topVar.fields ?? [];
    for (let i = 1; i < searchPath.length; i++) {
      if (searchPath[i] === '[]') continue;
      const field = fields.find(f => f.name === searchPath[i]);
      if (!field) return null;
      if (i === searchPath.length - 1) return { typeStr: field.type, doc: field.doc };
      fields = field.fields ?? [];
    }
    return null;
  }

  // ── Definition ─────────────────────────────────────────────────────────────

  async getDefinitionLocation(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): Promise<vscode.Location | null> {
    const content = document.getText();
    const nodes = this.parser.parse(content);

    const partialLocation = await this.findPartialDefinitionAtPosition(nodes, position, ctx);
    if (partialLocation) return partialLocation;

    const hit = this.findNodeAtPosition(nodes, position, ctx.vars, [], nodes);
    if (!hit) return null;

    const { node, stack, vars: hitVars } = hit;

    // Determine which path segment the cursor is hovering over
    let hoveredIndex = node.path.length - 1;
    if (node.rawText) {
      const cursorOffset = position.character - (node.col - 1);
      const pathStr = node.path[0] === '$' ? '$.' + node.path.slice(1).join('.')
        : node.path[0].startsWith('$') ? node.path.join('.') : '.' + node.path.join('.');
      const pathStart = node.rawText.indexOf(pathStr);
      if (pathStart !== -1) {
        const offsetInPath = cursorOffset - pathStart;
        if (offsetInPath >= 0 && offsetInPath <= pathStr.length) {
          let currentLen = 0;
          const segments = pathStr.split('.');
          for (let i = 0; i < segments.length; i++) {
            currentLen += segments[i].length + 1;
            if (offsetInPath <= currentLen) {
              hoveredIndex = node.path[0] === '$' ? i : node.path[0].startsWith('$') ? i : (i === 0 ? 0 : i - 1);
              break;
            }
          }
        }
      }
    }

    const targetPath = node.path.slice(0, hoveredIndex + 1);
    let pathForDef = targetPath;
    let topVarName = pathForDef[0];
    if (topVarName === '$' && pathForDef.length > 1) { topVarName = pathForDef[1]; pathForDef = pathForDef.slice(1); }
    if (!topVarName || topVarName === '.' || topVarName === '$') return null;

    if (topVarName.startsWith('$') && topVarName !== '$') {
      const declaredVar = this.findDeclaredVariableDefinition(node, nodes, position, ctx, hit.stack, hit.locals);
      if (declaredVar) return declaredVar;
      const rangeVar = this.findRangeAssignedVariable(node, stack, ctx);
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
        const fieldLoc = this.navigateToFieldDefinition(pathForDef.slice(1), passedVar.fields ?? [], ctx);
        if (fieldLoc) return fieldLoc;
        if (passedVar.defFile && passedVar.defLine) {
          const abs = this.resolveGoFile(passedVar.defFile);
          if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, passedVar.defLine - 1), (passedVar.defCol ?? 1) - 1));
        }
        if (rc.file) {
          const abs = this.graphBuilder.resolveGoFilePath(rc.file);
          if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, rc.line - 1), 0));
        }
      }
    }

    return this.findRangeVariableDefinition(pathForDef, stack, ctx);
  }

  private navigateToFieldDefinition(fieldPath: string[], fields: FieldInfo[], ctx: TemplateContext): vscode.Location | null {
    if (fieldPath.length === 0) return null;
    let current = fields;
    for (let i = 0; i < fieldPath.length; i++) {
      if (fieldPath[i] === '[]') continue;
      const field = current.find(f => f.name === fieldPath[i]);
      if (!field) return null;
      if (i === fieldPath.length - 1) {
        if (field.defFile && field.defLine) {
          const abs = this.resolveGoFile(field.defFile);
          if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, field.defLine - 1), (field.defCol ?? 1) - 1));
        }
        return null;
      }
      current = field.fields ?? [];
    }
    return null;
  }

  private findDefinitionInVar(path: string[], sourceVar: TemplateVar, ctx: TemplateContext): vscode.Location | null {
    let currentFields = sourceVar.fields ?? [];
    let currentTarget: TemplateVar | FieldInfo = sourceVar;
    for (const part of path) {
      if (part === '.' || part === '$' || part === '[]') continue;
      const field = currentFields.find(f => f.name === part);
      if (!field) { currentTarget = sourceVar; break; }
      currentTarget = field;
      currentFields = field.fields ?? [];
    }
    const t = currentTarget as any;
    if (t.defFile && t.defLine) {
      const abs = this.resolveGoFile(t.defFile);
      if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, t.defLine - 1), (t.defCol ?? 1) - 1));
    }
    return null;
  }

  private resolveGoFile(filePath: string): string | null {
    if (path.isAbsolute(filePath) && fs.existsSync(filePath)) return filePath;
    return this.graphBuilder.resolveGoFilePath(filePath);
  }

  private findDefinitionInScope(targetPath: string[], vars: Map<string, TemplateVar>, scopeStack: ScopeFrame[], ctx: TemplateContext): vscode.Location | null {
    let topVarName = targetPath[0];
    let searchPath = targetPath;
    if (topVarName === '$' && targetPath.length > 1) { topVarName = targetPath[1]; searchPath = targetPath.slice(1); }
    if (!topVarName || topVarName === '.' || topVarName === '$') return null;

    const topVar = vars.get(topVarName);
    if (topVar) {
      if (searchPath.length > 1) {
        const loc = this.navigateToFieldDefinition(searchPath.slice(1), topVar.fields ?? [], ctx);
        if (loc) return loc;
      }
      if (topVar.defFile && topVar.defLine) {
        const abs = this.resolveGoFile(topVar.defFile);
        if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, topVar.defLine - 1), (topVar.defCol ?? 1) - 1));
      }
    }

    const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
    if (dotFrame?.fields) return this.navigateToFieldDefinition(searchPath, dotFrame.fields, ctx);

    return null;
  }

  private findDeclaredVariableDefinition(
    targetNode: TemplateNode, nodes: TemplateNode[], position: vscode.Position,
    ctx: TemplateContext, scopeStack: ScopeFrame[], blockLocals: Map<string, TemplateVar>
  ): vscode.Location | null {
    const varName = targetNode.path[0];
    if (!varName?.startsWith('$')) return null;
    for (const node of nodes) {
      const result = this.findVariableAssignment(node, varName, position, ctx, scopeStack, blockLocals);
      if (result) return result;
    }
    return null;
  }

  private findVariableAssignment(
    node: TemplateNode, varName: string, position: vscode.Position,
    ctx: TemplateContext, scopeStack: ScopeFrame[], blockLocals: Map<string, TemplateVar>
  ): vscode.Location | null {
    if (node.kind === 'assignment' && node.assignVars?.includes(varName) && node.assignExpr) {
      const rhsPath = this.parser.parseDotPath(node.assignExpr);
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame?.fields) {
        const fieldPath = rhsPath[0] === '.' ? rhsPath.slice(1) : rhsPath;
        const loc = this.navigateToFieldDefinition(fieldPath, dotFrame.fields, ctx);
        if (loc) return loc;
      }
      return this.findVariableDefinition({ kind: 'variable', path: rhsPath, rawText: node.assignExpr, line: node.line, col: node.col }, ctx);
    }
    if (node.children) {
      for (const child of node.children) {
        const result = this.findVariableAssignment(child, varName, position, ctx, scopeStack, blockLocals);
        if (result) return result;
      }
    }
    return null;
  }

  private findVariableDefinition(node: TemplateNode, ctx: TemplateContext): vscode.Location | null {
    const topVarName = node.path[0] === '.' ? null : node.path[0];
    if (!topVarName) return null;
    for (const rc of ctx.renderCalls) {
      const v = rc.vars.find(v => v.name === topVarName);
      if (v?.defFile && v.defLine) {
        const abs = this.resolveGoFile(v.defFile);
        if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, v.defLine - 1), (v.defCol ?? 1) - 1));
      }
    }
    return null;
  }

  private findRangeAssignedVariable(node: TemplateNode, scopeStack: ScopeFrame[], ctx: TemplateContext): vscode.Location | null {
    if (!node.path[0]?.startsWith('$')) return null;
    for (const frame of scopeStack) {
      if (frame.isRange && frame.sourceVar?.defFile && frame.sourceVar.defLine) {
        const abs = this.resolveGoFile(frame.sourceVar.defFile);
        if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, frame.sourceVar.defLine - 1), (frame.sourceVar.defCol ?? 1) - 1));
      }
    }
    return null;
  }

  private findRangeVariableDefinition(targetPath: string[], scopeStack: ScopeFrame[], ctx: TemplateContext): vscode.Location | null {
    if (!targetPath.length || targetPath[0] === '.' || targetPath[0] === '$') return null;
    const firstName = targetPath[0];
    for (let i = scopeStack.length - 1; i >= 0; i--) {
      const frame = scopeStack[i];
      if (!frame.isRange || !frame.fields) continue;
      const field = frame.fields.find(f => f.name === firstName);
      if (!field) continue;
      if (targetPath.length > 1) {
        const loc = this.navigateToFieldDefinition(targetPath.slice(1), field.fields ?? [], ctx);
        if (loc) return loc;
      }
      if (field.defFile && field.defLine) {
        const abs = this.resolveGoFile(field.defFile);
        if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, field.defLine - 1), (field.defCol ?? 1) - 1));
      }
      if (frame.sourceVar?.defFile && frame.sourceVar.defLine) {
        const abs = this.resolveGoFile(frame.sourceVar.defFile);
        if (abs) return new vscode.Location(vscode.Uri.file(abs), new vscode.Position(Math.max(0, frame.sourceVar.defLine - 1), (frame.sourceVar.defCol ?? 1) - 1));
      }
    }
    return null;
  }

  private findTemplateNameHover(nodes: TemplateNode[], position: vscode.Position): string | null {
    for (const node of nodes) {
      if ((node.kind === 'partial' || node.kind === 'block' || node.kind === 'define') && (node.partialName || node.blockName)) {
        const name = node.partialName || node.blockName!;
        const nameMatch = node.rawText.match(new RegExp(`\\{\\{\\s*(template|block|define)\\s+"([^"]+)"`));
        if (nameMatch && nameMatch[2] === name) {
          const nameStartCol = (node.col - 1) + node.rawText.indexOf('"' + name + '"') + 1;
          if (position.line === node.line - 1 && position.character >= nameStartCol && position.character <= nameStartCol + name.length) {
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

  private async findPartialDefinitionAtPosition(nodes: TemplateNode[], position: vscode.Position, ctx: TemplateContext): Promise<vscode.Location | null> {
    for (const node of nodes) {
      if ((node.kind === 'partial' || node.kind === 'block' || node.kind === 'define') && (node.partialName || node.blockName)) {
        const name = node.partialName || node.blockName!;
        const nameMatch = node.rawText.match(new RegExp(`\\{\\{\\s*(template|block|define)\\s+"([^"]+)"`));
        if (nameMatch && nameMatch[2] === name) {
          const nameStartCol = (node.col - 1) + node.rawText.indexOf('"' + name + '"') + 1;
          if (position.line === node.line - 1 && position.character >= nameStartCol && position.character <= nameStartCol + name.length) {
            if (isFileBasedPartial(name)) {
              const templatePath = this.graphBuilder.resolveTemplatePath(name);
              if (templatePath) return new vscode.Location(vscode.Uri.file(templatePath), new vscode.Position(0, 0));
            } else {
              return await this.findNamedBlockDefinition(name, ctx);
            }
          }
        }
      }
      if (node.children) {
        const found = await this.findPartialDefinitionAtPosition(node.children, position, ctx);
        if (found) return found;
      }
    }
    return null;
  }

  private async findNamedBlockDefinition(name: string, ctx: TemplateContext): Promise<vscode.Location | null> {
    const graph = this.graphBuilder.getGraph();
    if (!graph) return null;

    const defineRegex = new RegExp(`\\{\\{\\s*(?:define|block)\\s+"${name}"`);
    const filesToSearch = [ctx.absolutePath, ...[...graph.templates.values()].map(t => t.absolutePath).filter(p => p !== ctx.absolutePath)];

    for (const filePath of filesToSearch) {
      try {
        const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath);
        let content = openDoc ? openDoc.getText() : '';
        if (!content) {
          if (!fs.existsSync(filePath)) continue;
          content = await fs.promises.readFile(filePath, 'utf-8');
        }
        if (!defineRegex.test(content)) continue;
        const defNode = this.findDefineNodeInAST(this.parser.parse(content), name);
        if (defNode) return new vscode.Location(vscode.Uri.file(filePath), new vscode.Position(defNode.line - 1, defNode.col - 1));
      } catch { /* ignore */ }
    }
    return null;
  }

  private findDefineNodeInAST(nodes: TemplateNode[], name: string): TemplateNode | null {
    for (const node of nodes) {
      if ((node.kind === 'define' || node.kind === 'block') && node.blockName === name) return node;
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
      if (node.endLine === undefined) continue;
      const afterStart = position.line > node.line - 1 || (position.line === node.line - 1 && position.character >= node.col - 1);
      const beforeEnd = position.line < node.endLine - 1 || (position.line === node.endLine - 1 && position.character <= (node.endCol ?? 0) - 1);
      if (afterStart && beforeEnd) {
        if (node.kind === 'block' || node.kind === 'define') {
          return this.findEnclosingBlockOrDefine(node.children ?? [], position) || node;
        } else if (node.children) {
          const found = this.findEnclosingBlockOrDefine(node.children, position);
          if (found) return found;
        }
      }
    }
    return null;
  }

  private findTemplateCallSite(nodes: TemplateNode[], partialName: string): TemplateNode | null {
    for (const node of nodes) {
      if (node.kind === 'partial' && node.partialName === partialName) return node;
      if (node.children) {
        const found = this.findTemplateCallSite(node.children, partialName);
        if (found) return found;
      }
    }
    return null;
  }

  private findCallSiteContext(
    nodes: TemplateNode[],
    blockName: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[]
  ): { typeStr: string, fields?: FieldInfo[] } | null {
    for (const node of nodes) {
      const isTemplateCall = node.kind === 'partial' && node.partialName === blockName;
      const isBlockDecl = node.kind === 'block' && node.blockName === blockName;

      if (isTemplateCall || isBlockDecl) {
        let contextArg: string;
        if (isTemplateCall) {
          contextArg = node.partialContext ?? '.';
        } else {
          contextArg = node.path.length === 0 ? '.'
            : node.path[0] === '.' ? '.' + node.path.slice(1).join('.') : '.' + node.path.join('.');
        }

        if (contextArg === '.' || contextArg === '') {
          const frame = scopeStack.slice().reverse().find(f => f.key === '.');
          if (frame) return { typeStr: frame.typeStr, fields: frame.fields };
          return { typeStr: 'context', fields: [...vars.values()] as any };
        }

        const result = resolvePath(this.parser.parseDotPath(contextArg), vars, scopeStack);
        return result.found ? { typeStr: result.typeStr, fields: result.fields } : null;
      }

      if (node.children) {
        let childStack = scopeStack;
        if (node.kind === 'range') {
          const elemScope = this.buildRangeScope(node.path, vars, scopeStack, { vars, renderCalls: [], absolutePath: '', templatePath: '' });
          if (elemScope) childStack = [...scopeStack, elemScope];
        } else if (node.kind === 'with' || node.kind === 'block') {
          if (node.path.length > 0 && node.path[0] !== '.') {
            const result = resolvePath(node.path, vars, scopeStack);
            if (result.found && result.fields !== undefined) {
              childStack = [...scopeStack, { key: '.', typeStr: result.typeStr, fields: result.fields }];
            }
          }
        }
        const found = this.findCallSiteContext(node.children, blockName, vars, childStack);
        if (found) return found;
      }
    }
    return null;
  }

  // ── Node-at-position traversal ─────────────────────────────────────────────

  private findNodeAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    rootNodes: TemplateNode[]
  ): { node: TemplateNode; stack: ScopeFrame[]; vars: Map<string, TemplateVar>; locals: Map<string, TemplateVar> } | null {
    const blockLocals = new Map<string, TemplateVar>();

    for (const node of nodes) {
      const startLine = node.line - 1;
      const startCol = node.col - 1;

      if (node.kind === 'assignment') {
        const result = resolvePath(node.path, vars, scopeStack, blockLocals);
        if (result.found && node.assignVars) this.applyAssignmentLocals(node.assignVars, result, blockLocals);
      }

      if (node.kind === 'variable') {
        const endCol = startCol + node.rawText.length;
        if (position.line === startLine && position.character >= startCol && position.character <= endCol) {
          return { node, stack: scopeStack, vars, locals: blockLocals };
        }
        continue;
      }

      if (node.endLine === undefined) {
        const endCol = startCol + (node.rawText?.length ?? 0);
        if (position.line === startLine && position.character >= startCol && position.character <= endCol) {
          return { node, stack: scopeStack, vars, locals: blockLocals };
        }
        continue;
      }

      const endLine = node.endLine - 1;
      const afterStart = position.line > startLine || (position.line === startLine && position.character >= startCol);
      const beforeEnd = position.line < endLine || (position.line === endLine && position.character <= (node.endCol ?? 0) - 1);
      if (!afterStart || !beforeEnd) continue;

      // Cursor on opening tag
      if (position.line === startLine && position.character <= startCol + node.rawText.length) {
        return { node, stack: scopeStack, vars, locals: blockLocals };
      }

      // Cursor inside body — build child scope and recurse
      const childScope = this.buildChildScope(node, vars, scopeStack, blockLocals, rootNodes);
      if (node.children) {
        const found = this.findNodeAtPosition(
          node.children, position,
          childScope?.childVars ?? vars,
          childScope?.childStack ?? scopeStack,
          rootNodes
        );
        if (found) return found;
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

    let scopeResult = this.findScopeAtPosition(nodes, position, ctx.vars, [], nodes, ctx);
    if (!scopeResult) {
      const enclosing = this.findEnclosingBlockOrDefine(nodes, position);
      if (enclosing?.blockName) {
        const callCtx = this.findCallSiteContext(nodes, enclosing.blockName, ctx.vars, []);
        if (callCtx) {
          scopeResult = { stack: [{ key: '.', typeStr: callCtx.typeStr, fields: callCtx.fields ?? [] }], locals: new Map() };
        }
      }
    }

    const { stack, locals } = scopeResult ?? { stack: [] as ScopeFrame[], locals: new Map<string, TemplateVar>() };

    const linePrefix = document.lineAt(position.line).text.slice(0, position.character);
    const match = linePrefix.match(/(?:\$|\.)[\w.]*$/);
    if (!match) return [];

    const rawPath = match[0];
    const parts = rawPath.split('.');
    let lookupPath: string[];
    let filterPrefix: string;

    if (rawPath.endsWith('.')) {
      lookupPath = this.parser.parseDotPath(rawPath);
      filterPrefix = '';
    } else {
      filterPrefix = parts[parts.length - 1];
      lookupPath = this.parser.parseDotPath(rawPath.slice(0, rawPath.length - filterPrefix.length));
    }

    let fields: FieldInfo[] = [];
    if (lookupPath.length === 1 && lookupPath[0] === '.') {
      const res = resolvePath(['.'], ctx.vars, stack, locals);
      fields = (res.found && res.fields) ? res.fields : (stack.slice().reverse().find(f => f.key === '.')?.fields ?? [...ctx.vars.values()] as any);
    } else if (lookupPath.length === 1 && lookupPath[0] === '$') {
      fields = [...ctx.vars.values()] as any;
    } else {
      const res = resolvePath(lookupPath, ctx.vars, stack, locals);
      if (res.found && res.fields) fields = res.fields;
    }

    return fields
      .filter(f => f.name.toLowerCase().startsWith(filterPrefix.toLowerCase()))
      .map(f => {
        const item = new vscode.CompletionItem(f.name, f.type === 'method' ? vscode.CompletionItemKind.Method : vscode.CompletionItemKind.Field);
        item.detail = f.isSlice ? `[]${f.type}` : f.type;
        if (f.doc) item.documentation = new vscode.MarkdownString(f.doc);
        return item;
      });
  }

  private findScopeAtPosition(
    nodes: TemplateNode[],
    position: vscode.Position,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    rootNodes: TemplateNode[],
    ctx: TemplateContext
  ): { stack: ScopeFrame[], locals: Map<string, TemplateVar> } | null {
    const blockLocals = new Map<string, TemplateVar>();

    for (const node of nodes) {
      if (node.kind === 'assignment') {
        const result = resolvePath(node.path, vars, scopeStack, blockLocals);
        if (result.found && node.assignVars) this.applyAssignmentLocals(node.assignVars, result, blockLocals);
      }

      if (node.endLine === undefined) continue;

      const startLine = node.line - 1;
      const endLine = node.endLine - 1;
      const afterStart = position.line > startLine || (position.line === startLine && position.character >= node.col - 1);
      const beforeEnd = position.line < endLine || (position.line === endLine && position.character <= (node.endCol ?? 0));

      if (afterStart && beforeEnd) {
        const childScope = this.buildChildScope(node, vars, scopeStack, blockLocals, rootNodes);
        const childVars = childScope?.childVars ?? vars;
        const childStack = childScope?.childStack ?? scopeStack;

        if (node.children) {
          const inner = this.findScopeAtPosition(node.children, position, childVars, childStack, rootNodes, ctx);
          if (inner) return inner;
        }

        return { stack: childStack, locals: blockLocals };
      }
    }

    return null;
  }

  // ── Utilities ──────────────────────────────────────────────────────────────

  private fieldsToVarMap(fields: FieldInfo[]): Map<string, TemplateVar> {
    const m = new Map<string, TemplateVar>();
    for (const f of fields) m.set(f.name, fieldInfoToTemplateVar(f));
    return m;
  }

  getTemplateDefinitionFromGo(document: vscode.TextDocument, position: vscode.Position): vscode.Location | null {
    const line = document.lineAt(position.line).text;
    const renderRegex = /\.Render\s*\(\s*"([^"]+)"/g;
    let match: RegExpExecArray | null;
    while ((match = renderRegex.exec(line)) !== null) {
      const templatePath = match[1];
      const matchStart = match.index + '.Render("'.length;
      if (position.character >= matchStart && position.character <= matchStart + templatePath.length) {
        const absPath = this.graphBuilder.resolveTemplatePath(templatePath);
        if (absPath) return new vscode.Location(vscode.Uri.file(absPath), new vscode.Position(0, 0));
      }
    }
    return null;
  }
}

function fieldInfoToTemplateVar(f: FieldInfo): TemplateVar {
  return { name: f.name, type: f.type, fields: f.fields, isSlice: f.isSlice, defFile: f.defFile, defLine: f.defLine, defCol: f.defCol, doc: f.doc };
}

function isFileBasedPartial(name: string): boolean {
  if (name.includes('/') || name.includes('\\')) return true;
  return ['.html', '.tmpl', '.gohtml', '.tpl', '.htm'].includes(name.slice(name.lastIndexOf('.')).toLowerCase());
}
