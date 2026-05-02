package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func TestRunSyncWritesTextPlan(t *testing.T) {
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
	if err := runSync(context.Background(), []string{"--bundle", dir, "--target", "dokploy"}, &stdout, &stderr); err != nil {
		t.Fatalf("sync failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Sync plan: " + dir + " -> dokploy",
		"[green] api",
		"readiness: ready_to_create",
		"prepare readiness: ready_to_create",
		"ready_to_create target_prepare app: prepare_target_app_shell",
		"info state-sync: no state sync resources detected",
		"Dry run only: no sync operations were executed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected sync output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunSyncWritesJSONPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "postgres support",
				Services: []manifest.Service{{
					Name:  "postgres",
					Image: "postgres:16-alpine",
					Mounts: []manifest.Mount{{
						Type:   "volume",
						Name:   "pgdata",
						Target: "/var/lib/postgresql/data",
						RW:     true,
					}},
				}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runSync(context.Background(), []string{"--bundle", dir, "--format", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("sync failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result syncplan.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("sync json did not decode: %v\n%s", err, stdout.String())
	}
	if result.APIVersion != syncplan.APIVersion || !result.DryRun || result.Target != "dokploy" || len(result.Apps) != 1 {
		t.Fatalf("unexpected sync json: %#v", result)
	}
	if len(result.Apps[0].Steps) < 3 {
		t.Fatalf("expected sync json steps, got %#v", result.Apps[0].Steps)
	}
}
