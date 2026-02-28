package validator

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rex-template-analyzer/ast"
)

var validTemplateName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// IsFileBasedPartial determines if a template name refers to a file path
// rather than a named block.
//
// Detection criteria:
//   - Contains path separators (/ or \)
//   - Has a recognized template file extension
//
// Recognized extensions:
//   - .html, .htm
//   - .tmpl, .tpl
//   - .gohtml
//
// Thread-safety: Pure function, safe for concurrent calls.
func IsFileBasedPartial(name string) bool {
	// Check for path separators
	if strings.ContainsAny(name, "/\\") {
		return true
	}

	// Check for template file extensions
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".tmpl", ".gohtml", ".tpl", ".htm":
		return true
	}

	return false
}

// isWhitespace checks if a byte is whitespace (space, tab, newline, carriage return).
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// ValidateTemplateFileStr exposes internal validation for testing.
// Validates template content string with provided variables.
func ValidateTemplateFileStr(
	content string,
	vars []ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	registry map[string][]NamedBlockEntry,
) []ValidationResult {
	varMap := buildVarMap(vars)
	return ValidateTemplateContent(content, varMap, templateName, baseDir, templateRoot, 1, registry)
}

// ParseAllNamedTemplates exposes named template parsing for testing.
func ParseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	return parseAllNamedTemplates(baseDir, templateRoot)
}

// ExtractNamedTemplatesFromContent exposes content extraction for testing.
func ExtractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry map[string][]NamedBlockEntry) {
	extractNamedTemplatesFromContent(content, absolutePath, templatePath, registry)
}
