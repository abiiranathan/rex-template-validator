# Rex Template Validator

A powerful VSCode extension and CLI tool for validating Go templates in [Rex](https://github.com/abiiranathan/rex) applications. It ensures your templates are type-safe by analyzing your Go handlers and validating variable usage within your HTML templates.

## Features

-   **Type-Safe Validation**: Detects missing variables, undefined fields, and type mismatches in your templates.
-   **Nested Scope Support**: Correctly handles `{{ range }}`, `{{ with }}`, and `{{ if }}` scoping rules, including nested slices and structs.
-   **Partial Template Validation**: Validates `{{ template "..." }}` calls, checking for file existence and context passing.
-   **Intelligent Hover**: Hover over variables in your HTML to see their Go type and available fields.
-   **Knowledge Graph**: Visualize the relationships between your Go handlers, templates, and variables.

## Installation

### VSCode Extension

1.  Clone this repository.
2.  Open the `extension` folder in VSCode.
3.  Press `F5` to launch the Extension Development Host.
4.  Open your Rex project folder.

(Marketplace link coming soon)

### CLI Tool

You can also use the analyzer as a standalone CLI tool.

```bash
cd analyzer
go build -o rex-analyzer .
./rex-analyzer -dir /path/to/your/project -validate
```

## Usage

The extension automatically activates when you open a Go or HTML file in a workspace containing Go code.

-   **Validation**: Open an HTML template. Errors will appear in the "Problems" tab.
-   **Hover**: Hover over `{{ .VariableName }}` to see type info.
-   **Knowledge Graph**: Run the command `Rex: Show Template Knowledge Graph` to see a visualization.

## Development

### Prerequisites

-   Go 1.21+
-   Node.js & npm

### Building

Use the included build script to compile both the Go analyzer and the VSCode extension:

```bash
./build.sh
```

### Architecture

1.  **Go Analyzer (`analyzer/`)**: Parses Go source code to extract `c.Render` calls and type definitions. It performs strict validation of template variables against Go types.
2.  **VSCode Extension (`extension/`)**: 
    -   Runs the Go analyzer to get validation errors and type data.
    -   Provides editor integration (Diagnostics, Hover, Completion).
    -   Visualizes the template dependency graph.

## License

MIT © [Dr. Abiira Nathan](https://github.com/abiiranathan)
