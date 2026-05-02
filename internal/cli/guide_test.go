package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, want := range []string{"No local migration run or default bundle was found.", "bort export --manifest manifest.json --output-dir bort-bundle", "bort help"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected guide output to contain %q, got:\n%s", want, output)
		}
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
	for _, want := range []string{"Migration cockpit:", "Migration: local bundle -> dokploy", "Next: confirm cutover readiness for 1 app(s)", "Continue: bort continue"} {
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
	runCommand(t, runMigrate, []string{"--bundle", "bort-bundle", "--run", "marketmap", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("guide failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"Migration cockpit: marketmap", "Migration: local bundle -> dokploy", "Next: confirm cutover readiness for 1 app(s)", "Continue: bort continue"} {
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

func TestRunContinueResumesLatestRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", "bort-bundle", "--run", "marketmap", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), []string{"continue"}, &stdout, &stderr); err != nil {
		t.Fatalf("continue failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"Migration cockpit: marketmap", "Next: confirm cutover readiness for 1 app(s)", "Continue: bort continue"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected continue output to contain %q, got:\n%s", want, output)
		}
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

	input := testGuideInput{Reader: strings.NewReader("4\n\n2\ninclude\nguided-run\n" + manifestPath + "\n")}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunWithInput(context.Background(), nil, input, &stdout, &stderr); err != nil {
		t.Fatalf("guided setup failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}

	output := stdout.String()
	for _, want := range []string{"Migration setup", "Migration cockpit: guided-run", "Migration: Existing manifest -> dokploy", "Environment: known values kept in private local files"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected guided output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "private-token") {
		t.Fatalf("guided output exposed env value:\n%s", output)
	}

	run := readJSONFile[migrationRun](t, filepath.Join(workDir, ".bort", "runs", "guided-run", "run.json"))
	if run.Source != "manifest" || run.EnvMode != guideEnvInclude || run.ManifestPath != manifestPath {
		t.Fatalf("unexpected guided run metadata: %#v", run)
	}
	envPath := filepath.Join(workDir, ".bort", "runs", "guided-run", "bundle", "api", ".env.api")
	envFile, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envFile), "API_TOKEN=private-token") {
		t.Fatalf("expected private env file to preserve known value, got %q", string(envFile))
	}
	envExamplePath := filepath.Join(workDir, ".bort", "runs", "guided-run", "bundle", "api", ".env.api.example")
	envExample, err := os.ReadFile(envExamplePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envExample), "private-token") {
		t.Fatalf("example env file exposed private value: %q", string(envExample))
	}
}
