package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
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
