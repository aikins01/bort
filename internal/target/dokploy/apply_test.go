package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
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

func TestNewClientFromEnvRejectsRemoteHTTP(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://dokploy.example")
	t.Setenv(EnvToken, "secret")
	if _, err := NewClientFromEnv(); err == nil || !strings.Contains(err.Error(), "non-loopback http") {
		t.Fatalf("expected remote http URL to be rejected, got %v", err)
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

func TestApplyResumeFromPrimesCompletedCreateSteps(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  web:\n    image: example/api:latest\n"), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]Project{{ProjectID: "p1", Name: "api", Environments: []ProjectEnvironment{{EnvironmentID: "env1", Name: "production"}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/project.create":
			t.Fatalf("project.create should not run while priming resume state")
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			if r.URL.Query().Get("name") != "api" || r.URL.Query().Get("environmentId") != "env1" {
				t.Fatalf("unexpected compose search query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "c1", Name: "api", AppName: "compose-api"}}, Total: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.create":
			t.Fatalf("compose.create should not run while priming resume state")
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			var req updateComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode compose.update: %v", err)
			}
			if req.ComposeID != "c1" {
				t.Fatalf("expected resumed compose id c1, got %#v", req)
			}
			updates++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		BaseURL:    server.URL,
		Token:      "secret",
		HTTPClient: server.Client(),
		Docker: &fakeDockerRunner{
			outputs: map[string][]byte{
				"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			},
			runOutputs: map[string][]byte{
				"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": {},
			},
		},
	}
	plan := Plan{
		ResumeFrom: 2,
		Steps: []Step{
			{Kind: StepCreateProject, App: "api", Ref: "api"},
			{Kind: StepCreateService, App: "api", Ref: "api"},
			{Kind: StepUploadEnv, App: "api", Ref: "api"},
		},
		Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{{
			Name:      "api",
			Directory: "api",
			TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
				ComposeApp: preparer.DokployComposeApp{Name: "api", ComposePath: "compose.yaml"},
			}},
		}}},
	}
	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("apply resume: %v", err)
	}
	if updates != 1 {
		t.Fatalf("expected one compose.update, got %d", updates)
	}
}

func TestApplySkipsPlatformAppSteps(t *testing.T) {
	client := &Client{}
	var progress []StepProgress
	fn := func(p StepProgress) {
		progress = append(progress, p)
	}
	plan := Plan{
		Steps: []Step{
			{Kind: StepCreateProject, App: "source", Ref: "source"},
			{Kind: StepPauseSource, App: "source", Ref: "source"},
		},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{{Name: "source", Role: "platform"}}},
		OnProgress: &fn,
	}
	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("apply platform skip: %v", err)
	}
	var skipped int
	for _, p := range progress {
		if p.Status == StepStatusSkipped {
			skipped++
		}
		if p.Status == StepStatusError {
			t.Fatalf("unexpected error progress: %#v", p)
		}
	}
	if skipped != len(plan.Steps) {
		t.Fatalf("expected %d skipped platform steps, got %d progress=%#v", len(plan.Steps), skipped, progress)
	}
}

func TestApplyStopsBeforeStepWhenPersistenceHookFails(t *testing.T) {
	sentinel := errors.New("persist started step")
	beforeStep := func(StepProgress) error {
		return sentinel
	}
	var progress []StepProgress
	onProgress := func(p StepProgress) {
		progress = append(progress, p)
	}
	client := &Client{}
	err := client.Apply(context.Background(), Plan{
		Steps:      []Step{{Kind: StepCreateProject, App: "source", Ref: "source"}},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{{Name: "source", Role: "platform"}}},
		BeforeStep: &beforeStep,
		OnProgress: &onProgress,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected before-step failure, got %v", err)
	}
	if len(progress) != 0 {
		t.Fatalf("expected no progress after before-step failure, got %#v", progress)
	}
}

func TestApplyResumesPausedSourceWhenPersistenceFailsBetweenSteps(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container web-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
		"stop web-id":                     []byte("web-id\n"),
	}}
	var beforeCalls int
	beforeStep := func(StepProgress) error {
		beforeCalls++
		if beforeCalls == 2 {
			return errors.New("persist failed")
		}
		return nil
	}
	var progress []StepProgress
	onProgress := func(p StepProgress) {
		progress = append(progress, p)
	}
	client := &Client{Docker: runner}
	err := client.Apply(context.Background(), Plan{
		Steps:      []Step{{Kind: StepPauseSource, App: "api", Ref: "api"}, {Kind: StepCreateVolume, App: "api", Ref: "data"}},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
		BeforeStep: &beforeStep,
		OnProgress: &onProgress,
	})
	if err == nil || !strings.Contains(err.Error(), "persist failed") {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	for _, p := range progress {
		if p.Step.Kind == StepResumeSource && p.Status == StepStatusOK {
			return
		}
	}
	t.Fatalf("expected cleanup to resume the paused source, got %#v", progress)
}

func TestActivePatchGuardBlocksGitBackedCompose(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres.1.task\n"),
		},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres.1.task psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": []byte(`{"patchId":"bort-api-compose","filePath":"docker|compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n"),
		},
	}
	client := &Client{Docker: runner}
	err := client.validateNoActiveBortOverrides(context.Background(), "compose-api")
	if err == nil || !strings.Contains(err.Error(), "active Bort-owned Dokploy patch") || !strings.Contains(err.Error(), "bort-api-compose") || !strings.Contains(err.Error(), "docker|compose.yml") {
		t.Fatalf("expected active patch guard, got %v", err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("expected one Dokploy DB patch inspection, got %#v", runner.runs)
	}
	sql := string(runner.runs[0].Stdin)
	for _, want := range []string{"c.\"composeId\" = 'compose-api'", "json_build_object", "p.\"patchId\" like 'bort-%'", "sourceType"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("expected patch guard sql to contain %q, got:\n%s", want, sql)
		}
	}
}

func TestApplyPushImageChecksActivePatchWithoutEnvOrRoutes(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres.1.task\n")},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres.1.task psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": []byte(`{"patchId":"bort-api-compose","filePath":"docker|compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n"),
		},
	}
	client := &Client{Docker: runner}
	app := preparer.AppPlan{Name: "api", Directory: "api", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
		Project:    preparer.DokployProject{Name: "api", Environment: "production"},
		ComposeApp: preparer.DokployComposeApp{Name: "api", ComposePath: "compose.yaml"},
	}}}
	actx := &applyContext{plan: Plan{Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}}, cache: map[string]*appCache{"api": {ComposeID: "compose-1"}}}
	err := client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api", Ref: "api"})
	if err == nil || !strings.Contains(err.Error(), "active Bort-owned Dokploy patch") {
		t.Fatalf("expected unconditional deployment path to block active patch, got %v", err)
	}
}

func TestActivePatchGuardRejectsMissingComposeID(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	if err := client.validateNoActiveBortOverrides(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "missing composeId") {
		t.Fatalf("expected missing composeId to fail closed, got %v", err)
	}
}

func TestParseActiveBortOverridePatchesFailsClosedOnMalformedRow(t *testing.T) {
	if _, err := parseActiveBortOverridePatches("not-json\n"); err == nil {
		t.Fatal("expected malformed patch row to fail closed")
	}
}

func TestActivePatchGuardFailsClosedWhenInspectionCannotFindDokploy(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	err := client.validateNoActiveBortOverrides(context.Background(), "compose-api")
	if err == nil || !strings.Contains(err.Error(), "active patch inspection") || !strings.Contains(err.Error(), "docker output not stubbed") {
		t.Fatalf("expected active patch inspection to fail closed, got %v", err)
	}
}

func TestActivePatchGuardFailsClosedWhenDokployDatabaseIsUnavailable(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps --format {{.Names}}": []byte("unrelated\n"),
	}}
	client := &Client{Docker: runner}

	if err := client.validateNoActiveBortOverrides(context.Background(), "compose-api"); err == nil || !strings.Contains(err.Error(), "dokploy postgres container was not found") {
		t.Fatalf("expected missing Dokploy DB to fail closed, got %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("expected no Dokploy DB query when postgres is absent, got %#v", runner.runs)
	}
}

func TestPlanFromArtifactsUsesGroupedProjectAndStableComposeNames(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{
		{
			Name: "demo-app",
			TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
				Project:    preparer.DokployProject{Name: "demo-project", Environment: "production"},
				ComposeApp: preparer.DokployComposeApp{Name: "demo-api"},
			}},
		},
		{
			Name: "demo postgres",
			TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
				Project:    preparer.DokployProject{Name: "demo-project", Environment: "production"},
				ComposeApp: preparer.DokployComposeApp{Name: "demo-postgres"},
			}},
		},
	}}

	plan := PlanFromArtifacts(prepare, syncplan.Result{}, gateway.Result{})
	projectRefs := []string{}
	composeRefs := []string{}
	for _, step := range plan.Steps {
		switch step.Kind {
		case StepCreateProject:
			projectRefs = append(projectRefs, step.Ref)
		case StepCreateService:
			composeRefs = append(composeRefs, step.Ref)
		}
	}
	if strings.Join(projectRefs, ",") != "demo-project,demo-project" {
		t.Fatalf("expected both apps to use grouped project demo-project, got %v", projectRefs)
	}
	if strings.Join(composeRefs, ",") != "demo-api,demo-postgres" {
		t.Fatalf("expected stable compose names, got %v", composeRefs)
	}
}

func TestApplyCreateStepsReuseGroupedProjectPerApp(t *testing.T) {
	bundleDir := t.TempDir()
	for _, dir := range []string{"demo-app", "demo-postgres"} {
		appDir := filepath.Join(bundleDir, dir)
		if err := os.MkdirAll(appDir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  app:\n    image: example/"+dir+":latest\n"), 0o600); err != nil {
			t.Fatalf("write compose %s: %v", dir, err)
		}
	}

	createdComposes := []CreateComposeRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]Project{{ProjectID: "p-demo-project", Name: "demo-project", Environments: []ProjectEnvironment{{EnvironmentID: "env-prod", Name: "production"}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/project.create":
			t.Fatalf("project.create should not run for existing grouped project")
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			if r.URL.Query().Get("environmentId") != "env-prod" {
				t.Fatalf("expected compose search in grouped project environment, got %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{}, Total: 0})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.create":
			var req CreateComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode compose.create: %v", err)
			}
			if req.EnvironmentID != "env-prod" {
				t.Fatalf("expected compose create in grouped project environment, got %#v", req)
			}
			createdComposes = append(createdComposes, req)
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c-" + req.Name, Name: req.Name, AppName: "compose-" + req.Name})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		BaseURL:    server.URL,
		Token:      "secret",
		HTTPClient: server.Client(),
		Docker: &fakeDockerRunner{
			outputs: map[string][]byte{
				"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			},
			runOutputs: map[string][]byte{
				"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": {},
			},
		},
	}
	plan := Plan{
		Steps: []Step{
			{Kind: StepCreateProject, App: "demo-app", Ref: "demo-project"},
			{Kind: StepCreateService, App: "demo-app", Ref: "demo-api"},
			{Kind: StepCreateProject, App: "demo postgres", Ref: "demo-project"},
			{Kind: StepCreateService, App: "demo postgres", Ref: "demo-postgres"},
		},
		Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{
			{
				Name:      "demo-app",
				Directory: "demo-app",
				TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
					Project:    preparer.DokployProject{Name: "demo-project", Environment: "production"},
					ComposeApp: preparer.DokployComposeApp{Name: "demo-api", ComposePath: "compose.yaml"},
				}},
			},
			{
				Name:      "demo postgres",
				Directory: "demo-postgres",
				TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
					Project:    preparer.DokployProject{Name: "demo-project", Environment: "production"},
					ComposeApp: preparer.DokployComposeApp{Name: "demo-postgres", ComposePath: "compose.yaml"},
				}},
			},
		}},
	}
	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("apply grouped create steps: %v", err)
	}
	if len(createdComposes) != 2 || createdComposes[0].Name != "demo-api" || createdComposes[1].Name != "demo-postgres" {
		t.Fatalf("expected stable compose creates in grouped project, got %#v", createdComposes)
	}
}

func TestFormatEnvUploadValueKeepsMultilineValuesSingleLine(t *testing.T) {
	value := normalizeEnvUploadValue("\"line1\\nline two\"")
	if value != "line1\nline two" {
		t.Fatalf("unexpected normalized value: %q", value)
	}
	if got := formatEnvUploadValue(value); got != `line1\nline two` {
		t.Fatalf("expected literal newline escape without quotes, got %q", got)
	}
	if got := formatEnvUploadValue("hello world"); got != `"hello world"` {
		t.Fatalf("expected spaces to stay quoted, got %q", got)
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
			_ = json.NewEncoder(w).Encode([]Domain{{DomainID: "d1", Host: "api.example.com", DomainType: "compose", ComposeID: "c1", ServiceName: "web", Port: 3000, Path: "/", InternalPath: "/"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.create":
			t.Fatalf("domain.create should not be called when host exists")
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.update":
			t.Fatalf("domain.update should not be called when domain is current")
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

func TestCreateDomainUpdatesExistingWhenServiceChanged(t *testing.T) {
	var received UpdateDomainRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
			_ = json.NewEncoder(w).Encode([]Domain{{DomainID: "d1", Host: "api.example.com", DomainType: "compose", ComposeID: "c1", ServiceName: "api-old", Port: 8080, Path: "/", InternalPath: "/"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.update":
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatalf("decode domain.update: %v", err)
			}
			_ = json.NewEncoder(w).Encode(Domain{DomainID: "d1", Host: received.Host, DomainType: received.DomainType, ComposeID: "c1", ServiceName: received.ServiceName, Port: received.Port})
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.create":
			t.Fatalf("domain.create should not be called when host exists")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	domain, err := client.CreateDomain(context.Background(), CreateDomainRequest{Host: "api.example.com", ComposeID: "c1", ServiceName: "api-new", Port: 8080})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if domain.ServiceName != "api-new" {
		t.Fatalf("expected updated domain, got %#v", domain)
	}
	if received.DomainID != "d1" || received.Host != "api.example.com" || received.ServiceName != "api-new" || received.Port != 8080 || received.DomainType != "compose" {
		t.Fatalf("unexpected update body: %#v", received)
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

func TestApplyActivateRoutesUpdatesDomainsAndRedeploys(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "example-app")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	compose := `services:
  api-current:
    image: example/api:latest
    expose:
      - "8080"
    labels:
      - traefik.enable=true
      - traefik.http.routers.old.rule=Host(` + "`" + `api.example.com` + "`" + `)
`
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	var updatedDomain UpdateDomainRequest
	var updatedCompose updateComposeRequest
	deploys := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			if err := json.NewDecoder(r.Body).Decode(&updatedCompose); err != nil {
				t.Fatalf("decode compose.update: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
			_ = json.NewEncoder(w).Encode([]Domain{{DomainID: "d1", Host: "api.example.com", DomainType: "compose", ComposeID: "c1", ServiceName: "api-stale", Port: 8080, Path: "/", InternalPath: "/"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/domain.update":
			if err := json.NewDecoder(r.Body).Decode(&updatedDomain); err != nil {
				t.Fatalf("decode domain.update: %v", err)
			}
			_ = json.NewEncoder(w).Encode(Domain{DomainID: updatedDomain.DomainID, Host: updatedDomain.Host, DomainType: updatedDomain.DomainType, ComposeID: "c1", ServiceName: updatedDomain.ServiceName, Port: updatedDomain.Port})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			deploys++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		BaseURL:    server.URL,
		Token:      "secret",
		HTTPClient: server.Client(),
		Docker: &fakeDockerRunner{
			outputs: map[string][]byte{
				"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			},
			runOutputs: map[string][]byte{
				"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": {},
			},
		},
	}
	plan := Plan{
		Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{{
			Name:      "example-app",
			Directory: "example-app",
			TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
				ComposeApp: preparer.DokployComposeApp{Name: "example-app", ComposePath: "compose.yaml"},
			}},
		}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{
			Name: "example-app",
			Routes: []gateway.Route{{
				Host:        "api.example.com",
				ServiceName: "api-stale",
				Port:        "8080",
				Source:      "traefik.http.routers.https-0-stack-api.rule",
			}},
		}}},
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{"example-app": {ComposeID: "c1"}}}
	if err := client.applyActivateRoutes(context.Background(), actx, Step{Kind: StepActivateRoutes, App: "example-app", Ref: "routes"}); err != nil {
		t.Fatalf("applyActivateRoutes: %v", err)
	}
	if strings.Contains(updatedCompose.ComposeFile, "traefik.") {
		t.Fatalf("expected source traefik labels stripped before redeploy, got:\n%s", updatedCompose.ComposeFile)
	}
	if updatedDomain.ServiceName != "api-current" {
		t.Fatalf("expected stale domain service to be updated, got %#v", updatedDomain)
	}
	if !updatedDomain.HTTPS || updatedDomain.CertificateType != "letsencrypt" {
		t.Fatalf("expected route domain to enable letsencrypt https, got %#v", updatedDomain)
	}
	if deploys != 1 {
		t.Fatalf("expected one compose deploy after domain update, got %d", deploys)
	}
}

func TestApplyActivateRoutesDetectsMigratedVolumeDriftAfterDeploy(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  web:\n    image: example/web\n"), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	deploys := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			deploys++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &latePostDeployTargetRunner{}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	app := preparer.AppPlan{Name: "api", Directory: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Target: "/data"}}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}}}
	entry := actx.entry("api")
	entry.ComposeID = "c1"
	entry.ComposeAppName = "stack-1"
	entry.MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"): {Service: "web", Target: "/data", VolumeName: "migrated-vol"},
	}

	err := client.applyActivateRoutes(context.Background(), actx, Step{Kind: StepActivateRoutes, App: "api", Ref: "routes"})
	if err == nil || !strings.Contains(err.Error(), "changed from migrated volume migrated-vol to fresh-vol") {
		t.Fatalf("expected migrated volume drift after deploy, got %v", err)
	}
	if deploys != 1 {
		t.Fatalf("expected route activation deploy before validation, got %d", deploys)
	}
	if !fakeOutputCalled(&fakeDockerRunner{outputArgs: runner.outputArgs}, "stop", "web-id") {
		t.Fatalf("expected drifted target container to stop, calls=%#v", runner.outputArgs)
	}
}
