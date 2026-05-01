package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func TestRunPrepareWritesTextPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{{
					Name:  "api",
					Image: "example/api:latest",
				}},
				Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runPrepare(context.Background(), []string{"--bundle", dir, "--target", "dokploy"}, &stdout, &stderr); err != nil {
		t.Fatalf("prepare failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Prepare plan: " + dir + " -> dokploy",
		"[green] api",
		"info compose: would create dokploy compose app from compose.yaml",
		"info route: would create dokploy domain api.example.com for service api",
		"Dry run only: no resources were created or changed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected prepare output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunPrepareWritesJSONPlan(t *testing.T) {
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
	if err := runPrepare(context.Background(), []string{"--bundle", dir, "--format", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("prepare failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result preparer.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("prepare json did not decode: %v\n%s", err, stdout.String())
	}
	if result.Target != "dokploy" || len(result.Apps) != 1 || result.Apps[0].Name != "api" {
		t.Fatalf("unexpected prepare json: %#v", result)
	}
}

func TestRunPrepareFiltersByApp(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
			{Name: "worker", Services: []manifest.Service{{Name: "worker", Image: "example/worker"}}, Routes: []manifest.Route{{Host: "worker.example.com", ServiceName: "worker"}}},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runPrepare(context.Background(), []string{"--bundle", filepath.Clean(dir), "--app", "worker"}, &stdout, &stderr); err != nil {
		t.Fatalf("prepare failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "[green] worker") || strings.Contains(output, "[green] api") {
		t.Fatalf("expected prepare app filter to include only worker, got:\n%s", output)
	}
}
