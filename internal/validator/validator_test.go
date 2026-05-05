package validator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
)

func TestValidateWarnsForPortableIssues(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{
					{
						Name:  "api-web",
						Image: "example/api:latest",
						Environment: []manifest.EnvVar{
							{Name: "DATABASE_PASSWORD", Sensitive: true},
						},
						Mounts: []manifest.Mount{{Type: "bind", Source: "/var/lib/api", Target: "/data"}},
					},
				},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusYellow {
		t.Fatalf("expected yellow status, got %#v", result)
	}
	assertIssue(t, result.Apps[0], "env.sensitive_blank")
	assertIssue(t, result.Apps[0], "compose.absolute_bind_mount")
	assertIssue(t, result.Apps[0], "routes.none")
	assertIssue(t, result.Apps[0], "compose.docker_unavailable")
}

func TestValidateErrorsOnSensitiveValue(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "api")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.json"), `{"outputDir":"`+dir+`","source":"test","apps":[{"name":"api","directory":"api"}]}`)
	writeFile(t, filepath.Join(appDir, "compose.yaml"), "services:\n  api:\n    image: example/api\n")
	writeFile(t, filepath.Join(appDir, ".env.example"), "API_TOKEN=secret\n")
	writeFile(t, filepath.Join(appDir, "routes.json"), `[{"host":"api.example.com","serviceName":"api"}]`)
	writeFile(t, filepath.Join(appDir, "storages.json"), `[]`)
	writeMinimalTopology(t, appDir, []manifest.Route{{Host: "api.example.com", ServiceName: "api"}})
	writeFile(t, filepath.Join(appDir, "migration-report.md"), "# report\n")
	writeRunbook(t, appDir)

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusRed {
		t.Fatalf("expected red status, got %#v", result)
	}
	assertIssue(t, result.Apps[0], "env.sensitive_value_present")
}

func TestValidateAllowsPrivateSensitiveValueFiles(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "api")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.json"), `{"outputDir":"`+dir+`","source":"test","apps":[{"name":"api","directory":"api","privateEnvValues":true}]}`)
	writeFile(t, filepath.Join(appDir, "compose.yaml"), "services:\n  api:\n    image: example/api\n    env_file:\n      - .env\n")
	writeFile(t, filepath.Join(appDir, ".env.example"), "API_TOKEN=\n")
	writeFile(t, filepath.Join(appDir, ".env"), "API_TOKEN=secret\n")
	writeFile(t, filepath.Join(appDir, "routes.json"), `[{"host":"api.example.com","serviceName":"api"}]`)
	writeFile(t, filepath.Join(appDir, "storages.json"), `[]`)
	writeMinimalTopology(t, appDir, []manifest.Route{{Host: "api.example.com", ServiceName: "api"}})
	writeFile(t, filepath.Join(appDir, "migration-report.md"), "# report\n")
	writeRunbook(t, appDir)

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	assertIssue(t, result.Apps[0], "env.private_value_present")
	assertNoIssue(t, result.Apps[0], "env.sensitive_value_present")
	assertNoIssue(t, result.Apps[0], "env.sensitive_blank")
}

func TestValidateParsesLongComposeSyntax(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "api")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.json"), `{"outputDir":"`+dir+`","source":"test","apps":[{"name":"api","directory":"api"}]}`)
	writeFile(t, filepath.Join(appDir, "compose.yaml"), `services:
  api:
    image: example/api
    container_name: api
    ports:
      - target: 3000
        published: 8080
    volumes:
      - type: bind
        source: /var/lib/api
        target: /data
      - type: volume
        source: db-data
        target: /var/lib/db
`)
	writeFile(t, filepath.Join(appDir, ".env.example"), "")
	writeFile(t, filepath.Join(appDir, "routes.json"), `[{"host":"api.example.com","serviceName":"api"}]`)
	writeFile(t, filepath.Join(appDir, "storages.json"), `[]`)
	writeMinimalTopology(t, appDir, []manifest.Route{{Host: "api.example.com", ServiceName: "api"}})
	writeFile(t, filepath.Join(appDir, "migration-report.md"), "# report\n")
	writeRunbook(t, appDir)

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusYellow {
		t.Fatalf("expected yellow status, got %#v", result)
	}
	assertIssue(t, result.Apps[0], "compose.container_name")
	assertIssue(t, result.Apps[0], "compose.host_port")
	assertIssue(t, result.Apps[0], "compose.absolute_bind_mount")
	assertIssue(t, result.Apps[0], "compose.undeclared_named_volume")
}

func TestValidateRequiresTopologyArtifacts(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "api")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.json"), `{"outputDir":"`+dir+`","source":"test","apps":[{"name":"api","directory":"api"}]}`)
	writeFile(t, filepath.Join(appDir, "compose.yaml"), "services:\n  api:\n    image: example/api\n")
	writeFile(t, filepath.Join(appDir, ".env.example"), "")
	writeFile(t, filepath.Join(appDir, "routes.json"), `[{"host":"api.example.com","serviceName":"api"}]`)
	writeFile(t, filepath.Join(appDir, "storages.json"), `[]`)
	writeFile(t, filepath.Join(appDir, "migration-report.md"), "# report\n")

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusRed {
		t.Fatalf("expected red status, got %#v", result)
	}
	assertIssue(t, result.Apps[0], "bundle.missing_file")
}

func TestValidateFlagsTopologyIssues(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "web",
				Metadata: map[string]string{"migrationRole": "candidate", "coolify.project": "demo-project"},
				Services: []manifest.Service{
					{
						Name:        "web",
						Image:       "example/web:latest",
						Environment: []manifest.EnvVar{{Name: "DATABASE_URL"}},
						Mounts:      []manifest.Mount{{Type: "bind", Source: "/srv/web/uploads", Target: "/uploads"}},
					},
				},
				Routes: []manifest.Route{{Host: "web.example.com", ServiceName: "web", Port: "3000"}},
			},
			{
				Name:     "postgres primary",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support", "coolify.project": "demo-project"},
				Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
			},
			{
				Name:     "postgres replica",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support", "coolify.project": "demo-project"},
				Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Validate(context.Background(), Options{BundleDir: dir, AppName: "web", DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusYellow {
		t.Fatalf("expected yellow status, got %#v", result)
	}
	assertIssue(t, result.Apps[0], "topology.external_requirements")
	assertIssue(t, result.Apps[0], "topology.linked_resource_candidates")
	assertIssue(t, result.Apps[0], "topology.bind_mounts")
	assertIssue(t, result.Apps[0], "topology.env_values_redacted")
	assertNoIssue(t, result.Apps[0], "routes.none")
}

func TestValidateSkipsRouteGapForSupportTopology(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "postgres support",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support"},
				Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	assertNoIssue(t, result.Apps[0], "routes.none")
}

func assertIssue(t *testing.T, app AppResult, code string) {
	t.Helper()
	for _, issue := range app.Issues {
		if issue.Code == code {
			return
		}
	}
	t.Fatalf("expected issue %s in %#v", code, app.Issues)
}

func assertNoIssue(t *testing.T, app AppResult, code string) {
	t.Helper()
	for _, issue := range app.Issues {
		if issue.Code == code {
			t.Fatalf("did not expect issue %s in %#v", code, app.Issues)
		}
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMinimalTopology(t *testing.T, appDir string, routes []manifest.Route) {
	t.Helper()
	writeJSONFile(t, filepath.Join(appDir, "topology.json"), analyzer.Topology{Routes: routes})
}

func writeRunbook(t *testing.T, appDir string) {
	t.Helper()
	writeFile(t, filepath.Join(appDir, "migration-runbook.md"), "# migration runbook\n")
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(contents)+"\n")
}
