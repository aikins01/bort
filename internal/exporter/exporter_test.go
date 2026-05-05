package exporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/analyzer"
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

	var topology analyzer.Topology
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(appDir, "topology.json"))), &topology); err != nil {
		t.Fatal(err)
	}
	if len(topology.Routes) != 1 || topology.Routes[0].Host != "blog.example.com" {
		t.Fatalf("unexpected topology routes: %#v", topology.Routes)
	}
	if len(topology.StatefulVolumes) != 1 || topology.StatefulVolumes[0].Name != "ghost_content" {
		t.Fatalf("unexpected topology stateful volumes: %#v", topology.StatefulVolumes)
	}
	if len(topology.RiskReasons) == 0 {
		t.Fatalf("expected topology risk reasons, got %#v", topology)
	}
	runbook := readFile(t, filepath.Join(appDir, "migration-runbook.md"))
	if !strings.Contains(runbook, "# migration runbook") || !strings.Contains(runbook, "## source control") || !strings.Contains(runbook, "does not require those credentials") || !strings.Contains(runbook, "## cutover checklist") {
		t.Fatalf("expected migration runbook, got:\n%s", runbook)
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

func TestExportStripsCoolifyOnlyEnvFromGeneratedCompose(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Environment: []manifest.EnvVar{
				{Name: "COOLIFY_FQDN", Value: "api.example.com", ValueKnown: true},
				{Name: "SOURCE_COMMIT", Value: "abc123", ValueKnown: true},
				{Name: "APP_ENV", Value: "production", ValueKnown: true},
			},
			Services: []manifest.Service{{
				Name:  "web",
				Image: "example/web",
				Environment: []manifest.EnvVar{
					{Name: "COOLIFY_URL", Value: "https://api.example.com", ValueKnown: true},
					{Name: "SERVICE_FQDN_WEB", Value: "api.example.com", ValueKnown: true},
					{Name: "PORT", Value: "3000", ValueKnown: true},
				},
			}},
		}},
	}

	if _, err := Export(m, Options{OutputDir: dir, IncludeEnvValues: true}); err != nil {
		t.Fatal(err)
	}

	appDir := filepath.Join(dir, "api")
	compose := readFile(t, filepath.Join(appDir, "compose.yaml"))
	if strings.Contains(compose, "COOLIFY_") || strings.Contains(compose, "SOURCE_COMMIT") {
		t.Fatalf("expected compose env_file refs to omit coolify-only env, got:\n%s", compose)
	}
	for _, file := range []string{".env", ".env.example", ".env.web", ".env.web.example"} {
		env := readFile(t, filepath.Join(appDir, file))
		if strings.Contains(env, "COOLIFY_") || strings.Contains(env, "SOURCE_COMMIT") {
			t.Fatalf("expected %s to omit coolify-only env, got %q", file, env)
		}
	}
	if env := readFile(t, filepath.Join(appDir, ".env")); !strings.Contains(env, "APP_ENV=production\n") {
		t.Fatalf("expected app env retained, got %q", env)
	}
	if env := readFile(t, filepath.Join(appDir, ".env.web")); !strings.Contains(env, "PORT=3000\n") {
		t.Fatalf("expected service env retained, got %q", env)
	}
	if env := readFile(t, filepath.Join(appDir, ".env.web")); !strings.Contains(env, "SERVICE_FQDN_WEB=api.example.com\n") {
		t.Fatalf("expected Coolify service magic env preserved for review, got %q", env)
	}
}

func TestExportSanitizesRawCoolifyCompose(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  api:
    image: example/api
    environment:
      COOLIFY_FQDN: api.example.com
      SOURCE_COMMIT: abc123
      SERVICE_URL_WEB: https://api.example.com
      APP_ENV: production
    labels:
      coolify.managed: "true"
      traefik.http.routers.api.rule: Host(` + "`" + `api.example.com` + "`" + `)
      com.example.keep: yes
  worker:
    image: example/worker
    environment:
      - COOLIFY_URL=https://api.example.com
      - QUEUE=default
    labels:
      - caddy_0=https://worker.example.com
      - com.example.keep=yes
`
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name:    "api",
			Compose: &manifest.ComposeSource{Raw: compose},
		}},
	}

	summary, err := Export(m, Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	out := readFile(t, filepath.Join(dir, "api", "compose.yaml"))
	for _, blocked := range []string{"COOLIFY_FQDN", "COOLIFY_URL", "SOURCE_COMMIT", "coolify.managed", "traefik.http.routers", "caddy_0"} {
		if strings.Contains(out, blocked) {
			t.Fatalf("expected raw compose sanitizer to remove %q, got:\n%s", blocked, out)
		}
	}
	for _, want := range []string{"APP_ENV: production", "SERVICE_URL_WEB: https://api.example.com", "QUEUE=default", "com.example.keep"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected sanitized compose to retain %q, got:\n%s", want, out)
		}
	}
	warnings := strings.Join(summary.Apps[0].Warnings, "\n")
	if len(summary.Apps) != 1 || !strings.Contains(warnings, "removed source platform") || !strings.Contains(warnings, "SERVICE_URL_WEB") {
		t.Fatalf("expected sanitizer warning, got %#v", summary.Apps)
	}
}

func TestExportIncludeEnvValuesWritesPrivateEnvFiles(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:        "api",
				Environment: []manifest.EnvVar{{Name: "APP_SECRET", Value: "app-secret", ValueKnown: true, Sensitive: true}},
				Services: []manifest.Service{{
					Name:  "web",
					Image: "example/web",
					Environment: []manifest.EnvVar{
						{Name: "API_TOKEN", Value: "service-secret", ValueKnown: true, Sensitive: true},
					},
				}},
			},
		},
	}

	if _, err := Export(m, Options{OutputDir: dir, IncludeEnvValues: true}); err != nil {
		t.Fatal(err)
	}

	appDir := filepath.Join(dir, "api")
	compose := readFile(t, filepath.Join(appDir, "compose.yaml"))
	for _, want := range []string{".env", ".env.web"} {
		if !strings.Contains(compose, want) {
			t.Fatalf("expected compose to reference private %s, got:\n%s", want, compose)
		}
	}
	for _, file := range []string{".env", ".env.web"} {
		env := readFile(t, filepath.Join(appDir, file))
		if !strings.Contains(env, "secret") {
			t.Fatalf("expected private %s to contain env value, got %q", file, env)
		}
	}
	for _, file := range []string{".env.example", ".env.web.example"} {
		env := readFile(t, filepath.Join(appDir, file))
		if strings.Contains(env, "secret") {
			t.Fatalf("did not expect example %s to expose env value, got %q", file, env)
		}
	}
}

func TestExportIncludeEnvValuesPreservesKnownEmptyValues(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name: "api",
			Services: []manifest.Service{{
				Name:  "web",
				Image: "example/web",
				Environment: []manifest.EnvVar{
					{Name: "KNOWN_EMPTY", ValueKnown: true},
					{Name: "UNKNOWN_EMPTY"},
				},
			}},
		}},
	}

	if _, err := Export(m, Options{OutputDir: dir, IncludeEnvValues: true}); err != nil {
		t.Fatal(err)
	}

	env := readFile(t, filepath.Join(dir, "api", ".env.web"))
	if !strings.Contains(env, "KNOWN_EMPTY=\"\"\n") {
		t.Fatalf("expected known empty value to be quoted, got %q", env)
	}
	if !strings.Contains(env, "UNKNOWN_EMPTY=\n") {
		t.Fatalf("expected unknown empty value to remain blank, got %q", env)
	}
}

func TestExportProjectGroupDoesNotUseBroadCoolifyProject(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name:     "demo-api",
			Metadata: map[string]string{"coolify.project": "demo-project", "coolify.environment": "production", "migrationRole": "candidate"},
			Services: []manifest.Service{{Name: "web", Image: "example/web"}},
		}},
	}

	summary, err := Export(m, Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Apps) != 1 || summary.Apps[0].ProjectGroup == nil {
		t.Fatalf("expected project group in summary, got %#v", summary.Apps)
	}
	group := summary.Apps[0].ProjectGroup
	if group.Name != "demo-api" || group.Environment != "production" || group.Source != "app" {
		t.Fatalf("unexpected project group: %#v", group)
	}

	var index Summary
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "index.json"))), &index); err != nil {
		t.Fatal(err)
	}
	if index.Apps[0].ProjectGroup == nil || index.Apps[0].ProjectGroup.Name == "demo-project" {
		t.Fatalf("expected project group in index.json, got %#v", index.Apps[0])
	}
}

func TestExportGroupsSupportResourceWithDetectedOwner(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Metadata: map[string]string{"migrationRole": "candidate"},
				Services: []manifest.Service{{
					Name:        "web",
					Image:       "example/api",
					Environment: []manifest.EnvVar{{Name: "DATABASE_URL", Value: "postgres://user:pass@api-db:5432/app", ValueKnown: true, Sensitive: true}},
				}},
			},
			{
				ID:       "compose:api-db",
				Name:     "api database",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support", "coolify.composeProject": "api-db"},
				Services: []manifest.Service{{
					Name:   "postgres",
					Image:  "postgres:16-alpine",
					Labels: map[string]string{"com.docker.compose.project": "api-db"},
				}},
			},
		},
	}

	summary, err := Export(m, Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Apps) != 2 {
		t.Fatalf("expected two apps, got %#v", summary.Apps)
	}
	for _, app := range summary.Apps {
		if app.Name != "api database" {
			continue
		}
		if app.ProjectGroup == nil || app.ProjectGroup.Name != "api" || app.ProjectGroup.Source != "detected-link" {
			t.Fatalf("expected support resource grouped with api, got %#v", app.ProjectGroup)
		}
		return
	}
	t.Fatalf("support app missing from summary: %#v", summary.Apps)
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

func TestExportRunbookIncludesLinkedResources(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Platform: "coolify",
				Metadata: map[string]string{"migrationRole": "candidate", "coolify.project": "demo-project"},
				Services: []manifest.Service{{
					Name:        "web",
					Image:       "example/api",
					Environment: []manifest.EnvVar{{Name: "DATABASE_URL"}},
					Networks:    []manifest.ServiceNetwork{{Name: "api-db-net"}},
				}},
			},
			{
				ID:       "compose:postgres",
				Name:     "postgres db",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support", "coolify.project": "demo-project"},
				Services: []manifest.Service{{
					Name:     "postgres",
					Image:    "postgres:16-alpine",
					Networks: []manifest.ServiceNetwork{{Name: "api-db-net"}},
				}},
			},
		},
	}

	if _, err := Export(m, Options{OutputDir: dir, AppName: "api"}); err != nil {
		t.Fatal(err)
	}

	appDir := filepath.Join(dir, "api")
	var topology analyzer.Topology
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(appDir, "topology.json"))), &topology); err != nil {
		t.Fatal(err)
	}
	if len(topology.LinkedResources) != 1 || topology.LinkedResources[0].App != "postgres db" {
		t.Fatalf("expected linked postgres resource, got %#v", topology.LinkedResources)
	}
	runbook := readFile(t, filepath.Join(appDir, "migration-runbook.md"))
	for _, want := range []string{"possible `database` resource `postgres db`", "postgres on postgres", "shared Docker network"} {
		if !strings.Contains(runbook, want) {
			t.Fatalf("expected runbook to contain %q, got:\n%s", want, runbook)
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
