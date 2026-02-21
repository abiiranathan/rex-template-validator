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
import { TemplateParser, resolvePath, ResolveResult } from './templateParser';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { inferExpressionType, TypeResult } from './compiler/expressionParser';

export class TemplateValidator {
  private parser = new TemplateParser();
  private graphBuilder: KnowledgeGraphBuilder;
  private outputChannel: vscode.OutputChannel;

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

    const rootScope: ScopeFrame[] = [];
    if (ctx.isMap || ctx.isSlice || ctx.rootTypeStr) {
      rootScope.push({
        key: '.',
        typeStr: ctx.rootTypeStr ?? 'context',
        fields: [...ctx.vars.values()] as unknown as FieldInfo[],
        isMap: ctx.isMap,
        keyType: ctx.keyType,
        elemType: ctx.elemType,
        isSlice: ctx.isSlice
      });
    }

    this.validateNodes(nodes, ctx.vars, rootScope, errors, ctx, filePath);
    return errors;
  }

  // ── Named block registry helpers ───────────────────────────────────────────

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

  private applyAssignmentLocals(
    assignVars: string[],
    result: TypeResult | ResolveResult,
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

  /**
   * Helper to process assignments in various traversal contexts (validation, hover, completion).
   * It attempts to infer the type of the expression; failing that, it falls back to path resolution.
   */
  private processAssignment(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>
  ) {
    if (node.kind !== 'assignment') return;

    let resolvedType: TypeResult | ResolveResult | null = null;

    // 1. Try expression inference
    try {
      if (node.assignExpr) {
        const exprType = inferExpressionType(node.assignExpr, vars, scopeStack, blockLocals);
        if (exprType) resolvedType = exprType;
      }
    } catch { }

    // 2. Fallback to path resolution
    if (!resolvedType) {
      const result = resolvePath(node.path, vars, scopeStack, blockLocals);
      // We only accept empty-path resolution (which returns context) if the expression is explicitly "." or "$"
      const isExplicitContext = node.assignExpr === '.' || node.assignExpr === '$';
      const isValidPath =
        (node.path.length > 0 &&
          !(node.path.length === 1 && node.path[0] === '.' && !isExplicitContext)) ||
        isExplicitContext;

      if (result.found && isValidPath) {
        resolvedType = result;
      }
    }

    if (resolvedType && node.assignVars?.length) {
      this.applyAssignmentLocals(node.assignVars, resolvedType, blockLocals);
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
        this.outputChannel.appendLine(`Validating assignment: "${node.assignExpr}" at ${node.line}:${node.col}`);

        let resolvedType: TypeResult | ResolveResult | null = null;

        // 1. Try expression evaluation first
        try {
          if (node.assignExpr) {
            const exprType = inferExpressionType(node.assignExpr, vars, scopeStack, blockLocals);
            if (exprType) {
              resolvedType = exprType;
              this.outputChannel.appendLine(`  Assignment expression inferred: ${JSON.stringify(exprType)}`);
            }
          }
        } catch (e) {
          this.outputChannel.appendLine(`  Assignment inference error: ${e}`);
        }

        // 2. Fallback to path resolution (only if expression eval failed)
        if (!resolvedType) {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          const isExplicitContext = node.assignExpr === '.' || node.assignExpr === '$';
          const isValidPath = (node.path.length > 0 && !(node.path.length === 1 && node.path[0] === '.' && !isExplicitContext)) || isExplicitContext;

          if (result.found && isValidPath) {
            resolvedType = result;
            this.outputChannel.appendLine(`  Assignment resolved via path: ${JSON.stringify(result)}`);
          } else {
            this.outputChannel.appendLine(`  Assignment path resolution ignored (empty path or invalid).`);
          }
        }

        if (resolvedType) {
          if (node.assignVars?.length) {
            this.applyAssignmentLocals(node.assignVars, resolvedType, blockLocals);
          }
        } else {
          this.outputChannel.appendLine(`  Assignment validation failed.`);
          errors.push({
            message: `Expression "${node.assignExpr}" is invalid or undefined`,
            line: node.line,
            col: node.col,
            severity: 'error',
            variable: node.assignExpr,
          });
        }
        break;
      }

      case 'variable': {
        if (node.path.length === 0) break;
        if (node.path[0] === '.') break;
        if (node.path[0] === '$' && node.path.length === 1) break;

        const cleanExpr = node.rawText ? node.rawText.replace(/^\{\{-?\s*/, '').replace(/\s*-?\}\}$/, '') : '';

        this.outputChannel.appendLine(`Validating variable/expression: "${cleanExpr}" at ${node.line}:${node.col}`);

        // 1. Always attempt expression evaluation first
        let exprType = null;
        try {
          if (cleanExpr) {
            this.outputChannel.appendLine(`  Attempting inferExpressionType for: "${cleanExpr}"`);
            exprType = inferExpressionType(cleanExpr, vars, scopeStack, blockLocals);
            this.outputChannel.appendLine(`  inferExpressionType result: ${exprType ? JSON.stringify(exprType) : 'null'}`);
          }
        } catch (e) {
          this.outputChannel.appendLine(`  inferExpressionType threw error: ${e}`);
        }

        if (exprType) {
          // Expression is valid
          this.outputChannel.appendLine(`  Expression valid.`);
          break;
        }

        this.outputChannel.appendLine(`  Expression inference failed. Checking for sub-variables...`);

        // 2. If expression failed, check for specific missing sub-variables
        if (cleanExpr) {
          const refs = cleanExpr.match(/(\(index\s+(?:\$|\.)[\w\d_.]+\s+[^)]+\)(?:\.[\w\d_.]+)*|(?:\$|\.)[\w\d_.[\]]*)/g);

          let subVarError = false;
          if (refs) {
            for (const ref of refs) {
              if (/^\.\d+$/.test(ref)) continue;
              if (ref === '...') continue;

              const subPath = this.parser.parseDotPath(ref);
              if (subPath.length === 0 || (subPath.length === 1 && (subPath[0] === '.' || subPath[0] === '$'))) continue;

              const subResult = resolvePath(subPath, vars, scopeStack, blockLocals);
              this.outputChannel.appendLine(`  Checking sub-variable "${ref}" -> Found: ${subResult.found}`);

              if (!subResult.found) {
                let errCol = node.col;
                const refIdx = node.rawText.indexOf(ref);
                if (refIdx !== -1) errCol = node.col + refIdx;

                const displayPath = subPath[0] === '$'
                  ? '$.' + subPath.slice(1).join('.')
                  : subPath[0].startsWith('$')
                    ? subPath.join('.')
                    : '.' + subPath.join('.');

                const errMsg = `Template variable "${displayPath}" is not defined in the render context`;
                this.outputChannel.appendLine(`    Error: ${errMsg}`);

                errors.push({
                  message: errMsg,
                  line: node.line,
                  col: errCol,
                  severity: 'error',
                  variable: ref,
                });
                subVarError = true;
              }
            }
          }
          if (subVarError) break;
        }

        // 3. Fallback: check if it's a simple path that failed evaluation for some reason
        const pathResolved = resolvePath(node.path, vars, scopeStack, blockLocals).found;
        this.outputChannel.appendLine(`  Fallback pathResolved: ${pathResolved} (path: ${node.path.join('.')})`);

        const isComplex = cleanExpr && /[\s|()]/.test(cleanExpr);

        if (!pathResolved || isComplex) {
          this.outputChannel.appendLine(`  Validation FAILED. isComplex: ${isComplex}`);

          let message = `Template variable "${cleanExpr}" is not defined in the render context`;
          if (isComplex) {
            message = `Invalid expression or function call: "${cleanExpr}"`;
          } else {
            const displayPath =
              node.path[0] === '$'
                ? '$.' + node.path.slice(1).join('.')
                : node.path[0].startsWith('$')
                  ? node.path.join('.')
                  : '.' + node.path.join('.');
            message = `Template variable "${displayPath}" is not defined in the render context`;
          }

          errors.push({
            message: message,
            line: node.line,
            col: node.col,
            severity: 'error',
            variable: node.rawText,
          });
        }
        break;
      }

      case 'range': {
        const isBareDotsRange =
          node.path.length === 0 ||
          (node.path.length === 1 && node.path[0] === '.');

        if (!isBareDotsRange) {
          const result = resolvePath(node.path, vars, scopeStack, blockLocals);
          if (!result.found) {
            errors.push({
              message: `Range target ".${node.path.join('.')}" is not defined`,
              line: node.line,
              col: node.col,
              severity: 'error',
              variable: node.path[0],
            });
            break;
          }
        }

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
    }

    if (!node.children || node.children.length === 0) return;

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
      this.validateNodes(node.children, vars, scopeStack, errors, ctx, filePath);
    }
  }

  private resolveNamedBlockChildScope(
    node: TemplateNode,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar>,
    currentFilePath: string
  ): { childVars: Map<string, TemplateVar>; childStack: ScopeFrame[] } | null {
    const name = node.blockName!;

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

    if (!isFileBasedPartial(node.partialName)) {
      this.validateNamedBlockCallSite(node, vars, scopeStack, blockLocals, errors, ctx, filePath);
      return;
    }

    const partialCtx = this.graphBuilder.findPartialContext(node.partialName, filePath);
    if (!partialCtx) {
      let errCol = node.col;
      if (node.rawText && node.partialName) {
        const nameIdx = node.rawText.indexOf(`"${node.partialName}"`);
        if (nameIdx !== -1) {
          errCol = node.col + nameIdx + 1;
        }
      }
      errors.push({
        message: `Partial template "${node.partialName}" could not be found`,
        line: node.line,
        col: errCol,
        severity: 'warning',
        variable: node.partialName,
      });
      return;
    }

    if (!fs.existsSync(partialCtx.absolutePath)) return;

    const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
    try {
      const openDoc = vscode.workspace.textDocuments.find(
        d => d.uri.fsPath === partialCtx.absolutePath
      );
      const content = openDoc
        ? openDoc.getText()
        : fs.readFileSync(partialCtx.absolutePath, 'utf8');
      const partialErrors = this.validate(
        content,
        {
          ...partialCtx,
          vars: resolved.vars,
          isMap: resolved.isMap,
          keyType: resolved.keyType,
          elemType: resolved.elemType,
          isSlice: resolved.isSlice,
          rootTypeStr: resolved.rootTypeStr
        },
        partialCtx.absolutePath
      );
      for (const e of partialErrors) {
        errors.push({ ...e, message: `[in partial "${node.partialName}"] ${e.message}` });
      }
    } catch { /* ignore read errors */ }
  }

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
    }

    if (!entry) {
      this.validateNamedBlockFromCurrentFile(
        callNode, vars, scopeStack, blockLocals, errors, ctx, filePath
      );
      return;
    }

    const contextArg = callNode.partialContext ?? '.';
    const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
    const partialVars = resolved.vars;

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

    try {
      const openDoc = vscode.workspace.textDocuments.find(
        d => d.uri.fsPath === entry.absolutePath
      );
      const blockFileContent = openDoc
        ? openDoc.getText()
        : fs.readFileSync(entry.absolutePath, 'utf8');

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
    const resolved = this.resolvePartialVars(contextArg, vars, scopeStack, blockLocals);
    const partialVars = resolved.vars;

    try {
      const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === filePath);
      let content = '';
      if (openDoc) content = openDoc.getText();
      else if (fs.existsSync(filePath)) content = fs.readFileSync(filePath, 'utf8');
      else return;

      const blockNode = this.findDefineNodeInAST(this.parser.parse(content), callNode.partialName);
      if (!blockNode) {
        let errCol = callNode.col;
        if (callNode.rawText && callNode.partialName) {
          const nameIdx = callNode.rawText.indexOf(`"${callNode.partialName}"`);
          if (nameIdx !== -1) {
            errCol = callNode.col + nameIdx + 1;
          }
        }
        errors.push({
          message: `Template "${callNode.partialName}" not found`,
          line: callNode.line,
          col: errCol,
          severity: 'error',
          variable: callNode.partialName,
        });
        return;
      }
      if (!blockNode.children) return;

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
  ): { vars: Map<string, TemplateVar>; isMap?: boolean; keyType?: string; elemType?: string; isSlice?: boolean; rootTypeStr?: string } {
    if (contextArg === '.' || contextArg === '$') {
      const dotFrame = scopeStack.slice().reverse().find(f => f.key === '.');
      if (dotFrame) {
        const result = new Map<string, TemplateVar>();
        if (dotFrame.fields) {
          for (const f of dotFrame.fields) result.set(f.name, fieldInfoToTemplateVar(f));
        }
        return {
          vars: result,
          isMap: dotFrame.isMap,
          keyType: dotFrame.keyType,
          elemType: dotFrame.elemType,
          isSlice: dotFrame.isSlice,
          rootTypeStr: dotFrame.typeStr
        };
      }
      return { vars: new Map(vars) };
    }

    const result = resolvePath(
      this.parser.parseDotPath(contextArg),
      vars,
      scopeStack,
      blockLocals
    );

    const partialVars = new Map<string, TemplateVar>();
    if (result.found && result.fields) {
      for (const f of result.fields) partialVars.set(f.name, fieldInfoToTemplateVar(f));
    }

    return {
      vars: partialVars,
      isMap: result.isMap,
      keyType: result.keyType,
      elemType: result.elemType,
      isSlice: result.isSlice,
      rootTypeStr: result.typeStr
    };
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

    if (node.rawText) {
      const cursorOffset = position.character - (node.col - 1);
      const subPathStr = this.extractPathAtCursor(node.rawText, cursorOffset);

      if (subPathStr) {
        const parts = this.parser.parseDotPath(subPathStr);
        if (parts.length > 0 && !(parts.length === 1 && parts[0] === '.' && subPathStr !== '.')) {
          const subResult = resolvePath(parts, hitVars, stack, hitLocals);
          if (subResult.found) {
            return this.buildHoverForPath(parts, subResult, hitVars, stack, hitLocals);
          }
        }
      }
    }

    const isBareVarDot = node.kind === 'variable' && node.path.length === 1 && node.path[0] === '.';
    const isPartialDotCtx =
      node.kind === 'partial' && (node.partialContext ?? '.') === '.';
    if (isBareVarDot || isPartialDotCtx) return this.buildDotHover(stack, hitVars);

    let result = resolvePath(node.path, hitVars, stack, hitLocals);
    let isExpressionFallback = false;
    let exprText = node.rawText;

    if (!result.found && node.rawText) {
      try {
        const cleanExpr = node.rawText.replace(/^\{\{-?\s*/, '').replace(/\s*-?\}\}$/, '');
        const exprType = inferExpressionType(cleanExpr, hitVars, stack, hitLocals);
        if (exprType) {
          result = {
            typeStr: exprType.typeStr,
            found: true,
            fields: exprType.fields,
            isSlice: exprType.isSlice,
            isMap: exprType.isMap,
            elemType: exprType.elemType,
            keyType: exprType.keyType,
          };
          isExpressionFallback = true;
          exprText = cleanExpr;
        }
      } catch (err) { }
    }

    if (!result.found) return null;

    const pathToUse = isExpressionFallback ? ['expression'] : node.path;
    return this.buildHoverForPath(pathToUse, result, hitVars, stack, hitLocals, exprText);
  }

  private extractPathAtCursor(text: string, offset: number): string | null {
    const allowedChars = /[a-zA-Z0-9_$.]/;

    let start = offset;
    while (start > 0 && allowedChars.test(text[start - 1])) {
      start--;
    }

    let end = offset;
    while (end < text.length && allowedChars.test(text[end])) {
      end++;
    }

    if (start >= end) return null;

    const candidate = text.substring(start, end);
    if (!candidate.includes('.') && !candidate.includes('$')) return null;

    return candidate;
  }

  private buildHoverForPath(
    path: string[],
    result: ResolveResult | TypeResult,
    vars: Map<string, TemplateVar>,
    stack: ScopeFrame[],
    locals?: Map<string, TemplateVar>,
    rawText?: string
  ): vscode.Hover {
    const varName = rawText && path.length === 1 && (path[0] === 'expression' || path[0] === 'unknown')
      ? rawText
      : path[0] === '.'
        ? '.'
        : path[0] === '$'
          ? '$.' + path.slice(1).join('.')
          : path[0].startsWith('$')
            ? path.join('.')
            : '.' + path.join('.');

    const md = new vscode.MarkdownString();
    md.isTrusted = true;
    md.appendCodeblock(`${varName}: ${result.typeStr}`, 'go');

    const varInfo = this.findVariableInfo(path, vars, stack, locals);
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

  private resolveNamedBlockCallCtxForHover(
    blockName: string,
    vars: Map<string, TemplateVar>,
    currentFileNodes: TemplateNode[],
    currentFilePath: string
  ): { typeStr: string; fields?: FieldInfo[]; isMap?: boolean; keyType?: string; elemType?: string; isSlice?: boolean } | null {
    const localCtx = this.findCallSiteContext(currentFileNodes, blockName, vars, []);
    if (localCtx) return localCtx;

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

    const { node, stack, vars: hitVars, locals: hitLocals } = hit;

    let targetPath: string[] = [];

    if (node.rawText) {
      const cursorOffset = position.character - (node.col - 1);
      const subPathStr = this.extractPathAtCursor(node.rawText, cursorOffset);
      if (subPathStr) {
        targetPath = this.parser.parseDotPath(subPathStr);
      }
    }

    if (targetPath.length === 0) {
      if (node.path.length > 0 && (node.path[0] === '.' || node.path[0].startsWith('$'))) {
        targetPath = node.path;
      } else {
        return null;
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
        nodes,
        position,
        ctx,
        hit.stack,
        hit.locals
      );
      if (declaredVar) return declaredVar;
      const rangeVar = this.findRangeAssignedVariable({ ...node, path: targetPath }, stack, ctx);
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

  private async findNamedBlockDefinitionLocation(
    name: string,
    ctx: TemplateContext
  ): Promise<vscode.Location | null> {
    const { entry } = this.resolveNamedBlock(name);
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
      this.processAssignment(node, vars, scopeStack, blockLocals);

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

      this.processAssignment(node, vars, scopeStack, blockLocals);

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

      if (
        position.line === startLine &&
        position.character <= startCol + node.rawText.length
      ) {
        return { node, stack: scopeStack, vars, locals: blockLocals };
      }

      let childVars = vars;
      let childStack = scopeStack;

      if ((node.kind === 'define' || node.kind === 'block') && node.blockName) {
        const callCtx = this.resolveNamedBlockCallCtxForHover(
          node.blockName,
          vars,
          rootNodes,
          ''
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

    const inComplexExpr = /\(\s*[^)]*$|\|\s*[^|}]*$/.test(linePrefix);

    if (inComplexExpr) {
      const match = linePrefix.match(/(?:\(|\|)\s*(.*)$/);
      if (match) {
        const partialExpr = match[1].trim();

        try {
          const exprType = inferExpressionType(partialExpr, ctx.vars, stack, locals);

          if (exprType?.fields) {
            return exprType.fields.map(f => {
              const item = new vscode.CompletionItem(
                f.name,
                vscode.CompletionItemKind.Field
              );
              item.detail = f.isSlice ? `[]${f.type}` : f.type;
              if (f.doc) {
                item.documentation = new vscode.MarkdownString(f.doc);
              }
              return item;
            });
          }
        } catch { }
      }
    }

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
      this.processAssignment(node, vars, scopeStack, blockLocals);

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
