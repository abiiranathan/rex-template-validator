# Integration Patch for validator.ts

This file contains the specific changes needed to integrate the expression parser into the existing validator without causing regressions.

## Step 1: Add Import

At the top of `validator.ts`, add:

```typescript
import { inferExpressionType } from './expressionParser';
```

## Step 2: Enhance getHoverInfo Method

Locate the `getHoverInfo` method and modify the type resolution section:

**Find this code (around line 200):**
```typescript
const result = resolvePath(node.path, hitVars, stack, hitLocals);
if (!result.found) return null;
```

**Replace with:**
```typescript
let result = resolvePath(node.path, hitVars, stack, hitLocals);

// Fallback to expression parser for complex expressions
if (!result.found && node.rawText) {
  try {
    const exprType = inferExpressionType(node.rawText, hitVars, stack);
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
    }
  } catch (err) {
    // Silently fall back to existing behavior
  }
}

if (!result.found) return null;
```

## Step 3: Enhance validateNode Method (Optional)

For additional validation of complex expressions, locate the `validateNode` method and add this case:

**Find the variable validation section (around line 600):**
```typescript
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
```

**Enhance with expression parser:**
```typescript
case 'variable': {
  if (node.path.length === 0) break;
  if (node.path[0] === '.') break;
  if (node.path[0] === '$' && node.path.length === 1) break;

  let pathResolved = resolvePath(node.path, vars, scopeStack, blockLocals).found;
  
  // Try expression parser for complex expressions (e.g., function calls)
  if (!pathResolved && node.rawText) {
    try {
      const exprType = inferExpressionType(node.rawText, vars, scopeStack);
      if (exprType) {
        pathResolved = true;
      }
    } catch {
      // Expression parsing failed, continue with error
    }
  }

  if (!pathResolved) {
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
```

## Step 4: Enhance getCompletions Method (Optional)

For context-aware completions in complex expressions:

**Find the getCompletions method and add before the existing logic:**
```typescript
getCompletions(
  document: vscode.TextDocument,
  position: vscode.Position,
  ctx: TemplateContext
): vscode.CompletionItem[] {
  const content = document.getText();
  const nodes = this.parser.parse(content);

  // ... existing scope resolution code ...

  const linePrefix = document.lineAt(position.line).text.slice(0, position.character);
  
  // Check if we're inside a function call or pipeline
  const inComplexExpr = /\(\s*[^)]*$|\|\s*[^|}]*$/.test(linePrefix);
  
  if (inComplexExpr) {
    const match = linePrefix.match(/(?:\(|\|)\s*(.*)$/);
    if (match) {
      const partialExpr = match[1].trim();
      
      // Try to infer type of the partial expression
      try {
        const exprType = inferExpressionType(partialExpr, ctx.vars, stack);
        
        if (exprType?.fields) {
          // Return field completions based on inferred type
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
      } catch {
        // Expression parsing failed, fall through to existing logic
      }
    }
  }

  // ... existing completion logic ...
}
```

## Verification

After applying these patches:

### Test 1: Hover on Complex Expression
Create a template with:
```html
{{if gt .Count 10}}
  <p>Many items</p>
{{end}}
```

Hover over `gt .Count 10` - you should see:
```
gt .Count 10: bool
```

### Test 2: Validation of Function Calls
Template:
```html
{{len .Items | printf "%d items"}}
```

Should validate without errors and show proper type inference in hover.

### Test 3: Pipeline Operations
Template:
```html
{{.Items | len}}
```

Hover should show:
```
.Items | len: int
```

### Test 4: No Regressions
All existing templates should continue to work as before. The expression parser only activates when basic path resolution fails, ensuring backward compatibility.

## Rollback Instructions

If issues arise, simply revert the changes:

1. Remove the `import { inferExpressionType }` line
2. Remove all code blocks wrapped in `try-catch` with `inferExpressionType`
3. Restore original `result = resolvePath(...)` patterns

The extension will continue to work as it did before, without expression parser support.

## Testing Checklist

- [ ] Hover on simple field access still works (`.Field`)
- [ ] Hover on comparison operations shows `bool` type
- [ ] Hover on `len` shows `int` type
- [ ] Hover on `printf` shows `string` type
- [ ] Pipeline operations infer correct final type
- [ ] Invalid expressions show appropriate errors
- [ ] Existing templates validate without new errors
- [ ] Performance is not noticeably degraded

## Performance Notes

The expression parser is only invoked when:
1. Basic path resolution fails (fallback behavior)
2. User hovers over an expression (lazy evaluation)
3. Completions are requested in complex contexts

This ensures minimal performance impact on existing workflows.
