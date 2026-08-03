package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
)

type testGuideInput struct {
	io.Reader
}

func (testGuideInput) guidePromptInput() {}

func TestRunWithoutArgsShowsGuidedStartWhenNoBundleOrRunExists(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("guide failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"No migration run was found.", "bort migrate --source coolify-local", "bort migrate --manifest manifest.json"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected guide output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestCommandHelpReturnsSuccess(t *testing.T) {
	for _, args := range [][]string{
		{"help"},
		{"help", "--advanced"},
		{"migrate", "-h"},
		{"rollback", "-h"},
		{"commit", "-h"},
		{"cleanup", "-h"},
		{"cleanup", "purge", "-h"},
		{"status", "-h"},
		{"env", "-h"},
		{"data", "-h"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if err := RunWithInput(context.Background(), args, strings.NewReader(""), &stdout, &stderr); err != nil {
				t.Fatalf("help failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "flag: help requested") || strings.Contains(stderr.String(), "flag: help requested") {
				t.Fatalf("help leaked flag.ErrHelp:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunWithoutArgsCreatesRunFromDefaultBundle(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("guide failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"local bundle → dokploy", "api", "READY"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected guide output to contain %q, got:\n%s", want, output)
		}
	}
	for _, notWant := range []string{"No migration run found", "Migration run created:", "Artifacts:"} {
		if strings.Contains(output, notWant) {
			t.Fatalf("did not expect guide output to contain %q, got:\n%s", notWant, output)
		}
	}
	entries, err := os.ReadDir(filepath.Join(workDir, ".bort", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one run directory, entries=%#v err=%v", entries, err)
	}
}

func TestRunWithoutArgsResumesLatestRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bort-bundle", "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("guide failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"local bundle → dokploy", "READY"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected resumed guide output to contain %q, got:\n%s", want, output)
		}
	}
	for _, notWant := range []string{"Creating a local dry-run", "Migration run:", "Artifacts:"} {
		if strings.Contains(output, notWant) {
			t.Fatalf("did not expect guide output to contain %q:\n%s", notWant, output)
		}
	}
}

func TestRunWithoutArgsDoesNotPromoteOrRefreshMtimeFallback(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "legacy-app", Services: []manifest.Service{{Name: "legacy-app", Image: "example/legacy:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "legacy-run"})
	statePath := filepath.Join(workDir, ".bort", "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(workDir, ".bort", "runs", "legacy-run", "run.json")
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunWithInput(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("guide failed: %v\nstderr:\n%s", err, stderr.String())
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("bare bort refreshed an mtime-selected run")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("bare bort promoted an mtime-selected run to current: %v", err)
	}
	for _, want := range []string{"legacy-app", "not selected for mutation", "bort migrate --run legacy-run"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected fallback output to contain %q, got:\n%s", want, stdout.String())
		}
	}
}

func TestRunWithoutArgsDoesNotRestoreStaleCurrentRunAfterRefresh(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	oldBundle := filepath.Join(workDir, "old-bundle")
	writeTestBundle(t, oldBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "old", Services: []manifest.Service{{Name: "old", Image: "example/old:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", oldBundle, "--run", "old-current"})
	oldRun, err := loadMigrationRun("old-current")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, err := writeRunSourceManifest(oldRun.Run.RunDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "old", Services: []manifest.Service{{Name: "old", Image: "example/old:v1"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRun.Run.Source = "docker"
	oldRun.Run.ManifestPath = manifestPath
	if err := writeJSONArtifact(filepath.Join(oldRun.Run.RunDir, "run.json"), oldRun.Run); err != nil {
		t.Fatal(err)
	}
	runRef, currentSelected, found, err := guideRunRef()
	if err != nil || !found || !currentSelected || runRef != oldRun.Run.RunDir {
		t.Fatalf("unexpected initial guide selection: ref=%q current=%t found=%t err=%v", runRef, currentSelected, found, err)
	}
	applyActive, err := applyRunActive(runRef)
	if err != nil || applyActive {
		t.Fatalf("unexpected initial apply state: active=%t err=%v", applyActive, err)
	}
	if !shouldAutoRescanRun(oldRun.Run) {
		t.Fatal("old current run is not eligible for source refresh")
	}
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	enteredPath := filepath.Join(workDir, "guide-entered")
	releasePath := filepath.Join(workDir, "guide-release")
	dockerScript := `#!/bin/sh
if [ "$1" = "ps" ]; then
  touch "$BORT_GUIDE_ENTERED"
  while [ ! -f "$BORT_GUIDE_RELEASE" ]; do sleep 0.01; done
  printf 'reviewed-container\n'
elif [ "$1" = "inspect" ]; then
  printf '%s\n' '[{"Id":"reviewed-container","Name":"/reviewed","Image":"sha256:reviewed","Config":{"Image":"example/reviewed:v2","Labels":{}},"State":{"Status":"running"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}}]'
elif [ "$1" = "image" ]; then
  printf '%s\n' '[{"Id":"sha256:reviewed","RepoDigests":[]}]'
fi
`
	if err := os.WriteFile(dockerPath, []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BORT_GUIDE_ENTERED", enteredPath)
	t.Setenv("BORT_GUIDE_RELEASE", releasePath)
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })
	errCh := make(chan error, 1)
	go func() {
		errCh <- runGuide(context.Background(), strings.NewReader(""), io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-errCh:
			t.Fatalf("guide exited before source refresh: %v", err)
		default:
		}
		if _, err := os.Stat(enteredPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("guide refresh did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	newBundle := filepath.Join(workDir, "new-bundle")
	writeTestBundle(t, newBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "new", Services: []manifest.Service{{Name: "new", Image: "example/new:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", newBundle, "--run", "new-current"})
	newRun, err := loadMigrationRun("new-current")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("stale guide refresh failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale guide refresh did not finish")
	}
	current, ok, err := currentRunRef()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || current != filepath.ToSlash(filepath.Clean(newRun.Run.RunDir)) {
		t.Fatalf("stale guide for %q replaced newer current run %q with %q", oldRun.Run.RunDir, newRun.Run.RunDir, current)
	}
}

func TestRunWithoutArgsSurfacesApplyLockProbeError(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "lock-error"})
	run, err := loadMigrationRun("lock-error")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workDir, "unexpected-lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(run.Run.RunDir, "apply.lock")); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunWithInput(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("guide failed instead of surfacing lock recovery: %v", err)
	}
	if !strings.Contains(stdout.String(), "LOCK ERROR") || !strings.Contains(stdout.String(), "apply.lock") {
		t.Fatalf("expected lock recovery guidance, got:\n%s", stdout.String())
	}
}

func TestRunWithoutArgsPromptsForManifestAndPrivateEnvMode(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	manifestPath := filepath.Join(workDir, "manifest.json")
	if err := writeJSONArtifact(manifestPath, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{{
					Name:  "api",
					Image: "example/api:latest",
					Environment: []manifest.EnvVar{
						{Name: "API_TOKEN", Value: "private-token", ValueKnown: true, Sensitive: true},
					},
				}},
				Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Source 4 (manifest), then manifest path. No env-mode prompt anymore.
	input := testGuideInput{Reader: strings.NewReader("4\n" + manifestPath + "\n")}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunWithInput(context.Background(), nil, input, &stdout, &stderr); err != nil {
		t.Fatalf("guided setup failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}

	output := stdout.String()
	for _, want := range []string{"Migration setup", "Existing manifest → dokploy"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected guided output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "private-token") {
		t.Fatalf("guided output exposed env value:\n%s", output)
	}

	entries, err := os.ReadDir(filepath.Join(workDir, ".bort", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one run directory, entries=%#v err=%v", entries, err)
	}
	runName := entries[0].Name()
	run := readJSONFile[migrationRun](t, filepath.Join(workDir, ".bort", "runs", runName, "run.json"))
	if run.Source != "manifest" || run.ManifestPath == manifestPath || !strings.HasPrefix(filepath.Base(run.ManifestPath), "manifest-") {
		t.Fatalf("unexpected guided run metadata: %#v", run)
	}
	if err := containedPath(run.RunDir, run.ManifestPath); err != nil {
		t.Fatalf("expected guided manifest to be a private run artifact: %v", err)
	}
	if err := containedPath(run.RunDir, run.BundleDir); err != nil {
		t.Fatalf("expected guided source bundle to be a private run snapshot: %v", err)
	}
	envPath := filepath.Join(run.BundleDir, "api", ".env.api")
	envFile, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envFile), "API_TOKEN=private-token") {
		t.Fatalf("expected private env file to preserve known value, got %q", string(envFile))
	}
	envExamplePath := filepath.Join(run.BundleDir, "api", ".env.api.example")
	envExample, err := os.ReadFile(envExamplePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envExample), "private-token") {
		t.Fatalf("example env file exposed private value: %q", string(envExample))
	}
}

func TestSourceRefreshPublishesVersionedManifestWithPlan(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	before := prepareScannableGuideRun(t, workDir, "source-refresh")
	metadataPath := filepath.Join(before.Run.RunDir, "run.json")
	metadataBefore, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(before.Run.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}

	refreshedBundle, refreshedManifest, err := refreshRunSourceBundle(context.Background(), before.Run)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(refreshedBundle)
	if refreshedManifest == before.Run.ManifestPath || !strings.HasPrefix(filepath.Base(refreshedManifest), "manifest-") {
		t.Fatalf("expected a new versioned source manifest, old=%s new=%s", before.Run.ManifestPath, refreshedManifest)
	}
	publishedBefore, err := readRunMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if publishedBefore.ManifestPath != before.Run.ManifestPath {
		t.Fatalf("new source manifest became visible before plan publication: %#v", publishedBefore)
	}
	if current, err := os.ReadFile(metadataPath); err != nil || !bytes.Equal(current, metadataBefore) {
		t.Fatalf("source scan changed published metadata before plan publication: err=%v", err)
	}
	if current, err := os.ReadFile(before.Run.ManifestPath); err != nil || !bytes.Equal(current, manifestBefore) {
		t.Fatalf("source scan changed the published manifest: err=%v", err)
	}

	operationLock, err := acquireRunOperationLock(before.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	after, refreshErr := refreshMigrationRunLockedWithInputs(before.Run.RunDir, refreshedBundle, refreshedManifest)
	operationLock.Release()
	if refreshErr != nil {
		t.Fatal(refreshErr)
	}
	if after.Run.ManifestPath != refreshedManifest {
		t.Fatalf("plan publication did not switch to the new manifest: %#v", after.Run)
	}
	for _, name := range []string{
		after.Run.Artifacts.Prepare,
		after.Run.Artifacts.Sync,
		after.Run.Artifacts.Cutover,
		after.Run.Artifacts.Rollback,
		after.Run.Artifacts.Commit,
		after.Run.Artifacts.Decisions,
		after.Run.Artifacts.Progress,
		after.Run.Artifacts.Applied,
	} {
		if _, err := os.Stat(runArtifactPath(after.Run.RunDir, name)); err != nil {
			t.Fatalf("published artifact %s does not exist: %v", name, err)
		}
	}
	if current, err := os.ReadFile(before.Run.ManifestPath); err != nil || !bytes.Equal(current, manifestBefore) {
		t.Fatalf("source refresh changed the previous manifest generation: err=%v", err)
	}
}

func TestFailedSourceRefreshPreservesPublishedManifestAndMetadata(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	before := prepareScannableGuideRun(t, workDir, "failed-source-refresh")
	metadataPath := filepath.Join(before.Run.RunDir, "run.json")
	metadataBefore, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(before.Run.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	progressPath := runArtifactPath(before.Run.RunDir, before.Run.Artifacts.Progress)
	if err := os.WriteFile(progressPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := refreshGuideRun(context.Background(), before.Run.RunDir, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("expected source refresh publication to fail")
	}
	metadataAfter, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(metadataAfter, metadataBefore) {
		t.Fatal("failed source refresh changed published run metadata")
	}
	manifestAfter, err := os.ReadFile(before.Run.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatal("failed source refresh changed the published manifest")
	}
	manifests, err := filepath.Glob(filepath.Join(before.Run.RunDir, "manifest-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0] != before.Run.ManifestPath {
		t.Fatalf("failed source refresh left an unpublished manifest: %#v", manifests)
	}
	entries, err := os.ReadDir(before.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "source-bundle-") || strings.HasPrefix(entry.Name(), "plan-") {
			t.Fatalf("failed source refresh left unpublished generation %s", entry.Name())
		}
	}
}

func TestSourceRefreshRejectsInvalidAppliedLedger(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	run := prepareScannableGuideRun(t, workDir, "invalid-applied")
	metadataPath := filepath.Join(run.Run.RunDir, "run.json")
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	appliedPath := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err := os.WriteFile(appliedPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := refreshGuideRun(context.Background(), run.Run.RunDir, strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "read applied migration progress") {
		t.Fatalf("expected invalid applied ledger to block refresh, got %v", err)
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("invalid applied ledger allowed the reviewed plan to be regenerated")
	}
}

func prepareScannableGuideRun(t *testing.T, workDir, runName string) loadedMigrationRun {
	t.Helper()
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	dockerScript := `#!/bin/sh
if [ "$1" = "ps" ]; then
  printf 'reviewed-container\n'
elif [ "$1" = "inspect" ]; then
  printf '%s\n' '[{"Id":"reviewed-container","Name":"/reviewed","Image":"sha256:reviewed","Config":{"Image":"example/reviewed:v2","Labels":{}},"State":{"Status":"running"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}}]'
elif [ "$1" = "image" ]; then
  printf '%s\n' '[{"Id":"sha256:reviewed","RepoDigests":[]}]'
fi
`
	if err := os.WriteFile(dockerPath, []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bundleDir := filepath.Join(workDir, "legacy-source-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed", Services: []manifest.Service{{Name: "reviewed", Image: "example/reviewed:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", runName})
	run, err := loadMigrationRun(runName)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, err := writeRunSourceManifest(run.Run.RunDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed", Services: []manifest.Service{{Name: "reviewed", Image: "example/reviewed:v1"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Run.Source = "docker"
	run.Run.BundleDir = bundleDir
	run.Run.SourceBundleDir = ""
	run.Run.ManifestPath = manifestPath
	run.Prepare.BundleDir = bundleDir
	run.Sync.BundleDir = bundleDir
	run.Cutover.BundleDir = bundleDir
	run.Rollback.BundleDir = bundleDir
	run.Commit.BundleDir = bundleDir
	run.Decisions.BundleDir = bundleDir
	run.Applied.BundleDir = bundleDir
	for path, value := range map[string]any{
		run.Run.Artifacts.Prepare:   run.Prepare,
		run.Run.Artifacts.Sync:      run.Sync,
		run.Run.Artifacts.Cutover:   run.Cutover,
		run.Run.Artifacts.Rollback:  run.Rollback,
		run.Run.Artifacts.Commit:    run.Commit,
		run.Run.Artifacts.Decisions: run.Decisions,
	} {
		if err := writeJSONArtifact(runArtifactPath(run.Run.RunDir, path), value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), run.Applied); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(filepath.Join(run.Run.RunDir, "run.json"), run.Run); err != nil {
		t.Fatal(err)
	}
	return run
}
