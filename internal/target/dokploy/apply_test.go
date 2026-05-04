package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aikins01/bort/internal/preparer"
)

func TestNewClientFromEnvRequiresURLAndToken(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvToken, "")
	if _, err := NewClientFromEnv(); err == nil {
		t.Fatalf("expected error when URL is missing")
	}
	t.Setenv(EnvBaseURL, "https://dokploy.example")
	if _, err := NewClientFromEnv(); err == nil {
		t.Fatalf("expected error when token is missing")
	}
	t.Setenv(EnvToken, "secret")
	client, err := NewClientFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.BaseURL != "https://dokploy.example" || client.Token != "secret" {
		t.Fatalf("unexpected client: %+v", client)
	}
}

func TestApplyDumpDataStoreNoopsForVolumeStrategyKinds(t *testing.T) {
	// mysql has no logical-dump implementation yet, so it migrates via
	// stopped-volume copy. the dump step in the plan must be a noop, not
	// ErrNotImplemented, because the volume sync path covers migration.
	client := &Client{BaseURL: "https://dokploy.example", Token: "secret", HTTPClient: http.DefaultClient}
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Kind: "mysql", Service: "db", Strategy: "migrate"}}
	plan := Plan{
		Steps:   []Step{{Kind: StepDumpDataStore, App: "api", Ref: "data-store:db"}},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("expected noop dump for non-logical store, got %v", err)
	}
}

func TestCreateProjectIsIdempotent(t *testing.T) {
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			http.Error(w, `{"code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}
		calls[r.Method+" "+r.URL.Path]++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]Project{{ProjectID: "p1", Name: "api"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/project.create":
			t.Fatalf("project.create should not be called when project exists")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	project, err := client.CreateProject(context.Background(), "api", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project.ProjectID != "p1" {
		t.Fatalf("expected existing project, got %#v", project)
	}
	if calls["POST /api/project.create"] != 0 {
		t.Fatalf("expected idempotent skip, got %#v", calls)
	}
}

func TestCreateProjectCreatesWhenMissing(t *testing.T) {
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			if created {
				_ = json.NewEncoder(w).Encode([]Project{{ProjectID: "p2", Name: "api"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]Project{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/project.create":
			created = true
			_ = json.NewEncoder(w).Encode(Project{ProjectID: "p2", Name: "api"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	project, err := client.CreateProject(context.Background(), "api", "managed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project.ProjectID != "p2" {
		t.Fatalf("expected new project p2, got %#v", project)
	}
}

func TestPingPropagatesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"UNAUTHORIZED","message":"bad token"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "wrong", HTTPClient: server.Client()}
	err := client.Ping(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || apiErr.Code != "UNAUTHORIZED" {
		t.Fatalf("expected APIError 401 UNAUTHORIZED, got %v", err)
	}
}

func TestCreateComposeIsIdempotent(t *testing.T) {
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			http.Error(w, `{"code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}
		calls[r.Method+" "+r.URL.Path]++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			if r.URL.Query().Get("name") != "api" || r.URL.Query().Get("environmentId") != "env1" {
				t.Fatalf("unexpected search query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "c1", Name: "api"}}, Total: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.create":
			t.Fatalf("compose.create should not be called when compose exists")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	compose, err := client.CreateCompose(context.Background(), CreateComposeRequest{Name: "api", EnvironmentID: "env1", ComposeFile: "services: {}"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if compose.ComposeID != "c1" {
		t.Fatalf("expected existing compose c1, got %#v", compose)
	}
	if calls["POST /api/compose.create"] != 0 {
		t.Fatalf("expected no compose.create calls, got %#v", calls)
	}
}

func TestCreateComposeCreatesWhenMissing(t *testing.T) {
	created := false
	var receivedBody CreateComposeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			if created {
				_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "c2", Name: "api"}}, Total: 1})
				return
			}
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{}, Total: 0})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.create":
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			created = true
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c2", Name: "api"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	compose, err := client.CreateCompose(context.Background(), CreateComposeRequest{Name: "api", EnvironmentID: "env1", ComposeFile: "services: {}"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if compose.ComposeID != "c2" {
		t.Fatalf("expected compose c2, got %#v", compose)
	}
	if receivedBody.ComposeType != "docker-compose" || receivedBody.SourceType != "raw" {
		t.Fatalf("expected raw docker-compose defaults, got %#v", receivedBody)
	}
}

func TestCreateDomainIsIdempotent(t *testing.T) {
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			http.Error(w, `{"code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}
		calls[r.Method+" "+r.URL.Path]++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
			_ = json.NewEncoder(w).Encode([]Domain{{DomainID: "d1", Host: "api.example.com", ComposeID: "c1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.create":
			t.Fatalf("domain.create should not be called when host exists")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	domain, err := client.CreateDomain(context.Background(), CreateDomainRequest{Host: "api.example.com", ComposeID: "c1", ServiceName: "web", Port: 3000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if domain.DomainID != "d1" {
		t.Fatalf("expected existing domain d1, got %#v", domain)
	}
	if calls["POST /api/domain.create"] != 0 {
		t.Fatalf("expected no domain.create calls, got %#v", calls)
	}
}

func TestCreateDomainCreatesWhenMissing(t *testing.T) {
	var receivedBody CreateDomainRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
			_ = json.NewEncoder(w).Encode([]Domain{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.create":
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(Domain{DomainID: "d2", Host: "api.example.com", ComposeID: "c1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	_, err := client.CreateDomain(context.Background(), CreateDomainRequest{Host: "api.example.com", ComposeID: "c1", ServiceName: "web", Port: 3000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody.DomainType != "compose" || receivedBody.CertificateType != "none" || receivedBody.Path != "/" || receivedBody.InternalPath != "/" {
		t.Fatalf("expected compose/none/// defaults, got %#v", receivedBody)
	}
	if receivedBody.HTTPS {
		t.Fatalf("expected https=false, got true")
	}
}

func TestUpdateAndDeployComposeSendExpectedBodies(t *testing.T) {
	var update updateComposeRequest
	var deploy deployComposeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			http.Error(w, `{"code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/compose.update":
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Fatalf("decode update: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		case "/api/compose.deploy":
			if err := json.NewDecoder(r.Body).Decode(&deploy); err != nil {
				t.Fatalf("decode deploy: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	if err := client.UpdateCompose(context.Background(), "c1", "services: {}", "FOO=bar\n"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if update.ComposeID != "c1" || update.SourceType != "raw" || update.Env != "FOO=bar\n" || update.ComposeFile != "services: {}" {
		t.Fatalf("unexpected update body: %#v", update)
	}
	if err := client.DeployCompose(context.Background(), "c1", "bort-migrate-run1"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if deploy.ComposeID != "c1" || deploy.Title != "bort-migrate-run1" {
		t.Fatalf("unexpected deploy body: %#v", deploy)
	}
}

func TestGetProjectAndFindEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/project.one" && r.URL.Query().Get("projectId") == "p1" {
			_ = json.NewEncoder(w).Encode(Project{
				ProjectID: "p1", Name: "api",
				Environments: []ProjectEnvironment{{EnvironmentID: "env-staging", Name: "staging"}, {EnvironmentID: "env-prod", Name: "production"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	project, err := client.GetProject(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	env := FindEnvironmentInProject(project, "production")
	if env == nil || env.EnvironmentID != "env-prod" {
		t.Fatalf("expected production env, got %#v", env)
	}
	missing := FindEnvironmentInProject(project, "qa")
	if missing == nil || missing.EnvironmentID != "env-staging" {
		t.Fatalf("expected fallback to first env, got %#v", missing)
	}
}
