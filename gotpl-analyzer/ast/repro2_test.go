package ast

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMapSliceElements(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

type PatientPayments struct {
	PatientName string
	BillID      int
}

type Context struct{}

func (c *Context) Render(tpl string, data map[string]any) {}

func main() {
    c := &Context{}
    groupedPayments := make(map[uint][]*PatientPayments)
    groupedPayments[1] = []*PatientPayments{{PatientName: "John", BillID: 1}}
    
	c.Render("index.html", map[string]any{
		"paymentsMap": groupedPayments,
	})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)
	if len(result.RenderCalls) == 0 {
		t.Fatal("expected at least one RenderCall")
	}

	call := result.RenderCalls[0]
	var paymentsMap *TemplateVar
	for _, v := range call.Vars {
		if v.Name == "paymentsMap" {
			vCopy := v
			paymentsMap = &vCopy
			break
		}
	}

	if paymentsMap == nil {
		t.Fatal("paymentsMap not found")
	}

	if len(paymentsMap.Fields) == 0 {
		debugJSON(t, paymentsMap)
		t.Fatalf("paymentsMap Fields is empty/nil!")
	}
    
    patientNameField := findField(paymentsMap.Fields, "PatientName")
    if patientNameField == nil {
        t.Fatalf("PatientName field not found inside paymentsMap")
    }
}
