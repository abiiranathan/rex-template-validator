import { TemplateParser } from './templateParser';

const p = new TemplateParser();
const content = `
{{ if .A }}
  A
{{ else if .B }}
  B
{{ else with .C }}
  C
{{ else range .D }}
  D
{{ else }}
  E
{{ end }}
`;

console.log(JSON.stringify(p.parse(content), null, 2));