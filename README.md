# Go Template LSP


A powerful VS Code extension and CLI tool for validating Go templates in any Go web application. It works with any framework as long as your code uses a `Render` function (or method) or `ExecuteTemplate` with a template name (string) as the first argument and a map (such as `map[string]interface{}` or `map[string]string`) as the second argument. The extension analyzes your Go handlers and validates variable usage within your HTML templates, including support for variables and map lookups of template names.

## Features

 - **Works with Any Go Framework**: As long as your code uses a `Render(template, map)` pattern, this extension will analyze and validate your templates.
- **Type-Safe Validation**: Detects missing variables, undefined fields, and type mismatches in your templates.
- **Nested Scope Support**: Correctly handles `{{ range }}`, `{{ with }}`, and `{{ if }}` scoping rules, including nested slices and structs.
- **Partial Template Validation**: Validates `{{ template "..." }}` calls, checking for file existence and context passing.
- **Block/Define Template Validation**: Aggregates errors across all call sites—errors are only reported if all usages are invalid.
- **Variable and map[string]string Lookups**: Supports both direct variable access and template lookups via `map[string]string` or similar patterns.
- **Intelligent Hover**: Hover over variables in your HTML to see their Go type and available fields.
- **Go to Definition**: Jump from template variables to their Go struct definitions.
- **Autocomplete**: Get context-aware completions for variables and fields in templates.
- **Knowledge Graph**: Visualize the relationships between your Go handlers, templates, and variables with the `Go Template LSP: Show Template Knowledge Graph` command.
- **Live Diagnostics**: Errors and warnings appear instantly in the Problems panel as you edit.
- **Custom Context Support**: Supports additional context variables via JSON files.
- **Highly Configurable**: Control analyzer path, debounce, template roots, and more.
1.  Open the `extension` folder in VSCode.
2.  Press `F5` to launch the Extension Development Host.
3.  Open your go project folder.

(Marketplace link coming soon)

### CLI Tool

You can also use the analyzer as a standalone CLI tool.

```bash
cd analyzer
go build -o gotpl-analyzer .
./gotpl-analyzer -dir /path/to/your/project -validate
```

## Usage

The extension automatically activates when you open a Go or HTML file in a workspace containing Go code.

-   **Validation**: Open an HTML template. Errors will appear in the "Problems" tab.
-   **Hover**: Hover over `{{ .VariableName }}` to see type info.
-   **Knowledge Graph**: Run the command `Go Template LSP: Show Template Knowledge Graph` to see a visualization.

## Development

### Prerequisites

-   Go 1.25+
-   Node.js & npm

### Building

Use the included build script to compile both the Go analyzer and the VSCode extension:

```bash
./build.sh
```

### Architecture

1.  **Go Analyzer (`analyzer/`)**: Parses Go source code to extract `c.Render`  and `c.ExecuteTemplate`  calls and type definitions. It performs strict validation of template variables against Go types.
2.  **VSCode Extension (`extension/`)**: 
    -   Runs the Go analyzer to get validation errors and type data.
    -   Provides editor integration (Diagnostics, Hover, Completion).
    -   Visualizes the template dependency graph.

## License

MIT © [Dr. Abiira Nathan](https://github.com/abiiranathan)
