/**
 * Package validator provides the core template validation logic for the Rex Template Validator.
 * It handles AST traversal, scope resolution, and diagnostic generation.
 *
 * Named blocks ({{ define "name" }} / {{ block "name" ... }}) are now resolved
 * from a cross-file NamedBlockRegistry built by KnowledgeGraphBuilder. This means
 * intellisense (hover, completion, validation) works correctly inside a named
 * block even when it lives in a different file from the template that calls it.
 *
 * Duplicate block-name detection: if the same name is declared in more than one
 * file, a diagnostic error is surfaced on every call-site that references it.
 */

import * as fs from 'fs';
import * as path from 'path';
import * as vscode from 'vscode';
import {
  FieldInfo,
  NamedBlockEntry,
  ScopeFrame,
  TemplateContext,
  TemplateNode,
  TemplateVar,
  ValidationError,
} from './types';
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

  async validateDocument(
    document: vscode.TextDocument,
    providedCtx?: TemplateContext
  ): Promise<vscode.Diagnostic[]> {
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

  // ── Named block registry helpers ───────────────────────────────────────────

  /**
   * Look up a named block from the cross-file registry.
   * Returns the entry or undefined.
   * Also checks for duplicate errors and returns them via the `duplicateError` field.
   */
  private resolveNamedBlock(name: string): {
    entry: NamedBlockEntry | undefined;
    isDuplicate: boolean;
    duplicateMessage?: string;
  } {
    const graph = this.graphBuilder.getGraph();
    const entries = graph.namedBlocks.get(name);

    if (!entries || entries.length === 0) {
      return { entry: undefined, isDuplicate: false };
    }

    if (entries.length > 1) {
      const locs = entries.map(e => `${e.templatePath}:${e.line}`).join(', ');
      return {
        entry: entries[0],
        isDuplicate: true,
        duplicateMessage: `Named block "${name}" is declared in multiple files: ${locs}. Only one declaration is allowed.`,
      };
    }

    return { entry: entries[0], isDuplicate: false };
  }

  // ── Shared scope helpers ───────────────────────────────────────────────────

  private buildChildScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    rootNodes: TemplateNode[]
  ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
    switch (node.kind) {
      case 'range': {
        if (node.path.length > 0) {
          const elemScope = this.buildRangeElemScope(node, vars, scopeStack, blockLocals);
          if (elemScope) return { childVars: vars, childStack: [...scopeStack, elemScope] };
        }
        return null;
      }

      case 'with': {
        if (node.path.length > 0) {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (result.found) {
            const childScope: ScopeFrame = {
              key: '.',
              typeStr: result.typeStr,
              fields: result.fields ?? [],
              isMap: result.isMap,
              keyType: result.keyType,
              elemType: result.elemType,
              isSlice: result.isSlice,
            };
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

      case 'block':
      case 'define': {
        return this.buildNamedBlockScope(node, vars, rootNodes);
      }

      default:
        return null;
    }
  }

  /**
   * Build scope for a named block body.
   *
   * Resolution order:
   * 1. Find the {{ template "name" .Ctx }} call site anywhere in rootNodes.
   * 2. If not found (e.g. this is a `define` with no inline call), fall back
   *    to the block's own context argument (for `block` nodes).
   * 3. As a last resort fall back to the full root-level vars.
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
        childStack: [{
          key: '.',
          typeStr: callCtx.typeStr,
          fields: callCtx.fields ?? [],
          isMap: callCtx.isMap,
          keyType: callCtx.keyType,
          elemType: callCtx.elemType,
          isSlice: callCtx.isSlice
        }],
      };
    }


    if (node.kind === 'block' && node.path.length > 0) {
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

    const elemTypeStr =
      result.isSlice && result.typeStr.startsWith('[]')
        ? result.typeStr.slice(2)
        : result.isMap && result.elemType
          ? result.elemType
          : result.typeStr;

    let isElemSlice = false;
    let isElemMap = false;
    let elemKeyType: string | undefined;
    let elemInnerType = elemTypeStr;

    if (elemTypeStr.startsWith('[]')) {
      isElemSlice = true;
      elemInnerType = elemTypeStr.slice(2);
    } else if (elemTypeStr.startsWith('map[')) {
      isElemMap = true;
      let depth = 0;
      let splitIdx = -1;
      for (let i = 4; i < elemTypeStr.length; i++) {
        if (elemTypeStr[i] === '[') depth++;
        else if (elemTypeStr[i] === ']') {
          if (depth === 0) { splitIdx = i; break; }
          depth--;
        }
      }
      if (splitIdx !== -1) {
        elemKeyType = elemTypeStr.slice(4, splitIdx).trim();
        elemInnerType = elemTypeStr.slice(splitIdx + 1).trim();
      }
    }

    const elemScope: ScopeFrame = {
      key: '.',
      typeStr: elemTypeStr,
      fields: result.fields ?? [],
      isRange: true,
      sourceVar,
      isMap: isElemMap,
      keyType: elemKeyType,
      elemType: elemInnerType,
      isSlice: isElemSlice,
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
          isSlice: isElemSlice,
          isMap: isElemMap,
          keyType: elemKeyType,
          elemType: elemInnerType,
        });
      } else if (node.valVar) {
        elemScope.locals.set(node.valVar, {
          name: node.valVar,
          type: elemScope.typeStr,
          fields: elemScope.fields,
          isSlice: isElemSlice,
          isMap: isElemMap,
          keyType: elemKeyType,
          elemType: elemInnerType,
        });
      }
    }

    return elemScope;
  }

  private buildRangeScope(
    rangePath: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    _ctx: TemplateContext,
    blockLocals?: Map<string, TemplateVar>
  ): ScopeFrame | null {
    const result = resolvePath(rangePath, vars, scopeStack, blockLocals);
    if (!result.found) return null;

    let typeStr = result.typeStr;
    if (result.isSlice && typeStr.startsWith('[]')) typeStr = typeStr.slice(2);
    else if (result.isMap && result.elemType) typeStr = result.elemType;

    let sourceVar: TemplateVar | undefined;
    if (rangePath[0].startsWith('$') && rangePath[0] !== '$') {
      sourceVar =
        blockLocals?.get(rangePath[0]) ||
        scopeStack
          .slice()
          .reverse()
          .find(f => f.locals?.has(rangePath[0]))
          ?.locals?.get(rangePath[0]);
    } else {
      sourceVar = vars.get(rangePath[0]);
    }

    return { key: '.', typeStr, fields: result.fields ?? [], isRange: true, sourceVar };
  }

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
      blockLocals.set(assignVars[0], {
        name: assignVars[0],
        type: result.keyType ?? 'unknown',
        isSlice: false,
      });
      blockLocals.set(assignVars[1], {
        name: assignVars[1],
        type: result.elemType ?? 'unknown',
        fields: result.fields,
        isSlice: false,
      });
    } else if (assignVars.length === 2 && result.isSlice) {
      blockLocals.set(assignVars[0], { name: assignVars[0], type: 'int', isSlice: false });
      blockLocals.set(assignVars[1], {
        name: assignVars[1],
        type: result.elemType ?? 'unknown',
        fields: result.fields,
        isSlice: false,
      });
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
          errors.push({
            message: `Expression "${node.assignExpr}" is not defined`,
            line: node.line,
            col: node.col,
            severity: 'warning',
            variable: node.assignExpr,
          });
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
          const displayPath =
            node.path[0] === '$'
              ? '$.' + node.path.slice(1).join('.')
              : node.path[0].startsWith('$')
                ? node.path.join('.')
                : '.' + node.path.join('.');
          errors.push({
            message: `Template variable "${displayPath}" is not defined in the render context`,
            line: node.line,
            col: node.col,
            severity: 'error',
            variable: node.rawText,
          });
        }
        break;
      }

      case 'range': {
        // Handle both named-path ranges (.Items, $.Roles) and bare-dot ranges
        // (range $k, $v := .) — the latter iterates over the current dot context.
        const isBareDotsRange =
          node.path.length === 0 ||
          (node.path.length === 1 && node.path[0] === '.');

        if (!isBareDotsRange) {
          // Named path: validate that the target exists.
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (!result.found) {
            errors.push({
              message: `Range target ".${node.path.join('.')}" is not defined`,
              line: node.line,
              col: node.col,
              severity: 'error',
              variable: node.path[0],
            });
            break; // still fall through to children traversal below
          }
        }

        // Build elem scope (handles both bare-dot and named-path, and injects
        // $k/$v locals when the range uses variable assignment).
        const elemScope = this.buildRangeElemScope(node, vars, scopeStack, blockLocals);
        if (elemScope && node.children) {
          this.validateNodes(
            node.children,
            vars,
            [...scopeStack, elemScope],
            errors,
            ctx,
            filePath
          );
          return;
        }
        break;
      }

      case 'with': {
        if (node.path.length > 0) {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (!result.found) {
            errors.push({
              message: `".${node.path.join('.')}" is not defined`,
              line: node.line,
              col: node.col,
              severity: 'warning',
              variable: node.path[0],
            });
          } else if (result.fields !== undefined) {
            const childScope: ScopeFrame = {
              key: '.',
              typeStr: result.typeStr,
              fields: result.fields,
              isMap: result.isMap,
              keyType: result.keyType,
              elemType: result.elemType,
              isSlice: result.isSlice,
            };
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
            if (node.children) {
              this.validateNodes(
                node.children,
                vars,
                [...scopeStack, childScope],
                errors,
                ctx,
                filePath
              );
            }
            return;
          }
        }
        break;
      }

      case 'if': {
        if (node.path.length > 0) {
          if (!resolvePath(node.path, vars, scopeStack, blockLocals).found) {
            errors.push({
              message: `".${node.path.join('.')}" is not defined`,
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
        this.validatePartial(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
        return;
      }

      // `block` and `define` — defer child validation to the cross-file registry
      // path so we always validate them with the correct caller-provided scope.
      // Skip inline child traversal here; it is handled in validateNamedBlockBody.
      case 'block':
      case 'define': {
        this.validateNamedBlockBody(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
        return;
      }
    }

    if (node.children) {
      this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath);
    }
  }

  /**
   * Validate the body of a {{ define }} or {{ block }} node.
   *
   * Steps:
   * 1. Look up the block in the cross-file registry.
   * 2. Surface a duplicate error if it exists.
   * 3. Resolve scope from the call site ({{ template "name" .Ctx }}) or
   *    the block tag's own context arg.
   * 4. Walk children with that resolved scope.
   */
  private validateNamedBlockBody(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    if (!node.blockName) return;

    const { isDuplicate, duplicateMessage } = this.resolveNamedBlock(node.blockName);
    if (isDuplicate && duplicateMessage) {
      errors.push({
        message: duplicateMessage,
        line: node.line,
        col: node.col,
        severity: 'error',
        variable: node.blockName,
      });
      // Still validate with best-guess scope so other errors aren't silenced.
    }

    if (!node.children || node.children.length === 0) return;

    // Find the scope from the rootNodes of THIS file (current validate call).
    // We need to search all template files for a {{ template "name" ... }} call.
    const childScope = this.resolveNamedBlockChildScope(
      node,
      vars,
      scopeStack,
      blockLocals,
      filePath
    );

    if (childScope) {
      this.validateNodes(
        node.children,
        childScope.childVars,
        childScope.childStack,
        errors,
        ctx,
        filePath
      );
    } else {
      // No call site found — validate with current scope (best effort)
      this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath);
    }
  }

  /**
   * Resolve the child scope for a named block body by searching for a
   * {{ template "name" .Ctx }} call across all template files in the graph.
   */
  private resolveNamedBlockChildScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    currentFilePath: string
  ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
    const name = node.blockName!;

    // --- Phase 1: search the current file's AST (already parsed) -----------
    // We re-parse the current file to get rootNodes for call-site search.
    let currentFileNodes: TemplateNode[] = [];
    try {
      const openDoc = vscode.workspace.textDocuments.find(
        d => d.uri.fsPath === currentFilePath
      );
      const content = openDoc
        ? openDoc.getText()
        : fs.existsSync(currentFilePath)
          ? fs.readFileSync(currentFilePath, 'utf8')
          : '';
      if (content) currentFileNodes = this.parser.parse(content);
    } catch { /* ignore */ }

    const localCallCtx = this.findCallSiteContext(currentFileNodes, name, vars, scopeStack, new Map(blockLocals));
    if (localCallCtx) {
      return {
        childVars: this.fieldsToVarMap(localCallCtx.fields ?? []),
        childStack: [{
          key: '.',
          typeStr: localCallCtx.typeStr,
          fields: localCallCtx.fields ?? [],
          isMap: localCallCtx.isMap,
          keyType: localCallCtx.keyType,
          elemType: localCallCtx.elemType,
          isSlice: localCallCtx.isSlice
        }],
      };
    }

    // --- Phase 2: search all other template files in the graph --------------
    const graph = this.graphBuilder.getGraph();
    for (const [, templateCtx] of graph.templates) {
      if (!templateCtx.absolutePath || templateCtx.absolutePath === currentFilePath) continue;
      if (!fs.existsSync(templateCtx.absolutePath)) continue;

      try {
        const openDoc = vscode.workspace.textDocuments.find(
          d => d.uri.fsPath === templateCtx.absolutePath
        );
        const content = openDoc
          ? openDoc.getText()
          : fs.readFileSync(templateCtx.absolutePath, 'utf8');

        const fileNodes = this.parser.parse(content);
        const callCtx = this.findCallSiteContext(fileNodes, name, templateCtx.vars, []);
        if (callCtx) {
          return {
            childVars: this.fieldsToVarMap(callCtx.fields ?? []),
            childStack: [
              {
                key: '.',
                typeStr: callCtx.typeStr,
                fields: callCtx.fields ?? [],
                isMap: callCtx.isMap,
                keyType: callCtx.keyType,
                elemType: callCtx.elemType,
                isSlice: callCtx.isSlice
              },
            ],
          };
        }
      } catch { /* ignore */ }
    }

    // --- Phase 3: for `block` nodes, fall back to the block's own context ---
    if (node.kind === 'block' && node.path.length > 0) {
      const result = resolvePath(node.path, vars, scopeStack, blockLocals);
      if (result.found) {
        return {
          childVars: this.fieldsToVarMap(result.fields ?? []),
          childStack: [{ key: '.', typeStr: result.typeStr, fields: result.fields ?? [] }],
        };
      }
    }

    return null;
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
            const searchStart =
              nameIdx !== -1 ? nameIdx + node.partialName!.length + 2 : 0;
            const ctxIdx = node.rawText.indexOf(contextArg, searchStart);
            if (ctxIdx !== -1) {
              const p = '.' + contextPath.join('.');
              const pIdx = contextArg.indexOf(p);
              errCol = node.col + ctxIdx + (pIdx !== -1 ? pIdx : 0);
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

    // Named block (no file extension / path separator) → cross-file registry lookup
    if (!isFileBasedPartial(node.partialName)) {
      this.validateNamedBlockCallSite(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
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

    const partialVars = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
    try {
      const openDoc = vscode.workspace.textDocuments.find(
        d => d.uri.fsPath === partialCtx.absolutePath
      );
      const content = openDoc
        ? openDoc.getText()
        : fs.readFileSync(partialCtx.absolutePath, 'utf8');
      const partialErrors = this.validate(
        content,
        { ...partialCtx, vars: partialVars },
        partialCtx.absolutePath
      );
      for (const e of partialErrors) {
        errors.push({ ...e, message: `[in partial "${node.partialName}"] ${e.message}` });
      }
    } catch { /* ignore read errors */ }
  }

  /**
   * Validate a {{ template "named-block" .Ctx }} call against the cross-file registry.
   *
   * - Checks for duplicate declarations and surfaces an error.
   * - Finds the block's AST node (possibly in another file).
   * - Validates the block's children with the resolved scope.
   */
  private validateNamedBlockCallSite(
    callNode: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    errors: ValidationError[],
    ctx: TemplateContext,
    filePath: string
  ) {
    if (!callNode.partialName) return;

    const { entry, isDuplicate, duplicateMessage } = this.resolveNamedBlock(callNode.partialName);

    if (isDuplicate && duplicateMessage) {
      errors.push({
        message: duplicateMessage,
        line: callNode.line,
        col: callNode.col,
        severity: 'error',
        variable: callNode.partialName,
      });
      // Continue validation with whatever block we found (first one).
    }

    if (!entry) {
      // Block not found in registry — it may be defined in the same file below
      // the call site, so try the current file as a fallback.
      this.validateNamedBlockFromCurrentFile(
        callNode, vars, scopeStack, blockLocals, errors, ctx, filePath
      );
      return;
    }

    // Read the file that contains the block definition.
    const contextArg = callNode.partialContext ?? '.';
    const partialVars = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);

    let childStack: ScopeFrame[];
    if (contextArg === '.' || contextArg === '$') {
      childStack = scopeStack;
    } else {
      const result = resolvePath(
        this.parser.parseDotPath(contextArg),
        vars,
        scopeStack,
        blockLocals
      );
      childStack = result.found
        ? [{
          key: '.',
          typeStr: result.typeStr,
          fields: result.fields ?? [],
          isMap: result.isMap,
          keyType: result.keyType,
          elemType: result.elemType,
          isSlice: result.isSlice,
        }]
        : scopeStack;
    }

    if (!entry.node.children || entry.node.children.length === 0) return;

    // If the block is in the same file, validate directly (avoid re-reading).
    if (entry.absolutePath === filePath) {
      const blockErrors: ValidationError[] = [];
      this.validateNodes(entry.node.children, partialVars, childStack, blockErrors, ctx, filePath);
      for (const e of blockErrors) {
        errors.push({
          ...e,
          message: `[in block "${callNode.partialName}"] ${e.message}`,
        });
      }
      return;
    }

    // Block is in another file — validate with the resolved context.
    try {
      const openDoc = vscode.workspace.textDocuments.find(
        d => d.uri.fsPath === entry.absolutePath
      );
      const blockFileContent = openDoc
        ? openDoc.getText()
        : fs.readFileSync(entry.absolutePath, 'utf8');

      // Re-parse to get a fresh AST with correct line numbers.
      const blockFileNodes = this.parser.parse(blockFileContent);
      const freshBlockNode = this.findDefineNodeInAST(blockFileNodes, callNode.partialName);
      if (!freshBlockNode?.children) return;

      const blockCtx: TemplateContext = {
        templatePath: entry.templatePath,
        absolutePath: entry.absolutePath,
        vars: partialVars,
        renderCalls: ctx.renderCalls,
      };

      const blockErrors: ValidationError[] = [];
      this.validateNodes(
        freshBlockNode.children,
        partialVars,
        childStack,
        blockErrors,
        blockCtx,
        entry.absolutePath
      );
      for (const e of blockErrors) {
        errors.push({
          ...e,
          message: `[in block "${callNode.partialName}" @ ${entry.templatePath}] ${e.message}`,
        });
      }
    } catch { /* ignore */ }
  }

  /**
   * Fallback: search the current file's AST for the named block definition
   * when the registry lookup found nothing (e.g. the file hasn't been indexed yet).
   */
  private validateNamedBlockFromCurrentFile(
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
        const result = resolvePath(
          this.parser.parseDotPath(contextArg),
          vars,
          scopeStack,
          blockLocals
        );
        childStack = result.found
          ? [{
            key: '.',
            typeStr: result.typeStr,
            fields: result.fields ?? [],
            isMap: result.isMap,
            keyType: result.keyType,
            elemType: result.elemType,
            isSlice: result.isSlice,
          }]
          : scopeStack;
      }

      const blockErrors: ValidationError[] = [];
      this.validateNodes(
        blockNode.children,
        partialVars,
        childStack,
        blockErrors,
        ctx,
        filePath
      );
      for (const e of blockErrors) {
        errors.push({
          ...e,
          message: `[in block "${callNode.partialName}"] ${e.message}`,
        });
      }
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

    const result = resolvePath(
      this.parser.parseDotPath(contextArg),
      vars,
      scopeStack,
      blockLocals
    );
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
      // Try to find scope inside a define/block that might be in this file.
      const enclosing = this.findEnclosingBlockOrDefine(nodes, position);
      if (enclosing?.blockName) {
        const callCtx = this.resolveNamedBlockCallCtxForHover(
          enclosing.blockName,
          ctx.vars,
          nodes,
          document.uri.fsPath
        );
        if (callCtx) {
          const md = new vscode.MarkdownString();
          md.isTrusted = true;
          md.appendCodeblock(`. : ${callCtx.typeStr}`, 'go');
          if (callCtx.fields?.length) {
            md.appendMarkdown('\n\n---\n\n**Fields:**\n\n');
            for (const f of callCtx.fields.slice(0, 30)) {
              md.appendMarkdown(
                `**${f.name}** \`${(f as FieldInfo).isSlice ? `[]${f.type}` : f.type}\`\n\n`
              );
            }
          }
          return new vscode.Hover(md);
        }
      }
      return null;
    }

    const { node, stack, vars: hitVars, locals: hitLocals } = hit;

    const isBareVarDot = node.kind === 'variable' && node.path.length === 1 && node.path[0] === '.';
    const isPartialDotCtx =
      node.kind === 'partial' && (node.partialContext ?? '.') === '.';
    if (isBareVarDot || isPartialDotCtx) return this.buildDotHover(stack, hitVars);

    const result = resolvePath(node.path, hitVars, stack, hitLocals);
    if (!result.found) return null;

    const varName =
      node.path[0] === '.'
        ? '.'
        : node.path[0] === '$'
          ? '$.' + node.path.slice(1).join('.')
          : node.path[0].startsWith('$')
            ? node.path.join('.')
            : '.' + node.path.join('.');

    const md = new vscode.MarkdownString();
    md.isTrusted = true;
    md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');

    const varInfo = this.findVariableInfo(node.path, hitVars, stack, hitLocals);
    if (varInfo?.doc) {
      md.appendMarkdown('\n\n---\n\n');
      md.appendMarkdown(varInfo.doc);
    }

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

  /**
   * Resolve the call-site context for a named block from the cross-file registry
   * (used by hover when cursor is inside a define/block body).
   */
  private resolveNamedBlockCallCtxForHover(
    blockName: string,
    vars: Map<string, TemplateVar>,
    currentFileNodes: TemplateNode[],
    currentFilePath: string
  ): { typeStr: string; fields?: FieldInfo[]; isMap?: boolean; keyType?: string; elemType?: string; isSlice?: boolean } | null {
    // 1. Search current file first.
    const localCtx = this.findCallSiteContext(currentFileNodes, blockName, vars, []);
    if (localCtx) return localCtx;

    // 2. Search all other files.
    const graph = this.graphBuilder.getGraph();
    for (const [, templateCtx] of graph.templates) {
      if (!templateCtx.absolutePath || templateCtx.absolutePath === currentFilePath) continue;
      if (!fs.existsSync(templateCtx.absolutePath)) continue;
      try {
        const openDoc = vscode.workspace.textDocuments.find(
          d => d.uri.fsPath === templateCtx.absolutePath
        );
        const content = openDoc
          ? openDoc.getText()
          : fs.readFileSync(templateCtx.absolutePath, 'utf8');
        const fileNodes = this.parser.parse(content);
        const callCtx = this.findCallSiteContext(fileNodes, blockName, templateCtx.vars, []);
        if (callCtx) return callCtx;
      } catch { /* ignore */ }
    }
    return null;
  }

  private buildDotHover(
    scopeStack: ScopeFrame[],
    vars: Map<string, TemplateVar>
  ): vscode.Hover | null {
    const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
    const typeStr = dotFrame ? (dotFrame.typeStr ?? 'unknown') : 'RenderContext';
    const fields: FieldInfo[] =
      dotFrame?.fields ??
      [...vars.values()].map(v => ({
        name: v.name,
        type: v.type,
        fields: v.fields,
        isSlice: v.isSlice ?? false,
        doc: v.doc,
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
    if (topVarName === '$' && path.length > 1) {
      topVarName = path[1];
      searchPath = path.slice(1);
    }
    if (topVarName === '.' || topVarName === '$') return null;

    let topVar: TemplateVar | undefined;
    if (topVarName.startsWith('$') && topVarName !== '$') {
      topVar = blockLocals?.get(topVarName);
      if (!topVar) {
        for (let i = scopeStack.length - 1; i >= 0; i--) {
          if (scopeStack[i].locals?.has(topVarName)) {
            topVar = scopeStack[i].locals!.get(topVarName);
            break;
          }
        }
      }
    } else {
      topVar = vars.get(topVarName);
      if (!topVar) {
        const field = scopeStack
          .slice()
          .reverse()
          .find(f => f.key === '.')
          ?.fields?.find(f => f.name === topVarName);
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

    let hoveredIndex = node.path.length - 1;
    if (node.rawText) {
      const cursorOffset = position.character - (node.col - 1);
      const pathStr =
        node.path[0] === '$'
          ? '$.' + node.path.slice(1).join('.')
          : node.path[0].startsWith('$')
            ? node.path.join('.')
            : '.' + node.path.join('.');
      const pathStart = node.rawText.indexOf(pathStr);
      if (pathStart !== -1) {
        const offsetInPath = cursorOffset - pathStart;
        if (offsetInPath >= 0 && offsetInPath <= pathStr.length) {
          let currentLen = 0;
          const segments = pathStr.split('.');
          for (let i = 0; i < segments.length; i++) {
            currentLen += segments[i].length + 1;
            if (offsetInPath <= currentLen) {
              hoveredIndex =
                node.path[0] === '$'
                  ? i
                  : node.path[0].startsWith('$')
                    ? i
                    : i === 0
                      ? 0
                      : i - 1;
              break;
            }
          }
        }
      }
    }

    const targetPath = node.path.slice(0, hoveredIndex + 1);
    let pathForDef = targetPath;
    let topVarName = pathForDef[0];
    if (topVarName === '$' && pathForDef.length > 1) {
      topVarName = pathForDef[1];
      pathForDef = pathForDef.slice(1);
    }
    if (!topVarName || topVarName === '.' || topVarName === '$') return null;

    if (topVarName.startsWith('$') && topVarName !== '$') {
      const declaredVar = this.findDeclaredVariableDefinition(
        node,
        nodes,
        position,
        ctx,
        hit.stack,
        hit.locals
      );
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
        const fieldLoc = this.navigateToFieldDefinition(
          pathForDef.slice(1),
          passedVar.fields ?? [],
          ctx
        );
        if (fieldLoc) return fieldLoc;
        if (passedVar.defFile && passedVar.defLine) {
          const abs = this.resolveGoFile(passedVar.defFile);
          if (abs)
            return new vscode.Location(
              vscode.Uri.file(abs),
              new vscode.Position(
                Math.max(0, passedVar.defLine - 1),
                (passedVar.defCol ?? 1) - 1
              )
            );
        }
        if (rc.file) {
          const abs = this.graphBuilder.resolveGoFilePath(rc.file);
          if (abs)
            return new vscode.Location(
              vscode.Uri.file(abs),
              new vscode.Position(Math.max(0, rc.line - 1), 0)
            );
        }
      }
    }

    return this.findRangeVariableDefinition(pathForDef, stack, ctx);
  }

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
          if (abs)
            return new vscode.Location(
              vscode.Uri.file(abs),
              new vscode.Position(
                Math.max(0, field.defLine - 1),
                (field.defCol ?? 1) - 1
              )
            );
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
      if (abs)
        return new vscode.Location(
          vscode.Uri.file(abs),
          new vscode.Position(Math.max(0, t.defLine - 1), (t.defCol ?? 1) - 1)
        );
    }
    return null;
  }

  private resolveGoFile(filePath: string): string | null {
    if (path.isAbsolute(filePath) && fs.existsSync(filePath)) return filePath;
    return this.graphBuilder.resolveGoFilePath(filePath);
  }

  private findDefinitionInScope(
    targetPath: string[],
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
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
        const loc = this.navigateToFieldDefinition(searchPath.slice(1), topVar.fields ?? [], ctx);
        if (loc) return loc;
      }
      if (topVar.defFile && topVar.defLine) {
        const abs = this.resolveGoFile(topVar.defFile);
        if (abs)
          return new vscode.Location(
            vscode.Uri.file(abs),
            new vscode.Position(Math.max(0, topVar.defLine - 1), (topVar.defCol ?? 1) - 1)
          );
      }
    }

    const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
    if (dotFrame?.fields) return this.navigateToFieldDefinition(searchPath, dotFrame.fields, ctx);

    return null;
  }

  private findDeclaredVariableDefinition(
    targetNode: TemplateNode,
    nodes: TemplateNode[],
    position: vscode.Position,
    ctx: TemplateContext,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>
  ): vscode.Location | null {
    const varName = targetNode.path[0];
    if (!varName?.startsWith('$')) return null;
    for (const node of nodes) {
      const result = this.findVariableAssignment(
        node,
        varName,
        position,
        ctx,
        scopeStack,
        blockLocals
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
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>
  ): vscode.Location | null {
    if (
      node.kind === 'assignment' &&
      node.assignVars?.includes(varName) &&
      node.assignExpr
    ) {
      const rhsPath = this.parser.parseDotPath(node.assignExpr);
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame?.fields) {
        const fieldPath = rhsPath[0] === '.' ? rhsPath.slice(1) : rhsPath;
        const loc = this.navigateToFieldDefinition(fieldPath, dotFrame.fields, ctx);
        if (loc) return loc;
      }
      return this.findVariableDefinition(
        { kind: 'variable', path: rhsPath, rawText: node.assignExpr, line: node.line, col: node.col },
        ctx
      );
    }
    if (node.children) {
      for (const child of node.children) {
        const result = this.findVariableAssignment(
          child,
          varName,
          position,
          ctx,
          scopeStack,
          blockLocals
        );
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
        if (abs)
          return new vscode.Location(
            vscode.Uri.file(abs),
            new vscode.Position(Math.max(0, v.defLine - 1), (v.defCol ?? 1) - 1)
          );
      }
    }
    return null;
  }

  private findRangeAssignedVariable(
    node: TemplateNode,
    scopeStack: ScopeFrame[],
    ctx: TemplateContext
  ): vscode.Location | null {
    if (!node.path[0]?.startsWith('$')) return null;
    for (const frame of scopeStack) {
      if (frame.isRange && frame.sourceVar?.defFile && frame.sourceVar.defLine) {
        const abs = this.resolveGoFile(frame.sourceVar.defFile);
        if (abs)
          return new vscode.Location(
            vscode.Uri.file(abs),
            new vscode.Position(
              Math.max(0, frame.sourceVar.defLine - 1),
              (frame.sourceVar.defCol ?? 1) - 1
            )
          );
      }
    }
    return null;
  }

  private findRangeVariableDefinition(
    targetPath: string[],
    scopeStack: ScopeFrame[],
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
        const loc = this.navigateToFieldDefinition(targetPath.slice(1), field.fields ?? [], ctx);
        if (loc) return loc;
      }
      if (field.defFile && field.defLine) {
        const abs = this.resolveGoFile(field.defFile);
        if (abs)
          return new vscode.Location(
            vscode.Uri.file(abs),
            new vscode.Position(Math.max(0, field.defLine - 1), (field.defCol ?? 1) - 1)
          );
      }
      if (frame.sourceVar?.defFile && frame.sourceVar.defLine) {
        const abs = this.resolveGoFile(frame.sourceVar.defFile);
        if (abs)
          return new vscode.Location(
            vscode.Uri.file(abs),
            new vscode.Position(
              Math.max(0, frame.sourceVar.defLine - 1),
              (frame.sourceVar.defCol ?? 1) - 1
            )
          );
      }
    }
    return null;
  }

  private findTemplateNameHover(
    nodes: TemplateNode[],
    position: vscode.Position
  ): string | null {
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

  private async findPartialDefinitionAtPosition(
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
              if (templatePath)
                return new vscode.Location(vscode.Uri.file(templatePath), new vscode.Position(0, 0));
            } else {
              return await this.findNamedBlockDefinitionLocation(name, ctx);
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

  /**
   * Find the definition location of a named block by consulting the cross-file
   * registry first, then falling back to scanning files.
   */
  private async findNamedBlockDefinitionLocation(
    name: string,
    ctx: TemplateContext
  ): Promise<vscode.Location | null> {
    // 1. Cross-file registry lookup (fast path)
    const { entry } = this.resolveNamedBlock(name);
    if (entry) {
      return new vscode.Location(
        vscode.Uri.file(entry.absolutePath),
        new vscode.Position(entry.line - 1, entry.col - 1)
      );
    }

    // 2. Legacy scan fallback (for files not yet in the registry)
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
        const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath);
        let content = openDoc ? openDoc.getText() : '';
        if (!content) {
          if (!fs.existsSync(filePath)) continue;
          content = await fs.promises.readFile(filePath, 'utf-8');
        }
        if (!defineRegex.test(content)) continue;
        const defNode = this.findDefineNodeInAST(this.parser.parse(content), name);
        if (defNode)
          return new vscode.Location(
            vscode.Uri.file(filePath),
            new vscode.Position(defNode.line - 1, defNode.col - 1)
          );
      } catch { /* ignore */ }
    }
    return null;
  }

  private findDefineNodeInAST(
    nodes: TemplateNode[],
    name: string
  ): TemplateNode | null {
    for (const node of nodes) {
      if (
        (node.kind === 'define' || node.kind === 'block') &&
        node.blockName === name
      )
        return node;
      if (node.children) {
        const found = this.findDefineNodeInAST(node.children, name);
        if (found) return found;
      }
    }
    return null;
  }

  // ── Block/Define Context Inference ──────────────────────────────────────────

  private findEnclosingBlockOrDefine(
    nodes: TemplateNode[],
    position: vscode.Position
  ): TemplateNode | null {
    for (const node of nodes) {
      if (node.endLine === undefined) continue;
      const afterStart =
        position.line > node.line - 1 ||
        (position.line === node.line - 1 && position.character >= node.col - 1);
      const beforeEnd =
        position.line < node.endLine - 1 ||
        (position.line === node.endLine - 1 &&
          position.character <= (node.endCol ?? 0) - 1);
      if (afterStart && beforeEnd) {
        if (node.kind === 'block' || node.kind === 'define') {
          return (
            this.findEnclosingBlockOrDefine(node.children ?? [], position) || node
          );
        } else if (node.children) {
          const found = this.findEnclosingBlockOrDefine(node.children, position);
          if (found) return found;
        }
      }
    }
    return null;
  }

  private findCallSiteContext(
    nodes: TemplateNode[],
    blockName: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar> = new Map()
  ): { typeStr: string; fields?: FieldInfo[]; isMap?: boolean; keyType?: string; elemType?: string; isSlice?: boolean } | null {
    for (const node of nodes) {
      if (node.kind === 'assignment') {
        const result = resolvePath(node.path, vars, scopeStack, blockLocals);
        if (result.found && node.assignVars) {
          this.applyAssignmentLocals(node.assignVars, result, blockLocals);
        }
      }

      const isTemplateCall = node.kind === 'partial' && node.partialName === blockName;
      const isBlockDecl = node.kind === 'block' && node.blockName === blockName;

      if (isTemplateCall || isBlockDecl) {
        let contextArg: string;
        if (isTemplateCall) {
          contextArg = node.partialContext ?? '.';
        } else {
          contextArg =
            node.path.length === 0
              ? '.'
              : node.path[0] === '.'
                ? '.' + node.path.slice(1).join('.')
                : '.' + node.path.join('.');
        }

        if (contextArg === '.' || contextArg === '') {
          const frame = scopeStack.slice().reverse().find(f => f.key === '.');
          if (frame) return { typeStr: frame.typeStr, fields: frame.fields, isMap: frame.isMap, keyType: frame.keyType, elemType: frame.elemType, isSlice: frame.isSlice };
          return { typeStr: 'context', fields: [...vars.values()] as unknown as FieldInfo[] };
        }

        const result = resolvePath(this.parser.parseDotPath(contextArg), vars, scopeStack, blockLocals);
        return result.found ? {
          typeStr: result.typeStr,
          fields: result.fields,
          isMap: result.isMap,
          keyType: result.keyType,
          elemType: result.elemType,
          isSlice: result.isSlice
        } : null;
      }

      if (node.children) {
        let childStack = scopeStack;
        let childLocals = new Map(blockLocals);

        if (node.kind === 'range') {
          const elemScope = this.buildRangeElemScope(node, vars, scopeStack, childLocals);
          if (elemScope) childStack = [...scopeStack, elemScope];
        } else if (node.kind === 'with') {
          if (node.path.length > 0) {
            const result = resolvePath(node.path, vars, scopeStack, childLocals);
            if (result.found && result.fields !== undefined) {
              const childScope: ScopeFrame = {
                key: '.',
                typeStr: result.typeStr,
                fields: result.fields,
                isMap: result.isMap,
                keyType: result.keyType,
                elemType: result.elemType,
                isSlice: result.isSlice
              };
              if (node.valVar) {
                childScope.locals = new Map();
                childScope.locals.set(node.valVar, {
                  name: node.valVar,
                  type: result.typeStr,
                  fields: result.fields,
                  isSlice: result.isSlice ?? false,
                  isMap: result.isMap,
                  keyType: result.keyType,
                  elemType: result.elemType
                });
              }
              childStack = [...scopeStack, childScope];
            }
          }
        } else if (node.kind === 'block') {
          if (node.path.length > 0) {
            const result = resolvePath(node.path, vars, scopeStack, childLocals);
            if (result.found && result.fields !== undefined) {
              childStack = [...scopeStack, { key: '.', typeStr: result.typeStr, fields: result.fields }];
            }
          }
        }

        const found = this.findCallSiteContext(node.children, blockName, vars, childStack, childLocals);
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
  ): {
    node: TemplateNode;
    stack: ScopeFrame[];
    vars: Map<string, TemplateVar>;
    locals: Map<string, TemplateVar>;
  } | null {
    const blockLocals = new Map<string, TemplateVar>();

    for (const node of nodes) {
      const startLine = node.line - 1;
      const startCol = node.col - 1;

      if (node.kind === 'assignment') {
        const result = resolvePath(node.path, vars, scopeStack, blockLocals);
        if (result.found && node.assignVars)
          this.applyAssignmentLocals(node.assignVars, result, blockLocals);
      }

      if (node.kind === 'variable') {
        const endCol = startCol + node.rawText.length;
        if (
          position.line === startLine &&
          position.character >= startCol &&
          position.character <= endCol
        ) {
          return { node, stack: scopeStack, vars, locals: blockLocals };
        }
        continue;
      }

      if (node.endLine === undefined) {
        const endCol = startCol + (node.rawText?.length ?? 0);
        if (
          position.line === startLine &&
          position.character >= startCol &&
          position.character <= endCol
        ) {
          return { node, stack: scopeStack, vars, locals: blockLocals };
        }
        continue;
      }

      const endLine = node.endLine - 1;
      const afterStart =
        position.line > startLine ||
        (position.line === startLine && position.character >= startCol);
      const beforeEnd =
        position.line < endLine ||
        (position.line === endLine && position.character <= (node.endCol ?? 0) - 1);
      if (!afterStart || !beforeEnd) continue;

      // Cursor on opening tag
      if (
        position.line === startLine &&
        position.character <= startCol + node.rawText.length
      ) {
        return { node, stack: scopeStack, vars, locals: blockLocals };
      }

      // For define/block nodes, we need to search across files for the correct scope.
      // Build scope using the cross-file registry so hover works in any file.
      let childVars = vars;
      let childStack = scopeStack;

      if ((node.kind === 'define' || node.kind === 'block') && node.blockName) {
        // Prefer cross-file call-site context for block/define bodies.
        const callCtx = this.resolveNamedBlockCallCtxForHover(
          node.blockName,
          vars,
          rootNodes,
          '' // no current file path needed here; it will search all files
        );
        if (callCtx) {
          childVars = this.fieldsToVarMap(callCtx.fields ?? []);
          childStack = [{
            key: '.',
            typeStr: callCtx.typeStr,
            fields: callCtx.fields ?? [],
            isMap: callCtx.isMap,
            keyType: callCtx.keyType,
            elemType: callCtx.elemType,
            isSlice: callCtx.isSlice
          }];
        }
      } else {
        const childScope = this.buildChildScope(node, vars, scopeStack, blockLocals, rootNodes);
        if (childScope) {
          childVars = childScope.childVars;
          childStack = childScope.childStack;
        }
      }

      if (node.children) {
        const found = this.findNodeAtPosition(
          node.children,
          position,
          childVars,
          childStack,
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
        const callCtx = this.resolveNamedBlockCallCtxForHover(
          enclosing.blockName,
          ctx.vars,
          nodes,
          document.uri.fsPath
        );
        if (callCtx) {
          scopeResult = {
            stack: [{
              key: '.',
              typeStr: callCtx.typeStr,
              fields: callCtx.fields ?? [],
              isMap: callCtx.isMap,
              keyType: callCtx.keyType,
              elemType: callCtx.elemType,
              isSlice: callCtx.isSlice
            }],
            locals: new Map(),
          };
        }
      }
    }

    const { stack, locals } = scopeResult ?? {
      stack: [] as ScopeFrame[],
      locals: new Map<string, TemplateVar>(),
    };

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
      lookupPath = this.parser.parseDotPath(
        rawPath.slice(0, rawPath.length - filterPrefix.length)
      );
    }

    let fields: FieldInfo[] = [];
    if (lookupPath.length === 1 && lookupPath[0] === '.') {
      const res = resolvePath(['.'], ctx.vars, stack, locals);
      fields =
        res.found && res.fields
          ? res.fields
          : stack.slice().reverse().find(f => f.key === '.')?.fields ??
          ([...ctx.vars.values()] as unknown as FieldInfo[]);
    } else if (lookupPath.length === 1 && lookupPath[0] === '$') {
      fields = [...ctx.vars.values()] as unknown as FieldInfo[];
    } else {
      const res = resolvePath(lookupPath, ctx.vars, stack, locals);
      if (res.found && res.fields) fields = res.fields;
    }

    return fields
      .filter(f => f.name.toLowerCase().startsWith(filterPrefix.toLowerCase()))
      .map(f => {
        const item = new vscode.CompletionItem(
          f.name,
          f.type === 'method'
            ? vscode.CompletionItemKind.Method
            : vscode.CompletionItemKind.Field
        );
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
  ): { stack: ScopeFrame[]; locals: Map<string, TemplateVar> } | null {
    const blockLocals = new Map<string, TemplateVar>();

    for (const node of nodes) {
      if (node.kind === 'assignment') {
        const result = resolvePath(node.path, vars, scopeStack, blockLocals);
        if (result.found && node.assignVars)
          this.applyAssignmentLocals(node.assignVars, result, blockLocals);
      }

      if (node.endLine === undefined) continue;

      const startLine = node.line - 1;
      const endLine = node.endLine - 1;
      const afterStart =
        position.line > startLine ||
        (position.line === startLine && position.character >= node.col - 1);
      const beforeEnd =
        position.line < endLine ||
        (position.line === endLine && position.character <= (node.endCol ?? 0));

      if (afterStart && beforeEnd) {
        // For define/block, resolve scope from cross-file registry.
        if ((node.kind === 'define' || node.kind === 'block') && node.blockName) {
          const callCtx = this.resolveNamedBlockCallCtxForHover(
            node.blockName,
            vars,
            rootNodes,
            ctx.absolutePath
          );
          if (callCtx) {
            const childVars = this.fieldsToVarMap(callCtx.fields ?? []);
            const childStack: ScopeFrame[] = [
              {
                key: '.',
                typeStr: callCtx.typeStr,
                fields: callCtx.fields ?? [],
                isMap: callCtx.isMap,
                keyType: callCtx.keyType,
                elemType: callCtx.elemType,
                isSlice: callCtx.isSlice
              },
            ];
            if (node.children) {
              const inner = this.findScopeAtPosition(
                node.children,
                position,
                childVars,
                childStack,
                rootNodes,
                ctx
              );
              if (inner) return inner;
            }
            return { stack: childStack, locals: blockLocals };
          }
        }

        const childScope = this.buildChildScope(node, vars, scopeStack, blockLocals, rootNodes);
        const childVars = childScope?.childVars ?? vars;
        const childStack = childScope?.childStack ?? scopeStack;

        if (node.children) {
          const inner = this.findScopeAtPosition(
            node.children,
            position,
            childVars,
            childStack,
            rootNodes,
            ctx
          );
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
        if (absPath)
          return new vscode.Location(vscode.Uri.file(absPath), new vscode.Position(0, 0));
      }
    }
    return null;
  }
}

// ── Module-level helpers ───────────────────────────────────────────────────────

function fieldInfoToTemplateVar(f: FieldInfo): TemplateVar {
  return {
    name: f.name,
    type: f.type,
    fields: f.fields,
    isSlice: f.isSlice,
    defFile: f.defFile,
    defLine: f.defLine,
    defCol: f.defCol,
    doc: f.doc,
  };
}

function isFileBasedPartial(name: string): boolean {
  if (name.includes('/') || name.includes('\\')) return true;
  return ['.html', '.tmpl', '.gohtml', '.tpl', '.htm'].some(ext =>
    name.toLowerCase().endsWith(ext)
  );
}
