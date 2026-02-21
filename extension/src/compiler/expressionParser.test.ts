/**
 * Comprehensive test suite for the expression parser
 */

import { inferExpressionType } from './expressionParser';
import { TemplateVar, ScopeFrame, FieldInfo } from '../types';

// ── Test Data Setup ────────────────────────────────────────────────────────

function createTestVars(): Map<string, TemplateVar> {
  return new Map([
    ['User', {
      name: 'User',
      type: 'User',
      isSlice: false,
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'Age', type: 'int', isSlice: false },
        { name: 'Email', type: 'string', isSlice: false },
        {
          name: 'Profile',
          type: 'Profile',
          isSlice: false,
          fields: [
            { name: 'Bio', type: 'string', isSlice: false },
            { name: 'Avatar', type: 'string', isSlice: false },
          ],
        },
      ],
    }],
    ['Items', {
      name: 'Items',
      type: '[]Item',
      isSlice: true,
      elemType: 'Item',
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'Price', type: 'float64', isSlice: false },
        { name: 'Quantity', type: 'int', isSlice: false },
        {
          name: 'Tags',
          type: '[]string',
          isSlice: true,
          elemType: 'string',
        },
      ],
    }],
    ['Count', {
      name: 'Count',
      type: 'int',
      isSlice: false,
    }],
    ['Total', {
      name: 'Total',
      type: 'float64',
      isSlice: false,
    }],
    ['Active', {
      name: 'Active',
      type: 'bool',
      isSlice: false,
    }],
    ['Config', {
      name: 'Config',
      type: 'map[string]interface{}',
      isMap: true,
      keyType: 'string',
      elemType: 'interface{}',
      isSlice: false,
    }],
    ['Settings', {
      name: 'Settings',
      type: 'map[string]string',
      isMap: true,
      keyType: 'string',
      elemType: 'string',
      isSlice: false,
    }],
  ]);
}

// ── Test Runner ────────────────────────────────────────────────────────────

interface TestCase {
  name: string;
  expr: string;
  expectedType: string;
  expectedSlice?: boolean;
  expectedMap?: boolean;
  scope?: ScopeFrame[];
  blockLocals?: Map<string, TemplateVar>;
}

function runTest(tc: TestCase, vars: Map<string, TemplateVar>): boolean {
  try {
    const result = inferExpressionType(tc.expr, vars, tc.scope || [], tc.blockLocals);

    if (!result) {
      console.log(`✗ FAIL: ${tc.name}`);
      console.log(`  Expression: "${tc.expr}"`);
      console.log(`  Expected: ${tc.expectedType}`);
      console.log(`  Got: null`);
      return false;
    }

    const typeMatch = result.typeStr === tc.expectedType;
    const sliceMatch = tc.expectedSlice === undefined || result.isSlice === tc.expectedSlice;
    const mapMatch = tc.expectedMap === undefined || result.isMap === tc.expectedMap;

    if (typeMatch && sliceMatch && mapMatch) {
      console.log(`✓ PASS: ${tc.name}`);
      return true;
    } else {
      console.log(`✗ FAIL: ${tc.name}`);
      console.log(`  Expression: "${tc.expr}"`);
      console.log(`  Expected: ${tc.expectedType} (slice=${tc.expectedSlice}, map=${tc.expectedMap})`);
      console.log(`  Got: ${result.typeStr} (slice=${result.isSlice}, map=${result.isMap})`);
      return false;
    }
  } catch (err) {
    console.log(`✗ ERROR: ${tc.name}`);
    console.log(`  Expression: "${tc.expr}"`);
    console.log(`  Error: ${err}`);
    return false;
  }
}

// ── Test Suites ────────────────────────────────────────────────────────────

export function runAllTests() {
  console.log('╔═════════════════════════════════════════════════════════════╗');
  console.log('║       Expression Parser Test Suite                          ║');
  console.log('╚═════════════════════════════════════════════════════════════╝\n');

  const vars = createTestVars();
  let totalPassed = 0;
  let totalFailed = 0;

  // Run all test suites
  const suites = [
    { name: 'Basic Field Access', fn: testBasicFieldAccess },
    { name: 'Built-in Functions', fn: testBuiltinFunctions },
    { name: 'Comparison Operations', fn: testComparisonOps },
    { name: 'Logical Operations', fn: testLogicalOps },
    { name: 'Pipeline Operations', fn: testPipelines },
    { name: 'Collection Operations', fn: testCollections },
    { name: 'Map Operations', fn: testMapOps },
    { name: 'Scope and Context', fn: testScopeContext },
    { name: 'Local Variables', fn: testLocalVariables },
    { name: 'Complex Expressions', fn: testComplexExpressions },
    { name: 'Edge Cases', fn: testEdgeCases },
  ];

  for (const suite of suites) {
    console.log(`\n━━━ ${suite.name} ━━━\n`);
    const [passed, failed] = suite.fn(vars);
    totalPassed += passed;
    totalFailed += failed;
  }

  console.log('\n╔═════════════════════════════════════════════════════════════╗');
  console.log(`║ Total: ${totalPassed} passed, ${totalFailed} failed`.padEnd(62) + '║');
  console.log('╚═════════════════════════════════════════════════════════════╝\n');

  return { passed: totalPassed, failed: totalFailed };
}

// ── Test Suite 1: Basic Field Access ──────────────────────────────────────

function testBasicFieldAccess(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'Bare dot', expr: '.', expectedType: 'context' },
    { name: 'Simple field', expr: '.Count', expectedType: 'int' },
    { name: 'Nested field', expr: '.User.Name', expectedType: 'string' },
    { name: 'Deep nested field', expr: '.User.Profile.Bio', expectedType: 'string' },
    { name: 'Root context', expr: '$', expectedType: 'context' },
    { name: 'Root field access', expr: '$.Count', expectedType: 'int' },
    { name: 'Slice field', expr: '.Items', expectedType: '[]Item', expectedSlice: true },
    { name: 'Map field', expr: '.Config', expectedType: 'map[string]interface{}', expectedMap: true },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 2: Built-in Functions ──────────────────────────────────────

function testBuiltinFunctions(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'len on slice', expr: 'len .Items', expectedType: 'int' },
    { name: 'len on map', expr: 'len .Config', expectedType: 'int' },
    { name: 'index on slice', expr: 'index .Items 0', expectedType: 'Item' },
    { name: 'index on map', expr: 'index .Config "key"', expectedType: 'interface{}' },
    { name: 'slice operation', expr: 'slice .Items 0 5', expectedType: '[]Item' },
    { name: 'print function', expr: 'print .Count', expectedType: 'string' },
    { name: 'printf function', expr: 'printf "%d" .Count', expectedType: 'string' },
    { name: 'println function', expr: 'println .Count', expectedType: 'string' },
    { name: 'html escape', expr: 'html .User.Profile.Bio', expectedType: 'string' },
    { name: 'js escape', expr: 'js .User.Name', expectedType: 'string' },
    { name: 'urlquery escape', expr: 'urlquery .User.Email', expectedType: 'string' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 3: Comparison Operations ───────────────────────────────────

function testComparisonOps(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'eq comparison', expr: 'eq .Count 10', expectedType: 'bool' },
    { name: 'ne comparison', expr: 'ne .Count 0', expectedType: 'bool' },
    { name: 'lt comparison', expr: 'lt .Count 100', expectedType: 'bool' },
    { name: 'le comparison', expr: 'le .Count 100', expectedType: 'bool' },
    { name: 'gt comparison', expr: 'gt .Count 0', expectedType: 'bool' },
    { name: 'ge comparison', expr: 'ge .Count 0', expectedType: 'bool' },
    { name: 'eq with string', expr: 'eq .User.Name "admin"', expectedType: 'bool' },
    { name: 'nested eq', expr: 'eq (len .Items) 5', expectedType: 'bool' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 4: Logical Operations ──────────────────────────────────────

function testLogicalOps(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'not operation', expr: 'not .Active', expectedType: 'bool' },
    { name: 'and operation', expr: 'and .Active (gt .Count 0)', expectedType: 'bool' },
    { name: 'or operation', expr: 'or .Active (eq .Count 0)', expectedType: 'bool' },
    { name: 'nested and', expr: 'and (gt .Count 0) (lt .Count 100)', expectedType: 'bool' },
    { name: 'nested or', expr: 'or (eq .Count 0) (eq .Count -1)', expectedType: 'bool' },
    { name: 'complex logical', expr: 'and (not .Active) (gt .Count 5)', expectedType: 'bool' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 5: Pipeline Operations ─────────────────────────────────────

function testPipelines(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'Simple pipe', expr: '.Count | printf "%d"', expectedType: 'string' },
    { name: 'Multi-stage pipe', expr: '.Items | len | printf "%d"', expectedType: 'string' },
    { name: 'Pipe with comparison', expr: '.Count | gt 10', expectedType: 'bool' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 6: Collection Operations ───────────────────────────────────

function testCollections(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'Slice access', expr: 'index .Items 0', expectedType: 'Item' },
    { name: 'Slice length', expr: 'len .Items', expectedType: 'int' },
    { name: 'Slice operation', expr: 'slice .Items 1 3', expectedType: '[]Item' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 7: Map Operations ──────────────────────────────────────────

function testMapOps(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'Map index', expr: 'index .Config "key"', expectedType: 'interface{}' },
    { name: 'Map length', expr: 'len .Config', expectedType: 'int' },
    { name: 'String map index', expr: 'index .Settings "theme"', expectedType: 'string' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 8: Scope and Context ───────────────────────────────────────

function testScopeContext(vars: Map<string, TemplateVar>): [number, number] {
  const itemFields: FieldInfo[] = [
    { name: 'Name', type: 'string', isSlice: false },
    { name: 'Price', type: 'float64', isSlice: false },
  ];

  const scope: ScopeFrame[] = [
    {
      key: '.',
      typeStr: 'Item',
      fields: itemFields,
    },
  ];

  const tests: TestCase[] = [
    { name: 'Scoped field access', expr: '.Name', expectedType: 'string', scope },
    { name: 'Scoped nested access', expr: '.Price', expectedType: 'float64', scope },
    { name: 'Root access inside scope', expr: '$.User.Name', expectedType: 'string', scope },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 9: Local Variables ─────────────────────────────────────────

function testLocalVariables(vars: Map<string, TemplateVar>): [number, number] {
  const blockLocals = new Map<string, TemplateVar>([
    ['$item', {
      name: '$item',
      type: 'Item',
      isSlice: false,
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'Price', type: 'float64', isSlice: false },
      ],
    }],
    ['$idx', { name: '$idx', type: 'int', isSlice: false }],
  ]);

  const scopeWithLocals: ScopeFrame[] = [
    {
      key: '.',
      typeStr: 'context',
      locals: new Map([
        ['$parentVar', { name: '$parentVar', type: 'string', isSlice: false }]
      ])
    }
  ];

  const tests: TestCase[] = [
    { name: 'Simple local var', expr: '$item', expectedType: 'Item', blockLocals },
    { name: 'Local var field access', expr: '$item.Name', expectedType: 'string', blockLocals },
    { name: 'Local var in function', expr: 'index .Items $idx', expectedType: 'Item', blockLocals },
    { name: 'Local var comparison', expr: 'gt $item.Price 10.0', expectedType: 'bool', blockLocals },
    { name: 'Parent scope local var', expr: '$parentVar', expectedType: 'string', scope: scopeWithLocals },
    { name: 'Mix root and local', expr: 'eq $.Count $idx', expectedType: 'bool', blockLocals },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 10: Complex Expressions ────────────────────────────────────

function testComplexExpressions(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    {
      name: 'Nested function calls',
      expr: 'printf "%d items" (len .Items)',
      expectedType: 'string',
    },
    {
      name: 'Multiple comparisons',
      expr: 'and (gt .Count 0) (le .Count 100)',
      expectedType: 'bool',
    },
    {
      name: 'Pipeline with function',
      expr: '.Items | len | printf "Count: %d"',
      expectedType: 'string',
    },
    {
      name: 'Comparison in pipeline',
      expr: '.Count | gt 10',
      expectedType: 'bool',
    },
    {
      name: 'Complex logical with fields',
      expr: 'and (eq .User.Profile.Bio "") (not .Active)',
      expectedType: 'bool',
    }
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Test Suite 11: Edge Cases ─────────────────────────────────────────────

function testEdgeCases(vars: Map<string, TemplateVar>): [number, number] {
  const tests: TestCase[] = [
    { name: 'String literal', expr: '"hello world"', expectedType: 'string' },
    { name: 'Number literal', expr: '42', expectedType: 'float64' },
    { name: 'Parenthesized expression', expr: '(gt .Count 5)', expectedType: 'bool' },
  ];

  let passed = 0;
  let failed = 0;

  for (const tc of tests) {
    if (runTest(tc, vars)) passed++;
    else failed++;
  }

  return [passed, failed];
}

// ── Main Entry Point ───────────────────────────────────────────────────────

if (require.main === module) {
  const result = runAllTests();
  process.exit(result.failed > 0 ? 1 : 0);
}
