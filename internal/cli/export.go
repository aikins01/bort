package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
)

func runExport(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var manifestPath string
	var outputDir string
	var appName string
	var includeEnvValues bool

	fs.StringVar(&manifestPath, "manifest", "", "migration manifest path")
	fs.StringVar(&outputDir, "output-dir", "bort-bundle", "directory to write the migration bundle into")
	fs.StringVar(&appName, "app", "", "optional app name to export")
	fs.BoolVar(&includeEnvValues, "include-env-values", false, "write known environment values into private env files")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if manifestPath == "" {
		return fmt.Errorf("--manifest is required")
	}

	file, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer file.Close()

	var m manifest.Manifest
	if err := json.NewDecoder(file).Decode(&m); err != nil {
		return err
	}

	summary, err := exporter.Export(m, exporter.Options{OutputDir: outputDir, AppName: appName, IncludeEnvValues: includeEnvValues})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "exported %d app bundle(s) to %s\n", len(summary.Apps), summary.OutputDir)
	return nil
}
