package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
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
	if next := nextSafeStep(completed, nil); !strings.Contains(next.Action, "--source-retired") || !strings.Contains(next.Action, "disable future Coolify deployments") || strings.Contains(next.Action, "commit --apply") || strings.Contains(next.Action, "rollback --live") {
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
