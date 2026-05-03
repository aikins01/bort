package cli

import (
	"bytes"
	"context"
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

func TestRunMigrateCreatesLocalRunArtifactsAndSummary(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000"}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "marketmap", "--observation-window", "0", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate failed: %v\nstderr:\n%s", err, stderr.String())
	}

	runDir := filepath.Join(workDir, ".bort", "runs", "marketmap")
	for _, name := range []string{"run.json", "prepare.json", "sync.json", "cutover.json", "rollback.json", "commit.json", "decisions.json"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}

	run := readJSONFile[migrationRun](t, filepath.Join(runDir, "run.json"))
	if run.APIVersion != runAPIVersion || !run.DryRun || run.Name != "marketmap" || run.BundleDir != bundleDir || run.Target != "dokploy" {
		t.Fatalf("unexpected run metadata: %#v", run)
	}
	prepareResult := readJSONFile[preparer.Result](t, filepath.Join(runDir, "prepare.json"))
	syncResult := readJSONFile[syncplan.Result](t, filepath.Join(runDir, "sync.json"))
	cutoverResult := readJSONFile[gateway.Result](t, filepath.Join(runDir, "cutover.json"))
	rollbackResult := readJSONFile[rollbackplan.Result](t, filepath.Join(runDir, "rollback.json"))
	commitResult := readJSONFile[commitplan.Result](t, filepath.Join(runDir, "commit.json"))
	decisions := readJSONFile[runDecisions](t, filepath.Join(runDir, "decisions.json"))
	if prepareResult.APIVersion != preparer.APIVersion || syncResult.APIVersion != syncplan.APIVersion || cutoverResult.APIVersion != gateway.APIVersion || rollbackResult.APIVersion != rollbackplan.APIVersion || commitResult.APIVersion != commitplan.APIVersion {
		t.Fatalf("unexpected artifact api versions")
	}
	if !syncResult.DryRun || !cutoverResult.DryRun || !rollbackResult.DryRun || !commitResult.DryRun {
		t.Fatalf("expected all downstream artifacts to be dry-run")
	}
	if decisions.APIVersion != decisionsAPIVersion || !decisions.DryRun || len(decisions.Decisions) == 0 || decisions.Decisions[0].ID != "cutover" {
		t.Fatalf("unexpected decisions artifact: %#v", decisions)
	}

	output := stdout.String()
	for _, want := range []string{
		"Migration run created: .bort/runs/marketmap",
		"Overall: needs_decision (yellow)",
		"Apps: 1 total, 0 green, 1 yellow, 0 red",
		"Routes: 1 cutover, 1 rollback, 1 commit",
		"Decisions: 3 open",
		"Open decisions:",
		"cutover needs_decision: confirm cutover readiness for 1 app(s) (2 item(s))",
		"Next safe step: confirm cutover readiness for 1 app(s)",
		"Next decision: cutover",
		"Next artifact: .bort/runs/marketmap/decisions.json",
		"Dry run only: no target resources, sync operations, route changes, ownership commits, or source cleanup were executed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected migrate output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunStatusAndNextReadExistingRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "marketmap", "--observation-window", "0", "--rollback-window", "0"})

	var statusOut bytes.Buffer
	var statusErr bytes.Buffer
	if err := runStatus(context.Background(), []string{"--run", "marketmap"}, &statusOut, &statusErr); err != nil {
		t.Fatalf("status failed: %v\nstderr:\n%s", err, statusErr.String())
	}
	for _, want := range []string{"Migration run: .bort/runs/marketmap", "Run: marketmap", "Overall: needs_decision (yellow)", "Next decision: cutover", "Next artifact: .bort/runs/marketmap/decisions.json"} {
		if !strings.Contains(statusOut.String(), want) {
			t.Fatalf("expected status output to contain %q, got:\n%s", want, statusOut.String())
		}
	}

	var nextOut bytes.Buffer
	var nextErr bytes.Buffer
	if err := runNext(context.Background(), []string{"marketmap"}, &nextOut, &nextErr); err != nil {
		t.Fatalf("next failed: %v\nstderr:\n%s", err, nextErr.String())
	}
	for _, want := range []string{"Next safe step: confirm cutover readiness for 1 app(s)", "Artifact: .bort/runs/marketmap/decisions.json", "Decision: cutover", "Dry run only: no live migration action is executed by this command."} {
		if !strings.Contains(nextOut.String(), want) {
			t.Fatalf("expected next output to contain %q, got:\n%s", want, nextOut.String())
		}
	}
}

func TestRunMigratePreservesPrepareBlockersBeforeDecisions(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "blocked-app"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Overall: blocked (red)",
		"Open decisions:",
		"deploy_artifacts blocked: fix deploy artifacts for 1 app(s) (2 item(s))",
		"Next safe step: fix deploy artifacts for 1 app(s)",
		"Next decision: deploy_artifacts",
		"Next artifact: .bort/runs/blocked-app/decisions.json",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected blocked run output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunMigrateExcludesPlatformAppsFromGuidedSummary(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{
			{Name: "api", Metadata: map[string]string{"migrationRole": "candidate"}, Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
			{Name: "coolify-proxy", Metadata: map[string]string{"migrationRole": "platform"}, Services: []manifest.Service{{Name: "traefik", Image: "traefik:v3", Environment: []manifest.EnvVar{{Name: "PROXY_TOKEN"}}}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "platform-filter", "--observation-window", "0", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"Apps: 1 total", "Platform/internal apps excluded: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected platform-filtered output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "coolify-proxy/env.values_required") {
		t.Fatalf("did not expect platform gates in guided summary:\n%s", output)
	}
}

func writeTestBundle(t *testing.T, bundleDir string, m manifest.Manifest) {
	t.Helper()
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir}); err != nil {
		t.Fatal(err)
	}
}
