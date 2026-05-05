package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRunCommitWritesTextPlan(t *testing.T) {
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
	if err := runCommit(context.Background(), []string{"--bundle", dir, "--target", "dokploy"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Commit plan: " + dir + " -> dokploy",
		"[yellow] api",
		"readiness: needs_decision",
		"cutover readiness: needs_decision",
		"rollback window: 3600s",
		"needs_decision accept dokploy.domain:api.example.com; retire source.route:api.example.com service=api port=3000",
		"warn commit.rollback_window_closed: confirm rollback window for api.example.com is closed or explicitly waived before retiring source route",
		"Dry run only: no target ownership was committed and no source resources were retired.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected commit output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunCommitWritesJSONPlan(t *testing.T) {
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
	if err := runCommit(context.Background(), []string{"--bundle", dir, "--format", "json", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result commitplan.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("commit json did not decode: %v\n%s", err, stdout.String())
	}
	if result.APIVersion != commitplan.APIVersion || !result.DryRun || result.Target != "dokploy" || len(result.Apps) != 1 {
		t.Fatalf("unexpected commit json: %#v", result)
	}
	if len(result.Apps[0].Routes) != 1 || len(result.Apps[0].Steps) != 4 {
		t.Fatalf("expected commit route and steps, got %#v", result.Apps[0])
	}
	if result.Apps[0].RollbackWindowSeconds != 0 {
		t.Fatalf("expected explicit zero rollback window, got %#v", result.Apps[0])
	}
	for _, gate := range result.Apps[0].Gates {
		if gate.Code == "commit.rollback_window_closed" {
			t.Fatalf("did not expect rollback window gate for explicit zero window: %#v", result.Apps[0].Gates)
		}
	}
}

func TestRequireLiveApplySucceededAcceptsSkippedPlatformSteps(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{
			{Name: "proxy", Role: "platform"},
			{Name: "api"},
		}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{
			Name:   "api",
			Routes: []gateway.Route{{Host: "api.example.com"}},
		}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	for index, step := range steps {
		status := string(dokploy.StepStatusOK)
		if step.App == "proxy" {
			status = string(dokploy.StepStatusSkipped)
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: status,
		})
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		t.Fatalf("expected skipped platform steps to count as completed: %v", err)
	}
}
