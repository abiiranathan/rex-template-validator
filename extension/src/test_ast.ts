import { TemplateParser } from './/templateParser';

const content = `
{{ range .billedDrugs }}
  {{ template "billed-drug" . }}
{{ end }}

{{ block "billed-drug" . }}
  {{ .Name }}
{{ end }}
`;

const p = new TemplateParser();
console.log(JSON.stringify(p.parse(content), null, 2));
