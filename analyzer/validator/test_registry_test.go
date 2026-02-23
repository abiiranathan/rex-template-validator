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
	reg := make(map[string][]NamedBlockEntry)
	extractNamedTemplatesFromContent(content, "/fake/path/test.html", "test.html", reg)
	for k, entries := range reg {
		for _, v := range entries {
			t.Logf("Found %s at line %d:\n%q", k, v.Line, v.Content)
		}
	}
	if len(reg) != 2 {
		t.Errorf("Expected 2 templates, got %d", len(reg))
	}
}
