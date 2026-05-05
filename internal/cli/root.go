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
	case "version", "--version":
		fmt.Fprintf(stdout, "bort %s\n", version)
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
	case "cleanup":
		return runCleanup(ctx, args[1:], stdout, stderr)
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
	case "next":
		return runNext(ctx, args[1:], stdout, stderr)
	case "init-target":
		return runInitTarget(ctx, args[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q (run `bort help` for usage)", args[0])
	}
}

func printHelp(w io.Writer, advanced bool) error {
	st := newStyler(w)
	fmt.Fprintln(w, st.emph("bort")+" "+st.muted("— migrate self-hosted apps between PaaS platforms"))
	fmt.Fprintln(w)
	if advanced {
		writeAdvancedHelp(w, st)
	} else {
		writePrimaryHelp(w, st)
	}
	return nil
}

func writePrimaryHelp(w io.Writer, st *styler) {
	writeHelpSection(w, st, "Usage:", []helpLine{
		{verb: "bort", desc: "scan and show app-first migration status"},
		{verb: "bort env <app> KEY=value ...", desc: "record env values for an app in .bort/state.json"},
		{verb: "bort data <app> <store> --recreate|--migrate|--managed", desc: "record a data store strategy in .bort/state.json"},
		{verb: "bort migrate", desc: "create/update local dry-run migration artifacts"},
		{verb: "bort rollback", desc: "plan a rollback to the source"},
		{verb: "bort commit", desc: "plan final target acceptance"},
		{verb: "bort cleanup", desc: "inventory leftovers and remove safe stale Dokploy metadata"},
		{verb: "bort init-target dokploy", desc: "bootstrap a dokploy admin + api key from coolify credentials"},
	})
	writeHelpSection(w, st, "Other:", []helpLine{
		{verb: "bort help [--advanced]", desc: "show this help (advanced lists power-user verbs)"},
		{verb: "bort version", desc: "print the version"},
	})
}

func writeAdvancedHelp(w io.Writer, st *styler) {
	writeHelpSection(w, st, "Primary:", []helpLine{
		{verb: "bort", desc: "start or resume a migration (linear, app-first)"},
		{verb: "bort env", desc: "record env values for an app in .bort/state.json"},
		{verb: "bort data", desc: "record a data store strategy in .bort/state.json"},
		{verb: "bort migrate", desc: "create/update local dry-run migration artifacts"},
		{verb: "bort rollback", desc: "plan a rollback to the source"},
		{verb: "bort commit", desc: "plan final target acceptance"},
		{verb: "bort cleanup", desc: "dry-run leftover cleanup; --apply only removes safe Dokploy metadata"},
	})
	writeHelpSection(w, st, "Power-user pipeline (each step is local and dry-run only):", []helpLine{
		{verb: "bort scan", desc: "discover local resources and write a migration manifest"},
		{verb: "bort plan", desc: "summarize migration readiness from a manifest"},
		{verb: "bort export", desc: "write an inspectable local migration bundle"},
		{verb: "bort validate", desc: "validate an exported migration bundle"},
		{verb: "bort prepare", desc: "plan target resources from an exported bundle"},
		{verb: "bort sync", desc: "plan state sync work from prepared target resources"},
		{verb: "bort cutover", desc: "plan route cutover and rollback"},
	})
	writeHelpSection(w, st, "Scripting helpers (rarely needed; the linear status view shows the same info):", []helpLine{
		{verb: "bort status", desc: "print a run summary as text"},
		{verb: "bort next", desc: "print the next safe action for a run"},
	})
	fmt.Fprintln(w, st.muted(`Run "bort <command> -h" for command-specific flags.`))
}

type helpLine struct {
	verb string
	desc string
}

func writeHelpSection(w io.Writer, st *styler, heading string, lines []helpLine) {
	fmt.Fprintln(w, st.emph(heading))
	width := 0
	for _, l := range lines {
		if len(l.verb) > width {
			width = len(l.verb)
		}
	}
	for _, l := range lines {
		padding := strings.Repeat(" ", width-len(l.verb)+2)
		fmt.Fprintf(w, "  %s%s%s\n", st.emph(l.verb), padding, st.muted(l.desc))
	}
	fmt.Fprintln(w)
}
