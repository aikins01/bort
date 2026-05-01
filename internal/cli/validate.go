package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/aikins01/bort/internal/validator"
)

func runValidate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var appName string
	var format string

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&appName, "app", "", "optional app name to validate")
	fs.StringVar(&format, "format", "text", "output format: text, json")

	if err := fs.Parse(args); err != nil {
		return err
	}

	result, err := validator.Validate(ctx, validator.Options{BundleDir: bundleDir, AppName: appName})
	if err != nil {
		return err
	}

	switch format {
	case "text":
		writeValidationText(stdout, result)
	case "json":
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported validate format %q", format)
	}

	if result.Status == validator.StatusRed {
		return fmt.Errorf("bundle validation failed")
	}
	return nil
}

func writeValidationText(w io.Writer, result validator.Result) {
	fmt.Fprintf(w, "Bundle: %s\n", result.BundleDir)
	fmt.Fprintf(w, "Status: %s\n\n", result.Status)

	for _, app := range result.Apps {
		fmt.Fprintf(w, "[%s] %s\n", app.Status, app.Name)
		if len(app.Issues) == 0 {
			fmt.Fprintln(w, "  no issues")
		} else {
			for _, issue := range app.Issues {
				fmt.Fprintf(w, "  %s %s: %s\n", issue.Severity, issue.Code, issue.Message)
			}
		}
		fmt.Fprintln(w)
	}
}
