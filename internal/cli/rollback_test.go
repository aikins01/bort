package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
