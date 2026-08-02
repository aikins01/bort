package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	rollbackplan "github.com/aikins01/bort/internal/rollback"
)

func runRollback(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var format string
	var outputPath string
	var cutoverPlanPath string
	var runRef string
	observationWindowSeconds := rollbackplan.DefaultObservationWindowSeconds

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to roll back")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&cutoverPlanPath, "from-cutover", "", "read a prior cutover JSON plan artifact")
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.IntVar(&observationWindowSeconds, "observation-window", rollbackplan.DefaultObservationWindowSeconds, "observation window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("rollback does not accept positional argument %q", fs.Arg(0))
	}
	if err := checkOutputFormat("rollback", format); err != nil {
		return err
	}
	if strings.TrimSpace(runRef) != "" {
		for _, name := range []string{"app", "bundle", "from-cutover", "observation-window", "target"} {
			if flagSet(fs, name) {
				return fmt.Errorf("rollback --run does not accept --%s; the run already owns its reviewed rollback plan", name)
			}
		}
	}

	var result rollbackplan.Result
	useCurrentRun := cutoverPlanPath == "" && !flagSet(fs, "bundle") && !flagSet(fs, "target") && !flagSet(fs, "app") && !flagSet(fs, "observation-window")
	resolvedRun := strings.TrimSpace(runRef)
	useReviewedRun := resolvedRun != ""
	if !useReviewedRun && useCurrentRun {
		var err error
		resolvedRun, useReviewedRun, err = selectedRunRef(false)
		if err != nil {
			return err
		}
	}
	if useReviewedRun {
		run, err := loadMigrationRun(resolvedRun)
		if err != nil {
			return err
		}
		result = run.Rollback
	} else if cutoverPlanPath != "" {
		expect := artifactExpectations{AppName: appName}
		if flagWasSet(fs, "bundle") {
			expect.BundleDir = bundleDir
		}
		if flagWasSet(fs, "target") {
			expect.Target = target
		}
		cutoverPlan, err := readCutoverArtifact(cutoverPlanPath, expect)
		if err != nil {
			return err
		}
		result, err = rollbackplan.PlanFromCutover(cutoverPlan, observationWindowSeconds)
		if err != nil {
			return err
		}
	} else {
		var err error
		result, err = rollbackplan.Plan(rollbackplan.Options{
			BundleDir:                bundleDir,
			Target:                   target,
			AppName:                  appName,
			ObservationWindowSeconds: &observationWindowSeconds,
		})
		if err != nil {
			return err
		}
	}

	return writeFormattedOutput(stdout, outputPath, format, result, writeRollbackText)
}

func writeRollbackText(w io.Writer, result rollbackplan.Result) {
	fmt.Fprintf(w, "Rollback plan: %s -> %s\n", result.BundleDir, result.Target)
	fmt.Fprintf(w, "Status: %s\n\n", result.Status)

	for _, app := range result.Apps {
		fmt.Fprintf(w, "[%s] %s\n", app.Status, app.Name)
		fmt.Fprintf(w, "  readiness: %s\n", app.Readiness)
		fmt.Fprintf(w, "  cutover readiness: %s\n", app.CutoverReadiness)
		fmt.Fprintf(w, "  observe: %ds\n", app.ObservationWindowSeconds)
		if len(app.Routes) > 0 {
			fmt.Fprintln(w, "  routes:")
			for _, route := range app.Routes {
				fmt.Fprintf(w, "    %s %s -> %s", route.Readiness, route.TargetRef, route.CurrentRef)
				if route.ServiceName != "" {
					fmt.Fprintf(w, " service=%s", route.ServiceName)
				}
				if route.Port != "" {
					fmt.Fprintf(w, " port=%s", route.Port)
				}
				fmt.Fprintln(w)
			}
		}
		if len(app.Gates) > 0 {
			fmt.Fprintln(w, "  gates:")
			for _, gate := range app.Gates {
				fmt.Fprintf(w, "    %s %s: %s\n", gate.Severity, gate.Code, gate.Message)
			}
		}
		if len(app.Steps) > 0 {
			fmt.Fprintln(w, "  steps:")
			for _, step := range app.Steps {
				fmt.Fprintf(w, "    %s %s %s: %s\n", step.Readiness, step.Phase, step.ResourceRef, step.Action)
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
	fmt.Fprintln(w, "Dry run only: no routes were changed and no rollback actions were executed.")
}
