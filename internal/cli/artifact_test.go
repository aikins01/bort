package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func TestPlanArtifactsCanBePersistedAndConsumed(t *testing.T) {
	bundleDir := t.TempDir()
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
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir}); err != nil {
		t.Fatal(err)
	}

	artifactDir := t.TempDir()
	preparePath := filepath.Join(artifactDir, "prepare.json")
	syncPath := filepath.Join(artifactDir, "sync.json")
	cutoverPath := filepath.Join(artifactDir, "cutover.json")
	rollbackPath := filepath.Join(artifactDir, "rollback.json")
	commitPath := filepath.Join(artifactDir, "commit.json")

	runCommand(t, runPrepare, []string{"--bundle", bundleDir, "--format", "json", "--output", preparePath})
	prepareResult := readJSONFile[preparer.Result](t, preparePath)
	if prepareResult.BundleDir != bundleDir || prepareResult.APIVersion != preparer.APIVersion {
		t.Fatalf("unexpected prepare artifact: %#v", prepareResult)
	}

	runCommand(t, runSync, []string{"--from-prepare", preparePath, "--format", "json", "--output", syncPath})
	syncResult := readJSONFile[syncplan.Result](t, syncPath)
	if syncResult.BundleDir != bundleDir || syncResult.APIVersion != syncplan.APIVersion || !syncResult.DryRun {
		t.Fatalf("unexpected sync artifact: %#v", syncResult)
	}

	runCommand(t, runCutover, []string{"--from-prepare", preparePath, "--from-sync", syncPath, "--format", "json", "--output", cutoverPath, "--observation-window", "0"})
	cutoverResult := readJSONFile[gateway.Result](t, cutoverPath)
	if cutoverResult.BundleDir != bundleDir || cutoverResult.APIVersion != gateway.APIVersion || !cutoverResult.DryRun || len(cutoverResult.Apps[0].Routes) != 1 {
		t.Fatalf("unexpected cutover artifact: %#v", cutoverResult)
	}

	runCommand(t, runRollback, []string{"--from-cutover", cutoverPath, "--format", "json", "--output", rollbackPath, "--observation-window", "0"})
	rollbackResult := readJSONFile[rollbackplan.Result](t, rollbackPath)
	if rollbackResult.BundleDir != bundleDir || rollbackResult.APIVersion != rollbackplan.APIVersion || !rollbackResult.DryRun || len(rollbackResult.Apps[0].Routes) != 1 {
		t.Fatalf("unexpected rollback artifact: %#v", rollbackResult)
	}

	runCommand(t, runCommit, []string{"--from-cutover", cutoverPath, "--format", "json", "--output", commitPath, "--rollback-window", "0"})
	commitResult := readJSONFile[commitplan.Result](t, commitPath)
	if commitResult.BundleDir != bundleDir || commitResult.APIVersion != commitplan.APIVersion || !commitResult.DryRun || len(commitResult.Apps[0].Routes) != 1 {
		t.Fatalf("unexpected commit artifact: %#v", commitResult)
	}
	for _, gate := range commitResult.Apps[0].Gates {
		if gate.Code == "commit.rollback_window_closed" {
			t.Fatalf("did not expect rollback window gate after commit override: %#v", commitResult.Apps[0].Gates)
		}
	}
}

func TestRunSyncRejectsMismatchedPrepareArtifactBundle(t *testing.T) {
	bundleDir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	}
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir}); err != nil {
		t.Fatal(err)
	}

	preparePath := filepath.Join(t.TempDir(), "prepare.json")
	runCommand(t, runPrepare, []string{"--bundle", bundleDir, "--format", "json", "--output", preparePath})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runSync(context.Background(), []string{"--bundle", filepath.Join(bundleDir, "other"), "--from-prepare", preparePath}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "was created for bundle") {
		t.Fatalf("expected bundle compatibility error, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestRunCutoverRejectsSyncArtifactWithoutPrepareArtifact(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCutover(context.Background(), []string{"--from-sync", filepath.Join(t.TempDir(), "sync.json")}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--from-prepare is required") {
		t.Fatalf("expected missing prepare artifact error, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
}

type cliRunner func(context.Context, []string, io.Writer, io.Writer) error

func runCommand(t *testing.T, run cliRunner, args []string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatalf("command failed with args %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}
}

func readJSONFile[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, string(data))
	}
	return result
}
