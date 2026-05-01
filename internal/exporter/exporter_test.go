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
				Compose:   &manifest.ComposeSource{Resolved: "services:\n  ghost:\n    image: ghost:5\n"},
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
	compose := readFile(t, filepath.Join(appDir, "compose.yaml"))
	if !strings.Contains(compose, "image: ghost:5") {
		t.Fatalf("expected compose to be written, got %q", compose)
	}

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
