package coolifylocal

import (
	"context"
	"os"
	"path/filepath"
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

func TestScanLoadsComposeAndProxyArtifactsFromDataDir(t *testing.T) {
	dataDir := t.TempDir()
	appDir := filepath.Join(dataDir, "applications", "abc123")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	composeBody := "services:\n  web:\n    image: example/web\n"
	if err := os.WriteFile(filepath.Join(appDir, "docker-compose.yaml"), []byte(composeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	proxyDir := filepath.Join(dataDir, "proxy", "dynamic")
	if err := os.MkdirAll(proxyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxyDir, "coolify.yaml"), []byte("http: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scanner := &Scanner{
		DataDir: dataDir,
		Docker: fakeScanner{manifest: manifest.Manifest{
			APIVersion: manifest.APIVersion,
			Apps: []manifest.App{{
				ID:   "compose:abc123",
				Name: "abc123",
				Labels: map[string]string{
					"com.docker.compose.project": "abc123",
					"coolify.type":               "application",
					"coolify.resourceName":       "marketmap",
				},
				Services: []manifest.Service{{Name: "web"}},
			}},
		}},
	}

	result, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if app.Compose == nil || app.Compose.Raw != composeBody {
		t.Fatalf("expected compose raw populated, got %#v", app.Compose)
	}
	if app.Metadata["coolify.composeFile"] == "" {
		t.Fatalf("expected coolify.composeFile metadata, got %#v", app.Metadata)
	}
	if len(result.ProxyArtifacts) != 1 || result.ProxyArtifacts[0].Source != "traefik-dynamic" || result.ProxyArtifacts[0].Content != "http: {}\n" {
		t.Fatalf("expected one traefik-dynamic proxy artifact, got %#v", result.ProxyArtifacts)
	}
}

func TestLoadComposeRawRejectsPathTraversalUUID(t *testing.T) {
	dataDir := t.TempDir()
	if raw, _ := loadComposeRaw(dataDir, "application", "../etc"); raw != "" {
		t.Fatalf("expected traversal uuid to be rejected, got raw len=%d", len(raw))
	}
	if raw, _ := loadComposeRaw(dataDir, "application", "/abs/path"); raw != "" {
		t.Fatalf("expected absolute uuid to be rejected, got raw len=%d", len(raw))
	}
}

func TestLoadComposeRawRejectsParentSymlink(t *testing.T) {
	if runtimeIsWindows() {
		t.Skip("symlink redirect test relies on unix semantics")
	}
	dataDir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "docker-compose.yaml")
	if err := os.WriteFile(secret, []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "applications"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "applications", "abc123")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := loadComposeRaw(dataDir, "application", "abc123"); raw != "" {
		t.Fatalf("expected parent-symlink read to be refused, got raw len=%d", len(raw))
	}
}

func TestLoadProxyArtifactsRejectsDirSymlink(t *testing.T) {
	if runtimeIsWindows() {
		t.Skip("symlink redirect test relies on unix semantics")
	}
	dataDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "leak.yaml"), []byte("leak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "proxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "proxy", "dynamic")); err != nil {
		t.Fatal(err)
	}
	if got := loadProxyArtifacts(dataDir); len(got) != 0 {
		t.Fatalf("expected dynamic-dir symlink to be refused, got %#v", got)
	}
}

func runtimeIsWindows() bool { return os.PathSeparator == '\\' }

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
