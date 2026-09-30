package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestDokployTrafficRunIDIgnoresSymlinkedRunDirSpelling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	base := t.TempDir()
	realDir := filepath.Join(base, "real", "runs", "run-a")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	viaReal := migrationRun{Name: "run-a", RunDir: realDir, CreatedAt: now, BundleDigest: "digest-a"}
	viaAlias := migrationRun{Name: "run-a", RunDir: filepath.Join(base, "alias", "runs", "run-a"), CreatedAt: now, BundleDigest: "digest-a"}
	realID, err := dokployTrafficRunID(viaReal)
	if err != nil {
		t.Fatal(err)
	}
	aliasID, err := dokployTrafficRunID(viaAlias)
	if err != nil {
		t.Fatal(err)
	}
	if realID != aliasID {
		t.Fatalf("run ID depends on path spelling: real=%s alias=%s", realID, aliasID)
	}
}

func TestDokployTrafficOwnerSerializesRunsAcrossWorkspaces(t *testing.T) {
	resetDokployTrafficOwner(t)
	now := time.Now().UTC()
	workspace := t.TempDir()
	t.Chdir(workspace)
	claimed := persistRunFixture(t, migrationRun{Name: "run-a", RunDir: filepath.Join(".bort", "runs", "run-a"), CreatedAt: now, BundleDigest: "digest-a", RolledBackAt: &now})
	runB := migrationRun{Name: "run-b", RunDir: filepath.Join(t.TempDir(), "run-b"), CreatedAt: time.Now().UTC(), BundleDigest: "digest-b"}
	plan := dokploy.Plan{Steps: []dokploy.Step{{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"}}}

	if err := ensureDokployTrafficRunAvailable(claimed, plan); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(claimed, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(claimed, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}

	runADir, err := dokployTrafficRunDir(claimed)
	if err != nil {
		t.Fatal(err)
	}
	runALabel := "migration run \"run-a\" at \"" + runADir + "\""
	runA := claimed
	runA.RunDir = filepath.Join(workspace, ".bort", "runs", "run-a")

	t.Chdir(t.TempDir())
	if err := requireDokployTrafficTarget(runA, false); err != nil {
		t.Fatalf("same run lost ownership after changing workspaces: %v", err)
	}
	if err := ensureDokployTrafficRunAvailable(runB, plan); err == nil || !strings.Contains(err.Error(), "owned by "+runALabel) {
		t.Fatalf("expected second run target-mutation refusal, got %v", err)
	}
	if err := requireDokployTrafficTarget(runB, false); err == nil || !strings.Contains(err.Error(), "current durable owner is "+runALabel) {
		t.Fatalf("expected second run rollback refusal, got %v", err)
	}
	if err := markDokployTrafficSource(runA); err != nil {
		t.Fatal(err)
	}
	if err := ensureDokployTrafficRunAvailable(runB, plan); err == nil || !strings.Contains(err.Error(), "owned by "+runALabel) {
		t.Fatalf("source authority released ownership before rollback finalization: %v", err)
	}
	if err := releaseDokployTrafficOwner(runA); err != nil {
		t.Fatal(err)
	}
	if err := ensureDokployTrafficRunAvailable(runB, plan); err != nil {
		t.Fatalf("finalized rollback did not release the proxy for a new run: %v", err)
	}
}

func TestDokployTrafficOwnerBindsCredentialWithoutPersistingToken(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := migrationRun{Name: "bound", RunDir: filepath.Join(t.TempDir(), "bound"), CreatedAt: time.Now().UTC(), BundleDigest: "digest"}
	const token = "organization-a-secret-token"
	if err := claimDokployHostOwnership(run, "http://127.0.0.1:3030", dokployCredentialID(token)); err != nil {
		t.Fatal(err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found {
		t.Fatalf("read owner: found=%t err=%v", found, err)
	}
	wantDir, err := dokployTrafficRunDir(run)
	if err != nil {
		t.Fatal(err)
	}
	if owner.RunDir != wantDir || owner.TargetCredentialID != dokployCredentialID(token) {
		t.Fatalf("owner did not preserve run path and credential identity: %#v", owner)
	}
	path, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), token) {
		t.Fatal("durable host owner persisted the raw Dokploy token")
	}
	plan := dokploy.Plan{Steps: []dokploy.Step{{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"}}}
	err = validateDokployTrafficResume(run, plan, runApplied{}, "http://127.0.0.1:3030", dokployCredentialID("organization-b-secret-token"), 0)
	if err == nil || !strings.Contains(err.Error(), "different target credential identity") {
		t.Fatalf("expected swapped organization credential refusal, got %v", err)
	}
}

func TestInlineDokploySetupRefusesExistingRunBinding(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := migrationRun{Name: "bound", RunDir: filepath.Join(t.TempDir(), "bound"), CreatedAt: time.Now().UTC(), BundleDigest: "digest"}
	if err := validateInlineDokploySetup(run, runApplied{TargetOrigin: "http://127.0.0.1:3030"}, "http://127.0.0.1:3030"); err == nil || !strings.Contains(err.Error(), "restore the exact original target credential") {
		t.Fatalf("expected persisted target binding to block inline setup, got %v", err)
	}
	if err := claimDokployHostOwnership(run, "http://127.0.0.1:3030", dokployCredentialID("original-token")); err != nil {
		t.Fatal(err)
	}
	if err := validateInlineDokploySetup(run, runApplied{}, "http://127.0.0.1:3030"); err == nil || !strings.Contains(err.Error(), "bound to an existing Dokploy credential identity") {
		t.Fatalf("expected durable credential identity to block replacement setup, got %v", err)
	}
}

func TestDokployTrafficOwnerRefusesMissingStateAfterHandoff(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := migrationRun{Name: "resumed", RunDir: t.TempDir(), CreatedAt: time.Now().UTC(), BundleDigest: "digest"}
	plan := dokploy.Plan{Steps: []dokploy.Step{
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepStartDokployProxy, Ref: "dokploy-traefik"},
	}}
	if err := validateDokployTrafficResume(run, plan, runApplied{}, "http://127.0.0.1:3030", dokployCredentialID("test-token"), 1); err == nil || !strings.Contains(err.Error(), "without a matching durable host owner") {
		t.Fatalf("expected missing post-handoff owner to fail closed, got %v", err)
	}
}

func TestDokployTrafficOwnerRefusesMissingStateAfterRouteMutation(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := migrationRun{Name: "resumed", RunDir: t.TempDir(), CreatedAt: time.Now().UTC(), BundleDigest: "digest"}
	plan := dokploy.Plan{Steps: []dokploy.Step{
		{Kind: dokploy.StepInstallGateway, App: "api", Ref: "api.example.com"},
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepStartDokployProxy, Ref: "dokploy-traefik"},
	}}
	if index := dokployOwnershipStepIndex(plan); index != 0 {
		t.Fatalf("Dokploy ownership index = %d, want 0", index)
	}
	if err := validateDokployTrafficResume(run, plan, runApplied{}, "http://127.0.0.1:3030", dokployCredentialID("test-token"), 1); err == nil || !strings.Contains(err.Error(), "changed Dokploy target resources without a matching durable host owner") {
		t.Fatalf("expected missing post-route owner to fail closed, got %v", err)
	}
}

func TestDokployTrafficOwnerRefusesMissingStateAfterInterruptedRouteMutation(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := migrationRun{Name: "resumed", RunDir: t.TempDir(), CreatedAt: time.Now().UTC(), BundleDigest: "digest"}
	step := dokploy.Step{Kind: dokploy.StepInstallGateway, App: "api", Ref: "api.example.com"}
	plan := dokploy.Plan{Steps: []dokploy.Step{step, {Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"}}}
	applied := runApplied{Steps: []appliedStep{{
		Index: 0, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusStarted),
	}}}

	err := validateDokployTrafficResume(run, plan, applied, "http://127.0.0.1:3030", dokployCredentialID("test-token"), 0)
	if err == nil || !strings.Contains(err.Error(), "without a matching durable host owner") {
		t.Fatalf("expected interrupted route mutation with missing owner to fail closed, got %v", err)
	}
}

func TestTargetMutationDoesNotBorrowAnotherRunsDurableOwner(t *testing.T) {
	resetDokployTrafficOwner(t)
	runA := migrationRun{Name: "run-a", RunDir: filepath.Join(t.TempDir(), "run-a"), CreatedAt: time.Now().UTC(), BundleDigest: "digest-a"}
	runB := migrationRun{Name: "run-b", RunDir: filepath.Join(t.TempDir(), "run-b"), CreatedAt: time.Now().UTC(), BundleDigest: "digest-b"}
	if err := claimDokployHostOwnership(runB, "http://127.0.0.1:3030", dokployCredentialID("run-b-token")); err != nil {
		t.Fatal(err)
	}
	loaded := loadedMigrationRun{
		Run:     runA,
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Applied: runApplied{
			APIVersion:       appliedAPIVersion,
			RecoveryProtocol: appliedRecoveryProtocol,
			Steps: []appliedStep{{
				Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK),
			}},
		},
	}
	missing, err := targetMutationMissingDurableOwner(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !missing || !runMayHaveAmbiguousAuthority(loaded) {
		t.Fatalf("run A borrowed run B's owner: missing=%t ambiguous=%t", missing, runMayHaveAmbiguousAuthority(loaded))
	}
}

func TestDokploySourceCleanupRefusesEstablishedHostAuthority(t *testing.T) {
	resetDokployTrafficOwner(t)
	ownerRun := migrationRun{Name: "owner", RunDir: filepath.Join(t.TempDir(), "owner"), CreatedAt: time.Now().UTC(), BundleDigest: "owner"}
	cleanupRun := migrationRun{Name: "cleanup", RunDir: filepath.Join(t.TempDir(), "cleanup"), CreatedAt: time.Now().UTC(), BundleDigest: "cleanup"}
	if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(ownerRun, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	ownerDir, err := dokployTrafficRunDir(ownerRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureDokploySourceCleanupAvailable(cleanupRun); err == nil || !strings.Contains(err.Error(), "ownership belongs to migration run \"owner\" at \""+ownerDir+"\"") {
		t.Fatalf("expected historical source cleanup to refuse established target authority, got %v", err)
	}
}

func TestDokployTrafficOwnerRejectsMalformedAndSymlinkState(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		resetDokployTrafficOwner(t)
		path, err := dokployTrafficOwnerPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDokployTrafficOwner(); err == nil || !strings.Contains(err.Error(), "decode Dokploy traffic owner") {
			t.Fatalf("expected malformed owner refusal, got %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation may require elevated privileges")
		}
		resetDokployTrafficOwner(t)
		path, err := dokployTrafficOwnerPath()
		if err != nil {
			t.Fatal(err)
		}
		ownerRun := migrationRun{Name: "owner", RunDir: filepath.Join(t.TempDir(), "owner"), CreatedAt: time.Now().UTC(), BundleDigest: "owner"}
		if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
			t.Fatal(err)
		}
		if _, found, err := readDokployTrafficOwner(); err != nil || !found {
			t.Fatalf("claimed owner record is not valid: found=%t err=%v", found, err)
		}
		target := filepath.Join(t.TempDir(), "owner.json")
		if err := os.Rename(path, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDokployTrafficOwner(); err == nil || !strings.Contains(err.Error(), "read Dokploy traffic owner") {
			t.Fatalf("expected symlink owner state to fail closed on the no-follow read, got %v", err)
		}
	})
}

func TestDefaultDokployTrafficOwnerPathIsAbsoluteAcrossWorkspaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host Dokploy state is unsupported on Windows")
	}
	first, err := defaultDokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	second, err := defaultDokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(first) || first != second {
		t.Fatalf("host owner path must not depend on the working directory: %q vs %q", first, second)
	}
}

func TestReleasedHostOwnerSurvivesNextRunClaim(t *testing.T) {
	resetDokployTrafficOwner(t)
	now := time.Now().UTC()
	runA := persistRunFixture(t, migrationRun{Name: "run-a", RunDir: filepath.Join(t.TempDir(), "run-a"), Target: "dokploy", CreatedAt: now, BundleDigest: "digest-a", CommitStartedAt: &now, CommittedAt: &now, ResolvedAuthority: dokployTrafficTarget, AuthorityResolvedAt: &now})
	runB := migrationRun{Name: "run-b", RunDir: filepath.Join(t.TempDir(), "run-b"), CreatedAt: now, BundleDigest: "digest-b"}
	if err := claimDokployHostOwnership(runA, "http://127.0.0.1:3030", dokployCredentialID("token-a")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(runA, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := releaseDokployTargetOwner(runA); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(runB, "http://127.0.0.1:3030", dokployCredentialID("token-b")); err != nil {
		t.Fatal(err)
	}
	loaded := reloadRunFixture(t, runA)
	if loaded.Run.HostOwnerReleaseStartedAt == nil {
		t.Fatal("release did not persist its write-ahead marker in run.json")
	}
	if pending, err := completedDokployOwnerFinalization(loaded); err != nil || pending {
		t.Fatalf("completed run lost its finalization after another run claimed the host: pending=%t err=%v", pending, err)
	}
	if err := releaseDokployTargetOwner(loaded.Run); err != nil {
		t.Fatalf("repeat release after another run claimed the host: %v", err)
	}
	if _, found, err := matchingAuthorityRecoveryOwner(loaded.Run, dokployTrafficTarget); err != nil || found {
		t.Fatalf("authority recovery no longer recognizes the released owner: found=%t err=%v", found, err)
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil || !found || owner.RunName != "run-b" || owner.Authority != dokployTrafficPending {
		t.Fatalf("completed run disturbed the next run's ownership: found=%t err=%v owner=%#v", found, err, owner)
	}
}

func TestHostOwnerReleaseMarkerKeepsUnreleasedOwnerPending(t *testing.T) {
	resetDokployTrafficOwner(t)
	now := time.Now().UTC()
	run := persistRunFixture(t, migrationRun{Name: "run-a", RunDir: filepath.Join(t.TempDir(), "run-a"), Target: "dokploy", CreatedAt: now, BundleDigest: "digest-a", RollbackStartedAt: &now, RolledBackAt: &now})
	if err := claimDokployHostOwnership(run, "http://127.0.0.1:3030", dokployCredentialID("token-a")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficSource(run); err != nil {
		t.Fatal(err)
	}
	if err := markRunHostOwnerReleaseStartedLocked(run); err != nil {
		t.Fatal(err)
	}
	loaded := reloadRunFixture(t, run)
	if pending, err := completedDokployOwnerFinalization(loaded); err != nil || !pending {
		t.Fatalf("marker without a released owner must still require release: pending=%t err=%v", pending, err)
	}
	if err := releaseDokployTrafficOwner(loaded.Run); err != nil {
		t.Fatal(err)
	}
	owner, _, err := readDokployTrafficOwner()
	if err != nil || owner.Authority != dokployTrafficReleased {
		t.Fatalf("expected released owner, got %#v err=%v", owner, err)
	}
	if pending, err := completedDokployOwnerFinalization(loaded); err != nil || pending {
		t.Fatalf("release did not finalize: pending=%t err=%v", pending, err)
	}
}
