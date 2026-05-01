package coolifylocal

import (
	"context"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
)

func TestScanMarksSourceAsCoolifyLocal(t *testing.T) {
	scanner := &Scanner{Docker: fakeScanner{}}

	result, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.Platform != "coolify-local" {
		t.Fatalf("expected coolify-local source, got %#v", result.Source)
	}
	if len(result.Apps) != 1 || result.Apps[0].Name != "api" {
		t.Fatalf("expected docker scan apps to pass through, got %#v", result.Apps)
	}
}

func TestScanEnrichesAppsFromCoolifyLabels(t *testing.T) {
	scanner := &Scanner{Docker: fakeScanner{manifest: manifest.Manifest{
		APIVersion:  manifest.APIVersion,
		Kind:        "MigrationManifest",
		GeneratedAt: time.Unix(0, 0),
		Source:      manifest.Source{Platform: "docker", Hostname: "coolify-host"},
		Apps: []manifest.App{
			{
				ID:       "compose:abc123",
				Name:     "abc123",
				Platform: "coolify",
				Runtime:  "docker",
				Labels: map[string]string{
					"com.docker.compose.project":              "abc123",
					"com.docker.compose.project.config_files": "/artifacts/example/docker-compose.yml",
					"com.docker.compose.project.working_dir":  "/artifacts/example",
					"coolify.environmentName":                 "production",
					"coolify.managed":                         "true",
					"coolify.projectName":                     "vela",
					"coolify.resourceName":                    "marketmap",
					"coolify.serviceName":                     "marketmap",
					"coolify.type":                            "application",
				},
				Services: []manifest.Service{{Name: "web", Image: "example/web"}},
			},
		},
		Volumes: []manifest.Volume{{Name: "data", UsedBy: []string{"abc123"}}},
	}}}

	result, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if app.Name != "marketmap" {
		t.Fatalf("expected friendly app name, got %q", app.Name)
	}
	if app.Runtime != "application" {
		t.Fatalf("expected application runtime, got %q", app.Runtime)
	}
	if app.Metadata["migrationRole"] != "candidate" || app.Metadata["coolify.project"] != "vela" || app.Metadata["coolify.uuid"] != "abc123" {
		t.Fatalf("unexpected metadata: %#v", app.Metadata)
	}
	if len(result.Volumes) != 1 || len(result.Volumes[0].UsedBy) != 1 || result.Volumes[0].UsedBy[0] != "marketmap" {
		t.Fatalf("expected volume users to be renamed, got %#v", result.Volumes)
	}
}

type fakeScanner struct {
	manifest manifest.Manifest
}

func (s fakeScanner) Scan(context.Context, source.ScanOptions) (manifest.Manifest, error) {
	if s.manifest.Apps != nil {
		return s.manifest, nil
	}
	return manifest.Manifest{
		APIVersion:  manifest.APIVersion,
		Kind:        "MigrationManifest",
		GeneratedAt: time.Unix(0, 0),
		Source:      manifest.Source{Platform: "docker", Hostname: "coolify-host"},
		Apps:        []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api"}}}},
	}, nil
}
