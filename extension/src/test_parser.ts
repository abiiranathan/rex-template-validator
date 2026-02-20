import { TemplateParser } from './templateParser';

const p = new TemplateParser();
const content = `
{{ with .User }}
    {{ define "header" }}
        {{ .Age }}
    {{ end }}
    {{ .Name }}
{{ end }}
`;

const res = p.parse(content);
console.log(JSON.stringify(res, null, 2));
