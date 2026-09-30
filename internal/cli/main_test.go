package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aikins01/bort/internal/target/dokploy"
)

// bortCommand output must stay deterministic in tests, even when go test is
// invoked via sudo.
func TestMain(m *testing.M) {
	os.Setenv("SUDO_UID", "")
	os.Setenv(envDokployEndpointMode, "")
	dir, err := os.MkdirTemp("", "bort-cli-test-lock-")
	if err != nil {
		panic(err)
	}
	dokployLiveOperationLockPath = func() (string, error) {
		return filepath.Join(dir, "dokploy-live.lock"), nil
	}
	dokployTrafficOwnerPath = func() (string, error) {
		return filepath.Join(dir, "dokploy-traffic-owner.json"), nil
	}
	dokployLiveOperationsSupported = func() bool { return true }
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error { return nil }
	verifyLocalSourceEngine = func(context.Context, loadedMigrationRun) error { return nil }
	verifyLocalDokployClient = func(context.Context, *dokploy.Client) error { return nil }
	verifyLocalDokployCleanupHost = func(context.Context, *dokploy.Client) error { return nil }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func markRunLocallyScanned(t *testing.T, runRef, source string) {
	t.Helper()
	run, err := loadMigrationRun(runRef)
	if err != nil {
		t.Fatal(err)
	}
	run.Run.Source = source
	if err := writeJSONArtifact(filepath.Join(run.Run.RunDir, "run.json"), run.Run); err != nil {
		t.Fatal(err)
	}
}

func resetDokployTrafficOwner(t *testing.T) {
	t.Helper()
	path, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
	})
}

func persistRunFixture(t *testing.T, run migrationRun) migrationRun {
	t.Helper()
	run.APIVersion = runAPIVersion
	run.DryRun = true
	if err := os.MkdirAll(filepath.FromSlash(run.RunDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(filepath.Join(filepath.FromSlash(run.RunDir), "run.json"), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func reloadRunFixture(t *testing.T, run migrationRun) loadedMigrationRun {
	t.Helper()
	current, err := readRunMetadata(filepath.Join(filepath.FromSlash(run.RunDir), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	current.RunDir = run.RunDir
	return loadedMigrationRun{Run: current, Applied: newRunApplied(current)}
}
