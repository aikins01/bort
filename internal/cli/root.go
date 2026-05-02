package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

const version = "0.1.0-dev"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return RunWithInput(ctx, args, os.Stdin, stdout, stderr)
}

func RunWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return runGuide(ctx, stdin, stdout, stderr)
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
	case "prepare":
		return runPrepare(ctx, args[1:], stdout, stderr)
	case "sync":
		return runSync(ctx, args[1:], stdout, stderr)
	case "cutover":
		return runCutover(ctx, args[1:], stdout, stderr)
	case "rollback":
		return runRollback(ctx, args[1:], stdout, stderr)
	case "commit":
		return runCommit(ctx, args[1:], stdout, stderr)
	case "migrate":
		return runMigrate(ctx, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "continue":
		return runContinue(ctx, args[1:], stdout, stderr)
	case "next":
		return runNext(ctx, args[1:], stdout, stderr)
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
  bort
  bort scan [flags]
  bort plan [flags]
  bort export [flags]
  bort validate [flags]
  bort prepare [flags]
  bort sync [flags]
  bort cutover [flags]
  bort rollback [flags]
  bort commit [flags]
  bort migrate [flags]
  bort status --run <name-or-path>
  bort continue [--run <name-or-path>]
  bort next [--run <name-or-path>]
  bort version

Commands:
  bort      resume the latest run, or start a dry-run from bort-bundle
  scan      discover local resources and write a migration manifest
  plan      summarize migration readiness from a manifest
  export    write an inspectable local migration bundle
  validate  validate an exported migration bundle
  prepare   plan target resources from an exported bundle without mutating them
  sync      plan state sync work from prepared target resources without mutating them
  cutover   plan route cutover and rollback without mutating them
  rollback  plan route rollback without mutating routes
  commit    plan final target acceptance without retiring source resources
  migrate   create a local dry-run migration run with persisted artifacts
  status    summarize a local migration run without recomputing plans
  continue  show the migration cockpit and next safe action
  next      print the next safe action, or reopen the latest cockpit with no args
  version   print the CLI version

Run "bort <command> -h" for command-specific flags.
`
}
