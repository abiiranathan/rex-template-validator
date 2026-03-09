/**
 * Expression parser for Go template expressions.
 * 
 * This module implements a recursive descent parser to deduce types from
 * common template operations like index, slice, map access, comparisons, etc.
 * 
 * Supports:
 * - Arithmetic operations: +, -, *, /, %
 * - Comparison operations: eq, ne, lt, le, gt, ge
 * - Logical operations: and, or, not
 * - Built-in functions: index, slice, len, print, printf, println
 * - Method calls: .Method arg1 arg2
 * - Field access: .Field
 * - Pipeline operations: value | func
 */

import { FieldInfo, TemplateVar, ScopeFrame, FuncMapInfo, ParamInfo, extractBareType } from '../types';

// ── Token types ────────────────────────────────────────────────────────────

type TokenType =
    | 'IDENT'      // identifier or keyword
    | 'NUMBER'     // numeric literal
    | 'STRING'     // string literal
    | 'DOT'        // .
    | 'DOLLAR'     // $
    | 'LPAREN'     // (
    | 'RPAREN'     // )
    | 'PIPE'       // |
    | 'COMMA'      // ,
    | 'COLON'      // :
    | 'EOF';

interface Token {
    type: TokenType;
    value: string;
    pos: number;
}

// ── Expression AST ─────────────────────────────────────────────────────────

type ExprNode =
    | { kind: 'ident'; name: string }
    | { kind: 'number'; value: number }
    | { kind: 'string'; value: string }
    | { kind: 'field'; path: string[] }
    | { kind: 'call'; func: string; args: ExprNode[] }
    | { kind: 'method'; receiver: ExprNode; method: string; args: ExprNode[] }
    | { kind: 'index'; target: ExprNode; index: ExprNode }
    | { kind: 'slice'; target: ExprNode; low?: ExprNode; high?: ExprNode; max?: ExprNode }
    | { kind: 'pipeline'; stages: ExprNode[] }
    | { kind: 'binary'; op: string; left: ExprNode; right: ExprNode }
    | { kind: 'unary'; op: string; arg: ExprNode };

// ── Type inference result ──────────────────────────────────────────────────

export interface TypeResult {
    typeStr: string;
    fields?: FieldInfo[];
    isSlice?: boolean;
    isMap?: boolean;
    elemType?: string;
    keyType?: string;
    params?: ParamInfo[];
    returns?: ParamInfo[];
}

// ── Lexer ──────────────────────────────────────────────────────────────────

class Lexer {
    private input: string;
    private pos: number = 0;

    constructor(input: string) {
        this.input = input.trim();
    }

    peek(): Token {
        const saved = this.pos;
        const tok = this.next();
        this.pos = saved;
        return tok;
    }

    peekNext(): Token {
        const saved = this.pos;
        this.next(); // skip current token
        const tok = this.next(); // get next token
        this.pos = saved; // restore position
        return tok;
    }

    next(): Token {
        this.skipWhitespace();

        if (this.pos >= this.input.length) {
            return { type: 'EOF', value: '', pos: this.pos };
        }

        const ch = this.input[this.pos];
        const startPos = this.pos;

        // Single-character tokens
        switch (ch) {
            case '.':
                this.pos++;
                return { type: 'DOT', value: '.', pos: startPos };
            case '$':
                this.pos++;
                return { type: 'DOLLAR', value: '$', pos: startPos };
            case '(':
                this.pos++;
                return { type: 'LPAREN', value: '(', pos: startPos };
            case ')':
                this.pos++;
                return { type: 'RPAREN', value: ')', pos: startPos };
            case '|':
                this.pos++;
                return { type: 'PIPE', value: '|', pos: startPos };
            case ',':
                this.pos++;
                return { type: 'COMMA', value: ',', pos: startPos };
            case ':':
                this.pos++;
                return { type: 'COLON', value: ':', pos: startPos };
        }

        // String literals
        if (ch === '"' || ch === '`') {
            return this.scanString(ch);
        }

        // Numbers
        if (this.isDigit(ch) || (ch === '-' && this.isDigit(this.input[this.pos + 1]))) {
            return this.scanNumber();
        }

        // Identifiers and keywords
        if (this.isAlpha(ch) || ch === '_') {
            return this.scanIdent();
        }

        // Unknown character - treat as identifier for robustness
        this.pos++;
        return { type: 'IDENT', value: ch, pos: startPos };
    }

    private skipWhitespace() {
        while (this.pos < this.input.length && /\s/.test(this.input[this.pos])) {
            this.pos++;
        }
    }

    private scanString(quote: string): Token {
        const startPos = this.pos;
        this.pos++; // skip opening quote
        let value = '';

        while (this.pos < this.input.length && this.input[this.pos] !== quote) {
            if (this.input[this.pos] === '\\' && this.pos + 1 < this.input.length) {
                this.pos++; // skip escape
                value += this.input[this.pos];
            } else {
                value += this.input[this.pos];
            }
            this.pos++;
        }

        if (this.pos < this.input.length) {
            this.pos++; // skip closing quote
        }

        return { type: 'STRING', value, pos: startPos };
    }

    private scanNumber(): Token {
        const startPos = this.pos;
        let value = '';

        if (this.input[this.pos] === '-') {
            value += '-';
            this.pos++;
        }

        while (this.pos < this.input.length && (this.isDigit(this.input[this.pos]) || this.input[this.pos] === '.')) {
            value += this.input[this.pos];
            this.pos++;
        }

        return { type: 'NUMBER', value, pos: startPos };
    }

    private scanIdent(): Token {
        const startPos = this.pos;
        let value = '';

        while (
            this.pos < this.input.length &&
            (this.isAlphaNumeric(this.input[this.pos]) || this.input[this.pos] === '_')
        ) {
            value += this.input[this.pos];
            this.pos++;
        }

        return { type: 'IDENT', value, pos: startPos };
    }

    private isDigit(ch: string): boolean {
        return /[0-9]/.test(ch);
    }

    private isAlpha(ch: string): boolean {
        return /[a-zA-Z]/.test(ch);
    }

    private isAlphaNumeric(ch: string): boolean {
        return /[a-zA-Z0-9]/.test(ch);
    }
}

// ── Parser ─────────────────────────────────────────────────────────────────

class ExpressionParser {
    private lexer: Lexer;
    private funcMaps?: Map<string, FuncMapInfo>;

    constructor(input: string, funcMaps?: Map<string, FuncMapInfo>) {
        this.lexer = new Lexer(input);
        this.funcMaps = funcMaps;
    }

    parse(): ExprNode | null {
        try {
            return this.parsePipeline();
        } catch {
            return null;
        }
    }

    // pipeline: expr ( '|' expr )*
    private parsePipeline(): ExprNode {
        const stages: ExprNode[] = [];
        stages.push(this.parseExpr());

        while (this.lexer.peek().type === 'PIPE') {
            this.lexer.next(); // consume '|'
            stages.push(this.parseExpr());
        }

        if (stages.length === 1) {
            return stages[0];
        }

        return { kind: 'pipeline', stages };
    }

    // expr: primary | binary_expr | call | index | slice
    private parseExpr(): ExprNode {
        return this.parsePostfix();
    }

    // Postfix operations: index, slice, method calls
    private parsePostfix(): ExprNode {
        let expr = this.parsePrimary();

        while (true) {
            const tok = this.lexer.peek();

            if (tok.type === 'DOT') {
                const nextTok = this.lexer.peekNext();
                if (nextTok.type !== 'IDENT') break;

                this.lexer.next(); // consume '.'
                const nameTok = this.lexer.next(); // consume IDENT

                const nextType = this.lexer.peek().type;
                // Added 'DOLLAR' so variables ($var) are recognized as valid method arguments
                if (nextType === 'LPAREN' || nextType === 'IDENT' || nextType === 'DOT' || nextType === 'STRING' || nextType === 'NUMBER' || nextType === 'DOLLAR') {
                    const args = this.parseArgList();
                    expr = { kind: 'method', receiver: expr, method: nameTok.value, args };
                } else {
                    let basePath: string[];
                    if (expr.kind === 'field') {
                        basePath = expr.path;
                    } else if (expr.kind === 'ident') {
                        basePath = [expr.name];
                    } else {
                        basePath = [];
                    }
                    expr = { kind: 'field', path: [...basePath, nameTok.value] };
                }
            } else {
                break;
            }
        }

        return expr;
    }

    // primary: ident | number | string | field | call | '(' expr ')'
    private parsePrimary(): ExprNode {
        const tok = this.lexer.peek();

        // Parenthesized expression
        if (tok.type === 'LPAREN') {
            this.lexer.next(); // consume '('
            const expr = this.parseExpr();
            if (this.lexer.peek().type === 'RPAREN') {
                this.lexer.next(); // consume ')'
            }
            return expr;
        }

        // Field access: . or .Field or $.Field
        if (tok.type === 'DOT') {
            return this.parseFieldAccess();
        }

        // Root context or local variable: $ or $varName
        if (tok.type === 'DOLLAR') {
            return this.parseFieldAccess();
        }

        // String literal
        if (tok.type === 'STRING') {
            this.lexer.next();
            return { kind: 'string', value: tok.value };
        }

        // Number literal
        if (tok.type === 'NUMBER') {
            this.lexer.next();
            return { kind: 'number', value: parseFloat(tok.value) };
        }

        // Identifier: might be function call or variable
        if (tok.type === 'IDENT') {
            const name = tok.value;
            this.lexer.next();

            // All these keywords are treated as function calls in Go templates
            const functionKeywords = [
                'index', 'slice', 'len', 'print', 'printf', 'println', 'call',
                'html', 'js', 'urlquery', 'and', 'or', 'not', 'eq', 'ne',
                'lt', 'le', 'gt', 'ge', 'add', 'sub', 'mul', 'div', 'mod',
                'dict'
            ];

            if (functionKeywords.includes(name) || (this.funcMaps && this.funcMaps.has(name))) {
                const args = this.parseArgList();
                return { kind: 'call', func: name, args };
            }

            // Regular identifier
            return { kind: 'ident', name };
        }

        // Fallback: return a dummy node
        return { kind: 'ident', name: '' };
    }

    // Parse field access starting with . or $
    private parseFieldAccess(): ExprNode {
        const path: string[] = [];
        let tok = this.lexer.next();

        if (tok.type === 'DOT') {
            // Bare dot or .Field
            const next = this.lexer.peek();
            if (next.type === 'IDENT') {
                this.lexer.next();
                path.push(next.value);
            } else {
                // Bare dot
                return { kind: 'field', path: ['.'] };
            }
        } else if (tok.type === 'DOLLAR') {
            const next = this.lexer.peek();
            if (next.type === 'IDENT') {
                // Local variable: $varName
                this.lexer.next(); // consume IDENT
                path.push('$' + next.value);
            } else {
                // Root context: $ or $.Field
                path.push('$');
            }
        }

        // Continue with additional .Field segments
        while (this.lexer.peek().type === 'DOT') {
            const next = this.lexer.peekNext();
            if (next.type === 'IDENT') {
                this.lexer.next(); // consume '.'
                const nameTok = this.lexer.next(); // consume IDENT
                path.push(nameTok.value);
            } else {
                break;
            }
        }

        return { kind: 'field', path };
    }

    // Parse argument list (not wrapped in parentheses, space-separated)
    private parseArgList(): ExprNode[] {
        const args: ExprNode[] = [];

        while (true) {
            const tok = this.lexer.peek();

            // Stop at end of input or pipeline
            if (tok.type === 'EOF' || tok.type === 'PIPE') {
                break;
            }

            // Stop at closing parenthesis (for nested expressions)
            if (tok.type === 'RPAREN') {
                break;
            }

            // Parse the next argument (can be any expression including parenthesized)
            if (tok.type === 'LPAREN') {
                // Parenthesized sub-expression
                this.lexer.next(); // consume '('
                const expr = this.parseExpr();
                if (this.lexer.peek().type === 'RPAREN') {
                    this.lexer.next(); // consume ')'
                }
                args.push(expr);
            } else if (tok.type === 'DOT' || tok.type === 'DOLLAR') {
                // Field access
                args.push(this.parseFieldAccess());
            } else if (tok.type === 'STRING') {
                // String literal
                this.lexer.next();
                args.push({ kind: 'string', value: tok.value });
            } else if (tok.type === 'NUMBER') {
                // Number literal
                this.lexer.next();
                args.push({ kind: 'number', value: parseFloat(tok.value) });
            } else if (tok.type === 'IDENT') {
                // Could be a nested function call or identifier
                const name = tok.value;
                this.lexer.next();

                const functionKeywords = [
                    'index', 'slice', 'len', 'print', 'printf', 'println', 'call',
                    'html', 'js', 'urlquery', 'and', 'or', 'not', 'eq', 'ne',
                    'lt', 'le', 'gt', 'ge', 'add', 'sub', 'mul', 'div', 'mod',
                    'dict'
                ];

                if (functionKeywords.includes(name) || (this.funcMaps && this.funcMaps.has(name))) {
                    // Nested function call
                    const nestedArgs = this.parseArgList();
                    args.push({ kind: 'call', func: name, args: nestedArgs });
                } else {
                    args.push({ kind: 'ident', name });
                }
            } else {
                // Unknown token, stop parsing
                break;
            }

            // Optional comma separator
            if (this.lexer.peek().type === 'COMMA') {
                this.lexer.next();
            }
        }

        return args;
    }
}

// ── Type inference ─────────────────────────────────────────────────────────

export class TypeInferencer {
    private vars: Map<string, TemplateVar>;
    private scopeStack: ScopeFrame[];
    private blockLocals?: Map<string, TemplateVar>;
    private funcMaps?: Map<string, FuncMapInfo>;

    // Resolves fields for a named Go type (e.g. "User" → User's fields).
    // Used to hydrate return types of funcMap calls that only carry a type string.
    private fieldResolver?: (typeStr: string) => FieldInfo[] | undefined;

    constructor(
        vars: Map<string, TemplateVar>,
        scopeStack: ScopeFrame[],
        blockLocals?: Map<string, TemplateVar>,
        funcMaps?: Map<string, FuncMapInfo>,
        fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
    ) {
        this.vars = vars;
        this.scopeStack = scopeStack;
        this.blockLocals = blockLocals;
        this.funcMaps = funcMaps;
        this.fieldResolver = fieldResolver
    }

    /**
     * Infer the type of a template expression.
     */
    inferType(expr: string): TypeResult | null {
        const parser = new ExpressionParser(expr, this.funcMaps);
        const ast = parser.parse();
        if (!ast) return null;

        return this.inferNodeType(ast);
    }

    private inferNodeType(node: ExprNode): TypeResult | null {
        const result = this.inferNodeTypeRaw(node);
        if (!result) return null;

        // If we got a type string but no fields, attempt to hydrate fields from
        // the type registry. This covers funcMap return types, index results,
        // pipeline stage results, and any other path that yields a bare type name.
        if ((!result.fields || result.fields.length === 0) && result.typeStr && result.typeStr !== 'unknown'
            && result.typeStr !== 'bool' && result.typeStr !== 'string'
            && result.typeStr !== 'int' && result.typeStr !== 'float64'
            && result.typeStr !== 'context') {

            const bare = extractBareType(result.typeStr);
            const resolved = this.fieldResolver?.(bare);
            if (resolved) return { ...result, fields: resolved };
        }

        return result;
    }

    /**
     * Extracts the value type from a map type string using bracket-balanced scanning.
     * Handles nested maps like map[map[K]V]W correctly, where a naive indexOf(']')
     * would grab the inner bracket and return a garbage value type.
     * typeStr must already have its leading pointer stripped.
     */
    private extractMapValueType(typeStr: string): string {
        let depth = 0;
        for (let i = 4; i < typeStr.length; i++) {
            if (typeStr[i] === '[') depth++;
            else if (typeStr[i] === ']') {
                if (depth === 0) return typeStr.slice(i + 1);
                depth--;
            }
        }
        return 'unkown';
    }

    private inferNodeTypeRaw(node: ExprNode): TypeResult | null {
        switch (node.kind) {
            case 'ident': return this.inferIdentType(node.name);
            case 'number': return { typeStr: Number.isInteger(node.value) ? 'int' : 'float64' };
            case 'string': return { typeStr: 'string' };
            case 'field': return this.inferFieldType(node.path);
            case 'call': return this.inferCallType(node.func, node.args);
            case 'method': return this.inferMethodType(node.receiver, node.method, node.args);
            case 'index': return this.inferIndexType(node.target, node.index);
            case 'slice': return this.inferSliceType(node.target, node.low, node.high, node.max);
            case 'pipeline': return this.inferPipelineType(node.stages);
            case 'binary': return this.inferBinaryType(node.op, node.left, node.right);
            case 'unary': return this.inferUnaryType(node.op, node.arg);
            default: return null;
        }
    }

    private inferIdentType(name: string): TypeResult | null {
        // Check block locals first
        if (this.blockLocals?.has(name)) {
            const v = this.blockLocals.get(name)!;
            return {
                typeStr: v.type,
                fields: v.fields,
                isSlice: v.isSlice,
                isMap: v.isMap,
                elemType: v.elemType,
                keyType: v.keyType,
            };
        }

        // Check local variables in scope
        for (let i = this.scopeStack.length - 1; i >= 0; i--) {
            if (this.scopeStack[i].locals?.has(name)) {
                const v = this.scopeStack[i].locals!.get(name)!;
                return {
                    typeStr: v.type,
                    fields: v.fields,
                    isSlice: v.isSlice,
                    isMap: v.isMap,
                    elemType: v.elemType,
                    keyType: v.keyType,
                };
            }
        }

        // Check root variables
        const v = this.vars.get(name);
        if (v) {
            return {
                typeStr: v.type,
                fields: v.fields,
                isSlice: v.isSlice,
                isMap: v.isMap,
                elemType: v.elemType,
                keyType: v.keyType,
            };
        }

        return null;
    }

    private inferFieldType(path: string[]): TypeResult | null {
        if (path.length === 0) return null;

        // Bare dot
        if (path.length === 1 && path[0] === '.') {
            const frame = this.findDotFrame();
            if (frame) {
                return {
                    typeStr: frame.typeStr,
                    fields: frame.fields,
                    isMap: frame.isMap,
                    keyType: frame.keyType,
                    elemType: frame.elemType,
                    isSlice: frame.isSlice,
                };
            }
            return { typeStr: 'context' };
        }

        // Root context: $
        if (path[0] === '$') {
            if (path.length === 1) {
                return { typeStr: 'context' };
            }
            // $.Field
            const v = this.vars.get(path[1]);
            if (!v) return null;
            if (path.length === 2) {
                return {
                    typeStr: v.type,
                    fields: v.fields,
                    isSlice: v.isSlice,
                    isMap: v.isMap,
                    elemType: v.elemType,
                    keyType: v.keyType,
                };
            }
            return this.resolveFieldPath(path.slice(2), v.fields ?? []);
        }

        // Local variables (e.g. $item.Name)
        if (path[0].startsWith('$')) {
            const localType = this.inferIdentType(path[0]);
            if (localType) {
                if (path.length === 1) return localType;
                return this.resolveFieldPath(path.slice(1), localType.fields ?? []);
            }
        }

        // Check in current scope first
        const frame = this.findDotFrame();
        if (frame) {
            if (frame.fields && frame.fields.length > 0) {
                return this.resolveFieldPath(path, frame.fields);
            }

            // Dot frame exists but has no field metadata (unknown/unresolvable context).
            // Return a permissive unknown type so callers don't treat this as an
            // inference failure and escalate to a validation error.
            return { typeStr: 'unknown' };
        }

        // Root scope: resolve against top-level vars
        const topVar = this.vars.get(path[0]);
        if (topVar) {
            if (path.length === 1) {
                return {
                    typeStr: topVar.type,
                    fields: topVar.fields,
                    isSlice: topVar.isSlice,
                    isMap: topVar.isMap,
                    elemType: topVar.elemType,
                    keyType: topVar.keyType,
                };
            }
            return this.resolveFieldPath(path.slice(1), topVar.fields ?? []);
        }

        return null;
    }

    private resolveFieldPath(path: string[], fields: FieldInfo[]): TypeResult | null {
        let current = fields;

        for (let i = 0; i < path.length; i++) {
            const field = current.find(f => f.name === path[i]);
            if (!field) return null;

            let resolvedField = { ...field };

            // Handle standard methods
            if (field.type === 'method' && field.returns && field.returns.length > 0) {
                let retType = field.returns[0].type;
                const bare = extractBareType(retType);
                const retFields = field.returns[0].fields; // Extract from return type
                const retDoc = field.returns[0].doc;       // Propagate doc from return ParamInfo

                const resolvedFields = this.fieldResolver?.(bare);
                resolvedField = {
                    ...field,
                    type: retType,
                    fields: (resolvedFields && resolvedFields.length > 0) ? resolvedFields : (retFields || field.fields),
                    isSlice: retType.startsWith('[]'),
                    isMap: retType.startsWith('map['),
                    doc: retDoc || field.doc,
                };
            }
            // Handle struct fields that are functions (e.g., Price func() float64)
            else if (field.type.startsWith('func(')) {
                const match = field.type.match(/func\([^)]*\)\s*(.+)/);
                if (match && match[1]) {
                    let retType = match[1].trim();
                    // Handle multiple returns: func() (string, error) -> string
                    if (retType.startsWith('(')) {
                        const commaIdx = retType.indexOf(',');
                        const endIdx = retType.indexOf(')');
                        const cutIdx = commaIdx !== -1 ? commaIdx : endIdx;
                        retType = retType.slice(1, cutIdx).trim();
                    }

                    const bare = extractBareType(retType);

                    const resolvedFields = this.fieldResolver?.(bare);
                    resolvedField = {
                        ...field,
                        type: retType,
                        fields: (resolvedFields && resolvedFields.length > 0) ? resolvedFields : field.fields,
                        isSlice: retType.startsWith('[]'),
                        isMap: retType.startsWith('map['),
                        // func() fields carry no return ParamInfo doc; preserve the field's own doc
                        doc: field.doc,
                    };
                }
            }

            if (i === path.length - 1) {
                return {
                    typeStr: resolvedField.type,
                    fields: resolvedField.fields,
                    isSlice: resolvedField.isSlice,
                    isMap: resolvedField.isMap,
                    elemType: resolvedField.elemType,
                    keyType: resolvedField.keyType,
                    params: resolvedField.params,
                    returns: resolvedField.returns,
                };
            }

            current = resolvedField.fields ?? [];
        }

        return null;
    }

    private inferCallType(func: string, args: ExprNode[]): TypeResult | null {
        // 1. Prioritize user-defined functions from FuncMap. 
        // This allows them to override built-ins like 'add' and directly provides the precise return type.
        // NOTE: We always infer dict call from builtins for proper context resolution.
        if (func != "dict" && this.funcMaps?.has(func)) {
            const fn = this.funcMaps.get(func)!;
            if (fn.returns && fn.returns.length > 0) {
                let retType = fn.returns[0].type;
                const bare = extractBareType(retType);
                const retFields = fn.returns[0].fields || fn.returnTypeFields;

                const resolvedFields = this.fieldResolver?.(bare);
                return { typeStr: retType, fields: (resolvedFields && resolvedFields.length > 0) ? resolvedFields : retFields };
            }
        }

        // 2. Fallback to built-in template functions and operators
        switch (func) {
            case 'index': {
                if (args.length < 2) return null;
                let currentType = this.inferNodeType(args[0]);
                if (!currentType) return null;

                for (let i = 1; i < args.length; i++) {
                    // Strip pointer from the collection type before inspecting its shape.
                    let targetTypeStr = currentType.typeStr;
                    while (targetTypeStr.startsWith('*')) targetTypeStr = targetTypeStr.slice(1);

                    let elemTypeStr = '';
                    if (currentType.elemType) {
                        elemTypeStr = currentType.elemType;
                    } else if (targetTypeStr.startsWith('[]')) {
                        elemTypeStr = targetTypeStr.slice(2);
                    } else if (targetTypeStr.startsWith('map[')) {
                        elemTypeStr = this.extractMapValueType(targetTypeStr);
                    }

                    if (!elemTypeStr) return null;

                    // Go templates auto-dereference pointers; strip so that downstream type
                    // lookups and field resolution always receive a bare struct name.
                    // e.g. map[uint][]*Payment → index → '[]*Payment' → elemType 'Payment'
                    while (elemTypeStr.startsWith('*')) elemTypeStr = elemTypeStr.slice(1);

                    const bareType = extractBareType(elemTypeStr);
                    let isSlice = false, isMap = false;
                    let nextElemType = '', nextKeyType = '';

                    if (elemTypeStr.startsWith('[]')) {
                        isSlice = true;
                        nextElemType = elemTypeStr.slice(2);
                        // Strip pointer from the slice's own element type.
                        while (nextElemType.startsWith('*')) nextElemType = nextElemType.slice(1);
                    } else if (elemTypeStr.startsWith('map[')) {
                        isMap = true;
                        let depth = 0, splitIdx = -1;
                        for (let j = 4; j < elemTypeStr.length; j++) {
                            if (elemTypeStr[j] === '[') depth++;
                            else if (elemTypeStr[j] === ']') {
                                if (depth === 0) { splitIdx = j; break; }
                                depth--;
                            }
                        }
                        if (splitIdx !== -1) {
                            nextKeyType = elemTypeStr.slice(4, splitIdx);
                            nextElemType = elemTypeStr.slice(splitIdx + 1);
                            // Strip pointer from map value type.
                            while (nextElemType.startsWith('*')) nextElemType = nextElemType.slice(1);
                        }
                    }

                    currentType = {
                        typeStr: elemTypeStr,
                        fields: this.fieldResolver?.(bareType),
                        isSlice,
                        isMap,
                        elemType: nextElemType || undefined,
                        keyType: nextKeyType || undefined,
                    };
                }

                return currentType;
            }

            case 'slice': {
                // slice collection low high
                if (args.length < 1) return null;
                const target = this.inferNodeType(args[0]);
                if (!target) return null;

                // slice returns the same type as input for slices/arrays
                return target;
            }

            case 'len':
                // len returns int
                return { typeStr: 'int' };

            case 'print':
            case 'printf':
            case 'println':
                // print functions return string
                return { typeStr: 'string' };

            case 'html':
            case 'js':
            case 'urlquery':
                // Escaping functions return string
                return { typeStr: 'string' };

            case 'dict': {
                if (args.length % 2 !== 0) {
                    // Invalid dict call: odd number of arguments
                    return { typeStr: 'map[string]any', isMap: true, fields: [] }; // Return a map type, but empty fields
                }

                const fields: FieldInfo[] = [];
                for (let i = 0; i < args.length; i += 2) {
                    const keyArg = args[i];
                    const valueArg = args[i + 1];

                    if (keyArg.kind !== 'string') {
                        // Dict keys must be string literals, cannot infer dynamically
                        // Continue to process other pairs, but this one is problematic
                        continue;
                    }

                    const keyName = keyArg.value;
                    const valueType = this.inferNodeType(valueArg);

                    if (valueType) {
                        fields.push({
                            name: keyName,
                            type: valueType.typeStr,
                            fields: valueType.fields,
                            isSlice: valueType.isSlice ?? false,
                            isMap: valueType.isMap ?? false,
                            elemType: valueType.elemType,
                            keyType: valueType.keyType,
                        });
                    } else {
                        // Couldn't infer type of value, add as unknown
                        fields.push({
                            name: keyName,
                            type: 'unknown',
                            isSlice: false,
                            isMap: false,
                        });
                    }
                }
                return { typeStr: 'map[string]any', isMap: true, fields };
            }

            case 'call': {
                // call function arg...
                if (args.length < 1) return { typeStr: 'unknown' };

                const target = this.inferNodeType(args[0]);
                if (!target) return { typeStr: 'unknown' };

                // If the target is a direct variable holding a func (e.g., call $myFunc)
                // we unwrap its return type here. (Field paths are already unwrapped by resolveFieldPath)
                if (target.typeStr.startsWith('func(')) {
                    const match = target.typeStr.match(/func\([^)]*\)\s*(.+)/);
                    if (match && match[1]) {
                        let retType = match[1].trim();
                        if (retType.startsWith('(')) {
                            const commaIdx = retType.indexOf(',');
                            const endIdx = retType.indexOf(')');
                            const cutIdx = commaIdx !== -1 ? commaIdx : endIdx;
                            retType = retType.slice(1, cutIdx).trim();
                        }

                        const bare = extractBareType(retType);
                        const resolvedFields = this.fieldResolver?.(bare);

                        return {
                            typeStr: retType,
                            fields: (resolvedFields && resolvedFields.length > 0) ? resolvedFields : target.fields,
                            isSlice: retType.startsWith('[]'),
                            isMap: retType.startsWith('map['),
                        };
                    }
                }

                // Return the evaluated target
                return target;
            }

            // Comparison operators - all return bool
            case 'eq':
            case 'ne':
            case 'lt':
            case 'le':
            case 'gt':
            case 'ge':
                return { typeStr: 'bool' };

            // Logical operators - all return bool
            case 'and':
            case 'or':
                return { typeStr: 'bool' };

            case 'not':
                return { typeStr: 'bool' };

            // Arithmetic operators
            case 'add':
            case 'sub':
            case 'mul':
            case 'div':
            case 'mod': {
                // Try to infer from arguments
                if (args.length >= 2) {
                    const leftType = this.inferNodeType(args[0]);
                    const rightType = this.inferNodeType(args[1]);

                    if (leftType?.typeStr === 'string' || rightType?.typeStr === 'string') {
                        return { typeStr: 'string' };
                    }
                    if (this.isNumericType(leftType?.typeStr) && this.isNumericType(rightType?.typeStr)) {
                        return leftType!;
                    }
                }
                return { typeStr: 'float64' };
            }

            default:
                return null;
        }
    }

    private inferMethodType(receiver: ExprNode, method: string, args: ExprNode[]): TypeResult | null {
        const receiverType = this.inferNodeType(receiver);
        if (!receiverType || !receiverType.fields) return null;

        const methodField = receiverType.fields.find(f => f.name === method);
        if (!methodField) return null;

        // Check if returns has values
        if (methodField.returns && methodField.returns.length > 0) {
            let retType = methodField.returns[0].type;
            const bare = extractBareType(retType);
            const retFields = methodField.returns[0].fields; // Extract from return type

            const resolvedFields = this.fieldResolver?.(bare);
            return {
                typeStr: retType,
                fields: (resolvedFields && resolvedFields.length > 0) ? resolvedFields : (retFields || methodField.fields),
                // Propagate slice/map info if available from resolved fields or type string
                isSlice: retType.startsWith('[]'),
                isMap: retType.startsWith('map['),
            };
        }

        return { typeStr: 'unknown' };
    }

    private inferIndexType(target: ExprNode, _index: ExprNode): TypeResult | null {
        const targetType = this.inferNodeType(target);
        if (!targetType) return null;

        // Strip pointer from the collection type before inspecting its shape.
        let targetTypeStr = targetType.typeStr;
        while (targetTypeStr.startsWith('*')) targetTypeStr = targetTypeStr.slice(1);

        let elemTypeStr = '';
        if (targetType.elemType) {
            elemTypeStr = targetType.elemType;
        } else if (targetTypeStr.startsWith('[]')) {
            elemTypeStr = targetTypeStr.slice(2);
        } else if (targetTypeStr.startsWith('map[')) {
            elemTypeStr = this.extractMapValueType(targetTypeStr);
        }

        if (!elemTypeStr) return null;

        // Go templates auto-dereference pointers.
        while (elemTypeStr.startsWith('*')) elemTypeStr = elemTypeStr.slice(1);

        const bareType = extractBareType(elemTypeStr);
        let isSlice = false, isMap = false;
        let nextElemType = '', nextKeyType = '';

        if (elemTypeStr.startsWith('[]')) {
            isSlice = true;
            nextElemType = elemTypeStr.slice(2);
            while (nextElemType.startsWith('*')) nextElemType = nextElemType.slice(1);
        } else if (elemTypeStr.startsWith('map[')) {
            isMap = true;
            let depth = 0, splitIdx = -1;
            for (let i = 4; i < elemTypeStr.length; i++) {
                if (elemTypeStr[i] === '[') depth++;
                else if (elemTypeStr[i] === ']') {
                    if (depth === 0) { splitIdx = i; break; }
                    depth--;
                }
            }
            if (splitIdx !== -1) {
                nextKeyType = elemTypeStr.slice(4, splitIdx);
                nextElemType = elemTypeStr.slice(splitIdx + 1);
                while (nextElemType.startsWith('*')) nextElemType = nextElemType.slice(1);
            }
        }

        return {
            typeStr: elemTypeStr,
            fields: this.fieldResolver?.(bareType),
            isSlice,
            isMap,
            elemType: nextElemType || undefined,
            keyType: nextKeyType || undefined,
        };
    }

    private inferSliceType(target: ExprNode, _low?: ExprNode, _high?: ExprNode, _max?: ExprNode): TypeResult | null {
        const targetType = this.inferNodeType(target);
        if (!targetType) return null;

        // Slicing returns the same type
        return targetType;
    }

    private inferPipelineType(stages: ExprNode[]): TypeResult | null {
        if (stages.length === 0) return null;

        // In a pipeline, the output of each stage becomes the input of the next
        // For now, return the type of the last stage
        return this.inferNodeType(stages[stages.length - 1]);
    }

    private inferBinaryType(op: string, left: ExprNode, right: ExprNode): TypeResult | null {
        const leftType = this.inferNodeType(left);
        const rightType = this.inferNodeType(right);

        switch (op) {
            case 'eq':
            case 'ne':
            case 'lt':
            case 'le':
            case 'gt':
            case 'ge':
                // Comparison operators return bool
                return { typeStr: 'bool' };

            case 'and':
            case 'or':
                // Logical operators return bool
                return { typeStr: 'bool' };

            case 'add':
            case 'sub':
            case 'mul':
            case 'div':
            case 'mod':
                // Arithmetic operators: if both are numbers, return number
                // For strings, add is concatenation
                if (leftType?.typeStr === 'string' || rightType?.typeStr === 'string') {
                    return { typeStr: 'string' };
                }
                if (this.isNumericType(leftType?.typeStr) && this.isNumericType(rightType?.typeStr)) {
                    // Return the wider type
                    return leftType!;
                }
                return { typeStr: 'float64' };

            default:
                return null;
        }
    }

    private inferUnaryType(op: string, arg: ExprNode): TypeResult | null {
        switch (op) {
            case 'not':
                // not returns bool
                return { typeStr: 'bool' };

            default:
                return null;
        }
    }

    private findDotFrame(): ScopeFrame | undefined {
        for (let i = this.scopeStack.length - 1; i >= 0; i--) {
            if (this.scopeStack[i].key === '.') {
                return this.scopeStack[i];
            }
        }
        return undefined;
    }

    private isNumericType(typeStr?: string): boolean {
        if (!typeStr) return false;
        return [
            'int', 'int8', 'int16', 'int32', 'int64',
            'uint', 'uint8', 'uint16', 'uint32', 'uint64',
            'float32', 'float64',
            'complex64', 'complex128',
            'byte', 'rune',
        ].includes(typeStr);
    }
}

// ── Public API ─────────────────────────────────────────────────────────────

/**
 * Parse and infer the type of a template expression.
 * 
 * @param expr - The template expression to parse
 * @param vars - Available template variables
 * @param scopeStack - Current scope stack
 * @param blockLocals - Local variables defined in the current block
 * @returns Type result or null if inference fails
 */
export function inferExpressionType(
    expr: string,
    vars: Map<string, TemplateVar>,
    scopeStack: ScopeFrame[],
    blockLocals?: Map<string, TemplateVar>,
    funcMaps?: Map<string, FuncMapInfo>,
    fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
): TypeResult | null {
    const inferencer = new TypeInferencer(vars, scopeStack, blockLocals, funcMaps, fieldResolver);
    return inferencer.inferType(expr);
}
