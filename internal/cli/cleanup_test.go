package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRunCleanupInventoriesLeftoversAndSafeMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]dokploy.Project{{ProjectID: "p-proxy", Name: "proxy"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.one":
			if r.URL.Query().Get("projectId") != "p-proxy" {
				t.Fatalf("unexpected project.one query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(dokploy.Project{ProjectID: "p-proxy", Name: "proxy"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
			t.Fatalf("did not expect domain lookup for empty project")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")

	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Git: &manifest.GitSource{
				Repository: "https://github.com/example/api",
				Branch:     "main",
				Provider:   "github",
				SourceType: "App\\Models\\GithubApp",
				SourceID:   "42",
			},
			Services: []manifest.Service{{
				ID:       "cid123",
				Name:     "web",
				Image:    "example/api:latest",
				Mounts:   []manifest.Mount{{Type: "volume", Name: "api-data", Target: "/data"}},
				Networks: []manifest.ServiceNetwork{{Name: "api-net"}},
			}},
			Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "web"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "cleanup-run", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCleanup(context.Background(), []string{"--run", "cleanup-run"}, &stdout, &stderr); err != nil {
		t.Fatalf("cleanup failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Cleanup plan: .bort/runs/cleanup-run -> dokploy",
		"[present] proxy (p-proxy): safe metadata cleanup candidate: empty project with zero domains visible",
		"[absent] source: no Dokploy project with this stale platform name is visible",
		"Source containers inventoried for later purge:",
		"api/web web",
		"Source-control credentials left untouched:",
		"api https://github.com/example/api (github/coolify_github_app)",
		"Source volumes and bind mounts preserved:",
		"api api-data -> /data (volume)",
		"Source networks preserved:",
		"api api-net",
		"Target artifacts kept by default:",
		"Dry run only: run `bort cleanup --apply`",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected cleanup output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Source containers, volumes, networks, and target apps were removed") {
		t.Fatalf("cleanup dry run claimed destructive removal:\n%s", output)
	}
}

func TestCleanupStaleProjectNameCollisions(t *testing.T) {
	run := loadedMigrationRun{Prepare: preparer.Result{Apps: []preparer.AppPlan{
		{Name: "api", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{Project: preparer.DokployProject{Name: "proxy"}}}},
		{Name: "worker", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{Project: preparer.DokployProject{Name: "app"}}}},
	}}}
	collisions := cleanupStaleProjectNameCollisions(run, []string{"coolify-proxy", "proxy", "source"})
	if len(collisions) != 1 || collisions[0] != "proxy" {
		t.Fatalf("unexpected collisions: %#v", collisions)
	}
}
