package validator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	writeFile(t, filepath.Join(appDir, "migration-report.md"), "# report\n")

	result, err := Validate(context.Background(), Options{BundleDir: dir, DockerPath: "definitely-not-docker"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusRed {
		t.Fatalf("expected red status, got %#v", result)
	}
	assertIssue(t, result.Apps[0], "env.sensitive_value_present")
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

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
