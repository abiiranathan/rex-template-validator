package validator

import (
	"testing"
)

func TestExtractNamedTemplates(t *testing.T) {
	content := `
		{{ define "header" }}
			<h1>{{ .Title }}</h1>
			{{ if .Subtitle }}
				<h2>{{ .Subtitle }}</h2>
			{{ end }}
		{{ end }}
		
		{{ block "footer" .Data }}<footer>{{ .Copyright }}</footer>{{ end }}
	`
	reg := make(map[string]NamedTemplate)
	extractNamedTemplatesFromContent(content, "test.html", reg)
	for k, v := range reg {
		t.Logf("Found %s at line %d:\n%q", k, v.LineNum, v.Content)
	}
	if len(reg) != 2 {
		t.Errorf("Expected 2 templates, got %d", len(reg))
	}
}
