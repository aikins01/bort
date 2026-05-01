package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aikins01/bort/internal/source"
	"github.com/aikins01/bort/internal/source/localdocker"
)

func runScan(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var sourceName string
	var outputPath string
	var format string
	var includeEnvValues bool

	fs.StringVar(&sourceName, "source", "docker", "source adapter to scan: docker")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&format, "format", "json", "output format: json")
	fs.BoolVar(&includeEnvValues, "include-env-values", false, "include environment variable values in the manifest")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if format != "json" {
		return fmt.Errorf("unsupported scan format %q", format)
	}

	scanner, err := scannerFor(sourceName)
	if err != nil {
		return err
	}

	result, err := scanner.Scan(ctx, source.ScanOptions{IncludeEnvValues: includeEnvValues})
	if err != nil {
		return err
	}

	var out io.Writer = stdout
	var file *os.File
	if outputPath != "-" {
		file, err = os.Create(outputPath)
		if err != nil {
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

func scannerFor(name string) (source.Scanner, error) {
	switch name {
	case "docker", "local-docker":
		return localdocker.NewScanner(), nil
	default:
		return nil, fmt.Errorf("unsupported source %q", name)
	}
}
