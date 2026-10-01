package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRunCommitWritesTextPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), []string{"--bundle", dir, "--target", "dokploy"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Commit plan: " + dir + " -> dokploy",
		"[yellow] api",
		"readiness: needs_decision",
		"cutover readiness: needs_decision",
		"rollback window: 3600s",
		"needs_decision accept dokploy.domain:api.example.com; retire source.route:api.example.com service=api port=3000",
		"warn commit.rollback_window_closed: confirm rollback window for api.example.com is closed or explicitly waived before retiring source route",
		"Dry run only: no target ownership was committed and no source resources were retired.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected commit output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunCommitWritesJSONPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), []string{"--bundle", dir, "--format", "json", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result commitplan.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("commit json did not decode: %v\n%s", err, stdout.String())
	}
	if result.APIVersion != commitplan.APIVersion || !result.DryRun || result.Target != "dokploy" || len(result.Apps) != 1 {
		t.Fatalf("unexpected commit json: %#v", result)
	}
	if len(result.Apps[0].Routes) != 1 || len(result.Apps[0].Steps) != 4 {
		t.Fatalf("expected commit route and steps, got %#v", result.Apps[0])
	}
	if result.Apps[0].RollbackWindowSeconds != 0 {
		t.Fatalf("expected explicit zero rollback window, got %#v", result.Apps[0])
	}
	for _, gate := range result.Apps[0].Gates {
		if gate.Code == "commit.rollback_window_closed" {
			t.Fatalf("did not expect rollback window gate for explicit zero window: %#v", result.Apps[0].Gates)
		}
	}
}

func TestRunCommitApplyRejectsIgnoredPlanningFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCommit(context.Background(), []string{"--apply", "--app", "api"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not accept --app") {
		t.Fatalf("expected commit apply to reject ignored app scope, got %v", err)
	}
}

func TestRunCommitApplyRejectsPositionalArguments(t *testing.T) {
	err := runCommit(context.Background(), []string{"--apply", "purge"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not accept positional argument") {
		t.Fatalf("expected positional argument to be rejected before source retirement, got %v", err)
	}
}

func TestRunCommitRejectsEmptyExplicitRun(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		state.CurrentRun = ".bort/runs/current"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--run="}, {"--apply", "--run="}} {
		var stdout bytes.Buffer
		err := runCommit(context.Background(), args, &stdout, io.Discard)
		if err == nil || err.Error() != "commit requires a non-empty --run value" {
			t.Fatalf("args=%v: expected empty run rejection, got %v", args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("args=%v: empty run produced output before rejection: %q", args, stdout.String())
		}
	}
}

func TestRunCommitRejectsEmptyCutoverArtifact(t *testing.T) {
	var stdout bytes.Buffer
	err := runCommit(context.Background(), []string{"--from-cutover="}, &stdout, io.Discard)
	if err == nil || err.Error() != "commit requires a non-empty --from-cutover value" {
		t.Fatalf("expected empty cutover artifact rejection, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("empty cutover artifact produced output before rejection: %q", stdout.String())
	}

	err = runCommit(context.Background(), []string{"--apply", "--from-cutover="}, io.Discard, io.Discard)
	if err == nil || err.Error() != "commit --apply does not accept --from-cutover; select the run with --run" {
		t.Fatalf("expected apply mode to reject the cutover artifact flag, got %v", err)
	}
}

func TestRunCommitDefaultsToCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "reviewed-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed-app", Services: []manifest.Service{{Name: "reviewed-app", Image: "example/reviewed:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "reviewed-run"})
	reviewed, err := loadMigrationRun("reviewed-run")
	if err != nil {
		t.Fatal(err)
	}
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "stale-app", Services: []manifest.Service{{Name: "stale-app", Image: "example/stale:latest"}}}},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Commit plan: "+reviewed.Run.BundleDir+" -> dokploy") || !strings.Contains(stdout.String(), "reviewed-app") {
		t.Fatalf("expected commit to use the current run artifact, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "stale-app") {
		t.Fatalf("commit planned from the default bundle instead of the current run:\n%s", stdout.String())
	}
}

func TestRunCommitHonorsExplicitRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	explicitBundle := filepath.Join(workDir, "explicit-bundle")
	currentBundle := filepath.Join(workDir, "current-bundle")
	writeTestBundle(t, explicitBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "explicit-app", Services: []manifest.Service{{Name: "explicit-app", Image: "example/explicit:latest"}}}},
	})
	writeTestBundle(t, currentBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "current-app", Services: []manifest.Service{{Name: "current-app", Image: "example/current:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", explicitBundle, "--run", "explicit-run"})
	runCommand(t, runMigrate, []string{"--bundle", currentBundle, "--run", "current-run"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), []string{"--run", "explicit-run"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "explicit-app") || strings.Contains(stdout.String(), "current-app") {
		t.Fatalf("expected explicit run to override current run, got:\n%s", stdout.String())
	}
}

func TestRunCommitUsesDefaultBundleWithoutCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	reviewedBundle := filepath.Join(workDir, "reviewed-bundle")
	writeTestBundle(t, reviewedBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed-app", Services: []manifest.Service{{Name: "reviewed-app", Image: "example/reviewed:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", reviewedBundle, "--run", "reviewed-run"})
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "default-app", Services: []manifest.Service{{Name: "default-app", Image: "example/default:latest"}}}},
	})
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		state.CurrentRun = ""
		return true
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runCommit(context.Background(), nil, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "default-app") || strings.Contains(stdout.String(), "reviewed-app") {
		t.Fatalf("expected bare commit to plan from the default bundle without a current run, got:\n%s", stdout.String())
	}
}

func TestNewRunRequiresSuccessfulOutcomeBeforeCommit(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "missing-outcome"})
	run, err := loadMigrationRun("missing-outcome")
	if err != nil {
		t.Fatal(err)
	}
	if !run.Run.ApplyOutcomeRequired {
		t.Fatal("expected new run metadata to require a successful apply outcome")
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	applied := newRunApplied(run.Run)
	for index, step := range steps {
		applied.Steps = append(applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("missing-outcome")
	if err != nil {
		t.Fatal(err)
	}
	var cockpit bytes.Buffer
	writeAppFirstCockpit(&cockpit, run)
	if strings.Contains(cockpit.String(), "TARGET LIVE") {
		t.Fatalf("complete steps without a durable outcome were shown as target live:\n%s", cockpit.String())
	}
	if err := applyCommitFromArgs(context.Background(), "missing-outcome", io.Discard); err == nil || !strings.Contains(err.Error(), "no successful live-apply outcome") {
		t.Fatalf("expected commit to reject complete steps without a durable outcome, got %v", err)
	}
	succeededAt := time.Now().UTC()
	applied.SucceededAt = &succeededAt
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dokploy.EnvBaseURL, "http://127.0.0.1:3030")
	t.Setenv(dokploy.EnvToken, "test-token")
	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	err = applyCommitFromArgs(context.Background(), "missing-outcome", io.Discard)
	if !errors.Is(err, errDokployLiveOperationActive) {
		t.Fatalf("expected another Dokploy live operation to block commit, got %v", err)
	}
	targetLock.Release()
	blocked, err := loadMigrationRun("missing-outcome")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Run.CommitStartedAt != nil {
		t.Fatal("commit recorded a start before acquiring the host Dokploy lock")
	}
}

func TestCommitRefusesCoolifySourceBeforeRetirement(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho \"$*\" >> docker-calls\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", DockerEngineID: "engine-reviewed"},
		Apps: []manifest.App{{
			Name:     "api",
			Services: []manifest.Service{{ID: "source-id", Name: "web", Image: "example/api:latest"}},
			Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "web", Port: "3000"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", "coolify-commit", "--observation-window", "0", "--rollback-window", "0"})
	run, err := loadMigrationRun("coolify-commit")
	if err != nil {
		t.Fatal(err)
	}
	applied := newRunApplied(run.Run)
	now := time.Now().UTC()
	applied.SucceededAt = &now
	for index, step := range dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps {
		applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK)})
	}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}

	forbidLocalSourceVerification(t)
	err = applyCommitFromArgs(context.Background(), "coolify-commit", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Bort cannot durably fence that orchestrator") || !strings.Contains(err.Error(), authorityRecoverySourceRetiredCommand(run)) {
		t.Fatalf("expected Coolify commit refusal with exact manual-retirement command, got %v", err)
	}
	blocked, err := loadMigrationRun("coolify-commit")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Run.CommitStartedAt != nil {
		t.Fatal("Coolify commit refusal recorded source retirement")
	}
	if _, err := os.Stat(filepath.Join(run.Run.RunDir, "source-pause.json")); !os.IsNotExist(err) {
		t.Fatalf("Coolify commit refusal created source pause state: %v", err)
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("Coolify commit refusal made Docker calls: %v", err)
	}
}

func TestCommitRefusesIncompleteSourceAuthorityRecoveryBeforeDocker(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho \"$*\" >> docker-calls\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name:   "api",
			Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "web", Port: "3000"}},
			Services: []manifest.Service{{
				ID:    "source-id",
				Name:  "web",
				Image: "example/api:latest",
				Mounts: []manifest.Mount{{
					Type: "volume", Name: "api-data", Target: "/data",
				}},
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", "source-recovery", "--observation-window", "0", "--rollback-window", "0"})
	run, err := loadMigrationRun("source-recovery")
	if err != nil {
		t.Fatal(err)
	}
	applied := newRunApplied(run.Run)
	now := time.Now().UTC()
	applied.SucceededAt = &now
	for index, step := range dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps {
		applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK)})
	}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficSource); err != nil {
		t.Fatal(err)
	}

	forbidLocalSourceVerification(t)
	err = applyCommitFromArgs(context.Background(), "source-recovery", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "manual source-authority recovery is incomplete") || !strings.Contains(err.Error(), "recover source-recovery as source") {
		t.Fatalf("expected incomplete source-authority recovery refusal, got %v", err)
	}
	blocked, err := loadMigrationRun("source-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Run.CommitStartedAt != nil {
		t.Fatal("commit recorded source retirement during incomplete source-authority recovery")
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("blocked commit made Docker calls: %v", err)
	}
}

func TestCommitRetryRepublishesCompletedMetadataBeforeOwnerRelease(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "durable-commit")
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficTarget); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommitStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommittedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunHostOwnerReleaseStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	previous := releaseAuthorityStagingVolumePins
	cleanupCalls := 0
	releaseAuthorityStagingVolumePins = func(_ context.Context, _ loadedMigrationRun, plan dokploy.Plan, targetAuthority bool) error {
		cleanupCalls++
		if plan.RunName != run.Run.Name || plan.RunDir != run.Run.RunDir || plan.RunID == "" || !targetAuthority {
			t.Fatalf("commit pin cleanup received incomplete target identity: plan=%#v target=%t", plan, targetAuthority)
		}
		return nil
	}
	t.Cleanup(func() { releaseAuthorityStagingVolumePins = previous })
	path := filepath.Join(run.Run.RunDir, "run.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyCommitFromArgs(context.Background(), run.Run.Name, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("completed commit metadata was not atomically republished before owner release")
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("completed commit owner = %#v, found=%t err=%v", owner, found, err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("completed commit pin cleanup calls = %d, want 1", cleanupCalls)
	}
}

func TestCommitRetryKeepsOwnerWhenPinCleanupFails(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "durable-commit-pin-cleanup")
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficTarget); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommitStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommittedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	previous := releaseAuthorityStagingVolumePins
	releaseAuthorityStagingVolumePins = func(context.Context, loadedMigrationRun, dokploy.Plan, bool) error {
		return errors.New("pin cleanup failed")
	}
	t.Cleanup(func() { releaseAuthorityStagingVolumePins = previous })

	err := applyCommitFromArgs(context.Background(), run.Run.Name, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "host ownership remains held") || !strings.Contains(err.Error(), "pin cleanup failed") {
		t.Fatalf("expected pin cleanup failure, got %v", err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficTarget {
		t.Fatalf("pin cleanup failure released owner: owner=%#v found=%t err=%v", owner, found, err)
	}
}

func TestCommitRetryKeepsOwnerWhenCompletedMetadataRepublishFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	run := writeAmbiguousAuthorityRun(t, "durable-commit-readonly")
	if err := markRunAuthorityResolvedLocked(run.Run, dokployTrafficTarget); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommitStartedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := markRunCommittedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(run.Run.RunDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(run.Run.RunDir, 0o700) })
	err := applyCommitFromArgs(context.Background(), run.Run.Name, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "reconfirm completed commit metadata durability") {
		t.Fatalf("expected metadata republish failure, got %v", err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficTarget {
		t.Fatalf("owner released before completed commit metadata was durable: %#v found=%t err=%v", owner, found, err)
	}
}

func TestLegacyCompleteLedgerDoesNotRequireSuccessfulOutcomeMarker(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "legacy-run"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	run.Applied.APIVersion = appliedLegacyAPIVersion
	run.Applied.TargetOrigin = "http://127.0.0.1:3030"
	steps := dokploy.LegacyPlanFromArtifactsV1Alpha1(run.Prepare, run.Sync, run.Cutover).Steps
	for index, step := range steps {
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		t.Fatalf("expected a legacy complete ledger to remain accepted: %v", err)
	}
}

func TestLiveApplyRecoveryErrorsPreserveExternalRunDirectory(t *testing.T) {
	externalRunDir := filepath.Join(t.TempDir(), "selected-run")
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "selected-run", RunDir: externalRunDir, ApplyOutcomeRequired: true},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	want := liveApplyCommand(run)
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected missing-outcome recovery command %q, got %v", want, err)
	}
	run.Run.ApplyOutcomeRequired = false
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected missing-step recovery command %q, got %v", want, err)
	}
}

func TestRequireLiveApplySucceededAcceptsSkippedPlatformSteps(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{
			{Name: "proxy", Role: "platform"},
			{Name: "api"},
		}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{
			Name:   "api",
			Routes: []gateway.Route{{Host: "api.example.com"}},
		}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	succeededAt := time.Now().UTC()
	run.Applied.SucceededAt = &succeededAt
	for index, step := range steps {
		status := string(dokploy.StepStatusOK)
		if step.App == "proxy" {
			status = string(dokploy.StepStatusSkipped)
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: status,
		})
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		t.Fatalf("expected skipped platform steps to count as completed: %v", err)
	}
}

func TestRequireLiveApplySucceededRejectsReorderedLedgerIndexes(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}, {Name: "worker"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{
			{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}},
			{Name: "worker", Routes: []gateway.Route{{Host: "worker.example.com"}}},
		}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  len(steps) - 1 - index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), "plan index") {
		t.Fatalf("expected reordered completed ledger steps to be rejected, got %v", err)
	}
}

func TestRequireLiveApplySucceededRejectsLaterFailedLedgerEntry(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	for index, step := range steps {
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK), UpdatedAt: time.Unix(1, 0)})
	}
	failed := steps[0]
	run.Applied.Steps = append(run.Applied.Steps, appliedStep{Index: len(steps), Kind: string(failed.Kind), App: failed.App, Ref: failed.Ref, Status: string(dokploy.StepStatusError), UpdatedAt: time.Unix(2, 0)})
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), string(failed.Kind)) {
		t.Fatalf("expected later failed ledger entry to invalidate historical success, got %v", err)
	}
}

func TestRequireLiveApplySucceededForAppsIgnoresUnselectedApps(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}, {Name: "worker"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{
			{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}},
			{Name: "worker", Routes: []gateway.Route{{Host: "worker.example.com"}}},
		}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	succeededAt := time.Now().UTC()
	run.Applied.SucceededAt = &succeededAt
	for index, step := range steps {
		if step.App != "" && step.App != "api" {
			continue
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceededForApps(run, map[string]struct{}{"api": {}}); err != nil {
		t.Fatalf("expected selected app's completed ledger steps to count as successful live apply: %v", err)
	}
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("expected all-app guard to still reject missing worker steps, got %v", err)
	}
}

func TestRequireLiveApplySucceededForAppsSkippingAllowsMissingSkippedKinds(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	if len(steps) == 0 {
		t.Fatal("expected live plan steps")
	}
	succeededAt := time.Now().UTC()
	run.Applied.SucceededAt = &succeededAt
	skipKind := steps[0].Kind
	for index, step := range steps {
		if step.Kind == skipKind {
			continue
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceededForAppsSkipping(run, nil, map[dokploy.StepKind]struct{}{skipKind: {}}); err != nil {
		t.Fatalf("expected missing skipped step kind to be ignored: %v", err)
	}
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), string(skipKind)) {
		t.Fatalf("expected regular guard to reject missing %s step, got %v", skipKind, err)
	}
}

func TestCoolifySourceRetirementRequiredDetectsLabeledAppsInDockerScans(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		apps   []preparer.AppPlan
		want   bool
	}{
		{name: "coolify-local source", source: "coolify-local", want: true},
		{name: "coolify-local-traefik source", source: "coolify-local-traefik", want: true},
		{name: "docker scan without coolify apps", source: "docker", apps: []preparer.AppPlan{{Name: "api", Platform: "docker"}}, want: false},
		{name: "docker scan with coolify-labeled app", source: "docker", apps: []preparer.AppPlan{{Name: "api", Platform: "docker"}, {Name: "web", Platform: "coolify"}}, want: true},
		{name: "legacy bundle without app platform", source: "docker", apps: []preparer.AppPlan{{Name: "api"}}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := loadedMigrationRun{Prepare: preparer.Result{Source: test.source, Apps: test.apps}}
			if got := coolifySourceRetirementRequired(run); got != test.want {
				t.Fatalf("coolifySourceRetirementRequired = %t, want %t", got, test.want)
			}
		})
	}
}

func forbidLocalSourceVerification(t *testing.T) {
	t.Helper()
	previousRun, previousEngine := verifyLocalSourceRun, verifyLocalSourceEngine
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error {
		t.Error("commit refusal probed the local source run")
		return nil
	}
	verifyLocalSourceEngine = func(context.Context, loadedMigrationRun) error {
		t.Error("commit refusal probed the local source engine")
		return nil
	}
	t.Cleanup(func() {
		verifyLocalSourceRun, verifyLocalSourceEngine = previousRun, previousEngine
	})
}

func TestManualCoolifySourceRetirementActionScopesProxyToRoutedCutovers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "web", ContainerID: "web-id", ContainerName: "web-x1y2"},
		{ServiceName: "db", ContainerID: "db-id"},
	}
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "coolify-run"},
		Prepare: preparer.Result{Source: "coolify-local", Apps: []preparer.AppPlan{app}},
	}
	if action := manualCoolifySourceRetirementAction(run); !strings.Contains(action, "`"+dockerCommand("rm -f web-x1y2 db-id")+"`") || !strings.Contains(action, "--source-retired") {
		t.Fatalf("route-free retirement hint did not name exactly the reviewed containers: %q", action)
	}
	run.Cutover = gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}}
	if action := manualCoolifySourceRetirementAction(run); !strings.Contains(action, "`"+dockerCommand("rm -f web-x1y2 db-id coolify-proxy")+"`") {
		t.Fatalf("routed retirement hint omitted the proxy handoff: %q", action)
	}
}
