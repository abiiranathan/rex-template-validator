import { TemplateParser } from './templateParser';
import { TemplateValidator } from './validator';
import { KnowledgeGraphBuilder } from './knowledgeGraph';
import * as vscode from 'vscode';

const p = new TemplateParser();
const content = `{{ block "billed-drug" .Drug }}
    {{ .Name }}
{{ end }}`;

// Dummy output channel
const outputChannel: any = { appendLine: console.log };
const validator = new TemplateValidator(outputChannel, {} as any);

const vars = new Map<string, any>([
  ['Drug', { name: 'Drug', type: 'Drug', isSlice: false, fields: [{ name: 'Name', type: 'string', isSlice: false, doc: 'The name' }] }]
]);

const doc: any = {
  getText: () => content,
  uri: { fsPath: 'test.html' }
};

const pos = new vscode.Position(1, 8); // Line 2, column 9 (hover over .Name)
// Wait, position lines are 0-based in vscode
const hit = (validator as any).findNodeAtPosition(p.parse(content), pos, vars, []);
console.log(JSON.stringify(hit, null, 2));

