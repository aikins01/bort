package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/aikins01/bort/internal/target/dokploy"
)

var releaseAuthorityStagingVolumePins = func(ctx context.Context, run loadedMigrationRun, plan dokploy.Plan, targetAuthority bool) error {
	client, err := authorityRecoveryDokployClient(run, targetAuthority)
	if err != nil {
		return err
	}
	return client.ReleaseStagingVolumePins(ctx, plan, targetAuthority)
}

func authorityRecoveryDokployClient(run loadedMigrationRun, targetAuthority bool) (*dokploy.Client, error) {
	if !targetAuthority {
		return &dokploy.Client{}, nil
	}
	client, err := lookupDokployClient(run.Run.Target)
	if err != nil {
		return nil, fmt.Errorf("load Dokploy credentials to verify target attachments: %w", err)
	}
	if err := validateAppliedTargetOrigin(run.Applied, client.BaseURL); err != nil {
		return nil, fmt.Errorf("verify target attachments: %w", err)
	}
	return client, nil
}

func runRecoverAuthority(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("recover-authority", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var runRef string
	var authority string
	var confirm string
	var sourceRetired bool
	fs.StringVar(&runRef, "run", "", "migration run name or directory")
	fs.StringVar(&authority, "authority", "", "manually verified authority: source or target")
	fs.StringVar(&confirm, "confirm", "", "confirm with the exact phrase: recover <run-name> as <authority>, or with --source-retired: recover <run-name> as target with source retired")
	fs.BoolVar(&sourceRetired, "source-retired", false, "confirm that source retirement was completed manually for target authority")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("recover-authority does not accept positional argument %q", fs.Arg(0))
	}
	if !flagSet(fs, "run") || strings.TrimSpace(runRef) == "" {
		return fmt.Errorf("recover-authority requires a non-empty --run value")
	}
	authority = strings.TrimSpace(authority)
	if authority != dokployTrafficSource && authority != dokployTrafficTarget {
		return fmt.Errorf("recover-authority requires --authority source or --authority target")
	}
	if sourceRetired && authority != dokployTrafficTarget {
		return fmt.Errorf("recover-authority --source-retired requires --authority target")
	}

	resolvedRun, err := resolveRunRef(runRef, false)
	if err != nil {
		return err
	}
	operationLock, err := acquireRunOperationLock(resolvedRun)
	if err != nil {
		return fmt.Errorf("recover authority for run %q: %w", runRef, err)
	}
	defer operationLock.Release()
	run, err := loadMigrationRun(resolvedRun)
	if err != nil {
		return err
	}
	phrase := authorityRecoveryConfirmation(run.Run, authority)
	if sourceRetired {
		phrase = authorityRecoverySourceRetiredConfirmation(run.Run)
	}
	if confirm != phrase {
		return fmt.Errorf("authority recovery confirmation must be exactly %q", phrase)
	}
	if run.Run.Target != "dokploy" {
		return fmt.Errorf("recover-authority is only supported for target dokploy, got %q", run.Run.Target)
	}

	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		return fmt.Errorf("lock Dokploy live operations: %w", err)
	}
	defer targetLock.Release()
	run, err = loadMigrationRun(resolvedRun)
	if err != nil {
		return err
	}
	if err := validateAuthorityRecovery(run, authority, sourceRetired); err != nil {
		return err
	}
	if sourceRetired && !planRequiresDokployHostOwner(run, dokploy.PlanForCommit(run.Prepare, run.Cutover)) {
		return recordOwnerlessSourceRetirement(run, stdout)
	}
	owner, found, err := matchingAuthorityRecoveryOwner(run.Run, authority)
	if err != nil {
		return err
	}
	if !found {
		if authority == dokployTrafficSource && run.Run.AuthorityFinalizedAt == nil {
			if err := releaseRecoveredAuthorityStagingVolumePins(ctx, run, false); err != nil {
				return fmt.Errorf("remove source-authority staging-volume pins before finalization: %w", err)
			}
			if err := markRunAuthorityFinalizedLocked(run.Run); err != nil {
				return fmt.Errorf("record source-authority finalization: %w", err)
			}
		}
		if authority == dokployTrafficTarget && run.Run.CommittedAt != nil {
			if err := releaseRecoveredAuthorityStagingVolumePins(ctx, run, true); err != nil {
				return fmt.Errorf("remove target-authority staging-volume pins after source retirement: %w", err)
			}
		}
		fmt.Fprintf(stdout, "Authority recovery already complete for run %s: %s authority.\n", run.Run.Name, authority)
		return nil
	}
	if authority == dokployTrafficTarget && !sourceRetired {
		var sourceAttestationErr error
		if err := validateLocalSourceAttestation(run); err != nil {
			sourceAttestationErr = err
		} else {
			sourceAttestationErr = verifyLocalSourceRun(ctx, run)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sourceAttestationErr != nil {
			return fmt.Errorf("target authority was not recorded because source attestation failed: %v; after manually retiring the source, run `%s`", sourceAttestationErr, authorityRecoverySourceRetiredCommand(run))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := markRunAuthorityResolvedLocked(run.Run, authority); err != nil {
		return fmt.Errorf("record manual %s-authority resolution: %w", authority, err)
	}
	if authority == dokployTrafficSource {
		if err := markRunRollbackStartedLocked(run.Run); err != nil {
			return fmt.Errorf("record manual source recovery start: %w", err)
		}
		if owner.Authority != dokployTrafficSource {
			if err := markDokployTrafficSource(run.Run); err != nil {
				return fmt.Errorf("record durable source authority: %w", err)
			}
		}
		if err := markRunRolledBackLocked(run.Run); err != nil {
			return fmt.Errorf("record manual source recovery completion: %w", err)
		}
		if err := releaseRecoveredAuthorityStagingVolumePins(ctx, run, false); err != nil {
			return fmt.Errorf("source authority was recorded, but staging-volume pins could not be removed; host ownership remains held so this recovery can be retried: %w", err)
		}
		if err := releaseDokployTrafficOwner(run.Run); err != nil {
			return fmt.Errorf("release source-authority host ownership: %w", err)
		}
		if err := markRunAuthorityFinalizedLocked(run.Run); err != nil {
			return fmt.Errorf("record source-authority finalization: %w", err)
		}
		fmt.Fprintf(stdout, "Authority recovery complete for run %s: source authority recorded, run rolled back, host ownership released.\n", run.Run.Name)
		return nil
	}

	if owner.Authority != dokployTrafficTarget {
		if err := markDokployTrafficTarget(run.Run, owner.TargetOrigin); err != nil {
			return fmt.Errorf("record durable target authority: %w", err)
		}
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		return fmt.Errorf("record manual target recovery completion: %w", err)
	}
	if sourceRetired {
		if err := markRunCommitStartedLocked(run.Run); err != nil {
			return fmt.Errorf("record manual source retirement start: %w", err)
		}
		if err := markRunCommittedLocked(run.Run); err != nil {
			return fmt.Errorf("record manual source retirement completion: %w", err)
		}
		if err := releaseRecoveredAuthorityStagingVolumePins(ctx, run, true); err != nil {
			return fmt.Errorf("target authority and source retirement were recorded, but staging-volume pins could not be removed; host ownership remains held so this recovery can be retried: %w", err)
		}
		if err := releaseDokployTargetOwner(run.Run); err != nil {
			return fmt.Errorf("release target-authority host ownership: %w", err)
		}
		fmt.Fprintf(stdout, "Authority recovery complete for run %s: target authority and manual source retirement recorded, host ownership released. Bort did not mutate source resources.\n", run.Run.Name)
		return nil
	}
	if coolifySourceRetirementRequired(run) {
		fmt.Fprintf(stdout, "Authority recovery complete for run %s: target authority recorded. When ready to retire the source, %s.\n", run.Run.Name, manualCoolifySourceRetirementAction(run))
		return nil
	}
	fmt.Fprintf(stdout, "Authority recovery complete for run %s: target authority recorded. Run `%s` when ready to retire the source.\n", run.Run.Name, runScopedCommand(run, "commit --apply"))
	return nil
}

func releaseRecoveredAuthorityStagingVolumePins(ctx context.Context, run loadedMigrationRun, targetAuthority bool) error {
	plan := livePlanForApplied(run, run.Applied)
	plan.RunName = run.Run.Name
	plan.RunDir = run.Run.RunDir
	runID, err := dokployTrafficRunID(run.Run)
	if err != nil {
		return err
	}
	plan.RunID = runID
	plan.TargetIdentities = appliedTargetIdentities(run.Applied)
	plan.StagingTransferApps = appliedStagingTransferApps(run.Applied)
	return releaseAuthorityStagingVolumePins(ctx, run, plan, targetAuthority)
}

func recordOwnerlessSourceRetirement(run loadedMigrationRun, stdout io.Writer) error {
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficTarget); err != nil {
		return fmt.Errorf("record manual target-authority resolution: %w", err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		return fmt.Errorf("record manual target recovery completion: %w", err)
	}
	if err := markRunCommitStartedLocked(run.Run); err != nil {
		return fmt.Errorf("record manual source retirement start: %w", err)
	}
	if err := markRunCommittedLocked(run.Run); err != nil {
		return fmt.Errorf("record manual source retirement completion: %w", err)
	}
	fmt.Fprintf(stdout, "Authority recovery complete for run %s: target authority and manual source retirement recorded; this run never held host ownership. Bort did not mutate source resources.\n", run.Run.Name)
	return nil
}

func validateAuthorityRecovery(run loadedMigrationRun, authority string, sourceRetired bool) error {
	if run.Run.PurgedAt != nil {
		return fmt.Errorf("authority recovery refused: run %q is complete and purged", run.Run.Name)
	}
	if run.Run.CommittedAt != nil {
		if run.Run.ResolvedAuthority == dokployTrafficTarget && authority == dokployTrafficTarget {
			return nil
		}
		return fmt.Errorf("authority recovery refused: run %q already committed target authority", run.Run.Name)
	}
	if run.Run.RolledBackAt != nil {
		if run.Run.ResolvedAuthority == dokployTrafficSource && authority == dokployTrafficSource {
			return nil
		}
		return fmt.Errorf("authority recovery refused: run %q already rolled back to source authority", run.Run.Name)
	}
	if run.Run.CommitStartedAt != nil && run.Run.ResolvedAuthority == "" {
		if authority == dokployTrafficTarget && sourceRetired {
			return nil
		}
		return fmt.Errorf("authority recovery refused: source retirement already started for run %q; complete source retirement manually, then run `%s`", run.Run.Name, authorityRecoverySourceRetiredCommand(run))
	}
	if run.Run.ResolvedAuthority != "" && run.Run.ResolvedAuthority != authority {
		return fmt.Errorf("authority recovery refused: run %q already recorded %s authority", run.Run.Name, run.Run.ResolvedAuthority)
	}
	if run.Run.ResolvedAuthority != "" {
		return nil
	}
	if runMayHaveAmbiguousAuthority(run) {
		return nil
	}
	if sourceRetired && authority == dokployTrafficTarget && (run.Run.LiveAppliedAt != nil || liveApplySucceeded(run)) {
		return nil
	}
	if run.Run.LiveAppliedAt != nil || liveApplySucceeded(run) || run.Run.RollbackStartedAt != nil {
		if _, err := planAutomaticRollback(run); err != nil {
			return nil
		}
	}
	held, err := runHoldsPendingDokployOwner(run.Run)
	if err != nil {
		return fmt.Errorf("verify durable Dokploy host owner: %w", err)
	}
	if held && run.Applied.SucceededAt == nil {
		return nil
	}
	return fmt.Errorf("authority recovery refused: run %q has no ambiguous authority, unavailable automatic rollback, or unreleased host ownership to resolve", run.Run.Name)
}

func matchingAuthorityRecoveryOwner(run migrationRun, authority string) (dokployTrafficOwner, bool, error) {
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return dokployTrafficOwner{}, false, fmt.Errorf("verify durable Dokploy host owner: %w", err)
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return dokployTrafficOwner{}, false, err
	}
	if !found || owner.RunID != runID {
		released := !found || owner.Authority == dokployTrafficReleased || run.HostOwnerReleaseStartedAt != nil
		if authority == dokployTrafficSource && run.ResolvedAuthority == authority && run.RolledBackAt != nil && released {
			return owner, false, nil
		}
		if authority == dokployTrafficTarget && run.ResolvedAuthority == authority && run.CommittedAt != nil && released {
			return owner, false, nil
		}
		if found {
			return dokployTrafficOwner{}, false, fmt.Errorf("authority recovery refused: Dokploy host is owned by %s with %s authority", dokployOwnerRunLabel(owner), owner.Authority)
		}
		return dokployTrafficOwner{}, false, fmt.Errorf("authority recovery refused: run %q has no matching durable Dokploy host owner", run.Name)
	}
	if owner.Authority == dokployTrafficReleased {
		if authority == dokployTrafficSource && run.ResolvedAuthority == authority && run.RolledBackAt != nil {
			return owner, false, nil
		}
		if authority == dokployTrafficTarget && run.ResolvedAuthority == authority && run.CommittedAt != nil {
			return owner, false, nil
		}
		return dokployTrafficOwner{}, false, fmt.Errorf("authority recovery refused: run %q released host ownership before recovery completed", run.Name)
	}
	if owner.Authority == dokployTrafficSource && authority != dokployTrafficSource {
		return dokployTrafficOwner{}, false, fmt.Errorf("authority recovery refused: run %q already has durable source authority", run.Name)
	}
	return owner, true, nil
}

func authorityRecoveryConfirmation(run migrationRun, authority string) string {
	return fmt.Sprintf("recover %s as %s", run.Name, authority)
}

func authorityRecoverySourceRetiredConfirmation(run migrationRun) string {
	return fmt.Sprintf("recover %s as target with source retired", run.Name)
}

func authorityRecoveryCommand(run loadedMigrationRun, authority string) string {
	phrase := authorityRecoveryConfirmation(run.Run, authority)
	return runScopedCommand(run, fmt.Sprintf("recover-authority --authority %s --confirm %s", authority, shellQuote(phrase)))
}

func authorityRecoverySourceRetiredCommand(run loadedMigrationRun) string {
	phrase := authorityRecoverySourceRetiredConfirmation(run.Run)
	return runScopedCommand(run, fmt.Sprintf("recover-authority --authority target --source-retired --confirm %s", shellQuote(phrase)))
}

// pendingAuthorityRecoveryCommand names the command that finishes a
// recorded but unfinalized authority decision. Ownerless target recovery
// records its decision before the lifecycle marks, so resuming it needs
// the same source-retired attestation that started it.
func pendingAuthorityRecoveryCommand(run loadedMigrationRun) string {
	if run.Run.ResolvedAuthority == dokployTrafficTarget && !planRequiresDokployHostOwner(run, dokploy.PlanForCommit(run.Prepare, run.Cutover)) {
		return authorityRecoverySourceRetiredCommand(run)
	}
	return authorityRecoveryCommand(run, run.Run.ResolvedAuthority)
}

func authorityRecoveryInstruction(run loadedMigrationRun) string {
	if authorityRecoveryAvailable(run) {
		return fmt.Sprintf("inspect and preserve both sides, manually fence the other side and verify authority, then run `%s` for source or `%s` for target", authorityRecoveryCommand(run, dokployTrafficSource), authorityRecoveryCommand(run, dokployTrafficTarget))
	}
	return "inspect and preserve both sides, establish writer and traffic authority manually, then start a fresh migration run"
}
