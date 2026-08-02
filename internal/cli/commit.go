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
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.IntVar(&rollbackWindowSeconds, "rollback-window", commitplan.DefaultRollbackWindowSeconds, "rollback window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("commit does not accept positional argument %q", fs.Arg(0))
	}
	if apply {
		for _, name := range []string{"app", "bundle", "format", "from-cutover", "output", "rollback-window", "target"} {
			if flagSet(fs, name) {
				return fmt.Errorf("commit --apply does not accept --%s; select the run with --run", name)
			}
		}
		return applyCommitFromArgs(ctx, runRef, stderr)
	}
	if err := checkOutputFormat("commit", format); err != nil {
		return err
	}
	if strings.TrimSpace(runRef) != "" {
		for _, name := range []string{"app", "bundle", "from-cutover", "rollback-window", "target"} {
			if flagSet(fs, name) {
				return fmt.Errorf("commit --run does not accept --%s; the run already owns its reviewed commit plan", name)
			}
		}
	}

	var result commitplan.Result
	useCurrentRun := cutoverPlanPath == "" && !flagSet(fs, "bundle") && !flagSet(fs, "target") && !flagSet(fs, "app") && !flagSet(fs, "rollback-window")
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
		result = run.Commit
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

func applyCommitFromArgs(ctx context.Context, runRef string, stderr io.Writer) error {
	var err error
	runRef, err = resolveRunRef(runRef, false)
	if err != nil {
		return err
	}
	operationLock, err := acquireRunOperationLock(runRef)
	if err != nil {
		return fmt.Errorf("commit run %q: %w", runRef, err)
	}
	defer operationLock.Release()
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
	plan.ApprovedPrepareDecisions = approvedPrepareDecisions(run)
	fmt.Fprintf(stderr, "commit apply: run %s; planned %d step(s) to retire source\n", run.Run.Name, len(plan.Steps))
	if err := client.Apply(ctx, plan); err != nil {
		return err
	}
	if err := markRunCommittedLocked(run.Run); err != nil {
		return fmt.Errorf("source retirement completed, but migration commit metadata could not be recorded: %w", err)
	}
	return nil
}

func requireLiveApplySucceeded(run loadedMigrationRun) error {
	return requireLiveApplySucceededForAppsSkipping(run, nil, nil)
}

func liveApplySucceeded(run loadedMigrationRun) bool {
	return requireLiveApplySucceeded(run) == nil
}

func requireLiveApplySucceededForApps(run loadedMigrationRun, apps map[string]struct{}) error {
	return requireLiveApplySucceededForAppsSkipping(run, apps, nil)
}

func requireLiveApplySucceededForAppsSkipping(run loadedMigrationRun, apps map[string]struct{}, skipKinds map[dokploy.StepKind]struct{}) error {
	if run.Run.ApplyOutcomeRequired && run.Applied.SucceededAt == nil {
		return fmt.Errorf("run %q has no successful live-apply outcome recorded; run `%s` first", run.Run.Name, liveApplyCommand(run))
	}
	live := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	if len(live.Steps) == 0 {
		return fmt.Errorf("run %q has no live apply steps; nothing to commit", run.Run.Name)
	}
	byIndex := map[int]appliedStep{}
	latest := map[string]appliedStep{}
	for _, step := range run.Applied.Steps {
		if current, ok := byIndex[step.Index]; !ok || !step.UpdatedAt.Before(current.UpdatedAt) {
			byIndex[step.Index] = step
		}
		key := appliedStepKey(step.Kind, step.App, step.Ref)
		if current, ok := latest[key]; !ok || !step.UpdatedAt.Before(current.UpdatedAt) {
			latest[key] = step
		}
	}
	checked := 0
	for index, step := range live.Steps {
		if _, skip := skipKinds[step.Kind]; skip {
			continue
		}
		if !liveApplyStepInScope(step, apps) {
			continue
		}
		checked++
		key := appliedStepKey(string(step.Kind), step.App, step.Ref)
		recorded, ok := byIndex[index]
		if !ok || !appliedStepCompleted(recorded) || !appliedStepMatches(recorded, step) {
			return fmt.Errorf("run %q has not been live-applied; missing completed applied step %s %s/%s at plan index %d. run `%s` first", run.Run.Name, step.Kind, step.App, step.Ref, index, liveApplyCommand(run))
		}
		if current, ok := latest[key]; !ok || !appliedStepCompleted(current) {
			return fmt.Errorf("run %q has not been live-applied; latest applied outcome for step %s %s/%s is not complete. run `%s` first", run.Run.Name, step.Kind, step.App, step.Ref, liveApplyCommand(run))
		}
	}
	if checked == 0 {
		return fmt.Errorf("run %q has no live apply steps for the selected purge scope", run.Run.Name)
	}
	return nil
}

func liveApplyStepInScope(step dokploy.Step, apps map[string]struct{}) bool {
	if len(apps) == 0 || step.App == "" {
		return true
	}
	_, ok := apps[step.App]
	return ok
}

func appliedStepKey(kind, app, ref string) string {
	return kind + "\x00" + app + "\x00" + ref
}
