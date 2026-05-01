package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const version = "0.1.0-dev"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return printHelp(stdout)
	}

	switch args[0] {
	case "help", "--help", "-h":
		return printHelp(stdout)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "scan":
		return runScan(ctx, args[1:], stdout, stderr)
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr)
	case "export":
		return runExport(ctx, args[1:], stdout, stderr)
	case "validate":
		return runValidate(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], strings.TrimSpace(usage()))
	}
}

func printHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, usage())
	return err
}

func usage() string {
	return `bort migrates self-hosted apps between PaaS platforms.

Usage:
  bort scan [flags]
  bort plan [flags]
  bort export [flags]
  bort validate [flags]
  bort version

Commands:
  scan      discover local resources and write a migration manifest
  plan      summarize migration readiness from a manifest
  export    write an inspectable local migration bundle
  validate  validate an exported migration bundle
  version   print the CLI version

Run "bort <command> -h" for command-specific flags.
`
}
