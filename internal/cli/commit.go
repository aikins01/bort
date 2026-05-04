package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func runCommit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var format string
	var outputPath string
	var cutoverPlanPath string
	var apply bool
	var runRef string
	rollbackWindowSeconds := commitplan.DefaultRollbackWindowSeconds

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to commit")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&cutoverPlanPath, "from-cutover", "", "read a prior cutover JSON plan artifact")
	fs.BoolVar(&apply, "apply", false, "execute commit cleanup (stop source containers and coolify-proxy)")
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path (with --apply)")
	fs.IntVar(&rollbackWindowSeconds, "rollback-window", commitplan.DefaultRollbackWindowSeconds, "rollback window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if apply {
		return applyCommitFromArgs(ctx, runRef, stderr)
	}
	if err := checkOutputFormat("commit", format); err != nil {
		return err
	}

	var result commitplan.Result
	if cutoverPlanPath != "" {
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
		if flagWasSet(fs, "rollback-window") {
			cutoverPlan = withCutoverRollbackWindow(cutoverPlan, rollbackWindowSeconds)
		}
		result, err = commitplan.PlanFromCutover(cutoverPlan)
		if err != nil {
			return err
		}
	} else {
		var err error
		result, err = commitplan.Plan(commitplan.Options{BundleDir: bundleDir, Target: target, AppName: appName, RollbackWindowSeconds: &rollbackWindowSeconds})
		if err != nil {
			return err
		}
	}

	return writeFormattedOutput(stdout, outputPath, format, result, writeCommitText)
}

func withCutoverRollbackWindow(plan gateway.Result, seconds int) gateway.Result {
	for i := range plan.Apps {
		plan.Apps[i].RollbackWindowSeconds = seconds
	}
	return plan
}

func writeCommitText(w io.Writer, result commitplan.Result) {
	fmt.Fprintf(w, "Commit plan: %s -> %s\n", result.BundleDir, result.Target)
	fmt.Fprintf(w, "Status: %s\n\n", result.Status)

	for _, app := range result.Apps {
		fmt.Fprintf(w, "[%s] %s\n", app.Status, app.Name)
		fmt.Fprintf(w, "  readiness: %s\n", app.Readiness)
		fmt.Fprintf(w, "  cutover readiness: %s\n", app.CutoverReadiness)
		fmt.Fprintf(w, "  rollback window: %ds\n", app.RollbackWindowSeconds)
		if len(app.Routes) > 0 {
			fmt.Fprintln(w, "  routes:")
			for _, route := range app.Routes {
				fmt.Fprintf(w, "    %s accept %s; retire %s", route.Readiness, route.TargetRef, route.CurrentRef)
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
	fmt.Fprintln(w, "Dry run only: no target ownership was committed and no source resources were retired.")
}

// applyCommitFromArgs executes the commit-time cleanup against the
// requested run (or the latest run if none was named). it stops every
// source app container and reasserts that coolify-proxy is stopped, but
// does not remove anything — operators on a long rollback window can
// still docker start the source manually.
func applyCommitFromArgs(ctx context.Context, runRef string, stderr io.Writer) error {
	if strings.TrimSpace(runRef) == "" {
		latest, ok := latestRunRef()
		if !ok {
			return fmt.Errorf("no migration run found; create one before commit --apply")
		}
		runRef = latest
	}
	run, err := loadMigrationRun(runRef)
	if err != nil {
		return err
	}
	if run.Run.Target != "dokploy" {
		return fmt.Errorf("commit --apply is only supported for target dokploy, got %q", run.Run.Target)
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		return err
	}
	client, err := resolveDokployClient(ctx, run.Run.Target, os.Stdin, stderr)
	if err != nil {
		return err
	}
	plan := dokploy.PlanForCommit(run.Prepare, run.Cutover)
	plan.RunName = run.Run.Name
	plan.RunDir = run.Run.RunDir
	fmt.Fprintf(stderr, "commit apply: planned %d step(s) to retire source\n", len(plan.Steps))
	return client.Apply(ctx, plan)
}

// requireLiveApplySucceeded blocks commit --apply on runs whose live
// migrate phase never finished. without this guard, commit --apply on a
// freshly-planned (or partially-applied) run would stop production
// source containers before the target was actually serving traffic.
// each step in the computed live plan must have a matching ledger entry
// at the same index with status=ok and identical kind/app/ref — anything
// else means the live apply is stale or incomplete.
func requireLiveApplySucceeded(run loadedMigrationRun) error {
	live := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	if len(live.Steps) == 0 {
		return fmt.Errorf("run %q has no live apply steps; nothing to commit", run.Run.Name)
	}
	byIndex := map[int]appliedStep{}
	for _, step := range run.Applied.Steps {
		byIndex[step.Index] = step
	}
	for index, step := range live.Steps {
		got, ok := byIndex[index]
		if !ok {
			return fmt.Errorf("run %q has not been live-applied; missing applied step %d %s. run `bort migrate --live --run %s` first", run.Run.Name, index, step.Kind, run.Run.Name)
		}
		if got.Kind != string(step.Kind) || got.App != step.App || got.Ref != step.Ref {
			return fmt.Errorf("run %q live apply ledger is stale at step %d (expected %s %s/%s, got %s %s/%s); rerun the live apply before commit", run.Run.Name, index, step.Kind, step.App, step.Ref, got.Kind, got.App, got.Ref)
		}
		if got.Status != string(dokploy.StepStatusOK) {
			return fmt.Errorf("run %q live apply step %d (%s %s/%s) finished with status %q, not ok; resolve before commit", run.Run.Name, index, step.Kind, step.App, step.Ref, got.Status)
		}
	}
	return nil
}
