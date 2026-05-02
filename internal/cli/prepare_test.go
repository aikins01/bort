package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
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
		"readiness: ready_to_create",
		"target shell: ready_to_create compose from compose.yaml",
		"dokploy dry-run: compose app api (ready_to_create), 1 domains, 0 env files, 0 volumes",
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
	if result.APIVersion != preparer.APIVersion || result.Target != "dokploy" || len(result.Apps) != 1 || result.Apps[0].Name != "api" {
		t.Fatalf("unexpected prepare json: %#v", result)
	}
	app := result.Apps[0]
	if app.Readiness != preparer.ReadinessReadyToCreate || app.Resources.App.ComposePath != "compose.yaml" {
		t.Fatalf("unexpected prepare app resources: %#v", app)
	}
	if len(app.Resources.Domains) != 1 || app.Resources.Domains[0].Host != "api.example.com" {
		t.Fatalf("unexpected prepare domain resources: %#v", app.Resources.Domains)
	}
	if app.TargetResources == nil || app.TargetResources.Platform != "dokploy" || !app.TargetResources.DryRun || app.TargetResources.Dokploy == nil {
		t.Fatalf("unexpected prepare target resources: %#v", app.TargetResources)
	}
	if app.TargetResources.Dokploy.ComposeApp.Name != "api" || app.TargetResources.Dokploy.ComposeApp.Readiness != preparer.ReadinessReadyToCreate || len(app.TargetResources.Dokploy.Domains) != 1 {
		t.Fatalf("unexpected dokploy target resources: %#v", app.TargetResources.Dokploy)
	}
}

func TestRunPrepareJSONOmitsEnvValues(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{{
					Name:  "api",
					Image: "example/api:latest",
					Environment: []manifest.EnvVar{
						{Name: "PUBLIC_URL", Value: "not-for-json", ValueKnown: true},
						{Name: "API_TOKEN", Value: "private-token-value", ValueKnown: true, Sensitive: true},
					},
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
	if err := runPrepare(context.Background(), []string{"--bundle", dir, "--format", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("prepare failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, value := range []string{"not-for-json", "private-token-value"} {
		if strings.Contains(output, value) {
			t.Fatalf("prepare json exposed env value %q:\n%s", value, output)
		}
	}

	var result preparer.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("prepare json did not decode: %v\n%s", err, stdout.String())
	}
	if len(result.Apps) != 1 || len(result.Apps[0].Resources.EnvFiles) != 1 {
		t.Fatalf("unexpected env resources: %#v", result)
	}
	envFile := result.Apps[0].Resources.EnvFiles[0]
	if !slices.Contains(envFile.Keys, "PUBLIC_URL") || !slices.Contains(envFile.Keys, "API_TOKEN") || !slices.Contains(envFile.MissingValues, "API_TOKEN") {
		t.Fatalf("unexpected env resource keys: %#v", envFile)
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
