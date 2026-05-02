package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func runCutover(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cutover", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var format string
	var outputPath string
	var preparePlanPath string
	var syncPlanPath string
	observationWindowSeconds := gateway.DefaultObservationWindowSeconds
	rollbackWindowSeconds := gateway.DefaultRollbackWindowSeconds

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to cut over")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&preparePlanPath, "from-prepare", "", "read a prior prepare JSON plan artifact")
	fs.StringVar(&syncPlanPath, "from-sync", "", "read a prior sync JSON plan artifact")
	fs.IntVar(&observationWindowSeconds, "observation-window", gateway.DefaultObservationWindowSeconds, "observation window in seconds")
	fs.IntVar(&rollbackWindowSeconds, "rollback-window", gateway.DefaultRollbackWindowSeconds, "rollback window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkOutputFormat("cutover", format); err != nil {
		return err
	}

	result, err := cutoverResultFromOptions(bundleDir, target, appName, preparePlanPath, syncPlanPath, flagWasSet(fs, "bundle"), flagWasSet(fs, "target"), observationWindowSeconds, rollbackWindowSeconds)
	if err != nil {
		return err
	}

	return writeOutput(stdout, outputPath, func(out io.Writer) error {
		switch format {
		case "text":
			writeCutoverText(out, result)
		case "json":
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(result); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported cutover format %q", format)
		}

		return nil
	})
}

func cutoverResultFromOptions(bundleDir, target, appName, preparePlanPath, syncPlanPath string, bundleSet, targetSet bool, observationWindowSeconds, rollbackWindowSeconds int) (gateway.Result, error) {
	if preparePlanPath == "" && syncPlanPath == "" {
		return gateway.Plan(gateway.Options{
			BundleDir:                bundleDir,
			Target:                   target,
			AppName:                  appName,
			ObservationWindowSeconds: &observationWindowSeconds,
			RollbackWindowSeconds:    &rollbackWindowSeconds,
		})
	}
	if syncPlanPath != "" && preparePlanPath == "" {
		return gateway.Result{}, fmt.Errorf("--from-prepare is required when --from-sync is set")
	}

	expect := artifactExpectations{AppName: appName}
	if bundleSet {
		expect.BundleDir = bundleDir
	}
	if targetSet {
		expect.Target = target
	}

	var preparePlan preparer.Result
	var err error
	if preparePlanPath != "" {
		preparePlan, err = readPrepareArtifact(preparePlanPath, expect)
		if err != nil {
			return gateway.Result{}, err
		}
		if !bundleSet {
			bundleDir = preparePlan.BundleDir
		}
		if !targetSet {
			target = preparePlan.Target
		}
	}

	var syncResult syncplan.Result
	if syncPlanPath != "" {
		syncExpect := expect
		if preparePlanPath != "" {
			syncExpect.BundleDir = preparePlan.BundleDir
			syncExpect.Target = preparePlan.Target
		}
		syncResult, err = readSyncArtifact(syncPlanPath, syncExpect)
		if err != nil {
			return gateway.Result{}, err
		}
		if !bundleSet {
			bundleDir = syncResult.BundleDir
		}
		if !targetSet {
			target = syncResult.Target
		}
	}

	if preparePlanPath == "" {
		preparePlan, err = preparer.Plan(preparer.Options{BundleDir: bundleDir, Target: target, AppName: appName})
		if err != nil {
			return gateway.Result{}, err
		}
	}
	if syncPlanPath == "" {
		syncResult = syncplan.PlanFromPrepare(preparePlan)
	}

	return gateway.PlanFromPrepareAndSync(preparePlan, syncResult, observationWindowSeconds, rollbackWindowSeconds)
}

func writeCutoverText(w io.Writer, result gateway.Result) {
	fmt.Fprintf(w, "Cutover plan: %s -> %s\n", result.BundleDir, result.Target)
	fmt.Fprintf(w, "Status: %s\n\n", result.Status)

	for _, app := range result.Apps {
		fmt.Fprintf(w, "[%s] %s\n", app.Status, app.Name)
		fmt.Fprintf(w, "  readiness: %s\n", app.Readiness)
		fmt.Fprintf(w, "  prepare readiness: %s\n", app.PrepareReadiness)
		fmt.Fprintf(w, "  sync readiness: %s\n", app.SyncReadiness)
		fmt.Fprintf(w, "  observe: %ds, rollback window: %ds\n", app.ObservationWindowSeconds, app.RollbackWindowSeconds)
		if len(app.Routes) > 0 {
			fmt.Fprintln(w, "  routes:")
			for _, route := range app.Routes {
				fmt.Fprintf(w, "    %s %s -> %s", route.Readiness, route.CurrentRef, route.TargetRef)
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
