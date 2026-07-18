package coolify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestNewScannerURLPolicy(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr string
		forbid  string
	}{
		{name: "https remote accepted", baseURL: "https://coolify.example.com"},
		{name: "http localhost accepted", baseURL: "http://localhost:8000"},
		{name: "http loopback IPv4 accepted", baseURL: "http://127.0.0.1:8000"},
		{name: "http loopback IPv6 accepted", baseURL: "http://[::1]:8000"},
		{name: "http remote rejected", baseURL: "http://coolify.example.com", wantErr: "non-loopback http"},
		{name: "http remote IP rejected", baseURL: "http://203.0.113.10", wantErr: "non-loopback http"},
		{name: "unsupported scheme rejected", baseURL: "ftp://coolify.example.com", wantErr: "must use http or https"},
		{name: "userinfo rejected", baseURL: "https://user:sup3rsecret@coolify.example.com", wantErr: "must not embed credentials", forbid: "sup3rsecret"},
		{name: "malformed userinfo not echoed", baseURL: "https://user:sup3rsecret@coolify exam ple.com", wantErr: "invalid Coolify URL", forbid: "sup3rsecret"},
		{name: "scheme error does not echo credentials", baseURL: "ftp://user:sup3rsecret@coolify.example.com", wantErr: "must not embed credentials", forbid: "sup3rsecret"},
		{name: "empty host userinfo not echoed", baseURL: "http://user:sup3rsecret@", wantErr: "must not embed credentials", forbid: "sup3rsecret"},
		{name: "missing host rejected", baseURL: "http://", wantErr: "must include a host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewScanner(tt.baseURL, "token")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			if tt.forbid != "" && strings.Contains(err.Error(), tt.forbid) {
				t.Fatalf("error leaked embedded credential %q: %v", tt.forbid, err)
			}
		})
	}
}

func TestEndpointPaginationURLPolicy(t *testing.T) {
	scanner := &Scanner{BaseURL: "https://coolify.example.com", Token: "token"}

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "same origin absolute accepted", path: "https://coolify.example.com/api/v1/applications?page=2"},
		{name: "explicit default https port accepted", path: "https://coolify.example.com:443/api/v1/applications?page=2"},
		{name: "relative path accepted", path: "/api/v1/applications?page=2"},
		{name: "query relative accepted", path: "?page=2"},
		{name: "scheme downgrade rejected", path: "http://coolify.example.com/api/v1/applications?page=2", wantErr: "refusing Coolify pagination URL"},
		{name: "cross host rejected", path: "https://evil.example.com/api/v1/applications?page=2", wantErr: "refusing Coolify pagination URL"},
		{name: "non default port rejected", path: "https://coolify.example.com:8443/api/v1/applications?page=2", wantErr: "refusing Coolify pagination URL"},
		{name: "userinfo absolute rejected", path: "https://user@coolify.example.com/api/v1/applications?page=2", wantErr: "must not embed credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scanner.endpoint(tt.path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				if !strings.HasPrefix(got, "https://coolify.example.com") {
					t.Fatalf("unexpected endpoint %q", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestTokenHTTPClientRedirectPolicy(t *testing.T) {
	newVia := func(raw string) []*http.Request {
		return []*http.Request{httptest.NewRequest(http.MethodGet, raw, nil)}
	}

	t.Run("https to http downgrade rejected", func(t *testing.T) {
		client := tokenHTTPClient("https://coolify.example.com")
		req := httptest.NewRequest(http.MethodGet, "http://coolify.example.com/api/v1/applications", nil)
		if err := client.CheckRedirect(req, newVia("https://coolify.example.com/api/v1/applications")); err == nil || !strings.Contains(err.Error(), "refusing Coolify redirect") {
			t.Fatalf("expected redirect refusal, got %v", err)
		}
	})

	t.Run("cross origin subdomain rejected", func(t *testing.T) {
		client := tokenHTTPClient("https://coolify.example.com")
		req := httptest.NewRequest(http.MethodGet, "https://api.coolify.example.com/api/v1/applications", nil)
		if err := client.CheckRedirect(req, newVia("https://coolify.example.com/api/v1/applications")); err == nil || !strings.Contains(err.Error(), "different origin") {
			t.Fatalf("expected redirect refusal, got %v", err)
		}
	})

	t.Run("same origin with default port allowed", func(t *testing.T) {
		client := tokenHTTPClient("https://coolify.example.com")
		req := httptest.NewRequest(http.MethodGet, "https://coolify.example.com:443/api/v1/applications?page=2", nil)
		if err := client.CheckRedirect(req, newVia("https://coolify.example.com/api/v1/applications")); err != nil {
			t.Fatalf("expected redirect to be allowed, got %v", err)
		}
	})

	t.Run("loopback http stays allowed", func(t *testing.T) {
		client := tokenHTTPClient("http://localhost:3000")
		req := httptest.NewRequest(http.MethodGet, "http://localhost:3000/api/v1/applications?page=2", nil)
		if err := client.CheckRedirect(req, newVia("http://localhost:3000/api/v1/applications")); err != nil {
			t.Fatalf("expected redirect to be allowed, got %v", err)
		}
	})

	t.Run("ipv6 port-colliding origin rejected", func(t *testing.T) {
		client := tokenHTTPClient("https://[2001:db8::1]:8443")
		req := httptest.NewRequest(http.MethodGet, "https://[2001:db8::1:8443]/api/v1/applications", nil)
		if err := client.CheckRedirect(req, newVia("https://[2001:db8::1]:8443/api/v1/applications")); err == nil || !strings.Contains(err.Error(), "different origin") {
			t.Fatalf("expected redirect refusal, got %v", err)
		}
	})
}

func TestEndpointPaginationIPv6PortCollision(t *testing.T) {
	scanner := &Scanner{BaseURL: "https://[2001:db8::1]:8443", Token: "token"}

	if _, err := scanner.endpoint("https://[2001:db8::1:8443]/api/v1/applications?page=2"); err == nil || !strings.Contains(err.Error(), "refusing Coolify pagination URL") {
		t.Fatalf("expected colliding IPv6 origin to be refused, got %v", err)
	}
	if _, err := scanner.endpoint("https://[2001:db8::1]:8443/api/v1/applications?page=2"); err != nil {
		t.Fatalf("expected same IPv6 origin to be accepted, got %v", err)
	}

	defaultPortScanner := &Scanner{BaseURL: "https://[2001:db8::1]", Token: "token"}
	if _, err := defaultPortScanner.endpoint("https://[2001:db8::1]:443/api/v1/applications?page=2"); err != nil {
		t.Fatalf("expected explicit default port to match bare IPv6 host, got %v", err)
	}
}
