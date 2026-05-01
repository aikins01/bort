package exporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/manifest"
)

func TestExportWritesBundleForComposeApp(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify"},
		Apps: []manifest.App{
			{
				ID:        "coolify:application:app-1",
				Name:      "Ghost Blog",
				Platform:  "coolify",
				Runtime:   "application",
				BuildPack: "dockercompose",
				Git:       &manifest.GitSource{Repository: "https://github.com/example/ghost", Branch: "main"},
				Compose:   &manifest.ComposeSource{Raw: "services:\n  ghost:\n    image: ghost:5\n"},
				Environment: []manifest.EnvVar{
					{Name: "PUBLIC_URL", Value: "https://blog.example.com", ValueKnown: true},
					{Name: "DATABASE_PASSWORD", Value: "secret", ValueKnown: true, Sensitive: true},
				},
				Storages: []manifest.Storage{{Name: "ghost_content", Type: "volume", Target: "/var/lib/ghost/content"}},
				Routes:   []manifest.Route{{Host: "blog.example.com", ServiceName: "ghost", Port: "2368"}},
			},
		},
	}

	summary, err := Export(m, Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Apps) != 1 {
		t.Fatalf("expected one app summary, got %#v", summary.Apps)
	}

	appDir := filepath.Join(dir, "ghost-blog")
	assertMode(t, appDir, privateDirMode)
	compose := readFile(t, filepath.Join(appDir, "compose.yaml"))
	if !strings.Contains(compose, "image: ghost:5") {
		t.Fatalf("expected compose to be written, got %q", compose)
	}
	assertMode(t, filepath.Join(appDir, "compose.yaml"), privateFileMode)
	assertMode(t, filepath.Join(dir, "index.json"), privateFileMode)

	env := readFile(t, filepath.Join(appDir, ".env.example"))
	if !strings.Contains(env, "PUBLIC_URL=https://blog.example.com") {
		t.Fatalf("expected public env value, got %q", env)
	}
	if !strings.Contains(env, "DATABASE_PASSWORD=\n") || strings.Contains(env, "secret") {
		t.Fatalf("expected sensitive env to be blank, got %q", env)
	}

	var routes []manifest.Route
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(appDir, "routes.json"))), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Host != "blog.example.com" {
		t.Fatalf("unexpected routes: %#v", routes)
	}
}

func TestExportSkipsResolvedCompose(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify"},
		Apps: []manifest.App{
			{
				Name:    "api",
				Compose: &manifest.ComposeSource{Resolved: "services:\n  api:\n    image: example/api\n    environment:\n      API_TOKEN: secret\n"},
				Services: []manifest.Service{
					{Name: "api", Image: "example/api"},
				},
			},
		},
	}

	summary, err := Export(m, Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	compose := readFile(t, filepath.Join(dir, "api", "compose.yaml"))
	if strings.Contains(compose, "secret") || strings.Contains(compose, "API_TOKEN") {
		t.Fatalf("expected resolved compose to be skipped, got:\n%s", compose)
	}
	if !strings.Contains(compose, "image: \"example/api\"") {
		t.Fatalf("expected generated compose, got:\n%s", compose)
	}
	if len(summary.Apps) != 1 || !strings.Contains(strings.Join(summary.Apps[0].Warnings, "\n"), "skipped resolved compose") {
		t.Fatalf("expected resolved compose warning, got %#v", summary.Apps)
	}
}

func TestExportScopesServiceEnvFiles(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "stack",
				Services: []manifest.Service{
					{Name: "web", Image: "example/web", Environment: []manifest.EnvVar{{Name: "PORT", Value: "3000", ValueKnown: true}}},
					{Name: "worker", Image: "example/worker", Environment: []manifest.EnvVar{{Name: "PORT", Value: "4000", ValueKnown: true}}},
				},
			},
		},
	}

	if _, err := Export(m, Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	appDir := filepath.Join(dir, "stack")
	compose := readFile(t, filepath.Join(appDir, "compose.yaml"))
	for _, want := range []string{".env.web.example", ".env.worker.example"} {
		if !strings.Contains(compose, want) {
			t.Fatalf("expected compose to reference %s, got:\n%s", want, compose)
		}
	}
	if env := readFile(t, filepath.Join(appDir, ".env.web.example")); env != "PORT=3000\n" {
		t.Fatalf("unexpected web env file: %q", env)
	}
	if env := readFile(t, filepath.Join(appDir, ".env.worker.example")); env != "PORT=4000\n" {
		t.Fatalf("unexpected worker env file: %q", env)
	}
}

func TestExportGeneratesComposeFromServices(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Platform: "docker",
				Services: []manifest.Service{
					{
						Name:   "api-web",
						Image:  "example/api:latest",
						Ports:  []manifest.Port{{ContainerPort: "3000/tcp"}},
						Mounts: []manifest.Mount{{Type: "volume", Name: "api_data", Target: "/data"}},
					},
				},
			},
		},
	}

	if _, err := Export(m, Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	compose := readFile(t, filepath.Join(dir, "api", "compose.yaml"))
	for _, want := range []string{"services:", "api-web:", "image: \"example/api:latest\"", "expose:", "api_data:/data", "volumes:", "\"api_data\": {}"} {
		if !strings.Contains(compose, want) {
			t.Fatalf("expected compose to contain %q, got:\n%s", want, compose)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("expected %s mode %o, got %o", path, want, got)
	}
}
