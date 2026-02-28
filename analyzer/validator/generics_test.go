// filepath: analyzer/validator/generics_test.go
package validator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenericsAnalysis(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import "net/http"

type User struct {
	Name string
}

type Order struct {
	ID string
}

// Page is a generic pagination container.
type Page[T any] struct {
	Items      []T
	TotalCount int
}

// HasNext checks if there is a next page.
func (p *Page[T]) HasNext() bool {
	return false
}

// NestedGeneric tests generics inside generics
type Response[T any] struct {
	Data T
	Code int
}

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	userPage := Page[User]{
		Items:      []User{{Name: "Alice"}},
		TotalCount: 1,
	}

	orderPage := Page[Order]{
		Items:      []Order{{ID: "ORD-123"}},
		TotalCount: 1,
	}

	nested := Response[Page[User]]{
		Data: userPage,
		Code: 200,
	}

	Render(nil, "index.html", map[string]interface{}{
		"Users":  userPage,
		"Orders": orderPage,
		"Nested": nested,
	})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}

	goModContent := `module example.com/test
go 1.21
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	result := AnalyzeDir(tmpDir, "", DefaultConfig)

	if len(result.RenderCalls) == 0 {
		t.Fatal("expected at least one RenderCall, got 0")
	}

	call := result.RenderCalls[0]

	var usersVar, ordersVar, nestedVar *TemplateVar
	for _, v := range call.Vars {
		switch v.Name {
		case "Users":
			vCopy := v
			usersVar = &vCopy
		case "Orders":
			vCopy := v
			ordersVar = &vCopy
		case "Nested":
			vCopy := v
			nestedVar = &vCopy
		}
	}

	if usersVar == nil || ordersVar == nil || nestedVar == nil {
		t.Fatal("missing expected variables in RenderCall")
	}

	// 1. Verify Type String Formatting
	if usersVar.TypeStr != "Page[User]" {
		t.Errorf("expected Users type to be 'Page[User]', got %q", usersVar.TypeStr)
	}
	if ordersVar.TypeStr != "Page[Order]" {
		t.Errorf("expected Orders type to be 'Page[Order]', got %q", ordersVar.TypeStr)
	}
	if nestedVar.TypeStr != "Response[Page[User]]" {
		t.Errorf("expected Nested type to be 'Response[Page[User]]', got %q", nestedVar.TypeStr)
	}

	// 2. Verify Cache Collision Fix (Page[User] vs Page[Order])
	usersItems := findField(usersVar.Fields, "Items")
	if usersItems == nil {
		t.Fatal("expected 'Items' field on Page[User]")
	}
	if usersItems.TypeStr != "[]User" {
		t.Errorf("expected Page[User].Items type to be '[]User', got %q", usersItems.TypeStr)
	}
	if findField(usersItems.Fields, "Name") == nil {
		t.Error("expected Page[User].Items to contain 'Name' field from User struct")
	}

	ordersItems := findField(ordersVar.Fields, "Items")
	if ordersItems == nil {
		t.Fatal("expected 'Items' field on Page[Order]")
	}
	if ordersItems.TypeStr != "[]Order" {
		t.Errorf("expected Page[Order].Items type to be '[]Order', got %q", ordersItems.TypeStr)
	}
	if findField(ordersItems.Fields, "ID") == nil {
		t.Error("expected Page[Order].Items to contain 'ID' field from Order struct")
	}

	// 3. Verify Generic Method Extraction
	usersHasNext := findField(usersVar.Fields, "HasNext")
	if usersHasNext == nil {
		t.Fatal("expected 'HasNext' method on Page[User]")
	}
	if usersHasNext.TypeStr != "method" {
		t.Errorf("expected HasNext to be a method, got %q", usersHasNext.TypeStr)
	}

	if usersHasNext.Doc != "HasNext checks if there is a next page.\n" {
		t.Errorf("expected method doc to be extracted, got %q", usersHasNext.Doc)
	}

	// 4. Verify Nested Generics
	nestedData := findField(nestedVar.Fields, "Data")
	if nestedData == nil {
		t.Fatal("expected 'Data' field on Response[Page[User]]")
	}
	if nestedData.TypeStr != "Page[User]" {
		t.Errorf("expected nested Data type to be 'Page[User]', got %q", nestedData.TypeStr)
	}

	nestedDataItems := findField(nestedData.Fields, "Items")
	if nestedDataItems == nil || nestedDataItems.TypeStr != "[]User" {
		t.Errorf("expected nested Data.Items to be '[]User'")
	}
}
