package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aikins01/bort/internal/target/dokploy"
)

const dokployTrafficOwnerAPIVersion = "bort.dokploy-traffic-owner/v1alpha1"

const (
	dokployTrafficPending  = "pending"
	dokployTrafficTarget   = "target"
	dokployTrafficSource   = "source"
	dokployTrafficReleased = "released"
)

type dokployTrafficOwner struct {
	APIVersion         string    `json:"apiVersion"`
	RunID              string    `json:"runId"`
	RunName            string    `json:"runName"`
	RunDir             string    `json:"runDir,omitempty"`
	TargetOrigin       string    `json:"targetOrigin"`
	TargetCredentialID string    `json:"targetCredentialId,omitempty"`
	Authority          string    `json:"authority"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func trafficHandoffStepIndex(plan dokploy.Plan) int {
	for index, step := range plan.Steps {
		if step.Kind == dokploy.StepStopCoolifyProxy {
			return index
		}
	}
	return -1
}

func dokployOwnershipStepIndex(plan dokploy.Plan) int {
	if len(plan.Steps) == 0 {
		return -1
	}
	return 0
}

func planChangesDokployTraffic(plan dokploy.Plan) bool {
	for _, step := range plan.Steps {
		if step.Kind == dokploy.StepStopCoolifyProxy || step.Kind == dokploy.StepStopDokployProxy {
			return true
		}
	}
	return false
}

func planRequiresDokployHostOwner(run loadedMigrationRun, plan dokploy.Plan) bool {
	return planChangesDokployTraffic(plan) || run.Applied.RecoveryProtocol == appliedRecoveryProtocol
}

func dokployTrafficRunID(run migrationRun) (string, error) {
	runDir, err := dokployTrafficRunDir(run)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(filepath.Clean(runDir) + "\n" + run.Name + "\n" + run.CreatedAt.UTC().Format(time.RFC3339Nano) + "\n" + run.BundleDigest))
	return hex.EncodeToString(digest[:]), nil
}

func dokployTrafficRunDir(run migrationRun) (string, error) {
	runDir, err := filepath.Abs(filepath.FromSlash(run.RunDir))
	if err != nil {
		return "", fmt.Errorf("resolve migration run directory for Dokploy traffic ownership: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(runDir); err == nil {
		runDir = resolved
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve migration run directory for Dokploy traffic ownership: %w", err)
	}
	return filepath.Clean(runDir), nil
}

func dokployCredentialID(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func dokployOwnerRunLabel(owner dokployTrafficOwner) string {
	if owner.RunDir != "" {
		return fmt.Sprintf("migration run %q at %q", owner.RunName, owner.RunDir)
	}
	return fmt.Sprintf("migration run %q", owner.RunName)
}

func readDokployTrafficOwner() (dokployTrafficOwner, bool, error) {
	path, err := dokployTrafficOwnerPath()
	if err != nil {
		return dokployTrafficOwner{}, false, err
	}
	if path == "" {
		return dokployTrafficOwner{}, false, nil
	}
	contents, err := readFileNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dokployTrafficOwner{}, false, nil
		}
		return dokployTrafficOwner{}, false, fmt.Errorf("read Dokploy traffic owner %s: %w", path, err)
	}
	var owner dokployTrafficOwner
	if err := json.Unmarshal(contents, &owner); err != nil {
		return dokployTrafficOwner{}, false, fmt.Errorf("decode Dokploy traffic owner %s: %w", path, err)
	}
	if owner.APIVersion != dokployTrafficOwnerAPIVersion || owner.RunID == "" || owner.RunName == "" {
		return dokployTrafficOwner{}, false, fmt.Errorf("Dokploy traffic owner %s has invalid identity or API version %q", path, owner.APIVersion)
	}
	switch owner.Authority {
	case dokployTrafficPending, dokployTrafficTarget, dokployTrafficSource, dokployTrafficReleased:
	default:
		return dokployTrafficOwner{}, false, fmt.Errorf("Dokploy traffic owner %s has invalid authority %q", path, owner.Authority)
	}
	if owner.Authority != dokployTrafficSource && owner.Authority != dokployTrafficReleased && owner.TargetOrigin == "" {
		return dokployTrafficOwner{}, false, fmt.Errorf("Dokploy traffic owner %s has no target origin for %s authority", path, owner.Authority)
	}
	return owner, true, nil
}

func writeDokployTrafficOwner(owner dokployTrafficOwner) error {
	path, err := dokployTrafficOwnerPath()
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("host-wide Dokploy traffic ownership is unsupported on this platform")
	}
	if err := prepareDokployTrafficOwnerPath(path); err != nil {
		return err
	}
	owner.APIVersion = dokployTrafficOwnerAPIVersion
	owner.UpdatedAt = time.Now().UTC()
	if err := writeJSONArtifact(path, owner); err != nil {
		return fmt.Errorf("write Dokploy traffic owner %s: %w", path, err)
	}
	return nil
}

func ensureDokployTrafficRunAvailable(run migrationRun, plan dokploy.Plan) error {
	if dokployOwnershipStepIndex(plan) < 0 {
		return nil
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority == dokployTrafficReleased {
		return err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	if owner.RunID != runID {
		return fmt.Errorf("Dokploy host is owned by %s; refusing run %q because shared target resources cannot be mutated by independent runs safely", dokployOwnerRunLabel(owner), run.Name)
	}
	return nil
}

func ensureStandaloneDokployInitAvailable() error {
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority == dokployTrafficReleased {
		return err
	}
	return fmt.Errorf("Dokploy host is owned by %s with %s authority; refusing standalone init-target until that migration is committed or rolled back", dokployOwnerRunLabel(owner), owner.Authority)
}

func conflictingDokployHostOwner(run migrationRun) (dokployTrafficOwner, bool, error) {
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority == dokployTrafficReleased {
		return owner, false, err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return dokployTrafficOwner{}, false, err
	}
	return owner, owner.RunID != runID, nil
}

func runHoldsPendingDokployOwner(run migrationRun) (bool, error) {
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficPending {
		return false, err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return false, err
	}
	return owner.RunID == runID, nil
}

func validateInlineDokploySetup(run migrationRun, applied runApplied, targetOrigin string) error {
	if applied.TargetOrigin != "" || len(applied.Steps) > 0 || len(applied.Apps) > 0 || applied.SucceededAt != nil {
		return fmt.Errorf("migration run %q already has a bound or in-progress Dokploy target; restore the exact original target credential and reachability instead of running inline setup", run.Name)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority == dokployTrafficReleased {
		return err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	if owner.RunID != runID {
		return fmt.Errorf("Dokploy host is owned by %s; refusing inline target setup for run %q", dokployOwnerRunLabel(owner), run.Name)
	}
	if targetOrigin != "" && owner.TargetOrigin != targetOrigin {
		return fmt.Errorf("migration run %q owns Dokploy traffic for target %q, not %q", run.Name, owner.TargetOrigin, targetOrigin)
	}
	if owner.TargetCredentialID != "" {
		return fmt.Errorf("migration run %q is bound to an existing Dokploy credential identity; restore the exact original credential instead of creating or storing a replacement", run.Name)
	}
	return nil
}

func targetMutationMissingDurableOwner(run loadedMigrationRun) (bool, error) {
	if run.Applied.APIVersion != appliedAPIVersion || run.Applied.RecoveryProtocol != appliedRecoveryProtocol {
		return false, nil
	}
	plan := livePlanForApplied(run, run.Applied)
	ownershipIndex := dokployOwnershipStepIndex(plan)
	if ownershipIndex < 0 {
		return false, nil
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return false, err
	}
	if found && owner.Authority != dokployTrafficReleased {
		runID, err := dokployTrafficRunID(run.Run)
		if err != nil {
			return false, err
		}
		if owner.RunID == runID {
			return false, nil
		}
	}
	if completedApplyPrefix(plan.Steps, run.Applied) > ownershipIndex {
		return true, nil
	}
	for _, recorded := range run.Applied.Steps {
		if recorded.Index == ownershipIndex && appliedStepMatches(recorded, plan.Steps[ownershipIndex]) && appliedStepMayHaveRun(recorded) {
			return true, nil
		}
	}
	return false, nil
}

func validateDokployTrafficResume(run migrationRun, plan dokploy.Plan, applied runApplied, targetOrigin, targetCredentialID string, resumeFrom int) error {
	ownershipIndex := dokployOwnershipStepIndex(plan)
	if ownershipIndex < 0 {
		return nil
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	if !found || owner.Authority == dokployTrafficReleased {
		firstMutationAttempted := false
		for _, recorded := range applied.Steps {
			if recorded.Index == ownershipIndex && appliedStepMatches(recorded, plan.Steps[ownershipIndex]) && appliedStepMayHaveRun(recorded) {
				firstMutationAttempted = true
				break
			}
		}
		if resumeFrom > ownershipIndex || firstMutationAttempted {
			return fmt.Errorf("migration run %q changed Dokploy target resources without a matching durable host owner; refusing to infer authority", run.Name)
		}
		return nil
	}
	if owner.RunID != runID {
		return fmt.Errorf("Dokploy host is owned by %s; refusing run %q because shared target resources cannot be mutated by independent runs safely", dokployOwnerRunLabel(owner), run.Name)
	}
	if owner.TargetOrigin != targetOrigin {
		return fmt.Errorf("migration run %q owns Dokploy traffic for target %q, not %q", run.Name, owner.TargetOrigin, targetOrigin)
	}
	if owner.TargetCredentialID == "" {
		for _, recorded := range applied.Steps {
			if recorded.Index == ownershipIndex && appliedStepMatches(recorded, plan.Steps[ownershipIndex]) && appliedStepMayHaveRun(recorded) {
				return fmt.Errorf("migration run %q attempted a Dokploy target mutation without a durable credential identity; refusing to infer target organization authority", run.Name)
			}
		}
	} else if owner.TargetCredentialID != targetCredentialID {
		return fmt.Errorf("migration run %q owns Dokploy traffic with a different target credential identity; restore the exact Dokploy API key this run claimed the host with and retry", run.Name)
	}
	return nil
}

func claimDokployHostOwnership(run migrationRun, targetOrigin, targetCredentialID string) error {
	if targetCredentialID == "" {
		return fmt.Errorf("cannot claim Dokploy host ownership without a target credential identity")
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return err
	}
	if found && owner.Authority != dokployTrafficReleased {
		if owner.RunID != runID {
			return fmt.Errorf("Dokploy host is owned by %s; refusing run %q", dokployOwnerRunLabel(owner), run.Name)
		}
		if owner.TargetOrigin != targetOrigin {
			return fmt.Errorf("migration run %q owns Dokploy traffic for target %q, not %q", run.Name, owner.TargetOrigin, targetOrigin)
		}
		if owner.TargetCredentialID != "" && owner.TargetCredentialID != targetCredentialID {
			return fmt.Errorf("migration run %q owns Dokploy traffic with a different target credential identity; restore the exact Dokploy API key this run claimed the host with and retry", run.Name)
		}
	}
	runDir, err := dokployTrafficRunDir(run)
	if err != nil {
		return err
	}
	return writeDokployTrafficOwner(dokployTrafficOwner{RunID: runID, RunName: run.Name, RunDir: runDir, TargetOrigin: targetOrigin, TargetCredentialID: targetCredentialID, Authority: dokployTrafficPending})
}

func markDokployTrafficTarget(run migrationRun, targetOrigin string) error {
	return updateDokployTrafficAuthority(run, targetOrigin, dokployTrafficTarget)
}

func markDokployTrafficSource(run migrationRun) error {
	return updateDokployTrafficAuthority(run, "", dokployTrafficSource)
}

func releaseDokployTrafficOwner(run migrationRun) error {
	return releaseDokployHostOwner(run, dokployTrafficSource)
}

func releaseDokployTargetOwner(run migrationRun) error {
	return releaseDokployHostOwner(run, dokployTrafficTarget)
}

func releaseDokployHostOwner(run migrationRun, expectedAuthority string) error {
	if run.HostOwnerReleaseStartedAt == nil {
		if err := markRunHostOwnerReleaseStartedLocked(run); err != nil {
			return fmt.Errorf("record Dokploy host-owner release start: %w", err)
		}
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return err
	}
	if !found {
		if run.HostOwnerReleaseStartedAt != nil {
			return nil
		}
		return fmt.Errorf("migration run %q has no durable Dokploy host owner to release", run.Name)
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	if owner.RunID != runID {
		if run.HostOwnerReleaseStartedAt != nil {
			return nil
		}
		return fmt.Errorf("migration run %q cannot release Dokploy host ownership held by %s", run.Name, dokployOwnerRunLabel(owner))
	}
	if owner.Authority == dokployTrafficReleased {
		return nil
	}
	if owner.Authority != expectedAuthority {
		return fmt.Errorf("migration run %q cannot release Dokploy host ownership with %s authority", run.Name, owner.Authority)
	}
	owner.Authority = dokployTrafficReleased
	return writeDokployTrafficOwner(owner)
}

func ensureDokploySourceCleanupAvailable(run migrationRun) error {
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found {
		return err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	if owner.RunID == runID && owner.Authority == dokployTrafficPending {
		return nil
	}
	if owner.RunID == runID {
		return fmt.Errorf("migration run %q cannot resume historical source writers after %s authority was established", run.Name, owner.Authority)
	}
	return fmt.Errorf("migration run %q cannot resume historical source writers because Dokploy host ownership belongs to %s with %s authority", run.Name, dokployOwnerRunLabel(owner), owner.Authority)
}

func requireDokployTrafficTarget(run migrationRun, allowCompletedRollback bool) error {
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	validAuthority := owner.Authority == dokployTrafficTarget || allowCompletedRollback && owner.Authority == dokployTrafficSource
	if !found || owner.RunID != runID || !validAuthority {
		if found {
			return fmt.Errorf("migration run %q does not own target traffic; current durable owner is %s with %s authority", run.Name, dokployOwnerRunLabel(owner), owner.Authority)
		}
		return fmt.Errorf("migration run %q has no durable host-wide Dokploy traffic owner", run.Name)
	}
	return nil
}

func validateCurrentDokployTrafficOwner(run loadedMigrationRun, allowSource bool) error {
	if run.Applied.APIVersion != appliedAPIVersion || run.Applied.RecoveryProtocol != appliedRecoveryProtocol {
		return nil
	}
	return requireDokployTrafficTarget(run.Run, allowSource)
}

func completedDokployOwnerFinalization(run loadedMigrationRun) (bool, error) {
	if run.Run.Target != "dokploy" || run.Applied.APIVersion != appliedAPIVersion || run.Applied.RecoveryProtocol != appliedRecoveryProtocol || run.Run.CommittedAt == nil && run.Run.RolledBackAt == nil {
		return false, nil
	}
	if run.Run.ResolvedAuthority == dokployTrafficSource && run.Run.AuthorityFinalizedAt != nil {
		return false, nil
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return false, fmt.Errorf("verify completed migration host ownership: %w", err)
	}
	releaseStarted := run.Run.HostOwnerReleaseStartedAt != nil
	if !found {
		if releaseStarted {
			return false, nil
		}
		return false, fmt.Errorf("completed migration run %q has no durable Dokploy host owner", run.Run.Name)
	}
	runID, err := dokployTrafficRunID(run.Run)
	if err != nil {
		return false, err
	}
	if owner.RunID != runID {
		if releaseStarted {
			return false, nil
		}
		return false, fmt.Errorf("completed migration run %q cannot verify host-owner finalization because ownership belongs to %s with %s authority", run.Run.Name, dokployOwnerRunLabel(owner), owner.Authority)
	}
	if owner.Authority == dokployTrafficReleased {
		return false, nil
	}
	expectedAuthority := dokployTrafficTarget
	if run.Run.RolledBackAt != nil {
		expectedAuthority = dokployTrafficSource
	}
	if owner.Authority != expectedAuthority {
		return false, fmt.Errorf("completed migration run %q expected %s authority before host-owner release, but found %s authority", run.Run.Name, expectedAuthority, owner.Authority)
	}
	return true, nil
}

func ensureDokployTrafficTargetOwner(ctx context.Context, run loadedMigrationRun, client *dokploy.Client, allowCompletedRollback bool) error {
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return err
	}
	if found && owner.Authority != dokployTrafficPending {
		return requireDokployTrafficTarget(run.Run, allowCompletedRollback)
	}
	legacyCompleted := appliedPlanVersion(run.Applied) == appliedPlanV1Alpha1 && run.Applied.SucceededAt != nil
	if !legacyCompleted {
		return requireDokployTrafficTarget(run.Run, allowCompletedRollback)
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		return err
	}
	if run.Applied.TargetOrigin == "" {
		return fmt.Errorf("completed legacy run %q has no persisted Dokploy origin; refusing to bind prior live execution to current credentials. preserve both sides and establish traffic authority manually", run.Run.Name)
	}
	if found {
		runID, err := dokployTrafficRunID(run.Run)
		if err != nil {
			return err
		}
		if owner.RunID != runID {
			return requireDokployTrafficTarget(run.Run, allowCompletedRollback)
		}
	}
	if client == nil {
		client, err = lookupDokployClient(run.Run.Target)
		if err != nil {
			return fmt.Errorf("load Dokploy credentials for legacy traffic-owner adoption: %w", err)
		}
	}
	if client.BaseURL != run.Applied.TargetOrigin {
		return fmt.Errorf("completed legacy run %q is bound to Dokploy origin %s, not %s", run.Run.Name, run.Applied.TargetOrigin, client.BaseURL)
	}
	if found {
		if owner.TargetOrigin != client.BaseURL {
			return fmt.Errorf("completed legacy run %q has pending Dokploy ownership for target %q, not %q", run.Run.Name, owner.TargetOrigin, client.BaseURL)
		}
		credentialID := dokployCredentialID(client.Token)
		if owner.TargetCredentialID == "" || owner.TargetCredentialID != credentialID {
			return fmt.Errorf("completed legacy run %q has pending Dokploy ownership with a missing or different target credential identity", run.Run.Name)
		}
	}
	if err := client.VerifySameDockerHost(ctx); err != nil {
		return fmt.Errorf("verify Dokploy target is on this Docker host before legacy traffic-owner adoption: %w", err)
	}
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("verify Dokploy target before legacy traffic-owner adoption: %w", err)
	}
	if err := client.AdoptTargetTrafficAuthority(ctx); err != nil {
		return fmt.Errorf("legacy traffic-owner adoption refused: %w", err)
	}
	if !found {
		if err := claimDokployHostOwnership(run.Run, client.BaseURL, dokployCredentialID(client.Token)); err != nil {
			return err
		}
	}
	if err := markDokployTrafficTarget(run.Run, client.BaseURL); err != nil {
		return err
	}
	return nil
}

func updateDokployTrafficAuthority(run migrationRun, targetOrigin, authority string) error {
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		return err
	}
	runID, err := dokployTrafficRunID(run)
	if err != nil {
		return err
	}
	if !found || owner.RunID != runID {
		return fmt.Errorf("migration run %q does not own the durable Dokploy traffic handoff", run.Name)
	}
	if targetOrigin != "" && owner.TargetOrigin != targetOrigin {
		return fmt.Errorf("migration run %q owns Dokploy traffic for target %q, not %q", run.Name, owner.TargetOrigin, targetOrigin)
	}
	owner.Authority = authority
	return writeDokployTrafficOwner(owner)
}
