# Expression Parser for Go Templates

This module implements a recursive descent parser for Go template expressions, enabling type inference for complex operations beyond simple field access.

## Features

The expression parser supports:

### 1. Built-in Functions
- `index` - Array/map/slice indexing
- `slice` - Slice operations
- `len` - Length of collections
- `print`, `printf`, `println` - Formatting functions
- `html`, `js`, `urlquery` - Escaping functions
- `call` - Function invocation

### 2. Comparison Operations
- `eq` - Equal to
- `ne` - Not equal to
- `lt` - Less than
- `le` - Less than or equal
- `gt` - Greater than
- `ge` - Greater than or equal

### 3. Logical Operations
- `and` - Logical AND
- `or` - Logical OR
- `not` - Logical NOT

### 4. Arithmetic Operations (Future)
- `add` - Addition
- `sub` - Subtraction
- `mul` - Multiplication
- `div` - Division
- `mod` - Modulo

### 5. Pipeline Operations
Expressions can be chained using the pipe operator `|`:
```go
.Items | len | printf "%d"
```

### 6. Field and Method Access
- Field access: `.Field`, `$.Field`
- Nested access: `.User.Profile.Name`
- Method calls: `.Method arg1 arg2`

## Architecture

### Lexer
The lexer tokenizes template expressions into:
- Identifiers (keywords, function names, variables)
- Literals (strings, numbers)
- Operators (`.`, `$`, `|`, `(`, `)`, etc.)

### Parser
The parser implements a recursive descent algorithm with operator precedence:
1. **Pipeline** (lowest precedence)
2. **Logical OR** (`or`)
3. **Logical AND** (`and`)
4. **Comparison** (`eq`, `ne`, `lt`, `le`, `gt`, `ge`)
5. **Additive** (`add`, `sub`)
6. **Multiplicative** (`mul`, `div`, `mod`)
7. **Unary** (`not`)
8. **Postfix** (field access, method calls)
9. **Primary** (literals, identifiers, parentheses) (highest precedence)

### Type Inferencer
The type inferencer walks the AST and computes result types:
- Tracks variable types from the template context
- Respects scope (with/range blocks)
- Handles type transformations (e.g., `len` returns `int`, comparisons return `bool`)

## API

### Main Function

```typescript
import { inferExpressionType } from './expressionParser';

const typeResult = inferExpressionType(
  expr: string,              // The expression to parse
  vars: Map<string, TemplateVar>,  // Available variables
  scopeStack: ScopeFrame[]         // Current scope stack
);
```

### Return Type

```typescript
interface TypeResult {
  typeStr: string;        // Go type string (e.g., "int", "[]string")
  fields?: FieldInfo[];   // Available fields (for structs)
  isSlice?: boolean;      // Is this a slice type?
  isMap?: boolean;        // Is this a map type?
  elemType?: string;      // Element type (for slices/maps)
  keyType?: string;       // Key type (for maps)
}
```

## Usage Examples

### Example 1: Comparison Operations

```typescript
const vars = new Map([
  ['Count', { name: 'Count', type: 'int', isSlice: false }]
]);

const type = inferExpressionType('gt .Count 10', vars, []);
// Result: { typeStr: 'bool' }
```

### Example 2: Collection Operations

```typescript
const vars = new Map([
  ['Items', {
    name: 'Items',
    type: '[]Item',
    isSlice: true,
    elemType: 'Item'
  }]
]);

const type = inferExpressionType('len .Items', vars, []);
// Result: { typeStr: 'int' }

const type2 = inferExpressionType('index .Items 0', vars, []);
// Result: { typeStr: 'Item' }
```

### Example 3: Pipeline Operations

```typescript
const type = inferExpressionType('.Count | printf "%d"', vars, []);
// Result: { typeStr: 'string' }
```

### Example 4: Complex Expressions

```typescript
const expr = 'and (gt .Count 0) (lt .Count 100)';
const type = inferExpressionType(expr, vars, []);
// Result: { typeStr: 'bool' }
```

## Integration with Validator

The expression parser is designed to augment the existing template validator without causing regressions. Integration points:

### 1. Enhanced Hover Information

```typescript
async getHoverInfo(
  document: vscode.TextDocument,
  position: vscode.Position,
  ctx: TemplateContext
): Promise<vscode.Hover | null> {
  // ... existing code ...
  
  let result = resolvePath(node.path, hitVars, stack, hitLocals);
  
  // Fallback to expression parser for complex expressions
  if (!result.found && node.rawText) {
    const exprType = inferExpressionType(node.rawText, hitVars, stack);
    if (exprType) {
      result = { ...exprType, found: true };
    }
  }
  
  // ... continue with hover display ...
}
```

### 2. Enhanced Validation

```typescript
private validateNode(node: TemplateNode, ...): void {
  if (node.kind === 'variable') {
    const basicResult = resolvePath(node.path, vars, scopeStack, blockLocals);
    
    if (!basicResult.found && node.rawText) {
      const exprType = inferExpressionType(node.rawText, vars, scopeStack);
      
      if (!exprType) {
        errors.push({
          message: `Expression "${node.rawText}" could not be validated`,
          line: node.line,
          col: node.col,
          severity: 'warning',
        });
      }
    }
  }
}
```

### 3. Enhanced Completions

```typescript
getCompletions(
  document: vscode.TextDocument,
  position: vscode.Position,
  ctx: TemplateContext
): vscode.CompletionItem[] {
  const linePrefix = document.lineAt(position.line).text.slice(0, position.character);
  
  // Detect complex expression context
  const inComplexExpr = /\(\s*[^)]*$|\|\s*[^|}]*$/.test(linePrefix);
  
  if (inComplexExpr) {
    const match = linePrefix.match(/(?:\(|\|)\s*(.*)$/);
    if (match) {
      const exprType = inferExpressionType(match[1], ctx.vars, []);
      
      if (exprType?.fields) {
        return exprType.fields.map(f => new vscode.CompletionItem(f.name));
      }
    }
  }
  
  // ... existing completion logic ...
}
```

## Testing

Run the test suite:

```bash
bun ./expressionParser.test.ts
```

Expected output:
```
=== Expression Parser Tests ===

✓ PASS: ".Count" → int
✓ PASS: ".Users" → []User
✓ PASS: "len .Users" → int
✓ PASS: "index .Users 0" → User
✓ PASS: "gt .Count 10" → bool
✓ PASS: "and (gt .Count 0) (lt .Count 100)" → bool
✓ PASS: "printf "%d" .Count" → string
✓ PASS: ".Count | printf "%d"" → string

=== Results: 8 passed, 0 failed ===
```

## Supported Template Patterns

### Conditionals
```go
{{if gt .Count 10}}
  Many items
{{end}}

{{if and (gt .Count 0) (lt .Count 100)}}
  Valid range
{{end}}
```

### Loops with Filters
```go
{{range .Items}}
  {{if gt .Price 100}}
    {{.Name}}: ${{.Price}}
  {{end}}
{{end}}
```

### Pipelines
```go
{{.Items | len | printf "Total: %d"}}

{{.Name | printf "Hello, %s!" | html}}
```

### Complex Expressions
```html
{{$count := len .Items}}
{{if gt $count 0}}
  {{printf "Found %d items" $count}}
{{end}}
```

## Limitations

1. **Method Calls**: Method type inference returns `unknown` as it requires runtime type information
2. **Custom Functions**: User-defined template functions are not analyzed
3. **Type Conversions**: Implicit type conversions are not tracked
4. **Interfaces**: Interface method calls cannot be resolved statically

## Future Enhancements

1. **Enhanced Method Resolution**: Integrate with Go type analysis to resolve method return types
2. **Function Registry**: Allow registration of custom template functions with type signatures
3. **Advanced Type Inference**: Track type narrowing in conditionals
4. **Performance**: Cache parsed expressions and type results
5. **Error Recovery**: Improve error messages and partial results for malformed expressions

## Implementation Notes

### No Regressions
The expression parser is designed as an augmentation layer:
- All existing functionality remains unchanged
- Expression parsing is only invoked when basic path resolution fails
- Parsing errors are silently ignored, falling back to existing behavior
- No changes to existing AST structure or interfaces

### Performance
- Expression parsing is lazy (only invoked on hover/completion)
- Lexer and parser are lightweight (no external dependencies)
- Type inference uses existing type information (no additional lookups)

### Compatibility
- Works with all existing template syntax
- Compatible with Go 1.13+ template package semantics
- Follows html/template escaping rules
