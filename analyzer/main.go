package main

import (
	"encoding/json"
	"flag"
	"os"

	"github.com/rex-template-analyzer/validator"
)

func main() {
	dir := flag.String("dir", ".", "Go source directory to analyze")
	validate := flag.Bool("validate", false, "Validate templates against render calls")
	flag.Parse()

	result := validator.AnalyzeDir(*dir)

	if *validate {
		// Validate templates
		validationErrors := validator.ValidateTemplates(result.RenderCalls, *dir)
		output := struct {
			RenderCalls      []validator.RenderCall       `json:"renderCalls"`
			ValidationErrors []validator.ValidationResult `json:"validationErrors"`
			Errors           []string                     `json:"errors"`
		}{
			RenderCalls:      result.RenderCalls,
			ValidationErrors: validationErrors,
			Errors:           result.Errors,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(output)
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	}
}
