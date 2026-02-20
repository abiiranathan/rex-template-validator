1. Fix Live Validation & Rerun on Block Edit
Issue: The validation wouldn't trigger as you were typing, requiring a manual save. Additionally, editing partial blocks lost validation context because the validator's root context was not cleanly passed down during file re-validation.
Fixes:
- Swapped VS Code's onDidSaveTextDocument (for templates) to onDidChangeTextDocument so validation now evaluates live as you type. It leverages the existing debounce to ensure performance is not affected.
- In validator.ts, validateDocument was overriding the explicitly detected context from extension.ts that handled partial template fallbacks. It now natively accepts the correctly resolved TemplateContext and correctly applies diagnostics without throwing a missing context error. 
2. Add Hover Docs and Go-to-Definition for Defines & Blocks
Issue: Variables had hover definitions and go-to-definition mapping out to Go files, but partial/block calls explicitly lacked define/block hover/navigation.
Fixes:
- Updated the Hover Provider to detect when hovering over a template call {{ template "..." }}, {{ block "..." }}, or {{ define "..." }} block keyword. It now correctly identifies the template block name and returns Markdown hover documentation.
- Extensively modified the Go To Definition logic by adding findNamedBlockDefinition. When Go to Definition is clicked on a template "myBlock" partial, it will now prioritize the active file's AST and globally grep across all loaded files inside the KnowledgeGraphBuilder mapping to jump precisely to where the {{ define "myBlock" }} or {{ block "myBlock" }} is placed. I also included a fallback so any unsaved block edits update live.