package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aikins01/bort/internal/source"
	"github.com/aikins01/bort/internal/source/coolify"
	"github.com/aikins01/bort/internal/source/localdocker"
)

func runScan(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var sourceName string
	var outputPath string
	var format string
	var includeEnvValues bool
	var coolifyURL string

	fs.StringVar(&sourceName, "source", "docker", "source adapter to scan: docker, coolify")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&format, "format", "json", "output format: json")
	fs.BoolVar(&includeEnvValues, "include-env-values", false, "include environment variable values in the manifest")
	fs.StringVar(&coolifyURL, "coolify-url", os.Getenv("BORT_COOLIFY_URL"), "Coolify base URL, or BORT_COOLIFY_URL")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if format != "json" {
		return fmt.Errorf("unsupported scan format %q", format)
	}

	scanOptions := source.ScanOptions{
		IncludeEnvValues: includeEnvValues,
		Coolify: source.CoolifyOptions{
			BaseURL: coolifyURL,
			Token:   os.Getenv("BORT_COOLIFY_TOKEN"),
		},
	}

	scanner, err := scannerFor(sourceName, scanOptions)
	if err != nil {
		return err
	}

	result, err := scanner.Scan(ctx, scanOptions)
	if err != nil {
		return err
	}

	var out io.Writer = stdout
	var file *os.File
	if outputPath != "-" {
		file, err = os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return err
		}
		defer file.Close()
		out = file
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return err
	}

	if outputPath != "-" {
		fmt.Fprintf(stderr, "wrote migration manifest to %s\n", outputPath)
	}

	return nil
}

func scannerFor(name string, opts source.ScanOptions) (source.Scanner, error) {
	switch name {
	case "docker", "local-docker":
		return localdocker.NewScanner(), nil
	case "coolify":
		return coolify.NewScanner(opts.Coolify.BaseURL, opts.Coolify.Token)
	default:
		return nil, fmt.Errorf("unsupported source %q", name)
	}
}
