package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestCompletedLegacyStatefulRunRefusesCommitWithoutPersistedOrigin(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	installLegacyUpgradeDockerStub(t, workDir)
	installLegacyUpgradeDokployServer(t)
	writeCompletedLegacyRun(t, "legacy-commit", true, true)

	if err := runCommit(context.Background(), []string{"--apply", "--run", "legacy-commit"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot prove writer or traffic authority") {
		t.Fatalf("expected legacy commit to fail closed, got %v", err)
	}
	run, err := loadMigrationRun("legacy-commit")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.CommitStartedAt != nil || run.Run.CommittedAt != nil {
		t.Fatal("legacy commit refusal changed lifecycle state")
	}
	if run.Applied.APIVersion != appliedLegacyAPIVersion || run.Applied.TargetOrigin != "" {
		t.Fatalf("legacy commit refusal rewrote the ledger: %#v", run.Applied)
	}
	if phase := migrationRunPhase(run); phase != "authority-ambiguous" {
		t.Fatalf("completed legacy routed run phase=%q, want authority-ambiguous", phase)
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "establish writer and traffic authority manually") || !strings.Contains(next.Reason, "cannot prove writer or traffic authority") {
		t.Fatalf("completed legacy routed run gave unsafe next step: %#v", next)
	}
	_, found, err := readDokployTrafficOwner()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("legacy commit refusal created a traffic owner")
	}
	if _, err := os.Stat("source-stopped"); !os.IsNotExist(err) {
		t.Fatalf("legacy commit refusal changed the source: %v", err)
	}
}

func TestCompletedLegacyStatelessRunRefusesRollbackWithoutPersistedOrigin(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	installLegacyUpgradeDockerStub(t, workDir)
	installLegacyUpgradeDokployServer(t)
	writeCompletedLegacyRun(t, "legacy-rollback", false, true)

	if err := runRollback(context.Background(), []string{"--live", "--run", "legacy-rollback", "--confirm", "rollback legacy-rollback"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot prove writer or traffic authority") {
		t.Fatalf("expected legacy rollback to fail closed, got %v", err)
	}
	run, err := loadMigrationRun("legacy-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.RollbackStartedAt != nil || run.Run.RolledBackAt != nil {
		t.Fatal("legacy rollback refusal changed lifecycle state")
	}
	if run.Applied.APIVersion != appliedLegacyAPIVersion || run.Applied.TargetOrigin != "" {
		t.Fatalf("legacy rollback refusal rewrote the ledger: %#v", run.Applied)
	}
	_, found, err := readDokployTrafficOwner()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("legacy rollback refusal created a traffic owner")
	}
	for _, path := range []string{"target-proxy-stopped", "source-proxy-started"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy rollback refusal changed %s: %v", path, err)
		}
	}
}

func TestCompletedLegacyStatefulRunWithoutRoutesRefusesCommit(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	installLegacyUpgradeDockerStub(t, workDir)
	writeCompletedLegacyRun(t, "legacy-no-route", true, false)

	err := runCommit(context.Background(), []string{"--apply", "--run", "legacy-no-route"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot prove writer or traffic authority") {
		t.Fatalf("expected no-route legacy stateful commit to fail closed, got %v", err)
	}
	run, err := loadMigrationRun("legacy-no-route")
	if err != nil {
		t.Fatal(err)
	}
	if phase := migrationRunPhase(run); phase != "authority-ambiguous" {
		t.Fatalf("no-route legacy stateful phase=%q, want authority-ambiguous", phase)
	}
	if run.Run.CommitStartedAt != nil || run.Run.CommittedAt != nil {
		t.Fatal("no-route legacy stateful refusal changed commit lifecycle")
	}
	if _, err := os.Stat("source-stopped"); !os.IsNotExist(err) {
		t.Fatalf("no-route legacy stateful refusal changed the source: %v", err)
	}
}

func TestCompletedLegacyStatelessCoolifyRunRecordsManualRetirementWithoutHostOwner(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	installLegacyUpgradeDockerStub(t, workDir)
	installLegacyUpgradeDokployServer(t)
	writeCompletedLegacyRun(t, "legacy-coolify", false, false)

	commitErr := runCommit(context.Background(), []string{"--apply", "--run", "legacy-coolify"}, io.Discard, io.Discard)
	if commitErr == nil || !strings.Contains(commitErr.Error(), "Coolify can replace a stopped source container") {
		t.Fatalf("expected Coolify retirement refusal, got %v", commitErr)
	}
	run, err := loadMigrationRun("legacy-coolify")
	if err != nil {
		t.Fatal(err)
	}
	command := authorityRecoverySourceRetiredCommand(run)
	if !strings.Contains(commitErr.Error(), command) {
		t.Fatalf("commit refusal did not point at %q: %v", command, commitErr)
	}

	var stdout strings.Builder
	args := []string{"--run", "legacy-coolify", "--authority", "target", "--source-retired", "--confirm", authorityRecoverySourceRetiredConfirmation(run.Run)}
	if err := runRecoverAuthority(context.Background(), args, &stdout, io.Discard); err != nil {
		t.Fatalf("suggested recovery command failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "never held host ownership") {
		t.Fatalf("unexpected recovery output: %q", stdout.String())
	}
	run, err = loadMigrationRun("legacy-coolify")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.ResolvedAuthority != dokployTrafficTarget || run.Run.CommitStartedAt == nil || run.Run.CommittedAt == nil {
		t.Fatalf("manual retirement was not recorded: %#v", run.Run)
	}
	if _, found, err := readDokployTrafficOwner(); err != nil || found {
		t.Fatalf("ownerless recovery touched the host owner: found=%t err=%v", found, err)
	}
	if _, err := os.Stat("source-stopped"); !os.IsNotExist(err) {
		t.Fatalf("ownerless recovery changed the source: %v", err)
	}
	if err := runRecoverAuthority(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatalf("repeated recovery is not idempotent: %v", err)
	}
	if err := runCommit(context.Background(), []string{"--apply", "--run", "legacy-coolify"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("commit after manual retirement failed: %v", err)
	}
}

func TestCompletedLegacyRunCannotPromotePendingOwnerWithDifferentCredential(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	writeCompletedLegacyRun(t, "legacy-pending-owner", false, true)
	run, err := loadMigrationRun("legacy-pending-owner")
	if err != nil {
		t.Fatal(err)
	}
	const origin = "http://127.0.0.1:3030"
	run.Applied.TargetOrigin = origin
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(appliedPath, run.Applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, origin, dokployCredentialID("original-token")); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("legacy-pending-owner")
	if err != nil {
		t.Fatal(err)
	}
	err = ensureDokployTrafficTargetOwner(context.Background(), run, &dokploy.Client{BaseURL: origin, Token: "different-token"}, false)
	if err == nil || !strings.Contains(err.Error(), "different target credential identity") {
		t.Fatalf("expected pending ownership credential refusal, got %v", err)
	}
	owner, found, readErr := readDokployTrafficOwner()
	if readErr != nil || !found {
		t.Fatalf("read pending owner: found=%t err=%v", found, readErr)
	}
	if owner.Authority != dokployTrafficPending || owner.TargetCredentialID != dokployCredentialID("original-token") {
		t.Fatalf("credential mismatch changed pending ownership: %#v", owner)
	}
}

func writeCompletedLegacyRun(t *testing.T, name string, stateful, routed bool) {
	t.Helper()
	service := manifest.Service{ID: "source-id", Name: "web", Image: "example/api:latest"}
	if stateful {
		service.Mounts = []manifest.Mount{{Type: "volume", Name: "api-data", Target: "/data"}}
	}
	app := manifest.App{
		Name:     "api",
		Services: []manifest.Service{service},
	}
	if routed {
		app.Routes = []manifest.Route{{Host: "api.example.com", ServiceName: "web", Port: "3000"}}
	}
	writeTestBundle(t, "bundle", manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps:   []manifest.App{app},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bundle", "--run", name, "--observation-window", "0", "--rollback-window", "0"})
	run, err := loadMigrationRun(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range openSetupDecisions(run) {
		if err := recordReviewDecision(run, decision, run.Run.UpdatedAt.Add(2*time.Second)); err != nil {
			t.Fatalf("resolve setup decision: %v", err)
		}
	}
	now := time.Now().UTC()
	applied := runApplied{
		APIVersion:  appliedLegacyAPIVersion,
		RunName:     run.Run.Name,
		BundleDir:   run.Run.BundleDir,
		Target:      run.Run.Target,
		SucceededAt: &now,
	}
	for index, step := range dokploy.LegacyPlanFromArtifactsV1Alpha1(run.Prepare, run.Sync, run.Cutover).Steps {
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
	if err := writeJSONArtifact(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
}

func installLegacyUpgradeDokployServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/project.all" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]dokploy.Project{})
	}))
	t.Cleanup(server.Close)
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")
	return server
}

func installLegacyUpgradeDockerStub(t *testing.T, workDir string) {
	t.Helper()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
echo "$*" >> docker-calls
if [ "$1" = inspect ] && [ "$2" = --type ]; then
  ref="$4"
  running=true
  case "$ref" in
    coolify-proxy) [ -f source-proxy-started ] || running=false ;;
    dokploy-traefik) [ ! -f target-proxy-stopped ] || running=false ;;
    source-id) [ ! -f source-stopped ] || running=false ;;
  esac
  echo '[{"Id":"'"$ref"'","Name":"/'"$ref"'","State":{"Running":'"$running"',"Status":"running"},"HostConfig":{"RestartPolicy":{"Name":"always"}}}]'
elif [ "$1" = stop ]; then
  case "$*" in
    *dokploy-traefik) touch target-proxy-stopped ;;
    *source-id) touch source-stopped ;;
  esac
elif [ "$1" = start ] && [ "$2" = coolify-proxy ]; then
  touch source-proxy-started
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(strings.TrimSpace(stub)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
