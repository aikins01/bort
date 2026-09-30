package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	rollbackplan "github.com/aikins01/bort/internal/rollback"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var format string
	var outputPath string
	var cutoverPlanPath string
	var runRef string
	var live bool
	var confirm string
	observationWindowSeconds := rollbackplan.DefaultObservationWindowSeconds

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to roll back")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.StringVar(&cutoverPlanPath, "from-cutover", "", "read a prior cutover JSON plan artifact")
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.BoolVar(&live, "live", false, "return stateless traffic to a healthy source when safe; stateful authority recovery is manual")
	fs.StringVar(&confirm, "confirm", "", "confirm open rollback triggers with the exact phrase: rollback <run-name> (requires --live)")
	fs.IntVar(&observationWindowSeconds, "observation-window", rollbackplan.DefaultObservationWindowSeconds, "observation window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("rollback does not accept positional argument %q", fs.Arg(0))
	}
	if flagSet(fs, "run") && strings.TrimSpace(runRef) == "" {
		return fmt.Errorf("rollback requires a non-empty --run value")
	}
	if flagSet(fs, "confirm") && !live {
		return fmt.Errorf("rollback --confirm requires --live")
	}
	if live {
		for _, name := range []string{"app", "bundle", "format", "from-cutover", "observation-window", "output", "target"} {
			if flagSet(fs, name) {
				return fmt.Errorf("rollback --live does not accept --%s; select the run with --run", name)
			}
		}
		return applyRollbackFromArgs(ctx, runRef, confirm, stderr)
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
		blocker := automaticRollbackBlocker(ctx, run)
		available := blocker == nil
		if blocker != nil {
			result.AutomaticBlocker = blocker.Error()
		}
		result.AutomaticAvailable = &available
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
	fmt.Fprintf(w, "Status: %s\n", result.Status)
	if result.AutomaticAvailable != nil {
		if *result.AutomaticAvailable {
			fmt.Fprintln(w, "Automatic rollback: available")
		} else {
			fmt.Fprintf(w, "Automatic rollback: unavailable: %s\n", result.AutomaticBlocker)
		}
	}
	fmt.Fprintln(w)

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

func applyRollbackFromArgs(ctx context.Context, runRef, confirm string, stderr io.Writer) error {
	var err error
	runRef, err = resolveRunRef(runRef, false)
	if err != nil {
		return err
	}
	operationLock, err := acquireRunOperationLock(runRef)
	if err != nil {
		return fmt.Errorf("rollback run %q: %w", runRef, err)
	}
	defer operationLock.Release()
	run, err := loadMigrationRun(runRef)
	if err != nil {
		return err
	}
	if run.Run.RolledBackAt != nil {
		plan, planErr := dokploy.PlanForRollback(run.Prepare, run.Sync, run.Cutover)
		if planErr != nil || planRequiresDokployHostOwner(run, plan) {
			if err := releaseDokployTrafficOwner(run.Run); err != nil {
				return fmt.Errorf("release completed rollback traffic ownership: %w", err)
			}
		}
		fmt.Fprintf(stderr, "rollback already complete for run %s: traffic is back on the source; start a fresh run to migrate again\n", run.Run.Name)
		return nil
	}
	if err := validateRollbackApplyReady(run); err != nil {
		return err
	}
	if err := validateApplyResumeAuthority(run.Applied); err != nil {
		return fmt.Errorf("automatic rollback refused: %w; %s", err, authorityRecoveryInstruction(run))
	}
	plan, err := planAutomaticRollback(run)
	if err != nil {
		return fmt.Errorf("automatic rollback refused: %w; %s", err, authorityRecoveryInstruction(run))
	}
	if !dokployLiveOperationsSupported() {
		return fmt.Errorf("automatic rollback is unavailable on this platform; continue on the Linux source host")
	}
	phrase := "rollback " + run.Run.Name
	if confirm != "" && confirm != phrase {
		return fmt.Errorf("rollback confirmation must be exactly %q", phrase)
	}
	if err := verifyLocalSourceRun(ctx, run); err != nil {
		return fmt.Errorf("automatic rollback refused on an unverified Docker source: %w", err)
	}
	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		return fmt.Errorf("lock Dokploy live operations: %w", err)
	}
	defer targetLock.Release()
	requiresHostOwner := planRequiresDokployHostOwner(run, plan)
	if requiresHostOwner {
		if err := ensureDokployTrafficTargetOwner(ctx, run, nil, run.Run.RollbackStartedAt != nil); err != nil {
			return fmt.Errorf("automatic rollback refused: %w", err)
		}
	}
	if decisions := rollbackTriggerDecisions(run); len(decisions) > 0 {
		for _, decision := range decisions {
			fmt.Fprintf(stderr, "%s\n%s\n", decisionAction(decision), decisionReason(decision))
		}
		if confirm != phrase {
			return fmt.Errorf("rollback is blocked by %d unconfirmed rollback trigger(s); review `%s`, then confirm with `%s --confirm %s`", len(decisions), runScopedCommand(run, "rollback"), runScopedCommand(run, "rollback --live"), shellQuote(phrase))
		}
		for _, decision := range decisions {
			run.Progress = markReviewDecisionDone(run, decision, time.Now().UTC())
		}
		path, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Progress)
		if err != nil {
			return err
		}
		if err := writeRunProgress(path, run.Progress); err != nil {
			return err
		}
	}
	client := &dokploy.Client{}
	plan.RunName = run.Run.Name
	plan.RunDir = run.Run.RunDir
	plan.ApprovedPrepareDecisions = approvedPrepareDecisions(run)
	onProgress := func(p dokploy.StepProgress) {
		target := p.Step.App
		if target == "" {
			target = p.Step.Ref
		}
		line := fmt.Sprintf("rollback [%d/%d] %s %s: %s", p.Index+1, p.Total, p.Step.Kind, target, p.Status)
		if p.Err != nil {
			line += ": " + p.Err.Error()
		}
		fmt.Fprintln(stderr, line)
	}
	plan.OnProgress = &onProgress
	if err := markRunRollbackStartedLocked(run.Run); err != nil {
		return fmt.Errorf("record rollback start: %w", err)
	}
	fmt.Fprintf(stderr, "rollback live: run %s; planned %s to return traffic to the source\n", run.Run.Name, pluralize(len(plan.Steps), "step", "steps"))
	if err := client.Apply(ctx, plan); err != nil {
		return err
	}
	if planChangesDokployTraffic(plan) {
		if err := client.ReconcileSourceTrafficAuthority(ctx); err != nil {
			return fmt.Errorf("verify source traffic authority: %w", err)
		}
	}
	if requiresHostOwner {
		if err := markDokployTrafficSource(run.Run); err != nil {
			return fmt.Errorf("rollback completed, but source authority could not be recorded: %w", err)
		}
	}
	if err := markRunRolledBackLocked(run.Run); err != nil {
		return fmt.Errorf("rollback completed, but its outcome could not be recorded: %w", err)
	}
	if requiresHostOwner {
		if err := releaseDokployTrafficOwner(run.Run); err != nil {
			return fmt.Errorf("rollback was recorded, but its Dokploy host ownership could not be released: %w", err)
		}
	}
	writeRollbackAppliedSummary(stderr, run, plan)
	return nil
}

func planAutomaticRollback(run loadedMigrationRun) (dokploy.Plan, error) {
	if run.Run.ResolvedAuthority != "" {
		return dokploy.Plan{}, fmt.Errorf("run %q used manual %s-authority recovery; automatic rollback cannot infer the manually established boundary", run.Run.Name, run.Run.ResolvedAuthority)
	}
	return dokploy.PlanForRollback(run.Prepare, run.Sync, run.Cutover)
}

func automaticRollbackBlocker(ctx context.Context, run loadedMigrationRun) error {
	if err := validateRollbackApplyReady(run); err != nil {
		return err
	}
	if err := validateApplyResumeAuthority(run.Applied); err != nil {
		return err
	}
	plan, err := planAutomaticRollback(run)
	if err != nil {
		return err
	}
	if !dokployLiveOperationsSupported() {
		return fmt.Errorf("automatic rollback is unavailable on this platform; continue on the Linux source host")
	}
	if err := verifyLocalSourceRun(ctx, run); err != nil {
		return fmt.Errorf("source attestation failed: %w", err)
	}
	if planRequiresDokployHostOwner(run, plan) {
		if err := validateCurrentDokployTrafficOwner(run, run.Run.RollbackStartedAt != nil); err != nil {
			return err
		}
	}
	hostOperationActive, err := dokployLiveOperationActive()
	if err != nil {
		return fmt.Errorf("host-wide Dokploy operation lock could not be verified: %w", err)
	}
	if hostOperationActive {
		return fmt.Errorf("another process holds the host-wide Dokploy operation lock")
	}
	return nil
}

func incompleteRollbackRecovery(run loadedMigrationRun) string {
	if runMayHaveAmbiguousAuthority(run) {
		return authorityRecoveryInstruction(run)
	}
	if _, err := planAutomaticRollback(run); err != nil {
		return authorityRecoveryInstruction(run)
	}
	return fmt.Sprintf("run `%s` to finish recovery", runScopedCommand(run, "rollback --live"))
}

func validateRollbackApplyReady(run loadedMigrationRun) error {
	if run.Run.Target != "dokploy" {
		return fmt.Errorf("rollback --live is only supported for target dokploy, got %q", run.Run.Target)
	}
	if run.Run.PurgedAt != nil {
		return fmt.Errorf("rollback refused: run %q was already purged", run.Run.Name)
	}
	if run.Run.CommittedAt != nil {
		return fmt.Errorf("rollback refused: run %q was already committed; its source containers were retired", run.Run.Name)
	}
	if run.Run.CommitStartedAt != nil {
		return fmt.Errorf("rollback refused: source retirement started for run %q; %s", run.Run.Name, finishStartedAcceptanceAction(run))
	}
	if run.Run.RolledBackAt != nil {
		return fmt.Errorf("rollback refused: run %q was already rolled back; start a fresh run to migrate again", run.Run.Name)
	}
	if !runMayHaveAmbiguousAuthority(run) {
		if err := requireLiveApplySucceeded(run); err != nil {
			return fmt.Errorf("rollback requires a successful live apply: %w", err)
		}
	}
	active, err := applyRunActive(run.Run.RunDir)
	if err != nil {
		return fmt.Errorf("check live-apply lock: %w", err)
	}
	if active {
		return fmt.Errorf("rollback refused: live apply is running for run %q; wait for it to finish before rolling back", run.Run.Name)
	}
	return nil
}

func rollbackTriggerDecisions(run loadedMigrationRun) []runDecision {
	return openFilteredDecisions(run, func(item runDecisionItem) bool {
		return item.Stage == "rollback" && item.Code == "rollback.trigger_required"
	})
}

func writeRollbackAppliedSummary(w io.Writer, run loadedMigrationRun, plan dokploy.Plan) {
	fmt.Fprintln(w, "rollback complete: traffic is back on the source")
	if len(plan.Steps) == 0 {
		fmt.Fprintln(w, "this run stopped no source containers and changed no routes; the source was serving all along")
	}
	fmt.Fprintln(w, "the Dokploy target resources remain on the server; data written to the target after cutover was not copied back to the source")
	fmt.Fprintln(w, "live rollback does not enforce the stored observation window; continue monitoring the source for the reviewed period")
	fmt.Fprintln(w, "to migrate again, create a new named run with `bort migrate --run <new-name>` and current `--source` or `--bundle` inputs")
}
