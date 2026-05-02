package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
)

func TestRunCutoverWritesTextPlan(t *testing.T) {
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
	if err := runCutover(context.Background(), []string{"--bundle", dir, "--target", "dokploy", "--observation-window", "120", "--rollback-window", "900"}, &stdout, &stderr); err != nil {
		t.Fatalf("cutover failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Cutover plan: " + dir + " -> dokploy",
		"[yellow] api",
		"readiness: needs_decision",
		"sync readiness: ready_to_create",
		"observe: 120s, rollback window: 900s",
		"ready_to_create source.route:api.example.com -> dokploy.domain:api.example.com service=api port=3000",
		"warn cutover.health_check_required: verify target health for api.example.com before route cutover",
		"Dry run only: no routes were changed and no rollback actions were executed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected cutover output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunCutoverWritesJSONPlan(t *testing.T) {
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
	if err := runCutover(context.Background(), []string{"--bundle", dir, "--format", "json", "--observation-window", "0", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("cutover failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result gateway.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("cutover json did not decode: %v\n%s", err, stdout.String())
	}
	if result.APIVersion != gateway.APIVersion || !result.DryRun || result.Target != "dokploy" || len(result.Apps) != 1 {
		t.Fatalf("unexpected cutover json: %#v", result)
	}
	if len(result.Apps[0].Routes) != 1 || len(result.Apps[0].Steps) != 5 {
		t.Fatalf("expected cutover route and steps, got %#v", result.Apps[0])
	}
	if result.Apps[0].ObservationWindowSeconds != 0 || result.Apps[0].RollbackWindowSeconds != 0 {
		t.Fatalf("expected explicit zero cutover windows, got %#v", result.Apps[0])
	}
}
