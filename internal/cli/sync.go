package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	syncplan "github.com/aikins01/bort/internal/sync"
)

func runSync(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var format string
	var outputPath string
	var preparePlanPath string

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to sync")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&preparePlanPath, "from-prepare", "", "read a prior prepare JSON plan artifact")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkOutputFormat("sync", format); err != nil {
		return err
	}

	var result syncplan.Result
	if preparePlanPath != "" {
		expect := artifactExpectations{AppName: appName}
		if flagWasSet(fs, "bundle") {
			expect.BundleDir = bundleDir
		}
		if flagWasSet(fs, "target") {
			expect.Target = target
		}
		preparePlan, err := readPrepareArtifact(preparePlanPath, expect)
		if err != nil {
			return err
		}
		result = syncplan.PlanFromPrepare(preparePlan)
	} else {
		var err error
		result, err = syncplan.Plan(syncplan.Options{BundleDir: bundleDir, Target: target, AppName: appName})
		if err != nil {
			return err
		}
	}

	return writeFormattedOutput(stdout, outputPath, format, result, writeSyncText)
}

func writeSyncText(w io.Writer, result syncplan.Result) {
	fmt.Fprintf(w, "Sync plan: %s -> %s\n", result.BundleDir, result.Target)
	fmt.Fprintf(w, "Status: %s\n\n", result.Status)

	for _, app := range result.Apps {
		fmt.Fprintf(w, "[%s] %s\n", app.Status, app.Name)
		fmt.Fprintf(w, "  readiness: %s\n", app.Readiness)
		fmt.Fprintf(w, "  prepare readiness: %s\n", app.PrepareReadiness)
		if len(app.Gates) > 0 {
			fmt.Fprintln(w, "  gates:")
			for _, gate := range app.Gates {
				fmt.Fprintf(w, "    %s %s: %s\n", gate.Severity, gate.Code, gate.Message)
			}
		}
		if len(app.Steps) > 0 {
			fmt.Fprintln(w, "  steps:")
			for _, step := range app.Steps {
				fmt.Fprintf(w, "    %s %s %s: %s", step.Readiness, step.Phase, step.ResourceRef, step.Action)
				if step.Strategy != syncplan.StrategyNone {
					fmt.Fprintf(w, " via %s", step.Strategy)
				}
				if step.Pause != syncplan.PauseNone {
					fmt.Fprintf(w, " (pause: %s)", step.Pause)
				}
				fmt.Fprintln(w)
			}
		}
		if len(app.Actions) == 0 {
			fmt.Fprintln(w, "  no actions")
		} else {
			for _, action := range app.Actions {
				fmt.Fprintf(w, "  %s %s: %s\n", action.Severity, action.Kind, action.Message)
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "Dry run only: no sync operations were executed.")
}
