package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/manifest"
)

func TestValidateStateAppRejectsUnknownAppAfterRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writePrivateTestBundle(t, bundleDir, manifest.Manifest{
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
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--observation-window", "0", "--rollback-window", "0"})

	var stdout, stderr bytes.Buffer
	err := RunWithInput(context.Background(), []string{"env", "ap", "FIRST=1"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected validation error for unknown app")
	}
	if !strings.Contains(err.Error(), "known apps: api") {
		t.Fatalf("expected error to list known apps, got %v", err)
	}

	state, _ := readBortState(filepath.Join(workDir, ".bort", "state.json"))
	if _, ok := state.Apps["ap"]; ok {
		t.Fatalf("expected no state entry for rejected app, got %#v", state.Apps)
	}
}

func TestValidateStateDataStoreRejectsUnknownStoreAfterRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writePrivateTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{
					{Name: "api", Image: "example/api:latest"},
					{
						Name:   "postgres-1",
						Image:  "postgres:16-alpine",
						Mounts: []manifest.Mount{{Type: "volume", Name: "pgdata", Target: "/var/lib/postgresql/data"}},
						Labels: map[string]string{"com.docker.compose.service": "postgres"},
					},
				},
				Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--observation-window", "0", "--rollback-window", "0"})

	var stdout, stderr bytes.Buffer
	err := RunWithInput(context.Background(), []string{"data", "api", "redis", "--migrate"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected validation error for unknown data store")
	}
	if !strings.Contains(err.Error(), "known stores:") {
		t.Fatalf("expected error to list known stores, got %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := RunWithInput(context.Background(), []string{"data", "api", "postgres", "--migrate"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("expected known data store to be accepted, got %v", err)
	}
}

func TestValidateStateAppAcceptsAnyAppWhenNoRunExists(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"env", "anything", "FIRST=1"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("expected env to succeed without a run, got %v", err)
	}
}
