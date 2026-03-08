package main

import (
	"testing"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

// BenchmarkColdStart profiles the absolute worst-case scenario (initial load)
// where the Go packages must be loaded from disk and parsed from scratch.
//
//	`go test -bench=BenchmarkWarmStart -benchtime=5s -cpuprofile=cpu_warm.prof -memprofile=mem_warm.prof`
func BenchmarkColdStart(b *testing.B) {
	absDir := "/home/nabiizy/Code/go/eclinichmsgo"
	templateBase := absDir
	contextFile := absDir + "/rex-content.json"
	templateRoot := "templates"

	for b.Loop() {
		// Clear the cache to force a full re-parse of the Go AST
		ast.ClearCache()

		result := ast.AnalyzeDir(absDir, contextFile, ast.DefaultConfig)
		_, _, _ = validator.ValidateTemplates(result.RenderCalls, templateBase, templateRoot)
	}
}

// BenchmarkWarmStart profiles the performance during active development
// when the Go package cache is already populated.
//
//	`go test -bench=BenchmarkColdStart -benchtime=5s -cpuprofile=cpu_cold.prof -memprofile=mem_cold.prof`
func BenchmarkWarmStart(b *testing.B) {
	absDir := "/home/nabiizy/Code/go/eclinichmsgo"
	templateBase := absDir
	contextFile := absDir + "/rex.json"
	templateRoot := "templates"

	// Run once before the timer starts to populate the cache
	ast.AnalyzeDir(absDir, contextFile, ast.DefaultConfig)
	b.ResetTimer()

	for b.Loop() {
		// We DO NOT clear the cache here.
		result := ast.AnalyzeDir(absDir, contextFile, ast.DefaultConfig)
		_, _, _ = validator.ValidateTemplates(result.RenderCalls, templateBase, templateRoot)
	}
}

// View flame graphs
// go tool pprof -http=:8080 cpu_warm.prof
// go tool pprof -http=:8081 mem_warm.prof
