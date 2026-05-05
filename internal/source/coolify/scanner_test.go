package coolify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/source"
)

func TestScanApplicationsFromCoolifyAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("missing bearer token")
		}

		switch r.URL.Path {
		case "/api/v1/applications":
			writeJSON(t, w, []map[string]any{{"uuid": "app-1", "name": "ghost"}})
		case "/api/v1/applications/app-1":
			writeJSON(t, w, map[string]any{
				"uuid":                         "app-1",
				"name":                         "ghost",
				"build_pack":                   "dockercompose",
				"fqdn":                         "https://blog.example.com,https://www.blog.example.com",
				"git_repository":               "https://github.com/example/ghost-stack",
				"git_branch":                   "main",
				"source_id":                    42,
				"source_type":                  "App\\Models\\GithubApp",
				"repository_project_id":        987,
				"manual_webhook_secret_github": "secret-not-exported",
				"docker_compose":               "services:\n  ghost:\n    image: ghost:5\n    environment:\n      DATABASE_PASSWORD: secret\n",
				"docker_compose_raw":           "services:\n  ghost:\n    image: ghost:5\n",
				"docker_compose_domains":       `{"ghost":{"domain":"https://blog.example.com:2368"}}`,
				"docker_compose_location":      "/docker-compose.yml",
			})
		case "/api/v1/applications/app-1/envs":
			writeJSON(t, w, []map[string]any{{"key": "DATABASE_PASSWORD", "real_value": "secret", "is_shown_once": true}})
		case "/api/v1/applications/app-1/storages":
			writeJSON(t, w, []map[string]any{{"uuid": "storage-1", "name": "ghost_content", "mount_path": "/var/lib/ghost/content"}})
		case "/api/v1/services", "/api/v1/databases":
			writeJSON(t, w, []map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	scanner, err := NewScanner(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	scanner.Now = func() time.Time { return time.Unix(0, 0) }

	manifest, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(manifest.Apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(manifest.Apps))
	}
	app := manifest.Apps[0]
	if app.Name != "ghost" || app.BuildPack != "dockercompose" {
		t.Fatalf("unexpected app: %#v", app)
	}
	if app.Git == nil || app.Git.Repository != "https://github.com/example/ghost-stack" {
		t.Fatalf("expected git source, got %#v", app.Git)
	}
	if app.Git.Provider != "github" || app.Git.SourceID != "42" || app.Git.SourceType != "App\\Models\\GithubApp" || app.Git.RepositoryID != "987" {
		t.Fatalf("expected sanitized git source metadata, got %#v", app.Git)
	}
	if _, ok := app.Metadata["manual_webhook_secret_github"]; ok {
		t.Fatalf("did not expect webhook secret in metadata: %#v", app.Metadata)
	}
	if app.Compose == nil || app.Compose.Raw == "" {
		t.Fatalf("expected compose source, got %#v", app.Compose)
	}
	if app.Compose.Resolved != "" {
		t.Fatalf("expected resolved compose to be omitted by default, got %#v", app.Compose)
	}
	if len(app.Routes) != 3 {
		t.Fatalf("expected fqdn and compose-domain routes, got %#v", app.Routes)
	}
	if len(app.Environment) != 1 || app.Environment[0].ValueKnown {
		t.Fatalf("expected redacted env, got %#v", app.Environment)
	}
	if len(app.Storages) != 1 || app.Storages[0].Name != "ghost_content" {
		t.Fatalf("expected storage, got %#v", app.Storages)
	}
	if len(manifest.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", manifest.Warnings)
	}
}

func TestScanRedactsRepositoryCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			writeJSON(t, w, []map[string]any{{"uuid": "app-1", "name": "api"}})
		case "/api/v1/applications/app-1":
			writeJSON(t, w, map[string]any{
				"uuid":           "app-1",
				"name":           "api",
				"git_repository": "https://user:token@github.com/example/private-app.git",
			})
		case "/api/v1/applications/app-1/envs", "/api/v1/applications/app-1/storages", "/api/v1/services", "/api/v1/databases":
			writeJSON(t, w, []map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	scanner, err := NewScanner(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Apps) != 1 || manifest.Apps[0].Git == nil {
		t.Fatalf("expected scanned git source, got %#v", manifest.Apps)
	}
	if got := manifest.Apps[0].Git.Repository; got != "https://github.com/example/private-app.git" {
		t.Fatalf("expected credential-redacted repository, got %q", got)
	}
}

func TestEnvVarsIncludeValuesWhenRequested(t *testing.T) {
	vars := envVars([]map[string]any{{"key": "API_KEY", "real_value": "abc"}}, true)
	if len(vars) != 1 {
		t.Fatalf("expected 1 var, got %d", len(vars))
	}
	if !vars[0].ValueKnown || vars[0].Value != "abc" || !vars[0].Sensitive {
		t.Fatalf("unexpected var: %#v", vars[0])
	}
}

func TestComposeSourceIncludesResolvedOnlyWhenRequested(t *testing.T) {
	resource := map[string]any{
		"docker_compose":     "services:\n  api:\n    environment:\n      API_TOKEN: secret\n",
		"docker_compose_raw": "services:\n  api:\n    image: example/api\n",
	}

	redacted := composeSource(resource, false)
	if redacted == nil || redacted.Raw == "" || redacted.Resolved != "" {
		t.Fatalf("expected only raw compose by default, got %#v", redacted)
	}

	included := composeSource(resource, true)
	if included == nil || included.Resolved == "" {
		t.Fatalf("expected resolved compose when requested, got %#v", included)
	}
}

func TestGetListFollowsPagination(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications" {
			http.NotFound(w, r)
			return
		}

		switch r.URL.Query().Get("page") {
		case "", "1":
			writeJSON(t, w, map[string]any{
				"data":  []map[string]any{{"uuid": "app-1"}},
				"links": map[string]any{"next": server.URL + "/api/v1/applications?page=2"},
			})
		case "2":
			writeJSON(t, w, map[string]any{
				"data":  []map[string]any{{"uuid": "app-2"}},
				"links": map[string]any{"next": nil},
			})
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	scanner, err := NewScanner(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	items, err := scanner.getList(context.Background(), "/api/v1/applications")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || getString(items[0], "uuid") != "app-1" || getString(items[1], "uuid") != "app-2" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
