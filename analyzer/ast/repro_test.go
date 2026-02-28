// filepath: analyzer/validator/repro_test.go
package ast

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDeeplyNestedStructAnalysis(t *testing.T) {
	// 1. Create a temporary directory for the test Go module
	tmpDir := t.TempDir()

	// 2. Create a main.go with deeply nested structs
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

// Mocking a Render function
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
	var userVar *TemplateVar
	for _, v := range call.Vars {
		if v.Name == "User" {
			userVar = &v
			break
		}
	}

	if userVar == nil {
		t.Fatal("expected 'User' variable in RenderCall")
	}

	// Level 1: User.Name, User.Profile
	nameField := findField(userVar.Fields, "Name")
	if nameField == nil {
		t.Fatal("expected 'Name' field on User struct")
	}
	if nameField.TypeStr != "string" {
		t.Errorf("expected Name.TypeStr = 'string', got %q", nameField.TypeStr)
	}

	profileField := findField(userVar.Fields, "Profile")
	if profileField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Profile' field on User struct")
	}

	// Level 2: User.Profile.Bio, User.Profile.Address
	bioField := findField(profileField.Fields, "Bio")
	if bioField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Bio' field on Profile struct (User.Profile.Bio)")
	}

	addressField := findField(profileField.Fields, "Address")
	if addressField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Address' field on Profile struct (User.Profile.Address)")
	}

	// Level 3: User.Profile.Address.Street, .City, .Zip
	streetField := findField(addressField.Fields, "Street")
	if streetField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Street' field on Address struct (User.Profile.Address.Street)")
	}

	cityField := findField(addressField.Fields, "City")
	if cityField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'City' field on Address struct (User.Profile.Address.City)")
	}

	zipField := findField(addressField.Fields, "Zip")
	if zipField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Zip' field on Address struct (User.Profile.Address.Zip)")
	}

	_ = streetField
	_ = zipField
	t.Logf("City field: %+v", cityField)
}

// TestFourLevelNesting verifies that structs nested 4 levels deep are extracted.
func TestFourLevelNesting(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import "net/http"

type City struct {
	Name    string
	ZipCode string
}

type Address struct {
	Street string
	City   City
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
	user := User{}
	Render(nil, "index.html", map[string]interface{}{
		"User": user,
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
	var userVar *TemplateVar
	for _, v := range call.Vars {
		if v.Name == "User" {
			userVar = &v
			break
		}
	}
	if userVar == nil {
		t.Fatal("'User' variable not found")
	}

	profileField := findField(userVar.Fields, "Profile")
	if profileField == nil {
		debugJSON(t, userVar)
		t.Fatal("User.Profile not found")
	}

	addressField := findField(profileField.Fields, "Address")
	if addressField == nil {
		debugJSON(t, userVar)
		t.Fatal("User.Profile.Address not found")
	}

	cityField := findField(addressField.Fields, "City")
	if cityField == nil {
		debugJSON(t, userVar)
		t.Fatal("User.Profile.Address.City not found (City struct)")
	}

	// Level 4
	cityNameField := findField(cityField.Fields, "Name")
	if cityNameField == nil {
		debugJSON(t, userVar)
		t.Fatal("User.Profile.Address.City.Name not found")
	}

	zipCodeField := findField(cityField.Fields, "ZipCode")
	if zipCodeField == nil {
		debugJSON(t, userVar)
		t.Fatal("User.Profile.Address.City.ZipCode not found")
	}

	t.Logf("4-level nesting verified: User.Profile.Address.City.Name = %+v", cityNameField)
}

// TestSliceOfStructsWithNestedFields verifies that slice element types also
// have their nested fields extracted recursively.
func TestSliceOfStructsWithNestedFields(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import "net/http"

type Manufacturer struct {
	Name    string
	Country string
}

type Drug struct {
	Name         string
	Quantity     int
	Manufacturer Manufacturer
}

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	drugs := []Drug{}
	Render(nil, "drugs.html", map[string]interface{}{
		"Drugs": drugs,
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
	var drugsVar *TemplateVar
	for _, v := range call.Vars {
		if v.Name == "Drugs" {
			drugsVar = &v
			break
		}
	}
	if drugsVar == nil {
		t.Fatal("'Drugs' variable not found")
	}
	if !drugsVar.IsSlice {
		t.Error("expected Drugs to be a slice")
	}

	// Drugs is []Drug; its Fields should be Drug's fields
	mfField := findField(drugsVar.Fields, "Manufacturer")
	if mfField == nil {
		debugJSON(t, drugsVar)
		t.Fatal("Manufacturer field not found on Drug (slice element)")
	}

	nameField := findField(mfField.Fields, "Name")
	if nameField == nil {
		debugJSON(t, drugsVar)
		t.Fatal("Manufacturer.Name not found")
	}

	countryField := findField(mfField.Fields, "Country")
	if countryField == nil {
		debugJSON(t, drugsVar)
		t.Fatal("Manufacturer.Country not found")
	}

	t.Logf("Slice element nesting verified: Drugs[].Manufacturer.Name = %+v", nameField)
}

// TestSelfReferentialStruct verifies that self-referential types don't cause
// infinite recursion.
func TestSelfReferentialStruct(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import "net/http"

type TreeNode struct {
	Value    string
	Children []*TreeNode
}

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	node := TreeNode{}
	Render(nil, "tree.html", map[string]interface{}{
		"Root": node,
	})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	// Should not panic or hang
	result := AnalyzeDir(tmpDir, "", DefaultConfig)
	if len(result.RenderCalls) == 0 {
		t.Fatal("expected at least one RenderCall")
	}

	call := result.RenderCalls[0]
	var rootVar *TemplateVar
	for _, v := range call.Vars {
		if v.Name == "Root" {
			rootVar = &v
			break
		}
	}
	if rootVar == nil {
		t.Fatal("'Root' variable not found")
	}

	valueField := findField(rootVar.Fields, "Value")
	if valueField == nil {
		t.Fatal("expected 'Value' field on TreeNode")
	}

	t.Logf("Self-referential struct handled: Root.Value = %+v", valueField)
}

// TestSameStructUsedMultipleTimes verifies that when two different top-level
// variables share the same struct type, both get their fields populated even
// though the seen map would suppress the second one if shared across variables.
func TestSameStructUsedMultipleTimes(t *testing.T) {
	tmpDir := t.TempDir()

	mainContent := `package main

import "net/http"

type Address struct {
	Street string
	City   string
}

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	home := Address{Street: "123 Home St", City: "Springfield"}
	work := Address{Street: "456 Work Ave", City: "Shelbyville"}
	Render(nil, "addresses.html", map[string]interface{}{
		"HomeAddress": home,
		"WorkAddress": work,
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

	var homeVar, workVar *TemplateVar
	for _, v := range call.Vars {
		switch v.Name {
		case "HomeAddress":
			vCopy := v
			homeVar = &vCopy
		case "WorkAddress":
			vCopy := v
			workVar = &vCopy
		}
	}

	if homeVar == nil {
		t.Fatal("'HomeAddress' variable not found")
	}
	if workVar == nil {
		t.Fatal("'WorkAddress' variable not found")
	}

	// Both variables must have Street and City fields
	if findField(homeVar.Fields, "Street") == nil {
		debugJSON(t, homeVar)
		t.Error("HomeAddress.Street not found")
	}
	if findField(homeVar.Fields, "City") == nil {
		debugJSON(t, homeVar)
		t.Error("HomeAddress.City not found")
	}

	if findField(workVar.Fields, "Street") == nil {
		debugJSON(t, workVar)
		t.Error("WorkAddress.Street not found — same struct reused but fields missing")
	}
	if findField(workVar.Fields, "City") == nil {
		debugJSON(t, workVar)
		t.Error("WorkAddress.City not found — same struct reused but fields missing")
	}

	t.Log("Same struct used for multiple variables: both have all fields")
}

func findField(fields []FieldInfo, name string) *FieldInfo {
	for _, f := range fields {
		if f.Name == name {
			return &f
		}
	}
	return nil
}

func debugJSON(t *testing.T, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	t.Logf("Debug JSON:\n%s", string(b))
}
