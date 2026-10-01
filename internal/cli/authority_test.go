package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRecoverAuthorityRequiresExactConfirmationWithoutMutation(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "confirm-run")
	runPath := filepath.Join(run.Run.RunDir, "run.json")
	ownerPath, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	runBefore, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}

	err = runRecoverAuthority(context.Background(), []string{"--run", "confirm-run", "--authority", "source", "--confirm", "recover the source"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `must be exactly "recover confirm-run as source"`) {
		t.Fatalf("expected exact confirmation refusal, got %v", err)
	}
	runAfter, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerAfter, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runBefore, runAfter) || !bytes.Equal(ownerBefore, ownerAfter) {
		t.Fatal("wrong confirmation mutated run or owner state")
	}
}

func TestRecoverAuthorityRefusesDifferentOwnerWithoutMutation(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "blocked-run")
	other := migrationRun{Name: "other-run", RunDir: filepath.Join(t.TempDir(), "other-run"), CreatedAt: time.Now().UTC(), BundleDigest: "other"}
	otherID, err := dokployTrafficRunID(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDokployTrafficOwner(dokployTrafficOwner{RunID: otherID, RunName: other.Name, RunDir: other.RunDir, TargetOrigin: "http://127.0.0.1:3030", Authority: dokployTrafficPending}); err != nil {
		t.Fatal(err)
	}
	runPath := filepath.Join(run.Run.RunDir, "run.json")
	runBefore, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerPath, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}

	err = runRecoverAuthority(context.Background(), []string{"--run", "blocked-run", "--authority", "source", "--confirm", "recover blocked-run as source"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("owned by migration run %q at %q", other.Name, other.RunDir)) {
		t.Fatalf("expected different-owner refusal, got %v", err)
	}
	runAfter, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runBefore, runAfter) {
		t.Fatal("different-owner refusal mutated run state")
	}
	ownerAfter, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ownerBefore, ownerAfter) {
		t.Fatal("different-owner refusal mutated owner state")
	}
}

func TestRecoverAuthorityFinalizesSourceAndReleasesHost(t *testing.T) {
	writeAmbiguousAuthorityRun(t, "source-run")
	var output strings.Builder
	args := []string{"--run", "source-run", "--authority", "source", "--confirm", "recover source-run as source"}
	if err := runRecoverAuthority(context.Background(), args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	completed, err := loadMigrationRun("source-run")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.ResolvedAuthority != dokployTrafficSource || completed.Run.AuthorityResolvedAt == nil || completed.Run.RollbackStartedAt == nil || completed.Run.RolledBackAt == nil || completed.Run.AuthorityFinalizedAt == nil {
		t.Fatalf("source recovery lifecycle incomplete: %#v", completed.Run)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("source recovery owner = %#v, found=%t err=%v", owner, found, err)
	}
	other := migrationRun{Name: "next-run", RunDir: filepath.Join(t.TempDir(), "next-run"), CreatedAt: time.Now().UTC(), BundleDigest: "next"}
	plan := dokploy.Plan{Steps: []dokploy.Step{{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"}}}
	if err := ensureDokployTrafficRunAvailable(other, plan); err != nil {
		t.Fatalf("released source recovery blocked a new run: %v", err)
	}
	if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatalf("completed source recovery was not idempotent: %v", err)
	}
	if phase := migrationRunPhase(completed); phase != "rolled back" {
		t.Fatalf("source recovery phase=%q, want rolled back", phase)
	}
}

func TestRecoverAuthorityKeepsHostOwnershipWhenPinCleanupFails(t *testing.T) {
	for _, authority := range []string{dokployTrafficSource, dokployTrafficTarget} {
		t.Run(authority, func(t *testing.T) {
			run := writeAmbiguousAuthorityRun(t, "pin-cleanup-"+authority)
			previous := releaseAuthorityStagingVolumePins
			calls := 0
			releaseAuthorityStagingVolumePins = func(_ context.Context, _ loadedMigrationRun, plan dokploy.Plan, targetAuthority bool) error {
				calls++
				if plan.RunName != run.Run.Name || plan.RunDir != run.Run.RunDir || plan.RunID == "" || targetAuthority != (authority == dokployTrafficTarget) {
					t.Fatalf("pin cleanup received incomplete recovery identity: plan=%#v target=%t", plan, targetAuthority)
				}
				if calls == 1 {
					return errors.New("pin cleanup failed")
				}
				return nil
			}
			t.Cleanup(func() { releaseAuthorityStagingVolumePins = previous })

			args := []string{"--run", run.Run.Name, "--authority", authority, "--confirm", authorityRecoveryConfirmation(run.Run, authority)}
			if authority == dokployTrafficTarget {
				args = []string{"--run", run.Run.Name, "--authority", authority, "--source-retired", "--confirm", authorityRecoverySourceRetiredConfirmation(run.Run)}
			}
			err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "host ownership remains held") || !strings.Contains(err.Error(), "pin cleanup failed") {
				t.Fatalf("expected pin cleanup failure to retain host ownership, got %v", err)
			}
			owner, found, err := readDokployTrafficOwner()
			if err != nil || !found || owner.Authority != authority {
				t.Fatalf("pin cleanup failure released or changed owner: owner=%#v found=%t err=%v", owner, found, err)
			}
			interrupted, err := loadMigrationRun(run.Run.Name)
			if err != nil {
				t.Fatal(err)
			}
			if authority == dokployTrafficSource && (interrupted.Run.RolledBackAt == nil || interrupted.Run.AuthorityFinalizedAt != nil) {
				t.Fatalf("source recovery did not stop at pin cleanup boundary: %#v", interrupted.Run)
			}
			if authority == dokployTrafficTarget && interrupted.Run.CommittedAt == nil {
				t.Fatalf("target recovery did not record retirement before pin cleanup: %#v", interrupted.Run)
			}

			if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
				t.Fatalf("pin cleanup retry did not finish recovery: %v", err)
			}
			owner, found, err = readDokployTrafficOwner()
			if err != nil || !found || owner.Authority != dokployTrafficReleased || calls != 2 {
				t.Fatalf("recovery retry did not release owner: owner=%#v found=%t calls=%d err=%v", owner, found, calls, err)
			}
		})
	}
}

func TestAuthorityRecoveryCarriesAppliedTransferEvidenceIntoPinCleanup(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "transfer-evidence")
	run.Applied.PlanVersion = appliedPlanV1Alpha3
	run.Applied.Steps = []appliedStep{{
		Index:  1,
		Kind:   string(dokploy.StepRestoreDataStore),
		App:    "api",
		Ref:    "postgres:db",
		Status: string(dokploy.StepStatusError),
	}}
	previous := releaseAuthorityStagingVolumePins
	called := false
	releaseAuthorityStagingVolumePins = func(_ context.Context, _ loadedMigrationRun, plan dokploy.Plan, targetAuthority bool) error {
		called = true
		if !targetAuthority || len(plan.StagingTransferApps) != 1 || plan.StagingTransferApps[0] != "api" {
			t.Fatalf("pin cleanup did not receive transfer evidence: plan=%#v target=%t", plan, targetAuthority)
		}
		return nil
	}
	t.Cleanup(func() { releaseAuthorityStagingVolumePins = previous })

	if err := releaseRecoveredAuthorityStagingVolumePins(context.Background(), run, true); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("pin cleanup was not invoked")
	}
}

func TestRecoverAuthorityFinalizesTargetForCommitAndRefusesAutomaticRollback(t *testing.T) {
	writeAmbiguousAuthorityRun(t, "target-run")
	args := []string{"--run", "target-run", "--authority", "target", "--confirm", "recover target-run as target"}
	var output strings.Builder
	if err := runRecoverAuthority(context.Background(), args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--source-retired") || strings.Contains(output.String(), "commit --apply") {
		t.Fatalf("target recovery pointed a Coolify source at commit --apply instead of manual retirement: %q", output.String())
	}
	completed, err := loadMigrationRun("target-run")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.ResolvedAuthority != dokployTrafficTarget || completed.Run.AuthorityResolvedAt == nil || completed.Run.LiveAppliedAt == nil {
		t.Fatalf("target recovery lifecycle incomplete: %#v", completed.Run)
	}
	if err := requireLiveApplySucceeded(completed); err != nil {
		t.Fatalf("manual target authority did not enable commit: %v", err)
	}
	if _, err := planAutomaticRollback(completed); err == nil || !strings.Contains(err.Error(), "manual target-authority recovery") {
		t.Fatalf("manual target authority allowed automatic rollback: %v", err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficTarget {
		t.Fatalf("target recovery owner = %#v, found=%t err=%v", owner, found, err)
	}
	if phase := migrationRunPhase(completed); phase != "applied" {
		t.Fatalf("target recovery phase=%q, want applied", phase)
	}
	if next := nextSafeStep(completed, nil); !strings.Contains(next.Action, "--source-retired") || !strings.Contains(next.Action, "even if it is already stopped") || strings.Contains(next.Action, "commit --apply") || strings.Contains(next.Action, "rollback --live") {
		t.Fatalf("target recovery next step is unsafe: %#v", next)
	}
	if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatalf("completed target recovery was not idempotent: %v", err)
	}
}

func TestRecoverTargetAuthorityFinalizesManualRetirementWhenSourceIsStale(t *testing.T) {
	writeAmbiguousAuthorityRun(t, "stale-source")
	previous := verifyLocalSourceRun
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error {
		return fmt.Errorf("reviewed source container was replaced")
	}
	t.Cleanup(func() { verifyLocalSourceRun = previous })
	var output strings.Builder
	args := []string{"--run", "stale-source", "--authority", "target", "--confirm", "recover stale-source as target"}
	err := runRecoverAuthority(context.Background(), args, &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--source-retired") || !strings.Contains(err.Error(), "recover stale-source as target with source retired") {
		t.Fatalf("stale source did not require explicit manual-retirement confirmation: %v", err)
	}
	unresolved, err := loadMigrationRun("stale-source")
	if err != nil {
		t.Fatal(err)
	}
	if unresolved.Run.ResolvedAuthority != "" || unresolved.Run.CommittedAt != nil {
		t.Fatalf("failed source attestation mutated authority lifecycle: %#v", unresolved.Run)
	}
	args = []string{"--run", "stale-source", "--authority", "target", "--source-retired", "--confirm", "recover stale-source as target with source retired"}
	if err := runRecoverAuthority(context.Background(), args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	completed, err := loadMigrationRun("stale-source")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.ResolvedAuthority != dokployTrafficTarget || completed.Run.LiveAppliedAt == nil || completed.Run.CommitStartedAt == nil || completed.Run.CommittedAt == nil {
		t.Fatalf("manual target finalization incomplete: %#v", completed.Run)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("manual target finalization owner = %#v, found=%t err=%v", owner, found, err)
	}
	if !strings.Contains(output.String(), "manual source retirement recorded") || !strings.Contains(output.String(), "did not mutate source resources") {
		t.Fatalf("manual target finalization output omitted its boundary: %s", output.String())
	}
	if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatalf("completed manual target finalization was not idempotent: %v", err)
	}
}

func TestRecoverTargetAuthorityResumesManualRetirementBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		interrupt func(migrationRun) error
	}{
		{name: "after target authority"},
		{name: "after retirement start", interrupt: markRunCommitStartedLocked},
		{name: "after retirement completion", interrupt: func(run migrationRun) error {
			if err := markRunCommitStartedLocked(run); err != nil {
				return err
			}
			return markRunCommittedLocked(run)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := writeAmbiguousAuthorityRun(t, strings.ReplaceAll(test.name, " ", "-"))
			if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficTarget); err != nil {
				t.Fatal(err)
			}
			if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
				t.Fatal(err)
			}
			if err := markRunLiveAppliedLocked(run.Run); err != nil {
				t.Fatal(err)
			}
			if test.interrupt != nil {
				if err := test.interrupt(run.Run); err != nil {
					t.Fatal(err)
				}
			}
			phrase := authorityRecoverySourceRetiredConfirmation(run.Run)
			args := []string{"--run", run.Run.Name, "--authority", "target", "--source-retired", "--confirm", phrase}
			if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			completed, err := loadMigrationRun(run.Run.Name)
			if err != nil {
				t.Fatal(err)
			}
			owner, found, err := readDokployTrafficOwner()
			if err != nil || !found || owner.Authority != dokployTrafficReleased || completed.Run.CommittedAt == nil {
				t.Fatalf("manual target recovery did not resume: owner=%#v found=%t err=%v run=%#v", owner, found, err, completed.Run)
			}
		})
	}
}

func TestRecoverTargetAuthorityAcceptsRetiredSourceAfterInterruptedCommit(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "interrupted-commit")
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommitStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	sourceArgs := []string{"--run", "interrupted-commit", "--authority", "source", "--confirm", "recover interrupted-commit as source"}
	err := runRecoverAuthority(context.Background(), sourceArgs, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "source retirement already started") || !strings.Contains(err.Error(), "--source-retired") {
		t.Fatalf("interrupted retirement accepted source authority: %v", err)
	}
	targetArgs := []string{"--run", "interrupted-commit", "--authority", "target", "--confirm", "recover interrupted-commit as target"}
	err = runRecoverAuthority(context.Background(), targetArgs, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "source retirement already started") {
		t.Fatalf("interrupted retirement accepted target authority without a retirement attestation: %v", err)
	}
	unresolved, err := loadMigrationRun("interrupted-commit")
	if err != nil {
		t.Fatal(err)
	}
	if unresolved.Run.ResolvedAuthority != "" || unresolved.Run.CommittedAt != nil {
		t.Fatalf("refused recovery mutated authority lifecycle: %#v", unresolved.Run)
	}
	retiredArgs := []string{"--run", "interrupted-commit", "--authority", "target", "--source-retired", "--confirm", authorityRecoverySourceRetiredConfirmation(run.Run)}
	if err := runRecoverAuthority(context.Background(), retiredArgs, io.Discard, io.Discard); err != nil {
		t.Fatalf("explicit retirement attestation was refused: %v", err)
	}
	completed, err := loadMigrationRun("interrupted-commit")
	if err != nil {
		t.Fatal(err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased || completed.Run.ResolvedAuthority != dokployTrafficTarget || completed.Run.CommittedAt == nil {
		t.Fatalf("interrupted retirement was not finalized: owner=%#v found=%t err=%v run=%#v", owner, found, err, completed.Run)
	}
}

func TestRecoverAuthorityResumesAfterDurableResolution(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "resume-run")
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficSource); err != nil {
		t.Fatal(err)
	}
	interrupted, err := loadMigrationRun("resume-run")
	if err != nil {
		t.Fatal(err)
	}
	if phase := migrationRunPhase(interrupted); phase != "authority-finalizing" {
		t.Fatalf("interrupted recovery phase=%q, want authority-finalizing", phase)
	}
	next := nextSafeStep(interrupted, nil)
	if !strings.Contains(next.Action, "recover-authority") || !strings.Contains(next.Action, "recover resume-run as source") {
		t.Fatalf("interrupted recovery did not expose its exact resume command: %#v", next)
	}
	if err := runRecoverAuthority(context.Background(), []string{"--run", "resume-run", "--authority", "source", "--confirm", "recover resume-run as source"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	completed, err := loadMigrationRun("resume-run")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.RolledBackAt == nil {
		t.Fatal("resumed authority recovery did not finish rollback lifecycle")
	}
}

func TestRecoverAuthorityResumesSourceOwnerRelease(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "release-run")
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficSource); err != nil {
		t.Fatal(err)
	}
	if err := markRunRollbackStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficSource(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunRolledBackLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	interrupted, err := loadMigrationRun("release-run")
	if err != nil {
		t.Fatal(err)
	}
	if phase := migrationRunPhase(interrupted); phase != "authority-finalizing" {
		t.Fatalf("unreleased source recovery phase=%q, want authority-finalizing", phase)
	}
	if err := runRecoverAuthority(context.Background(), []string{"--run", "release-run", "--authority", "source", "--confirm", "recover release-run as source"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("resumed source recovery owner = %#v, found=%t err=%v", owner, found, err)
	}
	completed, err := loadMigrationRun("release-run")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.AuthorityFinalizedAt == nil {
		t.Fatal("resumed source recovery did not record authority finalization")
	}
}

func TestSourceAuthorityRecoveryRequiresPersistedFinalizationWithoutHostState(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "portable-finalization")
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficSource); err != nil {
		t.Fatal(err)
	}
	if err := markRunRollbackStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunRolledBackLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	ownerPath, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ownerPath); err != nil {
		t.Fatal(err)
	}
	interrupted, err := loadMigrationRun("portable-finalization")
	if err != nil {
		t.Fatal(err)
	}
	if phase := migrationRunPhase(interrupted); phase != "authority-finalizing" {
		t.Fatalf("missing finalization phase=%q, want authority-finalizing", phase)
	}
	args := []string{"--run", "portable-finalization", "--authority", "source", "--confirm", "recover portable-finalization as source"}
	if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	completed, err := loadMigrationRun("portable-finalization")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.AuthorityFinalizedAt == nil || migrationRunPhase(completed) != "rolled back" {
		t.Fatalf("source authority finalization incomplete: %#v", completed.Run)
	}
}

func TestRecoverAuthorityGuidanceKeepsMatchingOwnerBoundRun(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "guided-run")
	if phase := migrationRunPhase(run); phase != "authority-ambiguous" {
		t.Fatalf("ambiguous owner-bound run phase=%q, want authority-ambiguous", phase)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{"AUTHORITY UNKNOWN", "recover guided-run as source", "recover guided-run as target"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("owner-bound authority guidance missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "start a fresh migration run") {
		t.Fatalf("owner-bound authority guidance stranded its durable owner:\n%s", output.String())
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "recover-authority") || strings.Contains(next.Action, "fresh migration run") {
		t.Fatalf("owner-bound next step is not recoverable: %#v", next)
	}
}

func TestRunMetadataRejectsIncompleteAuthorityResolution(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "invalid-resolution")
	run.Run.ResolvedAuthority = dokployTrafficSource
	run.Run.AuthorityResolvedAt = nil
	path := filepath.Join(run.Run.RunDir, "run.json")
	if err := writeJSONArtifact(path, run.Run); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMigrationRun("invalid-resolution"); err == nil || !strings.Contains(err.Error(), "incomplete manual authority resolution") {
		t.Fatalf("expected incomplete authority metadata refusal, got %v", err)
	}
}

func writeAmbiguousAuthorityRun(t *testing.T, name string) loadedMigrationRun {
	t.Helper()
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", DockerEngineID: "engine-reviewed"},
		Apps: []manifest.App{{
			Name:     "api",
			Services: []manifest.Service{{ID: "source-id", Name: "web", Image: "example/api:latest"}},
			Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "web", Port: "3000"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", name, "--observation-window", "0", "--rollback-window", "0"})
	markRunLocallyScanned(t, name, "coolify-local")
	run, err := loadMigrationRun(name)
	if err != nil {
		t.Fatal(err)
	}
	applied := newRunApplied(run.Run)
	applied.Steps = []appliedStep{{Index: 0, Kind: string(dokploy.StepPushImage), App: "api", Ref: "example/api:latest", Status: string(dokploy.StepStatusError), UpdatedAt: time.Now().UTC(), Error: "outcome unknown"}}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun(name)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRecoverAuthorityAcceptsDefiniteFailureWhileHostOwnerIsPending(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "definite-run")
	definite := false
	applied := newRunApplied(run.Run)
	applied.Steps = []appliedStep{{Index: 0, Kind: string(dokploy.StepPushImage), App: "api", Ref: "example/api:latest", Status: string(dokploy.StepStatusError), MutationAmbiguous: &definite, UpdatedAt: time.Now().UTC(), Error: "dokploy http 404"}}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	run, err := loadMigrationRun("definite-run")
	if err != nil {
		t.Fatal(err)
	}
	if runMayHaveAmbiguousAuthority(run) {
		t.Fatal("fixture must be a definite, unambiguous failure")
	}
	if err := validateAuthorityRecovery(run, dokployTrafficSource, false); err != nil {
		t.Fatalf("definite failure holding the pending host owner must stay recoverable: %v", err)
	}
	args := []string{"--run", "definite-run", "--authority", "source", "--confirm", "recover definite-run as source"}
	if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("owner = %#v, found=%t err=%v", owner, found, err)
	}

	resetDokployTrafficOwner(t)
	run.Run.ResolvedAuthority = ""
	run.Run.AuthorityResolvedAt = nil
	run.Run.RollbackStartedAt = nil
	run.Run.RolledBackAt = nil
	run.Run.AuthorityFinalizedAt = nil
	if err := validateAuthorityRecovery(run, dokployTrafficSource, false); err == nil || !strings.Contains(err.Error(), "authority recovery refused") {
		t.Fatalf("definite failure without host ownership must still be refused, got %v", err)
	}
}

func TestAuthorityRecoveryClientBindsTargetCredentialsToRunOrigin(t *testing.T) {
	origin, err := dokploy.NormalizeTokenBaseURL("http://127.0.0.1:3030")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dokploy.EnvBaseURL, "http://127.0.0.1:3030")
	t.Setenv(dokploy.EnvToken, "token-1")

	source, err := authorityRecoveryDokployClient(loadedMigrationRun{}, dokploy.Plan{}, false)
	if err != nil || source.BaseURL != "" {
		t.Fatalf("source recovery should not need Dokploy credentials: client=%#v err=%v", source, err)
	}
	stateless, err := authorityRecoveryDokployClient(loadedMigrationRun{}, dokploy.Plan{}, true)
	if err != nil || stateless.BaseURL != "" {
		t.Fatalf("target recovery without staged volumes should not need Dokploy credentials: client=%#v err=%v", stateless, err)
	}
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Name: "api-data", Target: "/data"}}
	staged := dokploy.Plan{
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Steps:   []dokploy.Step{{Kind: dokploy.StepSyncVolume, App: "api", Ref: "volume:web -> /data"}, {Kind: dokploy.StepPushImage, App: "api"}},
	}
	target, err := authorityRecoveryDokployClient(loadedMigrationRun{Applied: runApplied{TargetOrigin: origin}}, staged, true)
	if err != nil {
		t.Fatal(err)
	}
	if target.BaseURL != origin || target.Token != "token-1" {
		t.Fatalf("target recovery client is not configured for the run origin: %#v", target)
	}
	if _, err := authorityRecoveryDokployClient(loadedMigrationRun{Applied: runApplied{TargetOrigin: "http://127.0.0.1:4040"}}, staged, true); err == nil || !strings.Contains(err.Error(), "bound to Dokploy origin") {
		t.Fatalf("target recovery accepted credentials for another Dokploy origin: %v", err)
	}
}

func TestRecoverAuthorityRefusesTargetForUnfinishedStagedTransfer(t *testing.T) {
	resetDokployTrafficOwner(t)
	t.Chdir(t.TempDir())
	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", DockerEngineID: "engine-reviewed"},
		Apps: []manifest.App{{
			Name: "api",
			Services: []manifest.Service{{
				ID: "source-id", Name: "web", Image: "example/api:latest",
				Mounts: []manifest.Mount{{Type: "volume", Name: "api-data", Target: "/data"}},
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", "unfinished", "--observation-window", "0", "--rollback-window", "0"})
	markRunLocallyScanned(t, "unfinished", "coolify-local")
	run, err := loadMigrationRun("unfinished")
	if err != nil {
		t.Fatal(err)
	}
	applied := newRunApplied(run.Run)
	applied.Steps = []appliedStep{{Index: 1, Kind: string(dokploy.StepSyncVolume), App: "api", Ref: "volume:web -> /data", Status: string(dokploy.StepStatusError), UpdatedAt: time.Now().UTC(), Error: "copy interrupted"}}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if run, err = loadMigrationRun("unfinished"); err != nil {
		t.Fatal(err)
	}
	if incomplete, err := incompleteStagingTransferApps(run); err != nil || len(incomplete) != 1 || incomplete[0] != "api" {
		t.Fatalf("unfinished staged transfer was not detected: %v %v", incomplete, err)
	}

	err = runRecoverAuthority(context.Background(), []string{"--run", "unfinished", "--authority", "target", "--confirm", authorityRecoveryConfirmation(run.Run, dokployTrafficTarget)}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not finish") || !strings.Contains(err.Error(), "--authority source") {
		t.Fatalf("target recovery accepted an unfinished staged transfer: %v", err)
	}
	after, err := loadMigrationRun("unfinished")
	if err != nil {
		t.Fatal(err)
	}
	if after.Run.ResolvedAuthority != "" || after.Run.CommittedAt != nil {
		t.Fatalf("refused target recovery recorded authority: %#v", after.Run)
	}
	if guidance := authorityRecoveryInstruction(after) + authorityRecoveryNextStep(after, "").Action; strings.Contains(guidance, "--authority target") || !strings.Contains(guidance, "--authority source") {
		t.Fatalf("recovery guidance offered target recovery for an unfinished transfer: %q", guidance)
	}
	var status bytes.Buffer
	writeAuthorityRecoveryGuidance(&status, newStyler(&status), after, "")
	if strings.Contains(status.String(), "--authority target") || !strings.Contains(status.String(), "--authority source") {
		t.Fatalf("status offered target recovery for an unfinished transfer:\n%s", status.String())
	}

	retried := newRunApplied(after.Run)
	if err := writeRunApplied(runArtifactPath(after.Run.RunDir, after.Run.Artifacts.Applied), retried); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after.Run.RunDir, "migrated-volumes.json"), []byte(`{"apiVersion":"bort.migrated-volumes/v1alpha2","startedApps":["api"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if after, err = loadMigrationRun("unfinished"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--run", "unfinished", "--authority", "target", "--confirm", authorityRecoveryConfirmation(after.Run, dokployTrafficTarget)},
		{"--run", "unfinished", "--authority", "target", "--source-retired", "--confirm", authorityRecoverySourceRetiredConfirmation(after.Run)},
	} {
		err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "did not finish") {
			t.Fatalf("target recovery ignored durable transfer evidence after the ledger was trimmed (%v): %v", args, err)
		}
	}
	final, err := loadMigrationRun("unfinished")
	if err != nil {
		t.Fatal(err)
	}
	owner, found, err := readDokployTrafficOwner()
	if final.Run.ResolvedAuthority != "" || final.Run.CommittedAt != nil || err != nil || !found || owner.Authority != dokployTrafficPending {
		t.Fatalf("refused target recovery changed run or owner state: run=%#v owner=%#v err=%v", final.Run, owner, err)
	}
}

func TestTargetRecoveryValidatesStagingVolumesBeforeRecordingAuthority(t *testing.T) {
	for _, sourceRetired := range []bool{false, true} {
		t.Run(fmt.Sprintf("source-retired=%t", sourceRetired), func(t *testing.T) {
			run := writeAmbiguousAuthorityRun(t, fmt.Sprintf("target-validate-%t", sourceRetired))
			previous := validateAuthorityStagingVolumePins
			validateAuthorityStagingVolumePins = func(_ context.Context, _ loadedMigrationRun, _ dokploy.Plan, targetAuthority bool) error {
				if !targetAuthority {
					t.Fatal("target recovery validated source authority")
				}
				return errors.New("migrated volume bort-v has no target container")
			}
			t.Cleanup(func() { validateAuthorityStagingVolumePins = previous })
			args := []string{"--run", run.Run.Name, "--authority", "target", "--confirm", authorityRecoveryConfirmation(run.Run, dokployTrafficTarget)}
			if sourceRetired {
				args = []string{"--run", run.Run.Name, "--authority", "target", "--source-retired", "--confirm", authorityRecoverySourceRetiredConfirmation(run.Run)}
			}
			err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "has no target container") {
				t.Fatalf("target recovery did not refuse on staging validation: %v", err)
			}
			after, err := loadMigrationRun(run.Run.Name)
			if err != nil {
				t.Fatal(err)
			}
			if after.Run.ResolvedAuthority != "" || after.Run.CommittedAt != nil || after.Run.LiveAppliedAt != nil {
				t.Fatalf("refused target recovery recorded state: %#v", after.Run)
			}
			if _, err := os.Stat(filepath.Join(after.Run.RunDir, "run.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
