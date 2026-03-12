# Go Template LSP (Language Server Protocol)

Bring the power of Go's static typing directly into your HTML templates. 

**Go Template LSP** is a powerful VS Code extension and standalone Go analyzer that provides real-time validation, rich IntelliSense, and seamless navigation for Go `html/template` and `text/template` files.

If you've ever been frustrated by discovering template typos, missing variables, or type mismatches only *after* compiling and running your server, this extension is for you.

## 🚀 Key Features

*   **Framework Agnostic**: Works out-of-the-box with any Go web framework (Fiber, Echo, Gin, Chi, standard library) as long as you use standard render patterns (e.g., `Render(name, data)` or `ExecuteTemplate(wr, name, data)`).
*   **Live Type-Safe Validation**: Detects missing variables, undefined struct fields, and type mismatches as you type. No more runtime panics for missing fields!
*   **Rich IntelliSense & Hover**: Hover over any `{{ .Variable }}` in your HTML to see its underlying Go type, documentation, and available fields. Trigger autocomplete to explore nested structs, maps, and slices.
*   **Deep Scope Understanding**: Correctly tracks the `.` context through `{{ range }}`, `{{ with }}`, and `{{ if }}` blocks. Fully supports local variable assignments (`{{ $v := . }}`).
*   **Cross-File Block Resolution**: Fully understands `{{ define "name" }}` and `{{ block "name" . }}` across your entire project. It aggregates context from all call sites to provide accurate autocompletion inside shared partials.
*   **Function & Method Support**: Understands both built-in template functions (`len`, `index`, `dict`, `html`, etc.) and your custom Go `FuncMap` injections.
*   **Seamless Navigation**: 
    *   **Go to Definition**: Jump directly from a template variable to its Go struct definition, or from a `{{ template "name" }}` call to the file where it's defined.
    *   **Go to Render Call**: Right-click any template to instantly jump to the Go handler(s) that render it.
    *   **Find References**: Find everywhere a specific template or block is used across your project.
*   **Knowledge Graph**: Visualize the relationships between your Go handlers, templates, and injected variables via an interactive UI panel.

## 🛠 Usage

The extension automatically activates when you open a Go or HTML/TMPL file in a workspace containing Go code. 

### Available Commands
Open the Command Palette (`Ctrl+Shift+P` / `Cmd+Shift+P`) and type `GoTpl`:

*   **GoTpl: Rebuild Template Index**: Forces a full re-analysis of your Go source code to discover new handlers, templates, and types.
*   **GoTpl: Validate Current Template**: Manually triggers validation for the active file.
*   **GoTpl: Show Template Knowledge Graph**: Opens a side panel showing all templates, which Go files render them, and exactly what variables are passed to them.
*   **GoTpl: Go to Render Call**: Jumps from the current template directly to the Go handler that renders it (also available via Right-Click Context Menu).

## ⚙️ Configuration

You can customize the extension via your `settings.json`:

```json
{
  "gotpl.goAnalyzerPath": "",         // Path to the gotpl analyzer binary. Leave empty to use the bundled binary.
  "gotpl.sourceDir": ".",             // Directory containing your Go source code
  "gotpl.templateRoot": "views",      // Subdirectory where your templates live
  "gotpl.debounceMs": 500,            // Delay before live validation triggers
  "gotpl.validate": true              // Toggle live diagnostics on/off
}
```

## 💻 CLI Tool

The core engine is written in Go and can be used in CI/CD pipelines as a standalone CLI tool to enforce template safety:

```bash
cd analyzer
go build -o gotpl-analyzer .
./gotpl-analyzer -dir /path/to/your/project -validate
```

Available flags:
```txt
Usage of ./rex-analyzer:
  -compress
    	Output gzip-compressed JSON
  -context-file string
    	Path to JSON file with additional context variables
  -daemon
    	Run as a long-lived JSON-RPC daemon over stdio
  -dir string
    	Go source directory to analyze (default ".")
  -named-templates
    	Return all named template as JSON
  -template-base-dir string
    	Base directory for template-root
  -template-root string
    	Root directory for templates
  -validate
    	Validate templates against render calls
  -view-context string
    	Show context for a specific template

```

## 🏗 Development & Building

### Prerequisites
*   Go 1.25+
*   Node.js & npm

### Build Instructions
Use the included build script to compile both the Go analyzer binary and the VS Code extension:

```bash
./build.sh
```

To debug in VS Code:
1. Open the `extension` folder.
2. Press `F5` to launch the Extension Development Host.

## 📄 License

MIT © [Dr. Abiira Nathan](https://github.com/abiiranathan)

# Technical Architecture: Go Template LSP

The Go Template LSP bridges the gap between statically typed Go source code and dynamically evaluated Go templates. To achieve high performance, live diagnostics, and deep AST understanding, the project is split into two communicating components: a **Go Analyzer Daemon** and a **TypeScript VS Code Extension**.

## 1. System Overview

```text
┌───────────────────────┐          JSON-RPC via stdin/stdout         ┌────────────────────────┐
│                       │ ◄───────────────────────────────────────── │                        │
│  TypeScript Extension │                                            │   Go Analyzer Daemon   │
│  (VS Code UI, Cache,  │ ─────────────────────────────────────────► │   (Go AST parsing,     │
│   Knowledge Graph)    │                                            │    Type extraction)    │
└───────────────────────┘                                            └────────────────────────┘
```

## 2. The Go Analyzer (Backend)

Written in Go, the analyzer uses the `go/ast`, `go/parser`, and `go/types` packages to deeply understand the user's Go project.

### Responsibilities
*   **Render Call Extraction**: Walks the Go AST looking for calls to `c.Render()`, `ExecuteTemplate()`, etc. It identifies the string argument (the template name) and the data argument (the context).
*   **Type Resolution**: When a data argument is passed to a template, the analyzer resolves its exact underlying Go type (Structs, Maps, Slices). It extracts field names, types, method signatures, and GoDoc comments.
*   **FuncMap Extraction**: Automatically discovers custom template functions injected via `template.FuncMap` and extracts their parameter and return types.
*   **Daemon Mode**: Instead of cold-booting for every keystroke, the analyzer runs as a persistent daemon. It listens over `stdin` for JSON-RPC requests from VS Code, allowing it to validate modified template strings in memory in milliseconds.

## 3. The TypeScript Extension (Client/LSP)

The extension provides the Language Server Protocol (LSP) features directly to VS Code. 

### Core Components

#### `KnowledgeGraphBuilder`
The central brain of the extension. During startup, it asks the Go Daemon to index the workspace. It builds an in-memory graph connecting:
*   **Go Handlers** -> **Templates**
*   **Templates** -> **Context Variables**
*   **Named Blocks** (`{{ define }}` / `{{ block }}`) -> **Call Sites** (`{{ template }}`)

This graph allows the extension to know *exactly* what variables are available inside a partial template based on how it was called in a completely different file.

#### `TemplateParser`
A custom recursive-descent parser written in TypeScript that generates an Abstract Syntax Tree (AST) of the HTML/Go-Template files. 
Unlike standard regex-based parsers, this parser understands nesting. It knows that a variable inside `{{ range .Users }}` belongs to a child scope where `.` represents a `User` struct.

#### `TypeInferencer` (`expressionParser.ts`)
A sophisticated type-inference engine for Go template syntax. If a user types `{{ (index .Users 0).GetProfile.Avatar }}`, the inferencer:
1. Resolves `.Users` to `[]User`.
2. Evaluates the `index` built-in function to extract the element type `User`.
3. Resolves the `.GetProfile` method, looking up its return type `Profile`.
4. Resolves the `.Avatar` field on the `Profile` struct, determining it is a `string`.

It handles map lookups, slice indexing, pointer dereferencing, custom `FuncMap` calls, and complex pipelines (`.Count | add 5 | printf "%d"`).

#### `ScopeUtils`
Handles the dynamic nature of the `.` (dot) context in Go templates. It traverses the AST downwards to the cursor position, pushing and popping `ScopeFrame` objects. It accurately tracks:
*   Local variable assignments (`{{ $val := .Name }}`).
*   Range loops with keys and values (`{{ range $k, $v := .Map }}`).
*   Context overrides (`{{ with .NestedStruct }}`).

### 4. How Language Features Work

*   **Live Diagnostics (Squiggly Lines)**: On every keystroke (debounced), the TS extension sends the current document text to the Go Daemon. The daemon parses the template, validates field accesses against the extracted Go types, and returns a list of errors.

*   **Hover & Autocomplete**: 
    1. VS Code asks for hover info at `line: 10, col: 15`.
    2. The `ScopeUtils` walks the AST to find the exact node and calculates the active scope stack.
    3. The `TypeInferencer` evaluates the path typed so far.
    4. The extension looks up the resulting type in the `KnowledgeGraph` and formats the fields, methods, and documentation into a rich Markdown tooltip or a list of Completion Items.
*   **Go to Definition**: 
    1. Resolves the type at the cursor.
    2. Looks up the `defFile`, `defLine`, and `defCol` metadata extracted by the Go Analyzer.
    3. Opens the original `.go` source file right to the struct field definition.

## 5. Notable Technical Challenges Solved

*   **Cross-File Block Context**: In Go, `{{ define "header" }}` rarely has its own context. It inherits context from wherever `{{ template "header" . }}` is called. The `KnowledgeGraph` parallel-scans all templates to find call sites, merges the context variables passed to them, and artificially injects that context into the `define` block for accurate intellisense.
*   **The `dict` Pattern**: A common workaround in Go templates is passing multiple variables via a map function: `{{ template "x" (dict "User" .User "Count" 5) }}`. The `TypeInferencer` natively understands the `dict` built-in, creating on-the-fly anonymous structs so autocomplete works flawlessly inside the child template.
*   **Automatic Pointer Dereferencing**: Go templates automatically dereference pointers (e.g., `*User` -> `User`). The type inferencer mimics this behavior, aggressively stripping `*` prefixes during path resolution so nested field lookups don't fail.

