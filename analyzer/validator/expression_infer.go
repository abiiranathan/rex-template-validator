package validator

import (
	"fmt"
	"maps"
	"strings"
	"text/template"
	templateparse "text/template/parse"

	"github.com/abiiranathan/gotpl-analyzer/ast"
)

type ExpressionTypeResult struct {
	TypeStr  string          `json:"typeStr"`
	Fields   []ast.FieldInfo `json:"fields,omitempty"`
	IsSlice  bool            `json:"isSlice,omitempty"`
	IsMap    bool            `json:"isMap,omitempty"`
	ElemType string          `json:"elemType,omitempty"`
	KeyType  string          `json:"keyType,omitempty"`
	Params   []ast.ParamInfo `json:"params,omitempty"`
	Returns  []ast.ParamInfo `json:"returns,omitempty"`
	Doc      string          `json:"doc,omitempty"`
	Literal  string          `json:"-"`
}

type expressionInferencer struct {
	vars         map[string]ast.TemplateVar
	scopeStack   []ScopeType
	blockLocals  map[string]ast.TemplateVar
	funcMaps     FuncMapRegistry
	typeRegistry map[string][]ast.FieldInfo
}

func InferExpressionType(
	expr string,
	vars map[string]ast.TemplateVar,
	scopeStack []ScopeType,
	blockLocals map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
	typeRegistry map[string][]ast.FieldInfo,
) *ExpressionTypeResult {
	inferencer := expressionInferencer{
		vars:         cloneTemplateVarMap(vars),
		scopeStack:   cloneScopeStack(scopeStack),
		blockLocals:  cloneTemplateVarMap(blockLocals),
		funcMaps:     maps.Clone(funcMaps),
		typeRegistry: maps.Clone(typeRegistry),
	}
	return inferencer.infer(expr)
}

func (i expressionInferencer) infer(expr string) *ExpressionTypeResult {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}

	// Collect all $var names from blockLocals and scopeStack so the parser accepts them
	localVarNames := i.collectLocalVarNames()

	tree, err := parseExpressionTree(expr, i.funcMaps, localVarNames)
	if err != nil {
		return nil
	}

	// Skip the pre-declaration actions and only infer the last action (the real expression)
	for idx := len(tree.Root.Nodes) - 1; idx >= 0; idx-- {
		action, ok := tree.Root.Nodes[idx].(*templateparse.ActionNode)
		if !ok {
			continue
		}
		return i.hydrateResult(i.inferPipe(action.Pipe))
	}

	return nil
}

// collectLocalVarNames returns all $var names from blockLocals and scopeStack Locals.
func (i expressionInferencer) collectLocalVarNames() []string {
	seen := make(map[string]struct{})
	for name := range i.blockLocals {
		if strings.HasPrefix(name, "$") && name != "$" {
			seen[name] = struct{}{}
		}
	}
	for _, scope := range i.scopeStack {
		for name := range scope.Locals {
			if strings.HasPrefix(name, "$") && name != "$" {
				seen[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
}

func parseExpressionTree(expr string, funcMaps FuncMapRegistry, localVarNames []string) (*templateparse.Tree, error) {
	funcDefs := template.FuncMap{}
	for _, name := range []string{"dict", "add", "sub", "mul", "div", "mod"} {
		funcDefs[name] = func(...any) any { return nil }
	}
	for name := range funcMaps {
		funcDefs[name] = func(...any) any { return nil }
	}

	// Pre-declare local $variables so Go's template parser accepts them
	var prefix string
	for _, name := range localVarNames {
		prefix += "{{ " + name + " := . }}"
	}

	tmpl, err := template.New("expr").Funcs(funcDefs).Parse(prefix + "{{ " + expr + " }}")
	if err != nil {
		return nil, err
	}
	if tmpl.Tree == nil {
		return nil, fmt.Errorf("missing parse tree")
	}
	return tmpl.Tree, nil
}

func (i expressionInferencer) inferPipe(pipe *templateparse.PipeNode) *ExpressionTypeResult {
	if pipe == nil {
		return nil
	}

	var piped *ExpressionTypeResult
	for _, cmd := range pipe.Cmds {
		piped = i.inferCommand(cmd, piped)
		if piped == nil {
			return nil
		}
	}
	return piped
}

func (i expressionInferencer) inferCommand(cmd *templateparse.CommandNode, piped *ExpressionTypeResult) *ExpressionTypeResult {
	if cmd == nil || len(cmd.Args) == 0 {
		return nil
	}

	if ident, ok := cmd.Args[0].(*templateparse.IdentifierNode); ok {
		args := make([]*ExpressionTypeResult, 0, len(cmd.Args)-1+boolToInt(piped != nil))
		for _, arg := range cmd.Args[1:] {
			args = append(args, i.inferNode(arg))
		}
		if piped != nil {
			args = append(args, piped)
		}
		return i.hydrateResult(i.inferFunctionCall(ident.Ident, cmd.Args[1:], args))
	}

	if len(cmd.Args) == 1 {
		return i.hydrateResult(i.inferNode(cmd.Args[0]))
	}

	return i.hydrateResult(i.inferNode(cmd.Args[0]))
}

func (i expressionInferencer) inferNode(node templateparse.Node) *ExpressionTypeResult {
	switch typed := node.(type) {
	case *templateparse.PipeNode:
		return i.inferPipe(typed)
	case *templateparse.DotNode:
		return i.currentDotType()
	case *templateparse.FieldNode:
		return i.resolveFieldPath(append([]string{"."}, typed.Ident...))
	case *templateparse.VariableNode:
		return i.resolveVariablePath(typed.Ident)
	case *templateparse.ChainNode:
		base := i.inferNode(typed.Node)
		return i.resolveChainedField(base, typed.Field)
	case *templateparse.IdentifierNode:
		return i.resolveIdentifier(typed.Ident)
	case *templateparse.StringNode:
		return &ExpressionTypeResult{TypeStr: "string", Literal: typed.Text}
	case *templateparse.NumberNode:
		if typed.IsInt {
			return &ExpressionTypeResult{TypeStr: "int"}
		}
		return &ExpressionTypeResult{TypeStr: "float64"}
	case *templateparse.BoolNode:
		return &ExpressionTypeResult{TypeStr: "bool"}
	case *templateparse.NilNode:
		return &ExpressionTypeResult{TypeStr: "nil"}
	default:
		return nil
	}
}

func (i expressionInferencer) resolveIdentifier(name string) *ExpressionTypeResult {
	if name == "" {
		return nil
	}

	if v, ok := i.blockLocals[name]; ok {
		return templateVarToExpressionResult(v)
	}

	for idx := len(i.scopeStack) - 1; idx >= 0; idx-- {
		if local, ok := i.scopeStack[idx].Locals[name]; ok {
			return templateVarToExpressionResult(local)
		}
	}

	if v, ok := i.vars[name]; ok {
		return templateVarToExpressionResult(v)
	}

	if funcMap, ok := i.funcMaps[name]; ok {
		return functionResultToExpressionResult(funcMap)
	}

	return nil
}

func (i expressionInferencer) currentDotType() *ExpressionTypeResult {
	if len(i.scopeStack) > 0 {
		current := i.scopeStack[len(i.scopeStack)-1]
		return &ExpressionTypeResult{
			TypeStr:  current.TypeStrOrContext(),
			Fields:   current.Fields,
			IsSlice:  current.IsSlice,
			IsMap:    current.IsMap,
			ElemType: current.ElemType,
			KeyType:  current.KeyType,
		}
	}
	root := buildRootScope(i.vars)
	return &ExpressionTypeResult{TypeStr: "context", Fields: root.Fields}
}

func (i expressionInferencer) resolveVariablePath(parts []string) *ExpressionTypeResult {
	if len(parts) == 0 {
		return nil
	}

	if parts[0] == "$" {
		if len(parts) == 1 {
			return &ExpressionTypeResult{TypeStr: "context", Fields: buildRootScope(i.vars).Fields}
		}
		if rootVar, ok := i.vars[parts[1]]; ok {
			result := templateVarToExpressionResult(rootVar)
			return i.resolveChainedField(result, parts[2:])
		}
		return nil
	}

	name := parts[0]
	if v, ok := i.blockLocals[name]; ok {
		return i.resolveChainedField(templateVarToExpressionResult(v), parts[1:])
	}
	for idx := len(i.scopeStack) - 1; idx >= 0; idx-- {
		if local, ok := i.scopeStack[idx].Locals[name]; ok {
			return i.resolveChainedField(templateVarToExpressionResult(local), parts[1:])
		}
	}
	if v, ok := i.vars[name]; ok {
		return i.resolveChainedField(templateVarToExpressionResult(v), parts[1:])
	}

	return nil
}

func (i expressionInferencer) resolveFieldPath(parts []string) *ExpressionTypeResult {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 && parts[0] == "." {
		return i.currentDotType()
	}
	if parts[0] == "." {
		base := i.currentDotType()
		if len(parts) == 1 {
			return base
		}
		return i.resolveChainedField(base, parts[1:])
	}
	return i.resolveIdentifier(parts[0])
}

func (i expressionInferencer) resolveChainedField(base *ExpressionTypeResult, parts []string) *ExpressionTypeResult {
	if base == nil {
		return nil
	}
	current := i.hydrateResult(base)
	for _, part := range parts {
		if part == "" {
			continue
		}
		if current == nil {
			return nil
		}
		if current.IsMap {
			current = i.collectionElementResult(current)
			continue
		}
		field := findFieldInfo(current.Fields, part)
		if field == nil {
			return nil
		}
		current = i.normalizeFieldResult(*field)
	}
	return i.hydrateResult(current)
}

func (i expressionInferencer) normalizeFieldResult(field ast.FieldInfo) *ExpressionTypeResult {
	if field.TypeStr == "method" && len(field.Returns) > 0 {
		ret := field.Returns[0]
		return i.hydrateResult(&ExpressionTypeResult{
			TypeStr: ret.TypeStr,
			Fields:  ret.Fields,
			Params:  field.Params,
			Returns: field.Returns,
			Doc:     ret.Doc,
		})
	}

	if strings.HasPrefix(field.TypeStr, "func(") {
		result := &ExpressionTypeResult{
			TypeStr: field.TypeStr,
			Fields:  field.Fields,
			Doc:     field.Doc,
		}
		if returns := parseFunctionReturns(field.TypeStr); len(returns) > 0 {
			result.TypeStr = returns[0]
			result.Fields = i.fieldsForType(returns[0], result.Fields)
		}
		return i.hydrateResult(result)
	}

	return i.hydrateResult(&ExpressionTypeResult{
		TypeStr:  field.TypeStr,
		Fields:   field.Fields,
		IsSlice:  field.IsSlice,
		IsMap:    field.IsMap,
		ElemType: field.ElemType,
		KeyType:  field.KeyType,
		Params:   field.Params,
		Returns:  field.Returns,
		Doc:      field.Doc,
	})
}

func (i expressionInferencer) inferFunctionCall(name string, rawArgs []templateparse.Node, args []*ExpressionTypeResult) *ExpressionTypeResult {
	if funcMap, ok := i.funcMaps[name]; ok && name != "dict" {
		return i.hydrateResult(functionResultToExpressionResult(funcMap))
	}

	switch name {
	case "index":
		if len(args) == 0 {
			return nil
		}
		current := args[0]
		for range args[1:] {
			current = i.collectionElementResult(current)
			if current == nil {
				return nil
			}
		}
		return i.hydrateResult(current)
	case "slice":
		if len(args) == 0 {
			return nil
		}
		return i.hydrateResult(args[0])
	case "len":
		return &ExpressionTypeResult{TypeStr: "int"}
	case "print", "printf", "println", "html", "js", "urlquery":
		return &ExpressionTypeResult{TypeStr: "string"}
	case "eq", "ne", "lt", "le", "gt", "ge", "and", "or", "not":
		return &ExpressionTypeResult{TypeStr: "bool"}
	case "add", "sub", "mul", "div", "mod":
		return inferArithmeticResult(args)
	case "dict":
		return i.inferDictResult(rawArgs, args)
	case "call":
		if len(args) == 0 {
			return &ExpressionTypeResult{TypeStr: "unknown"}
		}
		return i.inferCallableTarget(args[0])
	default:
		return nil
	}
}

func (i expressionInferencer) inferCallableTarget(target *ExpressionTypeResult) *ExpressionTypeResult {
	if target == nil {
		return &ExpressionTypeResult{TypeStr: "unknown"}
	}
	if len(target.Returns) > 0 {
		ret := target.Returns[0]
		return i.hydrateResult(&ExpressionTypeResult{
			TypeStr: ret.TypeStr,
			Fields:  ret.Fields,
			Doc:     ret.Doc,
		})
	}
	if strings.HasPrefix(target.TypeStr, "func(") {
		if returns := parseFunctionReturns(target.TypeStr); len(returns) > 0 {
			return i.hydrateResult(&ExpressionTypeResult{
				TypeStr: returns[0],
				Fields:  i.fieldsForType(returns[0], target.Fields),
			})
		}
	}
	return i.hydrateResult(target)
}

func (i expressionInferencer) inferDictResult(rawArgs []templateparse.Node, args []*ExpressionTypeResult) *ExpressionTypeResult {
	fields := make([]ast.FieldInfo, 0, len(args)/2)
	for idx := 0; idx+1 < len(args); idx += 2 {
		keyNode := args[idx]
		valueNode := i.hydrateResult(args[idx+1])

		if keyNode == nil || keyNode.TypeStr != "string" {
			continue
		}

		keyName := keyNode.Literal
		if keyName == "" && idx < len(rawArgs) {
			if literal, ok := rawArgs[idx].(*templateparse.StringNode); ok {
				keyName = literal.Text
			}
		}
		if keyName == "" {
			continue
		}

		// When the value couldn't be resolved (e.g. $.SomeVar in a context
		// with empty vars), still record the key with type "any" so it is
		// visible as a valid top-level variable in the partial's scope.
		// Previously nil was skipped, which made $.RedirectPrefix / $.Path
		// disappear from the dict fields when the caller had no var context.
		if valueNode == nil {
			fields = append(fields, ast.FieldInfo{
				Name:    keyName,
				TypeStr: "any",
			})
			continue
		}

		fields = append(fields, ast.FieldInfo{
			Name:     keyName,
			TypeStr:  valueNode.TypeStr,
			Fields:   valueNode.Fields,
			IsSlice:  valueNode.IsSlice,
			IsMap:    valueNode.IsMap,
			KeyType:  valueNode.KeyType,
			ElemType: valueNode.ElemType,
		})
	}
	return &ExpressionTypeResult{TypeStr: "map[string]any", IsMap: true, Fields: fields}
}

func (i expressionInferencer) collectionElementResult(current *ExpressionTypeResult) *ExpressionTypeResult {
	if current == nil {
		return nil
	}
	result := i.hydrateResult(current)
	elemType := result.ElemType
	if elemType == "" {
		elemType = unwrapCollectionElemType(result.TypeStr)
	}
	if elemType == "" {
		return nil
	}
	elemType = strings.TrimSpace(elemType)
	return i.hydrateResult(&ExpressionTypeResult{
		TypeStr:  elemType,
		Fields:   i.fieldsForType(elemType, nil),
		IsSlice:  strings.HasPrefix(strings.TrimLeft(elemType, "*"), "[]"),
		IsMap:    strings.HasPrefix(strings.TrimLeft(elemType, "*"), "map["),
		ElemType: unwrapCollectionElemType(elemType),
		KeyType:  unwrapMapKeyType(elemType),
	})
}

func (i expressionInferencer) hydrateResult(result *ExpressionTypeResult) *ExpressionTypeResult {
	if result == nil {
		return nil
	}
	if len(result.Fields) == 0 {
		result.Fields = i.fieldsForType(result.TypeStr, nil)
	}
	if result.IsMap && result.KeyType == "" {
		result.KeyType = unwrapMapKeyType(result.TypeStr)
	}
	if (result.IsMap || result.IsSlice) && result.ElemType == "" {
		result.ElemType = unwrapCollectionElemType(result.TypeStr)
	}
	return result
}

func (i expressionInferencer) fieldsForType(typeStr string, fallback []ast.FieldInfo) []ast.FieldInfo {
	if len(fallback) > 0 {
		return fallback
	}
	key := extractBareType(typeStr)
	if key == "" || isPrimitiveTypeName(key) {
		return nil
	}
	return i.typeRegistry[key]
}

func templateVarToExpressionResult(v ast.TemplateVar) *ExpressionTypeResult {
	return &ExpressionTypeResult{
		TypeStr:  v.TypeStr,
		Fields:   v.Fields,
		IsSlice:  v.IsSlice,
		IsMap:    v.IsMap,
		ElemType: v.ElemType,
		KeyType:  v.KeyType,
		Doc:      v.Doc,
	}
}

func functionResultToExpressionResult(funcMap ast.FuncMapInfo) *ExpressionTypeResult {
	if len(funcMap.Returns) == 0 {
		return &ExpressionTypeResult{TypeStr: "unknown", Doc: funcMap.Doc}
	}
	ret := funcMap.Returns[0]
	fields := ret.Fields
	if len(fields) == 0 {
		fields = funcMap.ReturnTypeFields
	}
	return &ExpressionTypeResult{
		TypeStr: ret.TypeStr,
		Fields:  fields,
		Params:  funcMap.Params,
		Returns: funcMap.Returns,
		Doc:     funcMap.Doc,
	}
}

func findFieldInfo(fields []ast.FieldInfo, name string) *ast.FieldInfo {
	for idx := range fields {
		if fields[idx].Name == name {
			return &fields[idx]
		}
	}
	return nil
}

func inferArithmeticResult(args []*ExpressionTypeResult) *ExpressionTypeResult {
	for _, arg := range args {
		if arg != nil && arg.TypeStr == "string" {
			return &ExpressionTypeResult{TypeStr: "string"}
		}
	}
	for _, arg := range args {
		if arg != nil && isNumericTypeName(arg.TypeStr) {
			return &ExpressionTypeResult{TypeStr: arg.TypeStr}
		}
	}
	return &ExpressionTypeResult{TypeStr: "float64"}
}

func parseFunctionReturns(typeStr string) []string {
	start := strings.Index(typeStr, ")")
	if start == -1 || start+1 >= len(typeStr) {
		return nil
	}
	returns := strings.TrimSpace(typeStr[start+1:])
	if returns == "" {
		return nil
	}
	if strings.HasPrefix(returns, "(") && strings.HasSuffix(returns, ")") {
		returns = strings.TrimSpace(returns[1 : len(returns)-1])
	}
	parts := strings.Split(returns, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func unwrapCollectionElemType(typeStr string) string {
	trimmed := strings.TrimLeft(strings.TrimSpace(typeStr), "*")
	if strings.HasPrefix(trimmed, "[]") {
		return strings.TrimLeft(strings.TrimSpace(trimmed[2:]), "*")
	}
	if strings.HasPrefix(trimmed, "map[") {
		depth := 0
		for idx := 4; idx < len(trimmed); idx++ {
			switch trimmed[idx] {
			case '[':
				depth++
			case ']':
				if depth == 0 {
					return strings.TrimLeft(strings.TrimSpace(trimmed[idx+1:]), "*")
				}
				depth--
			}
		}
	}
	return ""
}

func unwrapMapKeyType(typeStr string) string {
	trimmed := strings.TrimLeft(strings.TrimSpace(typeStr), "*")
	if !strings.HasPrefix(trimmed, "map[") {
		return ""
	}
	depth := 0
	for idx := 4; idx < len(trimmed); idx++ {
		switch trimmed[idx] {
		case '[':
			depth++
		case ']':
			if depth == 0 {
				return strings.TrimSpace(trimmed[4:idx])
			}
			depth--
		}
	}
	return ""
}

func extractBareType(typeStr string) string {
	trimmed := strings.TrimSpace(typeStr)
	for {
		switch {
		case strings.HasPrefix(trimmed, "*"):
			trimmed = strings.TrimSpace(trimmed[1:])
		case strings.HasPrefix(trimmed, "[]"):
			trimmed = strings.TrimSpace(trimmed[2:])
		case strings.HasPrefix(trimmed, "map["):
			elem := unwrapCollectionElemType(trimmed)
			if elem == "" {
				return trimmed
			}
			trimmed = elem
		default:
			return trimmed
		}
	}
}

func isPrimitiveTypeName(typeStr string) bool {
	switch strings.TrimSpace(typeStr) {
	case "", "any", "interface{}", "bool", "string", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "complex64", "complex128", "byte", "rune", "error", "nil", "context", "unknown":
		return true
	default:
		return false
	}
}

func isNumericTypeName(typeStr string) bool {
	switch strings.TrimSpace(typeStr) {
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "complex64", "complex128", "byte", "rune":
		return true
	default:
		return false
	}
}

func cloneTemplateVarMap(input map[string]ast.TemplateVar) map[string]ast.TemplateVar {
	if input == nil {
		return nil
	}
	return maps.Clone(input)
}

func cloneScopeStack(input []ScopeType) []ScopeType {
	if len(input) == 0 {
		return nil
	}
	result := make([]ScopeType, len(input))
	copy(result, input)
	for idx := range result {
		if result[idx].Locals != nil {
			result[idx].Locals = maps.Clone(result[idx].Locals)
		}
	}
	return result
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s ScopeType) TypeStrOrContext() string {
	if strings.TrimSpace(s.TypeStr) == "" {
		return "context"
	}
	return s.TypeStr
}
