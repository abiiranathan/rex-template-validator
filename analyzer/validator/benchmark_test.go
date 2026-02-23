package validator

import (
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkAnalyzeDir(b *testing.B) {
	// Setup a temporary directory with some Go code and templates
	tmpDir := b.TempDir()

	mainContent := `package main

import "net/http"

type Address struct {
	Street string
	City   string
	Zip    string
}

type Profile struct {
	Bio     string
	Address Address
}

type User struct {
	Name    string
	Profile Profile
}

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	user := User{
		Name: "Alice",
		Profile: Profile{
			Bio: "Gopher",
			Address: Address{
				Street: "123 Go Way",
				City:   "Gopolis",
				Zip:    "90210",
			},
		},
	}

	Render(nil, "index.html", map[string]interface{}{
		"User": user,
	})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		b.Fatalf("failed to write main.go: %v", err)
	}

	goModContent := `module example.com/test
go 1.21
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		b.Fatalf("failed to write go.mod: %v", err)
	}

	// Create a template file
	templateContent := `
{{ define "header" }}<h1>{{ .Title }}</h1>{{ end }}
{{ template "header" . }}
<ul>
{{ range .User.Profile.Address.Street }}
	<li>{{ . }}</li>
{{ end }}
</ul>
{{ if .User }}
	{{ .User.Name }}
{{ end }}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "index.html"), []byte(templateContent), 0644); err != nil {
		b.Fatalf("failed to write index.html: %v", err)
	}

	// Pre-calculate analysis result
	result := AnalyzeDir(tmpDir, "", DefaultConfig)
	if len(result.RenderCalls) == 0 {
		b.Fatal("Setup failed: no render calls found")
	}

	for b.Loop() {
		ValidateTemplates(result.RenderCalls, tmpDir, "")
	}
}

func BenchmarkValidateTemplateContent(b *testing.B) {
	// Benchmark purely the validation logic without file I/O if possible,
	// or minimal file I/O.
	// But ValidateTemplates reads files.
	// We can use ValidateTemplateFileStr which takes content string if we want to bypass I/O,
	// but ValidateTemplates is the public API.

	// Let's create a very complex template to stress the parser.
	complexTemplate := `
{{ $root := . }}
{{ range .Items }}
	{{ if .IsActive }}
		<div class="item">
			{{ .Name }} - {{ .Value }}
			{{ with .Details }}
				<span>{{ .Description }}</span>
				{{ if .Extra }}
					{{ range .Extra }}
						{{ . }}
					{{ end }}
				{{ end }}
			{{ end }}
		</div>
	{{ end }}
{{ end }}
`
	vars := []TemplateVar{
		{
			Name:    "Items",
			TypeStr: "[]Item",
			IsSlice: true,
			Fields: []FieldInfo{
				{Name: "Name", TypeStr: "string"},
				{Name: "Value", TypeStr: "int"},
				{Name: "IsActive", TypeStr: "bool"},
				{
					Name:    "Details",
					TypeStr: "Details",
					Fields: []FieldInfo{
						{Name: "Description", TypeStr: "string"},
						{
							Name:    "Extra",
							TypeStr: "[]string",
							IsSlice: true,
						},
					},
				},
			},
		},
	}

	registry := make(map[string]NamedTemplate)

	for b.Loop() {
		ValidateTemplateFileStr(complexTemplate, vars, "bench.html", ".", ".", registry)
	}
}
