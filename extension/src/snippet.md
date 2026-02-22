// ── Completions ────────────────────────────────────────────────────────────

  getCompletions(
    document: vscode.TextDocument,
    position: vscode.Position,
    ctx: TemplateContext
  ): vscode.CompletionItem[] {
    const completionItems: vscode.CompletionItem[] = [];
    const content = document.getText();
    const nodes = this.parser.parse(content);

    // 1. Determine Scope
    let scopeResult = this.findScopeAtPosition(nodes, position, ctx.vars, [], nodes, ctx);
    
    // Fallback: Check if we are inside a named block (define/block) that implies a specific context
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

    const lineText = document.lineAt(position.line).text;
    const linePrefix = lineText.slice(0, position.character);
    const replacementRange = new vscode.Range(position.line, position.character, position.line, position.character);

    // 2. Handle Complex Expressions (Pipes, Function Args)
    //    e.g. " | len" or " (call "
    const inComplexExpr = /\(\s*[^)]*$|\|\s*[^|}]*$/.test(linePrefix);
    if (inComplexExpr) {
      const match = linePrefix.match(/(?:\(|\|)\s*(.*)$/);
      if (match) {
        const partialExpr = match[1].trim();
        try {
          const exprType = inferExpressionType(partialExpr, ctx.vars, stack, locals, this.graphBuilder.getGraph().funcMaps);
          if (exprType?.fields) {
            return exprType.fields.map(f => {
              const item = new vscode.CompletionItem(
                f.name,
                f.type === 'method' ? vscode.CompletionItemKind.Method : vscode.CompletionItemKind.Field
              );
              item.detail = f.isSlice ? `[]${f.type}` : f.type;
              if (f.doc) item.documentation = new vscode.MarkdownString(f.doc);
              return item;
            });
          }
        } catch {
          // Fall through to standard completion
        }
      }
    }

    // 3. Standard Path Completion (Dot or Dollar)
    //    Matches: ".", "$", ".User", "$var.Field", "$user."
    const match = linePrefix.match(/(?:\$|\.)[\w.]*$/);

    // Case A: No dot/dollar prefix (e.g. typing a function name or start of a var)
    if (!match) {
      // Suggest Globals, Locals, and Functions
      this.addGlobalVariablesToCompletion(ctx.vars, completionItems, '', replacementRange);
      this.addLocalVariablesToCompletion(stack, locals, completionItems, '', replacementRange);
      this.addFunctionsToCompletion(this.graphBuilder.getGraph().funcMaps, completionItems, '', replacementRange);
      return completionItems;
    }

    // Case B: Path resolution
    const rawPath = match[0];
    let lookupPath: string[];
    let filterPrefix: string;

    if (rawPath.endsWith('.')) {
      // Example: "$user." -> lookup "$user", filter ""
      // Example: "."      -> lookup ".", filter ""
      lookupPath = this.parser.parseDotPath(rawPath);
      filterPrefix = '';
    } else {
      // Example: "$user.Nam" -> lookup "$user", filter "Nam"
      const parts = rawPath.split('.');
      filterPrefix = parts[parts.length - 1];
      const pathOnly = rawPath.slice(0, rawPath.length - filterPrefix.length);
      lookupPath = this.parser.parseDotPath(pathOnly);
    }

    // 3a. Bare Dot "." -> Current Context Fields
    if (lookupPath.length === 1 && (lookupPath[0] === '.' || lookupPath[0] === '')) {
      const dotFrame = stack.slice().reverse().find(f => f.key === '.');
      const fields = dotFrame?.fields ?? [...ctx.vars.values()].map(v => ({
        name: v.name,
        type: v.type,
        fields: v.fields,
        isSlice: v.isSlice ?? false,
        doc: v.doc,
      } as FieldInfo));
      
      this.addFieldsToCompletion({ fields }, completionItems, filterPrefix, replacementRange);
      return completionItems;
    }

    // 3b. Bare Dollar "$" -> Root Vars + Locals
    if (lookupPath.length === 1 && lookupPath[0] === '$') {
      this.addGlobalVariablesToCompletion(ctx.vars, completionItems, filterPrefix, replacementRange);
      this.addLocalVariablesToCompletion(stack, locals, completionItems, filterPrefix, replacementRange);
      return completionItems;
    }

    // 3c. Complex Path -> Resolve and show fields
    //     e.g. "$user", "$user.Profile", ".Items"
    const res = resolvePath(lookupPath, ctx.vars, stack, locals);
    if (res.found && res.fields) {
      this.addFieldsToCompletion({ fields: res.fields }, completionItems, filterPrefix, replacementRange);
    }

    return completionItems;
  }

  private addFunctionsToCompletion(
    funcMaps: Map<string, FuncMapInfo> | undefined,
    completionItems: vscode.CompletionItem[],
    partialName: string = '',
    replacementRange: vscode.Range
  ) {
    if (!funcMaps) return;
    for (const [name, fn] of funcMaps) {
      if (partialName && !name.startsWith(partialName)) continue;

      const item = new vscode.CompletionItem(name, vscode.CompletionItemKind.Function);
      const args = fn.args ? fn.args.join(', ') : '';
      const returnsList = fn.returns || [];
      let returns = returnsList.join(', ');
      if (returnsList.length > 1) {
        returns = `(${returns})`;
      }
      item.detail = `func(${args}) ${returns}`;
      if (fn.doc) {
        item.documentation = new vscode.MarkdownString(fn.doc);
      }
      // item.range = replacementRange; // Optional: let VS Code handle range replacement usually works better for functions
      completionItems.push(item);
    }
  }

  private addGlobalVariablesToCompletion(
    vars: Map<string, TemplateVar>,
    completionItems: vscode.CompletionItem[],
    partialName: string = '',
    replacementRange: vscode.Range
  ) {
    for (const [name, variable] of vars) {
      if (name.startsWith(partialName)) {
        const item = new vscode.CompletionItem(name, vscode.CompletionItemKind.Variable);
        item.detail = variable.type;
        item.documentation = new vscode.MarkdownString(variable.doc);
        // item.range = replacementRange;
        completionItems.push(item);
      }
    }
  }

  private addLocalVariablesToCompletion(
    scopeStack: ScopeFrame[],
    blockLocals: Map<string, TemplateVar> | undefined,
    completionItems: vscode.CompletionItem[],
    partialName: string = '',
    replacementRange: vscode.Range
  ) {
    // Add block locals
    if (blockLocals) {
      for (const [name, variable] of blockLocals) {
        if (name.startsWith(partialName)) {
          const item = new vscode.CompletionItem(name, vscode.CompletionItemKind.Variable);
          item.detail = variable.type;
          item.documentation = new vscode.MarkdownString(variable.doc);
          completionItems.push(item);
        }
      }
    }

    // Add variables from scope stack
    for (let i = scopeStack.length - 1; i >= 0; i--) {
      if (scopeStack[i].locals) {
        for (const [name, variable] of scopeStack[i].locals!) {
          if (name.startsWith(partialName)) {
            const item = new vscode.CompletionItem(name, vscode.CompletionItemKind.Variable);
            item.detail = variable.type;
            item.documentation = new vscode.MarkdownString(variable.doc);
            completionItems.push(item);
          }
        }
      }
    }
  }

  private addFieldsToCompletion(
    context: { fields?: FieldInfo[] },
    completionItems: vscode.CompletionItem[],
    partialName: string = '',
    replacementRange: vscode.Range
  ) {
    if (context.fields) {
      for (const field of context.fields) {
        if (field.name.toLowerCase().startsWith(partialName.toLowerCase())) {
          const item = new vscode.CompletionItem(field.name, vscode.CompletionItemKind.Field);
          item.detail = field.isSlice ? `[]${field.type}` : field.type;
          item.documentation = new vscode.MarkdownString(field.doc);
          completionItems.push(item);
        }
      }
    }
  }