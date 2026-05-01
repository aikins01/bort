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
				"uuid":                    "app-1",
				"name":                    "ghost",
				"build_pack":              "dockercompose",
				"fqdn":                    "https://blog.example.com,https://www.blog.example.com",
				"git_repository":          "https://github.com/example/ghost-stack",
				"git_branch":              "main",
				"docker_compose_raw":      "services:\n  ghost:\n    image: ghost:5\n",
				"docker_compose_domains":  `{"ghost":{"domain":"https://blog.example.com:2368"}}`,
				"docker_compose_location": "/docker-compose.yml",
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
	if app.Compose == nil || app.Compose.Raw == "" {
		t.Fatalf("expected compose source, got %#v", app.Compose)
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

func TestEnvVarsIncludeValuesWhenRequested(t *testing.T) {
	vars := envVars([]map[string]any{{"key": "API_KEY", "real_value": "abc"}}, true)
	if len(vars) != 1 {
		t.Fatalf("expected 1 var, got %d", len(vars))
	}
	if !vars[0].ValueKnown || vars[0].Value != "abc" || !vars[0].Sensitive {
		t.Fatalf("unexpected var: %#v", vars[0])
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
