// filepath: analyzer/ast/repro_test.go
package ast

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDeeplyNestedStructAnalysis(t *testing.T) {
	tmpDir := t.TempDir()

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
	user := User{}
	Render(nil, "index.html", map[string]interface{}{
		"User": user,
	})
}
`
	writeTestModule(t, tmpDir, mainContent)

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

	// ── Pre-flatten: inline field tree ───────────────────────────────────────
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

	bioField := findField(profileField.Fields, "Bio")
	if bioField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Bio' field on Profile struct")
	}

	addressField := findField(profileField.Fields, "Address")
	if addressField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Address' field on Profile struct")
	}

	streetField := findField(addressField.Fields, "Street")
	if streetField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'Street' field on Address struct")
	}
	_ = streetField

	cityField := findField(addressField.Fields, "City")
	if cityField == nil {
		debugJSON(t, userVar)
		t.Fatal("expected 'City' field on Address struct")
	}
	t.Logf("City field: %+v", cityField)

	// ── Post-flatten: verify the global type registry ─────────────────────────
	result.BuildTypeRegistry()

	if result.Types == nil {
		t.Fatal("Types registry must be populated after BuildTypeRegistry")
	}

	userFields, ok := result.Types["main.User"]
	if !ok {
		t.Fatal("'User' type not found in registry")
	}
	if findField(userFields, "Name") == nil {
		t.Error("registry: 'Name' field missing from User")
	}
	if findField(userFields, "Profile") == nil {
		t.Error("registry: 'Profile' field missing from User")
	}

	profileFields, ok := result.Types["main.Profile"]
	if !ok {
		t.Fatal("'Profile' type not found in registry")
	}
	if findField(profileFields, "Address") == nil {
		t.Error("registry: 'Address' field missing from Profile")
	}

	addressFields, ok := result.Types["main.Address"]
	if !ok {
		t.Fatal("'Address' type not found in registry")
	}
	if findField(addressFields, "City") == nil {
		t.Error("registry: 'City' field missing from Address")
	}

	// ── Post-Flatten: inline trees stripped, registry intact ──────────────────
	result.Flatten()

	if result.Types == nil {
		t.Fatal("Types registry must survive Flatten")
	}
	// After Flatten the inline Fields on RenderCall vars must be nil.
	for _, rc := range result.RenderCalls {
		for _, v := range rc.Vars {
			if len(v.Fields) != 0 {
				t.Errorf("after Flatten, var %q still has %d inline fields", v.Name, len(v.Fields))
			}
		}
	}
}

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
	writeTestModule(t, tmpDir, mainContent)

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

	// Registry should have all four types
	result.BuildTypeRegistry()
	for _, typeName := range []string{"main.User", "main.Profile", "main.Address", "main.City"} {
		if _, ok := result.Types[typeName]; !ok {
			t.Errorf("type %q missing from registry", typeName)
		}
	}
}

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
	writeTestModule(t, tmpDir, mainContent)

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

	mfField := findField(drugsVar.Fields, "Manufacturer")
	if mfField == nil {
		debugJSON(t, drugsVar)
		t.Fatal("Manufacturer field not found on Drug (slice element)")
	}
	if findField(mfField.Fields, "Name") == nil {
		debugJSON(t, drugsVar)
		t.Fatal("Manufacturer.Name not found")
	}

	// Registry
	result.BuildTypeRegistry()
	drugFields := result.Types["main.Drug"]
	if findField(drugFields, "Manufacturer") == nil {
		t.Error("registry: Drug.Manufacturer missing")
	}
	if result.Types["main.Manufacturer"] == nil {
		t.Error("registry: Manufacturer type missing")
	}
}

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
	writeTestModule(t, tmpDir, mainContent)

	// Must not panic or hang
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
	if findField(rootVar.Fields, "Value") == nil {
		t.Fatal("expected 'Value' field on TreeNode")
	}

	// Registry must also handle the cycle without panicking
	result.BuildTypeRegistry()
	if result.Types["main.TreeNode"] == nil {
		t.Error("registry: TreeNode type missing")
	}
}

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
	writeTestModule(t, tmpDir, mainContent)

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

	// Both must have inline fields (pre-flatten)
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

	// Post-flatten: Address appears exactly once in the registry, not twice
	result.BuildTypeRegistry()
	result.Flatten()

	addrFields, ok := result.Types["main.Address"]
	if !ok {
		t.Fatal("Address type missing from registry after Flatten")
	}
	if findField(addrFields, "Street") == nil {
		t.Error("registry Address.Street missing")
	}
	if findField(addrFields, "City") == nil {
		t.Error("registry Address.City missing")
	}
	t.Log("Same struct used for multiple variables: both have all fields; registry deduplicates")
}

// ── helpers shared across test files ─────────────────────────────────────────

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

// writeTestModule writes main.go + go.mod into tmpDir.
func writeTestModule(t *testing.T, tmpDir, mainContent string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}
	mod := "module example.com/test\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}
}
