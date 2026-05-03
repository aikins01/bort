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
		advanced := len(args) > 1 && (args[1] == "--advanced" || args[1] == "-a")
		return printHelp(stdout, advanced)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "env":
		return runEnv(ctx, args[1:], stdout, stderr)
	case "data":
		return runData(ctx, args[1:], stdout, stderr)
	case "migrate":
		return runMigrate(ctx, args[1:], stdout, stderr)
	case "rollback":
		return runRollback(ctx, args[1:], stdout, stderr)
	case "commit":
		return runCommit(ctx, args[1:], stdout, stderr)
	// power-user / advanced verbs (still wired so existing tests and scripts keep working).
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
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "continue":
		return runContinue(ctx, args[1:], stdin, stdout, stderr)
	case "next":
		return runNext(ctx, args[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], strings.TrimSpace(usage(false)))
	}
}

func printHelp(w io.Writer, advanced bool) error {
	_, err := fmt.Fprint(w, usage(advanced))
	return err
}

func usage(advanced bool) string {
	if advanced {
		return advancedUsage()
	}
	return `bort migrates self-hosted apps between PaaS platforms.

Usage:
  bort                              scan and show app-first migration status
  bort env <app> KEY=value ...      record env values for an app in .bort/state.json
  bort data <app> <store> --recreate|--migrate|--managed
                                    record a data store strategy in .bort/state.json
  bort migrate                      create/update local dry-run migration artifacts
  bort rollback                     plan a rollback to the source
  bort commit                       plan final target acceptance

Other:
  bort help [--advanced]            show this help (advanced lists power-user verbs)
  bort version                      print the version
`
}

func advancedUsage() string {
	return `bort migrates self-hosted apps between PaaS platforms.

Primary:
  bort                start or resume a migration (linear, app-first)
  bort env            record env values for an app in .bort/state.json
  bort data           record a data store strategy in .bort/state.json
  bort migrate        create/update local dry-run migration artifacts
  bort rollback       plan a rollback to the source
  bort commit         plan final target acceptance

Power-user pipeline (each step is local and dry-run only):
  bort scan      discover local resources and write a migration manifest
  bort plan      summarize migration readiness from a manifest
  bort export    write an inspectable local migration bundle
  bort validate  validate an exported migration bundle
  bort prepare   plan target resources from an exported bundle
  bort sync      plan state sync work from prepared target resources
  bort cutover   plan route cutover and rollback

Scripting helpers (rarely needed; the linear cockpit shows the same info):
  bort status    print a run summary as text
  bort continue  reopen the migration cockpit
  bort next      print the next safe action for a run

Run "bort <command> -h" for command-specific flags.
`
}
