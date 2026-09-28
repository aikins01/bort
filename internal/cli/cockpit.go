package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
)

func writeAppFirstCockpit(w io.Writer, run loadedMigrationRun) {
	writeAppFirstCockpitContext(context.Background(), w, run)
}

func writeAppFirstCockpitContext(ctx context.Context, w io.Writer, run loadedMigrationRun) {
	summary := summarizeMigrationRunContext(ctx, run)
	writeAppFirstCockpitSummary(w, run, summary)
}

func writeAppFirstCockpitSummary(w io.Writer, run loadedMigrationRun, summary migrationRunSummary) {
	apps := appsFromRun(run)
	phase := migrationRunPhaseWithNext(run, summary.Next)
	st := newStyler(w)

	ready, blocked := 0, 0
	for _, app := range apps {
		switch app.Health {
		case appHealthReady:
			ready++
		case appHealthBlocked:
			blocked++
		}
	}

	header := fmt.Sprintf("%s %s %s · %d of %d ready",
		runSourceLabel(summary.Run),
		st.muted("→"),
		st.emph(summary.Run.Target),
		ready, len(apps),
	)
	if blocked > 0 {
		header += " · " + st.glyph(fmt.Sprintf("%d blocked", blocked), sevBad)
	}
	header += " " + st.pill(migrationRunPhaseLabel(phase), severityForMigrationRunPhase(phase))
	fmt.Fprintln(w, header)
	if workspace := workspaceDir(); workspace != "" {
		fmt.Fprintln(w, st.muted("workspace: "+workspace))
	}
	fmt.Fprintln(w)

	if len(apps) == 0 {
		fmt.Fprintln(w, "No apps in this run.")
		fmt.Fprintln(w)
		writeCockpitPhaseGuidance(w, st, run, phase, summary, apps)
		return
	}

	for i, app := range apps {
		if i > 0 {
			fmt.Fprintln(w, st.muted(strings.Repeat("─", 60)))
		}
		nameLine := fmt.Sprintf("%s  %s",
			st.glyph(healthGlyph(app.Health), severityForHealth(app.Health)),
			st.emph(app.Name),
		)
		if st.color {
			nameLine += " " + st.pill(string(app.Health), severityForHealth(app.Health))
		}
		fmt.Fprintln(w, nameLine)
		for _, line := range app.Resources {
			fmt.Fprintf(w, "    %s  %-9s %s\n",
				st.glyph(line.Status, severityForGlyph(line.Status)),
				line.Label,
				st.muted(line.Detail),
			)
		}
		if len(app.Issues) > 0 {
			fmt.Fprintln(w, "    "+st.muted("Issues:"))
			for _, issue := range app.Issues {
				fmt.Fprintf(w, "      %s %s\n",
					st.glyph(severityGlyph(issue.Severity), severityForReadiness(issue.Severity)),
					issue.Title,
				)
				if issue.Detail != "" {
					fmt.Fprintf(w, "          %s\n", st.muted(issue.Detail))
				}
				if fix := issue.FixCommand(app.Name); fix != "" {
					fmt.Fprintf(w, "          %s\n", st.fix(fix))
				} else if next := issue.NextStep(); next != "" {
					fmt.Fprintf(w, "          %s\n", st.muted("next: "+next))
				}
			}
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, st.muted(strings.Repeat("─", 60)))
	parts := []string{fmt.Sprintf("%d apps", len(apps))}
	if ready > 0 {
		parts = append(parts, st.glyph(fmt.Sprintf("%d ready", ready), sevGood))
	}
	if needsWork := len(apps) - ready - blocked; needsWork > 0 {
		parts = append(parts, st.glyph(fmt.Sprintf("%d needs work", needsWork), sevWarn))
	}
	if blocked > 0 {
		parts = append(parts, st.glyph(fmt.Sprintf("%d blocked", blocked), sevBad))
	}
	fmt.Fprintf(w, "%s %s\n", st.emph("Plan:"), strings.Join(parts, " · "))
	if line := appliedFooter(run.Applied); line != "" {
		fmt.Fprintln(w, st.muted(line))
	}
	downstreamBlockers := openDownstreamBlockingDecisions(run)
	if len(downstreamBlockers) > 0 {
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Downstream blockers: %d open", len(downstreamBlockers))))
		writeCockpitDecisions(w, st, downstreamBlockers)
	}
	reviewDecisions := openReviewDecisions(run)
	if len(reviewDecisions) > 0 {
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Review-only decisions: %d open (non-blocking before live apply)", len(reviewDecisions))))
		writeCockpitDecisions(w, st, reviewDecisions)
	}
	writeCockpitPhaseGuidance(w, st, run, phase, summary, apps)
}

func writeCockpitPhaseGuidance(w io.Writer, st *styler, run loadedMigrationRun, phase string, summary migrationRunSummary, apps []appView) {
	if !dokployLiveOperationsSupported() {
		switch phase {
		case "empty", "committed", "rolled back", "purged":
		default:
			writeUnsupportedPlatformGuidance(w, st, run, phase)
			return
		}
	}
	switch phase {
	case "lock-error":
		fmt.Fprintln(w, st.muted("Live apply lock state could not be verified. Inspect the run's apply.lock before retrying."))
	case "host-lock-error":
		fmt.Fprintln(w, st.muted("The host-wide Dokploy operation lock could not be verified. Inspect /var/lib/bort/dokploy-live.lock before retrying."))
	case "install-recovery":
		command, found, err := dokployInstallationRecoveryCommand()
		switch {
		case err != nil:
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("A Dokploy installation was interrupted, but its recovery command could not be read: %v. Inspect /var/lib/bort/dokploy-live.lock.install-recovery-required before other host mutations.", err)))
		case !found:
			fmt.Fprintln(w, st.muted("A Dokploy installation was interrupted, but its recovery marker is no longer present. Re-check this run before other host mutations."))
		default:
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("A Dokploy installation was interrupted after host mutation may have started. Run `%s` to reconcile it before other host mutations.", command)))
		}
	case "host-busy":
		fmt.Fprintln(w, st.muted("Another process is changing this Dokploy host. Wait for that operation to finish, then check this run again."))
	case "empty":
		fmt.Fprintln(w, st.muted("This run contains no migratable applications. Platform-role entries are excluded from live apply; create a new run that selects at least one non-platform application."))
	case "inspection-only":
		if dokployLiveOperationsSupported() {
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("This run is available for inspection but lacks local source attestation. %s. %s.", summary.Next.Reason, summary.Next.Action)))
		} else {
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("This run is ready for inspection, but Dokploy live actions are unavailable on %s. Continue on the Linux source host.", runtime.GOOS)))
		}
	case "source-attestation-error":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("The reviewed local Docker source could not be verified: %s. %s.", summary.Next.Reason, summary.Next.Action)))
	case "applying":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Live apply is running; %s.", summary.Next.Action)))
	case "authority-finalizing":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Manual %s authority is recorded, but lifecycle finalization is incomplete; %s.", run.Run.ResolvedAuthority, summary.Next.Action)))
	case "authority-ambiguous":
		writeAuthorityRecoveryGuidance(w, st, run, "The stored run cannot prove writer or traffic authority. Automatic retry, commit, and rollback are unavailable.")
	case "authority-ambiguous-rollback":
		writeAuthorityRecoveryGuidance(w, st, run, "An authority-ambiguous recovery is incomplete. Target fencing may be partial and source state may be unchanged.")
	case "host-owned":
		owner, _, _ := conflictingDokployHostOwner(run.Run)
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Dokploy host mutations are owned by %s with %s authority. Finish that run before applying this one.", dokployOwnerRunLabel(owner), owner.Authority)))
	case "host-owner-error":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Dokploy host ownership could not be verified: %s. %s.", summary.Next.Reason, summary.Next.Action)))
	case "source-recovery":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("A historical source pause needs cleanup. Run `%s` to restart the source without transferring state; automatic state migration remains unavailable.", liveApplyCommand(run))))
	case "partial":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Live apply is incomplete. Run `%s` to resume safely.", liveApplyCommand(run))))
	case "manual-state":
		fmt.Fprintln(w, st.muted("Bort cannot continue this run: it was applied with an older plan version that copied state into the deployed target, and Dokploy cannot durably fence target writers during that copy. Complete target setup, state transfer, traffic cutover, and source retirement outside Bort, or create a new run (new runs stage state before the target is deployed)."))
	case "plan-blocked":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Live apply would refuse this plan before touching the target: %s. %s.", summary.Next.Reason, summary.Next.Action)))
	case "applied":
		if coolifySourceRetirementRequired(run) {
			if _, err := planAutomaticRollback(run); err != nil {
				fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target is live. Automatic rollback is unavailable: %v. If validation fails, preserve both sides and recover authority manually; otherwise, after the rollback window, %s.", err, manualCoolifySourceRetirementAction(run))))
				writeManualRollbackRecoveryCommands(w, st, run)
			} else {
				fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target is live. Verify it through the rollback window, then %s; if validation fails, run `%s` to roll back stateless traffic.", manualCoolifySourceRetirementAction(run), runScopedCommand(run, "rollback --live"))))
			}
		} else if _, err := planAutomaticRollback(run); err != nil {
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target is live. Automatic rollback is unavailable: %v. If validation fails, preserve both sides and recover authority manually; otherwise run `%s` after the rollback window.", err, runScopedCommand(run, "commit --apply"))))
			writeManualRollbackRecoveryCommands(w, st, run)
		} else {
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target is live. Verify it through the rollback window, then run `%s` to retire the source; if validation fails, run `%s` to roll back stateless traffic.", runScopedCommand(run, "commit --apply"), runScopedCommand(run, "rollback --live"))))
		}
	case "committed":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target accepted and source containers retired. Run `%s` to audit leftovers.", runScopedCommand(run, "cleanup"))))
	case "committing":
		if strings.HasPrefix(summary.Next.Action, "wait for the active Dokploy host operation") {
			fmt.Fprintln(w, st.muted("Source retirement is in progress and rollback is no longer available. Another process holds the host-wide operation lock; wait for it to finish, then check this run again."))
		} else {
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Source retirement is in progress and rollback is no longer available; %s.", summary.Next.Action)))
		}
	case "rolled back":
		fmt.Fprintf(w, "%s\n", st.muted("Rollback returned traffic to the source; the Dokploy target resources remain on the server. To migrate again, "+summary.Next.Action+"."))
	case "rolling back":
		if strings.HasPrefix(summary.Next.Action, "wait for the active Dokploy host operation") {
			fmt.Fprintln(w, st.muted("Rollback is in progress; traffic or source state may already have changed. Another process holds the host-wide operation lock; wait for it to finish, then check this run again."))
		} else {
			fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Rollback is in progress; traffic or source state may already have changed; %s.", summary.Next.Action)))
		}
	case "purged":
		fmt.Fprintln(w, st.muted("Migration complete. Target resources and source-control credentials were preserved."))
	case "planning":
		fmt.Fprintln(w, st.muted(issueActionFooter(apps)))
	default:
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("All app inputs ready. Run `%s` to apply, or run `%s` interactively to continue.", liveApplyCommand(run), bortCommand(""))))
	}
}

func writeUnsupportedPlatformGuidance(w io.Writer, st *styler, run loadedMigrationRun, phase string) {
	state := "This run is ready for inspection."
	switch phase {
	case "lock-error":
		state = "Live apply lock state could not be verified; inspect the run's apply.lock before retrying."
	case "host-lock-error":
		state = "The host-wide Dokploy operation lock could not be verified; inspect /var/lib/bort/dokploy-live.lock before retrying."
	case "host-owner-error":
		state = "Dokploy host ownership could not be verified; inspect /var/lib/bort/dokploy-traffic-owner.json before live work."
	case "empty":
		fmt.Fprintln(w, st.muted("This run contains no migratable applications. Platform-role entries are excluded from live apply; create a new run that selects at least one non-platform application."))
		return
	case "applying":
		state = "Live apply is recorded as running; inspect the run and its lock before resuming it."
	case "authority-finalizing":
		state = fmt.Sprintf("Manual %s authority is recorded, but lifecycle finalization is incomplete.", run.Run.ResolvedAuthority)
	case "authority-ambiguous", "authority-ambiguous-rollback":
		state = "The stored run cannot prove writer or traffic authority; preserve both sides before recovery."
	case "host-owned":
		state = "Another migration run owns shared Dokploy host mutations."
	case "source-recovery":
		state = "A historical source pause still needs cleanup before this run can be retired."
	case "partial":
		state = "Live apply is incomplete and must be resumed from its durable ledger."
	case "manual-state":
		state = "This stateful run was applied with an older plan version and requires manual transfer and authority recovery."
	case "plan-blocked":
		state = "Live apply would refuse this plan's state transfer; choose a data store strategy and re-plan, or change the source compose and scan a new run."
	case "applied":
		state = "The target is recorded live; acceptance or rollback is still pending."
	case "committing":
		state = "Source retirement started and must be completed; rollback is no longer available."
	case "rolling back":
		state = "Rollback started and must be completed before other migration work."
	case "committed":
		state = "The target is accepted and source containers are retired."
	case "rolled back":
		state = "Traffic is recorded back on the source; target resources remain."
	case "purged":
		state = "Migration and source purge are complete."
	case "planning":
		state = "The run still has planning requirements."
	}
	fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("%s Dokploy live actions are unavailable on %s; continue any mutation or recovery on the Linux source host.", state, runtime.GOOS)))
}

func writeCockpitDecisions(w io.Writer, st *styler, decisions []runDecision) {
	limit := min(len(decisions), 3)
	for _, decision := range decisions[:limit] {
		fmt.Fprintf(w, "  %s %s: %s (%d item(s))\n", decision.Kind, decision.Readiness, decision.Action, decision.Count)
	}
	if remaining := len(decisions) - limit; remaining > 0 {
		fmt.Fprintf(w, "  %s\n", st.muted(fmt.Sprintf("and %d more", remaining)))
	}
}

func migrationRunPhase(run loadedMigrationRun) string {
	if !dokployLiveOperationsSupported() {
		return migrationRunPhaseWithNext(run, runNextStep{})
	}
	return migrationRunPhaseWithNext(run, nextSafeStep(run, nil))
}

func migrationRunPhaseWithNext(run loadedMigrationRun, next runNextStep) string {
	applyActive, applyActiveErr := false, error(nil)
	if run.Run.LiveAppliedAt == nil {
		applyActive, applyActiveErr = applyRunActive(run.Run.RunDir)
	}
	ownerSupported := dokployLiveOperationsSupported()
	if authorityRecoveryPending(run) {
		return "authority-finalizing"
	}
	if ownerSupported {
		ownerFinalizationPending, ownerFinalizationErr := completedDokployOwnerFinalization(run)
		if ownerFinalizationErr != nil {
			return "host-owner-error"
		}
		if ownerFinalizationPending {
			if run.Run.RolledBackAt != nil {
				return "rolling back"
			}
			return "committing"
		}
	}
	if run.Run.PurgedAt != nil {
		return "purged"
	}
	if run.Run.CommittedAt != nil {
		return "committed"
	}
	if run.Run.RolledBackAt != nil && !authorityRecoveryPending(run) {
		return "rolled back"
	}
	if next.Phase != "" {
		return next.Phase
	}
	hostOperationActive, hostOperationErr := false, error(nil)
	var hostOwned bool
	var hostOwnerErr error
	var trafficOwnerErr error
	if ownerSupported {
		hostOperationActive, hostOperationErr = dokployLiveOperationActive()
		_, hostOwned, hostOwnerErr = conflictingDokployHostOwner(run.Run)
		trafficOwnerErr = validateCurrentDokployTrafficOwner(run, rollbackInProgress(run))
	}
	switch {
	case rollbackInProgress(run) && runMayHaveAmbiguousAuthority(run):
		return "authority-ambiguous-rollback"
	case rollbackInProgress(run) && trafficOwnerErr != nil && hostOwnerErr == nil:
		return "authority-ambiguous-rollback"
	case rollbackInProgress(run):
		return "rolling back"
	case run.Run.CommitStartedAt != nil && trafficOwnerErr != nil && hostOwnerErr == nil:
		return "authority-ambiguous"
	case run.Run.CommitStartedAt != nil:
		return "committing"
	case applyActive:
		return "applying"
	case hostOwnerErr != nil:
		return "host-owner-error"
	case applyActiveErr != nil:
		return "lock-error"
	case errors.Is(hostOperationErr, errDokployInstallRecoveryRequired):
		return "install-recovery"
	case hostOperationErr != nil:
		return "host-lock-error"
	case hostOperationActive:
		return "host-busy"
	case manualTargetAuthorityRecorded(run) && trafficOwnerErr != nil:
		return "host-owner-error"
	case manualTargetAuthorityRecorded(run):
		return "applied"
	case runMayHaveAmbiguousAuthority(run):
		return "authority-ambiguous"
	case (run.Run.LiveAppliedAt != nil || liveApplySucceeded(run)) && trafficOwnerErr != nil:
		return "authority-ambiguous"
	case run.Run.LiveAppliedAt != nil || liveApplySucceeded(run):
		return "applied"
	case hostOwned:
		return "host-owned"
	case validateStatefulLiveApply(run) != nil && len(interruptedStatefulSourceCleanup(run)) > 0:
		return "source-recovery"
	case validateStatefulLiveApply(run) != nil:
		return "manual-state"
	case len(run.Applied.Steps) > 0:
		return "partial"
	case stagedTransferRefusal(run) != nil:
		return "plan-blocked"
	case !hasMigratableRunApps(run):
		return "empty"
	case len(openSetupDecisions(run)) > 0:
		return "planning"
	case len(liveApplyBlockingDecisions(run)) > 0:
		return "planning"
	case !ownerSupported:
		return "inspection-only"
	default:
		return "ready"
	}
}

func migrationRunPhaseLabel(phase string) string {
	switch phase {
	case "applied":
		return "TARGET LIVE"
	case "purged":
		return "COMPLETE"
	case "lock-error", "host-lock-error", "host-owner-error":
		return "LOCK ERROR"
	case "install-recovery":
		return "INSTALL RECOVERY"
	case "host-busy":
		return "HOST BUSY"
	case "host-owned":
		return "HOST OWNED"
	case "empty":
		return "NO APPS"
	case "inspection-only":
		return "INSPECTION ONLY"
	case "source-attestation-error":
		return "SOURCE CHANGED"
	case "authority-ambiguous", "authority-ambiguous-rollback":
		return "AUTHORITY UNKNOWN"
	case "authority-finalizing":
		return "RECOVERY PENDING"
	case "manual-state":
		return "MANUAL STATE"
	case "plan-blocked":
		return "PLAN BLOCKED"
	case "source-recovery":
		return "SOURCE RECOVERY"
	default:
		return strings.ToUpper(phase)
	}
}

func severityForMigrationRunPhase(phase string) severity {
	switch phase {
	case "ready", "applied", "committed", "purged":
		return sevGood
	case "partial", "lock-error", "host-lock-error", "host-owner-error", "install-recovery", "host-busy", "host-owned", "authority-ambiguous", "authority-ambiguous-rollback", "authority-finalizing", "source-attestation-error":
		return sevBad
	default:
		return sevWarn
	}
}

func manualTargetAuthorityRecorded(run loadedMigrationRun) bool {
	return run.Run.ResolvedAuthority == dokployTrafficTarget && run.Run.LiveAppliedAt != nil
}

func rollbackInProgress(run loadedMigrationRun) bool {
	return run.Run.RollbackStartedAt != nil && !manualTargetAuthorityRecorded(run)
}

func authorityRecoveryPending(run loadedMigrationRun) bool {
	switch run.Run.ResolvedAuthority {
	case dokployTrafficSource:
		return run.Run.AuthorityFinalizedAt == nil
	case dokployTrafficTarget:
		return run.Run.LiveAppliedAt == nil
	default:
		return false
	}
}

func authorityRecoveryAvailable(run loadedMigrationRun) bool {
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority == dokployTrafficReleased {
		return false
	}
	runID, err := dokployTrafficRunID(run.Run)
	return err == nil && owner.RunID == runID
}

func writeAuthorityRecoveryGuidance(w io.Writer, st *styler, run loadedMigrationRun, prefix string) {
	if !authorityRecoveryAvailable(run) {
		fmt.Fprintln(w, st.muted(prefix+" Inspect and preserve both sides, establish authority manually, and start a fresh migration run."))
		return
	}
	fmt.Fprintln(w, st.muted(prefix+" After manually fencing the other side and verifying the chosen writer and traffic authority, finish this owner-bound run with one of:"))
	fmt.Fprintf(w, "%s\n", st.muted("  source: `"+authorityRecoveryCommand(run, dokployTrafficSource)+"`"))
	fmt.Fprintf(w, "%s\n", st.muted("  target: `"+authorityRecoveryCommand(run, dokployTrafficTarget)+"`"))
}

func writeManualRollbackRecoveryCommands(w io.Writer, st *styler, run loadedMigrationRun) {
	if run.Run.ResolvedAuthority != "" || !authorityRecoveryAvailable(run) {
		return
	}
	fmt.Fprintln(w, st.muted("  After manually restoring and verifying source authority, record it with:"))
	fmt.Fprintf(w, "%s\n", st.muted("  source: `"+authorityRecoveryCommand(run, dokployTrafficSource)+"`"))
}

func issueActionFooter(apps []appView) string {
	hasFix, hasNext := false, false
	for _, app := range apps {
		for _, issue := range app.Issues {
			if issue.FixCommand(app.Name) != "" {
				hasFix = true
			} else if issue.NextStep() != "" {
				hasNext = true
			}
		}
	}
	switch {
	case hasFix && hasNext:
		return fmt.Sprintf("Run the shown `fix:` commands and use the `next:` notes as a checklist, then re-run `%s` to recheck.", bortCommand(""))
	case hasFix:
		return fmt.Sprintf("Run the shown `fix:` commands, then re-run `%s` to recheck.", bortCommand(""))
	case hasNext:
		return fmt.Sprintf("Use the `next:` notes as a checklist. Re-run `%s` after changing Coolify/Dokploy settings or the bundle.", bortCommand(""))
	default:
		return fmt.Sprintf("Resolve the issues above, then re-run `%s` to recheck.", bortCommand(""))
	}
}

func appliedFooter(applied runApplied) string {
	if len(applied.Steps) == 0 {
		return ""
	}
	ok, errs := 0, 0
	for _, step := range applied.Steps {
		switch step.Status {
		case "ok", "skipped":
			ok++
		case "error":
			errs++
		}
	}
	parts := []string{fmt.Sprintf("Applied: %d step(s) recorded", len(applied.Steps))}
	if ok > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", ok))
	}
	if errs > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", errs))
	}
	return strings.Join(parts, " · ")
}

func severityForHealth(h appHealth) severity {
	switch h {
	case appHealthReady:
		return sevGood
	case appHealthBlocked:
		return sevBad
	default:
		return sevWarn
	}
}

func severityForGlyph(g string) severity {
	switch g {
	case "✓":
		return sevGood
	case "!":
		return sevWarn
	case "✕":
		return sevBad
	default:
		return sevDim
	}
}

func severityForReadiness(r preparer.Readiness) severity {
	switch r {
	case preparer.ReadinessBlocked:
		return sevBad
	case preparer.ReadinessNeedsInput:
		return sevWarn
	case preparer.ReadinessNeedsDecision:
		return sevDim
	default:
		return sevGood
	}
}

func overallAppHealth(apps []appView) appHealth {
	overall := appHealthReady
	for _, app := range apps {
		if healthRank(app.Health) > healthRank(overall) {
			overall = app.Health
		}
	}
	return overall
}

func healthGlyph(h appHealth) string {
	switch h {
	case appHealthReady:
		return "✓"
	case appHealthBlocked:
		return "✕"
	default:
		return "!"
	}
}

func severityGlyph(r preparer.Readiness) string {
	switch r {
	case preparer.ReadinessBlocked:
		return "✕"
	case preparer.ReadinessNeedsInput:
		return "!"
	case preparer.ReadinessNeedsDecision:
		return "?"
	default:
		return "·"
	}
}

func writeEnvFileValues(path, templatePath string, values map[string]string) error {
	contents, err := readFileNoFollow(path)
	if err != nil {
		if !os.IsNotExist(err) || templatePath == "" {
			return err
		}
		contents, err = readFileNoFollow(templatePath)
		if err != nil {
			return err
		}
	}
	seen := map[string]struct{}{}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	for index, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if value, hit := values[key]; hit {
			lines[index] = key + "=" + formatEnvValue(value)
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, key+"="+formatEnvValue(values[key]))
	}
	if err := writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if path != templatePath && templatePath != "" {
		return ensureEnvTemplateKeys(templatePath, values)
	}
	return nil
}

func ensureEnvTemplateKeys(path string, values map[string]string) error {
	contents, err := readFileNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			lines := make([]string, 0, len(keys))
			for _, key := range keys {
				lines = append(lines, key+"=")
			}
			return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		}
		return err
	}
	seen := map[string]struct{}{}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		key = strings.TrimSpace(key)
		if ok && key != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, key+"=")
	}
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func formatEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\n\r#'\"") {
		return strconv.Quote(value)
	}
	return value
}
