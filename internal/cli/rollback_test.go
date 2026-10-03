package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
	syncplan "github.com/aikins01/bort/internal/sync"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRunRollbackWritesTextPlan(t *testing.T) {
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
	if err := runRollback(context.Background(), []string{"--bundle", dir, "--target", "dokploy", "--observation-window", "120"}, &stdout, &stderr); err != nil {
		t.Fatalf("rollback failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Rollback plan: " + dir + " -> dokploy",
		"[yellow] api",
		"readiness: needs_decision",
		"cutover readiness: needs_decision",
		"observe: 120s",
		"needs_decision dokploy.domain:api.example.com -> source.route:api.example.com service=api port=3000",
		"warn rollback.source_health_required: verify source health for api.example.com before route rollback",
		"Dry run only: no routes were changed and no rollback actions were executed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected rollback output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunRollbackWritesJSONPlan(t *testing.T) {
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
	if err := runRollback(context.Background(), []string{"--bundle", dir, "--format", "json", "--observation-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("rollback failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result rollbackplan.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("rollback json did not decode: %v\n%s", err, stdout.String())
	}
	if result.APIVersion != rollbackplan.APIVersion || !result.DryRun || result.Target != "dokploy" || len(result.Apps) != 1 {
		t.Fatalf("unexpected rollback json: %#v", result)
	}
	if len(result.Apps[0].Routes) != 1 || len(result.Apps[0].Steps) != 3 {
		t.Fatalf("expected rollback route and steps, got %#v", result.Apps[0])
	}
	if result.Apps[0].ObservationWindowSeconds != 0 {
		t.Fatalf("expected explicit zero observation window, got %#v", result.Apps[0])
	}
}

func TestRunRollbackRejectsPositionalArguments(t *testing.T) {
	err := runRollback(context.Background(), []string{"typo", "--run", "recovery-run"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `rollback does not accept positional argument "typo"`) {
		t.Fatalf("expected positional argument rejection, got %v", err)
	}
}

func TestRunRollbackRejectsEmptyExplicitRun(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		state.CurrentRun = ".bort/runs/current"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := runRollback(context.Background(), []string{"--run="}, &stdout, io.Discard)
	if err == nil || err.Error() != "rollback requires a non-empty --run value" {
		t.Fatalf("expected empty run rejection, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("empty run produced output before rejection: %q", stdout.String())
	}
}

func TestRunRollbackDefaultsToCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "reviewed-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "reviewed-app",
			Services: []manifest.Service{{Name: "reviewed-app", Image: "example/reviewed:latest"}},
			Routes:   []manifest.Route{{Host: "reviewed.example.com", ServiceName: "reviewed-app"}},
		}},
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
	if err := runRollback(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("rollback failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Rollback plan: "+reviewed.Run.BundleDir+" -> dokploy") || !strings.Contains(stdout.String(), "reviewed-app") {
		t.Fatalf("expected rollback to use the current run artifact, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "stale-app") {
		t.Fatalf("rollback planned from the default bundle instead of the current run:\n%s", stdout.String())
	}
}

func TestRunRollbackUsesDefaultBundleWithoutCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	reviewedBundle := filepath.Join(workDir, "reviewed-bundle")
	writeTestBundle(t, reviewedBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "reviewed-app",
			Services: []manifest.Service{{Name: "reviewed-app", Image: "example/reviewed:latest"}},
			Routes:   []manifest.Route{{Host: "reviewed.example.com", ServiceName: "reviewed-app"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", reviewedBundle, "--run", "reviewed-run"})
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "default-app",
			Services: []manifest.Service{{Name: "default-app", Image: "example/default:latest"}},
			Routes:   []manifest.Route{{Host: "default.example.com", ServiceName: "default-app"}},
		}},
	})
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		state.CurrentRun = ""
		return true
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runRollback(context.Background(), nil, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "default-app") || strings.Contains(stdout.String(), "reviewed-app") {
		t.Fatalf("expected bare rollback to plan from the default bundle without a current run, got:\n%s", stdout.String())
	}
}

func TestRunRollbackLiveRejectsConflictingFlags(t *testing.T) {
	err := runRollback(context.Background(), []string{"--live", "--run", "r", "--format", "json"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rollback --live does not accept --format") {
		t.Fatalf("expected --live to reject --format, got %v", err)
	}
	err = runRollback(context.Background(), []string{"--live", "--run", "r", "--app", "api"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rollback --live does not accept --app") {
		t.Fatalf("expected --live to reject --app, got %v", err)
	}
	if err := runRollback(context.Background(), []string{"--confirm", "rollback r"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "requires --live") {
		t.Fatalf("expected confirmation without live to be refused, got %v", err)
	}
}

func TestWizardLifecycleWithoutAppliedLedger(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		run  migrationRun
		want string
	}{
		{name: "rollback started", run: migrationRun{RollbackStartedAt: &now}, want: "ROLLING BACK"},
		{name: "rolled back", run: migrationRun{RolledBackAt: &now}, want: "ROLLED BACK"},
		{name: "commit started", run: migrationRun{CommitStartedAt: &now}, want: "COMMITTING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.RunDir = t.TempDir()
			tc.run.Source = "docker"
			run := loadedMigrationRun{Run: tc.run, Prepare: preparer.Result{
				Source:               "docker",
				SourceDockerEngineID: "engine-reviewed",
				Apps: []preparer.AppPlan{{
					Name:      "api",
					Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
				}},
			}}
			var out bytes.Buffer
			if err := runWizard(context.Background(), run, strings.NewReader(""), &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "migrate --live") {
				t.Fatalf("expected %s without live-apply recommendation, got:\n%s", tc.want, out.String())
			}
		})
	}
}

func TestInterruptedCommitRefusesRollbackAndResumesCommit(t *testing.T) {
	resetDokployTrafficOwner(t)
	t.Chdir(t.TempDir())
	t.Setenv("BORT_DOKPLOY_URL", "http://127.0.0.1:3030")
	t.Setenv("BORT_DOKPLOY_TOKEN", "test-token")
	binDir := t.TempDir()
	stub := `#!/bin/sh
if [ "$*" = "info --format {{.ID}}" ]; then
  echo engine-reviewed
  exit 0
fi
if [ "$*" = "inspect --type container source-id" ]; then
  count=0
  if [ -f inspect-count ]; then count=$(cat inspect-count); fi
  count=$((count + 1))
  echo "$count" > inspect-count
  if [ -f fail-commit ] && [ "$count" -gt 1 ]; then
    grep -q commitStartedAt .bort/runs/retirement/run.json || exit 9
    echo 'injected inspection failure' >&2
    exit 1
  fi
  if [ -f source-stopped ]; then
    echo '[{"Id":"source-id","Name":"/web","State":{"Running":false,"Status":"exited"}}]'
  else
    echo '[{"Id":"source-id","Name":"/web","State":{"Running":true,"Status":"running"}}]'
  fi
  exit 0
fi
if [ "$*" = "stop source-id" ]; then
  grep -q commitStartedAt .bort/runs/retirement/run.json || exit 9
  touch source-stopped
  echo source-id
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "docker", DockerEngineID: "engine-reviewed"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{ID: "source-id", Name: "web", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", "retirement"})
	run, err := loadMigrationRun("retirement")
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range openSetupDecisions(run) {
		if err := recordReviewDecision(run, decision, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	applied := newRunApplied(run.Run)
	now := time.Now().UTC()
	applied.SucceededAt = &now
	for index, step := range dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps {
		applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: "ok"})
	}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("fail-commit", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCommit(context.Background(), []string{"--apply", "--run", "retirement"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "injected inspection failure") {
		t.Fatalf("expected interrupted commit, got %v", err)
	}
	run, err = loadMigrationRun("retirement")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.CommitStartedAt == nil || run.Run.CommittedAt != nil {
		t.Fatal("expected durable incomplete commit")
	}
	if err := runRollback(context.Background(), []string{"--live", "--run", "retirement"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "source retirement started") {
		t.Fatalf("expected rollback refusal during retirement, got %v", err)
	}
	if err := os.Remove("fail-commit"); err != nil {
		t.Fatal(err)
	}
	if err := runCommit(context.Background(), []string{"--apply", "--run", "retirement"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("expected commit recovery, got %v", err)
	}
	run, err = loadMigrationRun("retirement")
	if err != nil || run.Run.CommittedAt == nil {
		t.Fatalf("expected recorded commit completion, got %v", err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		t.Fatal(err)
	}
	if !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("expected commit to release host ownership, got %#v found=%t", owner, found)
	}
	nextRun := migrationRun{Name: "next", RunDir: filepath.Join(t.TempDir(), "next"), CreatedAt: time.Now().UTC(), BundleDigest: "next"}
	if err := ensureDokployTrafficRunAvailable(nextRun, dokploy.Plan{Steps: []dokploy.Step{{Kind: dokploy.StepCreateProject, App: "next", Ref: "next"}}}); err != nil {
		t.Fatalf("completed commit blocked the next migration: %v", err)
	}
	if err := runCommit(context.Background(), []string{"--apply", "--run", "retirement"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("repeated commit did not preserve released ownership: %v", err)
	}
	owner, found, err = readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("repeated commit changed host ownership, got %#v found=%t err=%v", owner, found, err)
	}
}

func TestValidateRollbackApplyReadyRefusals(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		run  loadedMigrationRun
		want string
	}{
		{name: "purged", run: loadedMigrationRun{Run: migrationRun{Name: "r", Target: "dokploy", RunDir: t.TempDir(), PurgedAt: &now}}, want: "already purged"},
		{name: "committed", run: loadedMigrationRun{Run: migrationRun{Name: "r", Target: "dokploy", RunDir: t.TempDir(), CommittedAt: &now}}, want: "already committed"},
		{name: "rolled back", run: loadedMigrationRun{Run: migrationRun{Name: "r", Target: "dokploy", RunDir: t.TempDir(), LiveAppliedAt: &now, RolledBackAt: &now}}, want: "already rolled back"},
		{name: "no live work", run: loadedMigrationRun{Run: migrationRun{Name: "r", Target: "dokploy", RunDir: t.TempDir()}}, want: "requires a successful live apply"},
		{name: "metadata only", run: loadedMigrationRun{Run: migrationRun{Name: "r", Target: "dokploy", RunDir: t.TempDir(), LiveAppliedAt: &now}}, want: "requires a successful live apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRollbackApplyReady(tt.run)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected refusal %q, got %v", tt.want, err)
			}
		})
	}
	ready := loadedMigrationRun{
		Run:     migrationRun{Name: "r", Target: "dokploy", RunDir: t.TempDir(), ApplyOutcomeRequired: true},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Applied: runApplied{SucceededAt: &now, Steps: []appliedStep{{Index: 0, Kind: "create_project", App: "api", Ref: "api", Status: "ok"}}},
	}
	if err := validateRollbackApplyReady(ready); err != nil {
		t.Fatalf("expected a live-applied run with no open triggers to be rollback-ready, got %v", err)
	}
	for _, status := range []string{"started", "error"} {
		ready.Applied.Steps[0].Status = status
		if err := validateRollbackApplyReady(ready); err == nil {
			t.Fatalf("expected %s ledger to be refused", status)
		}
	}
	ready.Applied.Steps[0].Status = "ok"
	ready.Applied.SucceededAt = nil
	if err := validateRollbackApplyReady(ready); err == nil {
		t.Fatal("expected complete ledger without successful outcome to be refused")
	}
}

func TestAutomaticRollbackAvailabilityChecksExecutionAndHostOwnership(t *testing.T) {
	resetDokployTrafficOwner(t)
	if err := automaticRollbackBlocker(context.Background(), loadedMigrationRun{
		Run: migrationRun{Name: "unapplied", Target: "dokploy", RunDir: t.TempDir()},
	}); err == nil || !strings.Contains(err.Error(), "successful live apply") {
		t.Fatalf("expected unapplied rollback to be unavailable, got %v", err)
	}

	now := time.Now().UTC()
	meta := migrationRun{
		Name:                 "missing-owner",
		RunDir:               t.TempDir(),
		Target:               "dokploy",
		CreatedAt:            now,
		BundleDigest:         strings.Repeat("a", 64),
		ApplyOutcomeRequired: true,
	}
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	plan := dokploy.PlanFromArtifacts(prepare, syncplan.Result{}, gateway.Result{})
	applied := newRunApplied(meta)
	for index, step := range plan.Steps {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied.SucceededAt = &now
	run := loadedMigrationRun{Run: meta, Prepare: prepare, Applied: applied}
	if err := automaticRollbackBlocker(context.Background(), run); err == nil || !strings.Contains(err.Error(), "no durable host-wide Dokploy traffic owner") {
		t.Fatalf("expected missing owner to make automatic rollback unavailable, got %v", err)
	}
	const targetOrigin = "http://127.0.0.1:3030"
	if err := claimDokployHostOwnership(meta, targetOrigin, dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(meta, targetOrigin); err != nil {
		t.Fatal(err)
	}

	previousSupported := dokployLiveOperationsSupported
	dokployLiveOperationsSupported = func() bool { return false }
	if err := automaticRollbackBlocker(context.Background(), run); err == nil || !strings.Contains(err.Error(), "unavailable on this platform") {
		t.Fatalf("expected unsupported platform to make automatic rollback unavailable, got %v", err)
	}
	dokployLiveOperationsSupported = previousSupported
	t.Cleanup(func() { dokployLiveOperationsSupported = previousSupported })

	hostLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer hostLock.Release()
	if err := automaticRollbackBlocker(context.Background(), run); err == nil || !strings.Contains(err.Error(), "holds the host-wide Dokploy operation lock") {
		t.Fatalf("expected active host operation to make automatic rollback unavailable, got %v", err)
	}
}

func TestIncompleteStatefulRollbackRequiresManualAuthorityRecovery(t *testing.T) {
	now := time.Now().UTC()
	run := loadedMigrationRun{
		Run: migrationRun{Name: "stateful", RollbackStartedAt: &now},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	want := "inspect and preserve both sides, establish writer and traffic authority manually, then start a fresh migration run"
	if got := incompleteRollbackRecovery(run); got != want {
		t.Fatalf("stateful rollback recovery = %q, want %q", got, want)
	}
	if err := validateLiveApplyReady(run); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "rollback --live") {
		t.Fatalf("live apply reported an unusable rollback retry: %v", err)
	}
}

func TestRunRollbackLiveRefusesAmbiguousLegacyProxyHandoff(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerStub := `#!/bin/sh
echo "$*" >> docker-calls
if [ "$1" = ps ] && [ "$2" = -a ]; then
  echo target-id
elif [ "$1" = inspect ] && [ "$2" = --type ]; then
  running=true
  case "$4" in
    source-id) [ -f source-started ] || running=false ;;
    target-id) [ -f target-stopped ] && running=false ;;
    coolify-proxy) [ -f proxy-started ] || running=false ;;
  esac
  echo '[{"Id":"'"$4"'","Name":"/'"$4"'","State":{"Running":'"$running"',"Status":"running"}}]'
elif [ "$1" = start ] && [ "$2" = source-id ]; then
  [ -f target-stopped ] || exit 23
  touch source-started
elif [ "$1" = start ] && [ "$2" = coolify-proxy ]; then
  touch proxy-started
elif [ "$1" = stop ] && [ "$2" = target-id ]; then
  touch target-stopped
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerStub), 0o700); err != nil {
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
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", "legacy", "--observation-window", "0", "--rollback-window", "0"})
	run, err := loadMigrationRun("legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range openSetupDecisions(run) {
		if err := recordReviewDecision(run, decision, run.Run.UpdatedAt.Add(2*time.Second)); err != nil {
			t.Fatalf("resolve prepare decision: %v", err)
		}
	}
	run.Prepare.Apps[0].Resources.SourceServices = nil
	run.Prepare.Apps[0].Resources.Volumes = nil
	preparePath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Prepare)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(preparePath, run.Prepare); err != nil {
		t.Fatal(err)
	}
	livePlan := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	applied := runApplied{APIVersion: appliedAPIVersion, RunName: run.Run.Name, BundleDir: run.Run.BundleDir, Target: run.Run.Target}
	for index, step := range livePlan.Steps {
		status := dokploy.StepStatusOK
		switch step.Kind {
		case dokploy.StepStartDokployProxy:
			status = dokploy.StepStatusStarted
		case dokploy.StepActivateRoutes, dokploy.StepResumeTarget:
			status = dokploy.StepStatusSkipped
		}
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: status})
		if status == dokploy.StepStatusStarted {
			break
		}
	}
	if ambiguity := appliedAuthorityAmbiguity(applied); ambiguity != applyTrafficAuthorityAmbiguous {
		t.Fatalf("fixture ambiguity = %d, want the proxy handoff alone (%d)", ambiguity, applyTrafficAuthorityAmbiguous)
	}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project.all":
			_ = json.NewEncoder(w).Encode([]dokploy.Project{{
				ProjectID:    "project-1",
				Name:         "api",
				Environments: []dokploy.ProjectEnvironment{{EnvironmentID: "environment-1", Name: "production"}},
			}})
		case "/api/compose.search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []dokploy.Compose{{ComposeID: "compose-1", Name: "api", AppName: "stack-api"}},
				"total": 1,
			})
		case "/api/compose.one":
			_ = json.NewEncoder(w).Encode(dokploy.Compose{ComposeID: "compose-1", Name: "api", AppName: "stack-api", EnvironmentID: "environment-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")

	if err := runCommit(context.Background(), []string{"--apply", "--run", "legacy"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no successful live-apply outcome") {
		t.Fatalf("expected ambiguous legacy handoff to remain ineligible for commit, got %v", err)
	}
	if err := runRollback(context.Background(), []string{"--live", "--run", "legacy", "--confirm", "rollback legacy"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot prove traffic authority after the shared proxy handoff") || !strings.Contains(err.Error(), "inspect the Coolify and Dokploy proxies") {
		t.Fatalf("expected ambiguous legacy proxy handoff to fail rollback closed, got %v", err)
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("ambiguous legacy rollback made Docker calls: %v", err)
	}
	refused, err := loadMigrationRun("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if refused.Run.RollbackStartedAt != nil || refused.Run.RolledBackAt != nil {
		t.Fatal("ambiguous legacy refusal changed rollback lifecycle state")
	}
}

func TestRunRollbackLiveRefusesCompletedStatefulRunWithoutMutation(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerStub := "#!/bin/sh\necho \"$*\" >> docker-calls\nexit 81\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", DockerEngineID: "engine-reviewed"},
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
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", "stateful", "--observation-window", "0", "--rollback-window", "0"})
	markRunLocallyScanned(t, "stateful", "coolify-local")
	run, err := loadMigrationRun("stateful")
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range openSetupDecisions(run) {
		if err := recordReviewDecision(run, decision, run.Run.UpdatedAt.Add(2*time.Second)); err != nil {
			t.Fatalf("resolve prepare decision: %v", err)
		}
	}
	livePlan := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	foundPause := false
	applied := newRunApplied(run.Run)
	for index, step := range livePlan.Steps {
		foundPause = foundPause || step.Kind == dokploy.StepPauseSource
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	if !foundPause {
		t.Fatal("stateful fixture did not produce a source pause")
	}
	now := time.Now().UTC()
	applied.SucceededAt = &now
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("stateful")
	if err != nil {
		t.Fatal(err)
	}
	if phase := migrationRunPhase(run); phase != "applied" {
		t.Fatalf("expected applied phase, got %q", phase)
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "recover-authority") || !strings.Contains(next.Action, "as source") || strings.Contains(next.Action, "rollback --live") {
		t.Fatalf("expected manual stateful recovery guidance, got %#v", next)
	}
	var cockpit strings.Builder
	writeAppFirstCockpit(&cockpit, run)
	for _, want := range []string{"TARGET LIVE", "Automatic rollback is unavailable: automatic rollback is unsafe for stateful app(s)", "recover authority manually", "source: `" + authorityRecoveryCommand(run, dokployTrafficSource) + "`"} {
		if !strings.Contains(cockpit.String(), want) {
			t.Fatalf("expected cockpit to contain %q, got:\n%s", want, cockpit.String())
		}
	}
	if strings.Contains(cockpit.String(), "rollback --live") {
		t.Fatalf("stateful cockpit recommended automatic rollback:\n%s", cockpit.String())
	}

	runPath := filepath.Join(run.Run.RunDir, "run.json")
	progressPath := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Progress)
	runBefore, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	progressBefore, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	err = runRollback(context.Background(), []string{"--live", "--run", "stateful", "--confirm", "rollback stateful"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no durable application fence") || !strings.Contains(err.Error(), "establish writer and traffic authority manually") {
		t.Fatalf("expected stateful rollback to fail closed, got %v", err)
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("stateful rollback refusal made Docker calls: %v", err)
	}
	runAfter, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	progressAfter, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runBefore, runAfter) || !bytes.Equal(progressBefore, progressAfter) {
		t.Fatal("stateful rollback refusal changed run lifecycle or progress")
	}
	refused, err := loadMigrationRun("stateful")
	if err != nil {
		t.Fatal(err)
	}
	if refused.Run.RollbackStartedAt != nil || refused.Run.RolledBackAt != nil {
		t.Fatal("stateful rollback refusal changed rollback lifecycle state")
	}
}

func TestRunRollbackLiveExecutesStoredStatelessRollbackAndGatesFollowUps(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerStub := `#!/bin/sh
echo "$*" >> docker-calls
grep -q rollbackStartedAt .bort/runs/demo/run.json || exit 9
if [ "$1" = inspect ] && [ "$2" = --type ]; then
  running=true
  policy=always
	name=app
  case "$4" in
	cid123) name=web ;;
    coolify-proxy) [ -f proxy-started ] || running=false ;;
    dokploy-traefik) [ ! -f proxy-stopped ] || running=false ;;
  esac
  [ ! -f "$4-unless" ] || policy=unless-stopped
	echo '[{"Id":"'"$4"'","Name":"/'"$name"'","State":{"Running":'"$running"',"Status":"running"},"HostConfig":{"RestartPolicy":{"Name":"'"$policy"'"}}}]'
elif [ "$1" = start ] && [ "$2" = coolify-proxy ]; then
  [ -f fail-observation ] || touch proxy-started
elif [ "$1" = stop ]; then
  touch proxy-stopped
elif [ "$1" = update ] && [ "$2" = --restart=unless-stopped ]; then
  touch "$3-unless"
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", DockerEngineID: "engine-reviewed"},
		Apps: []manifest.App{{
			Name:   "api",
			Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "web", Port: "3000"}},
			Services: []manifest.Service{{
				ID:    "cid123",
				Name:  "web",
				Image: "example/api:latest",
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "demo", "--observation-window", "0", "--rollback-window", "0"})
	markRunLocallyScanned(t, "demo", "coolify-local")
	run, err := loadMigrationRun("demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range openSetupDecisions(run) {
		if err := recordReviewDecision(run, decision, run.Run.UpdatedAt.Add(2*time.Second)); err != nil {
			t.Fatalf("resolve prepare decision: %v", err)
		}
	}
	applied := newRunApplied(run.Run)
	now := time.Now().UTC()
	applied.SucceededAt = &now
	applied.Apps["api"] = appliedApp{ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-1", ComposeAppName: "stack-api"}
	for index, step := range dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps {
		applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: "ok"})
	}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3000", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3000"); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	for _, confirm := range []string{"", "rollback other"} {
		err := runRollback(context.Background(), []string{"--live", "--run", "demo", "--confirm", confirm}, &stdout, &stderr)
		if err == nil || (!strings.Contains(err.Error(), "unconfirmed rollback trigger") && !strings.Contains(err.Error(), "confirmation must be exactly")) {
			t.Fatalf("expected refusal for confirmation %q, got %v", confirm, err)
		}
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("expected no Docker calls before confirmation, got %v", err)
	}
	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	err = runRollback(context.Background(), []string{"--live", "--run", "demo", "--confirm", "rollback demo"}, &stdout, &stderr)
	if !errors.Is(err, errDokployLiveOperationActive) {
		t.Fatalf("expected another Dokploy live operation to block rollback, got %v", err)
	}
	targetLock.Release()
	blocked, err := loadMigrationRun("demo")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Run.RollbackStartedAt != nil {
		t.Fatal("rollback recorded a start before acquiring the host Dokploy lock")
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("blocked rollback made Docker calls: %v", err)
	}
	if err := os.WriteFile("fail-observation", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRollback(context.Background(), []string{"--live", "--run", "demo", "--confirm", "rollback demo"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "did not stay running") {
		t.Fatalf("expected failure after proxy swap, got %v", err)
	}
	interrupted, err := loadMigrationRun("demo")
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Run.RollbackStartedAt == nil || interrupted.Run.RolledBackAt != nil || len(rollbackTriggerDecisions(interrupted)) != 0 {
		t.Fatal("expected durable rollback start and confirmation, but no completion")
	}
	if phase := migrationRunPhase(interrupted); phase != "rolling back" {
		t.Fatalf("expected rolling back, got %q", phase)
	}
	if next := nextSafeStep(interrupted, nil); !strings.Contains(next.Action, "rollback --live") || strings.Contains(next.Action, "commit") {
		t.Fatalf("expected rollback retry, got %#v", next)
	}
	if err := runCommit(context.Background(), []string{"--apply", "--run", "demo"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "rollback started") {
		t.Fatalf("expected commit refusal after interrupted rollback, got %v", err)
	}
	if err := runMigrate(context.Background(), []string{"--live", "--run", "demo"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "rollback started") {
		t.Fatalf("expected live apply refusal after interrupted rollback, got %v", err)
	}
	if err := os.Remove("fail-observation"); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := runRollback(context.Background(), []string{"--live", "--run", "demo"}, &stdout, &stderr); err != nil {
		t.Fatalf("rollback --live failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "rollback live: run demo") {
		t.Fatalf("expected rollback progress header, got:\n%s", stderr.String())
	}
	for _, want := range []string{
		"rollback [1/5] resume_source api: ok",
		"rollback [2/5] verify_source_health api: ok",
		"rollback [3/5] stop_dokploy_proxy dokploy-traefik: ok",
		"rollback [4/5] start_coolify_proxy coolify-proxy: ok",
		"rollback [5/5] observe_rollback api: ok",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("expected rollback progress %q, got:\n%s", want, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "rollback complete: traffic is back on the source") {
		t.Fatalf("expected rollback completion summary, got:\n%s", stderr.String())
	}

	rolled, err := loadMigrationRun("demo")
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Run.RolledBackAt == nil {
		t.Fatal("expected rolledBackAt to be recorded")
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		t.Fatal(err)
	}
	if !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("expected released traffic ownership after rollback, got %#v found=%t", owner, found)
	}
	if phase := migrationRunPhase(rolled); phase != "rolled back" {
		t.Fatalf("expected cockpit phase \"rolled back\", got %q", phase)
	}
	next := nextSafeStep(rolled, liveApplyBlockingDecisions(rolled))
	if !strings.Contains(next.Action, "create a new named migration run") {
		t.Fatalf("expected next safe step to recommend a fresh run, got %#v", next)
	}
	stdout.Reset()
	if err := runWizard(context.Background(), rolled, strings.NewReader(""), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "ROLLED BACK") || strings.Contains(stdout.String(), "migrate --live") {
		t.Fatalf("expected rolled-back wizard to show status without suggesting live apply, got:\n%s", stdout.String())
	}

	owner.Authority = dokployTrafficSource
	if err := writeDokployTrafficOwner(owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove("docker-calls"); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := runRollback(context.Background(), []string{"--live", "--run", "demo"}, io.Discard, &stderr); err != nil {
		t.Fatalf("repeat rollback after an unreleased owner failed: %v", err)
	}
	if !strings.Contains(stderr.String(), "rollback already complete") {
		t.Fatalf("repeat rollback did not report completion, got:\n%s", stderr.String())
	}
	if _, err := os.Stat("docker-calls"); !os.IsNotExist(err) {
		t.Fatalf("repeat rollback made Docker calls: %v", err)
	}
	owner, found, err = readDokployTrafficOwner()
	if err != nil || !found || owner.Authority != dokployTrafficReleased {
		t.Fatalf("repeat rollback did not release host ownership, got %#v found=%t err=%v", owner, found, err)
	}
	if err := runCommit(context.Background(), []string{"--apply", "--run", "demo"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("expected commit after rollback to be refused, got %v", err)
	}
	if err := runMigrate(context.Background(), []string{"--live", "--run", "demo"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("expected live apply after rollback to be refused, got %v", err)
	}
}
