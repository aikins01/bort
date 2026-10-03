package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestLocalDockerRunnerRefusesExplicitRemoteDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://remote.example")
	t.Setenv("DOCKER_CONTEXT", "production")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/tmp/remote-certs")
	_, err := (localDockerRunner{Path: filepath.Join(t.TempDir(), "docker")}).Output(context.Background(), "version")
	if err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Fatalf("expected explicit remote Docker target refusal, got %v", err)
	}
}

func TestPrimeResumeStateUsesPersistedComposeIdentity(t *testing.T) {
	projectCatalogRequests := 0
	composeSearchRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project.one":
			if r.URL.Query().Get("projectId") != "project-1" {
				t.Fatalf("unexpected project identity: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(Project{ProjectID: "project-1", Environments: []ProjectEnvironment{{EnvironmentID: "environment-1", Name: "production"}}})
		case "/api/compose.one":
			if r.URL.Query().Get("composeId") != "compose-selected" {
				t.Fatalf("unexpected compose identity: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-selected", Name: "api", AppName: "stack-api", EnvironmentID: "environment-1"})
		case "/api/project.all":
			projectCatalogRequests++
			http.Error(w, "name lookup must not run", http.StatusInternalServerError)
		case "/api/compose.search":
			composeSearchRequests++
			http.Error(w, "name lookup must not run", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := preparer.AppPlan{Name: "api"}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{
		Project:    preparer.DokployProject{Name: "api", Environment: "production"},
		ComposeApp: preparer.DokployComposeApp{Name: "api"},
	}}
	plan := Plan{
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Steps: []Step{
			{Kind: StepCreateProject, App: "api", Ref: "api"},
			{Kind: StepCreateService, App: "api", Ref: "api"},
			{Kind: StepInstallGateway, App: "api", Ref: "api.example.com"},
		},
		TargetIdentities: map[string]TargetIdentity{"api": {
			ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-selected", ComposeAppName: "stack-api",
		}},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	pausedApps := pausedSources{}
	coolifyProxyStopped := false

	if err := client.primeResumeState(context.Background(), actx, plan.Steps[:2], pausedApps, map[string]struct{}{}, &coolifyProxyStopped); err != nil {
		t.Fatalf("primeResumeState: %v", err)
	}
	entry := actx.entry("api")
	if entry.ProjectID != "project-1" || entry.EnvironmentID != "environment-1" || entry.ComposeID != "compose-selected" || entry.ComposeAppName != "stack-api" {
		t.Fatalf("resume did not hydrate persisted target identity: %#v", entry)
	}
	if projectCatalogRequests != 0 || composeSearchRequests != 0 {
		t.Fatalf("resume fell back to mutable names, project requests=%d compose searches=%d", projectCatalogRequests, composeSearchRequests)
	}
}

func TestHydratePersistedTargetIdentitiesLoadsSharedProjectOnce(t *testing.T) {
	projectRequests := 0
	composeRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project.one":
			projectRequests++
			_ = json.NewEncoder(w).Encode(Project{ProjectID: "project-1", Environments: []ProjectEnvironment{{EnvironmentID: "environment-1", Name: "production"}}})
		case "/api/compose.one":
			composeRequests++
			id := r.URL.Query().Get("composeId")
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: id, AppName: "stack-" + id, EnvironmentID: "environment-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	apps := []preparer.AppPlan{
		{Name: "alpha", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{}}},
		{Name: "beta", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{}}},
	}
	actx := &applyContext{plan: Plan{
		Prepare: preparer.Result{Apps: apps},
		TargetIdentities: map[string]TargetIdentity{
			"alpha": {ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-alpha"},
			"beta":  {ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-beta"},
		},
	}, cache: map[string]*appCache{}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}

	if err := client.hydratePersistedTargetIdentities(context.Background(), actx); err != nil {
		t.Fatal(err)
	}
	if projectRequests != 1 || composeRequests != 2 {
		t.Fatalf("expected one shared project lookup and two compose lookups, got projects=%d composes=%d", projectRequests, composeRequests)
	}
}

func TestHydratePersistedTargetIdentitiesRejectsEnvironmentOutsideProject(t *testing.T) {
	composeRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project.one":
			_ = json.NewEncoder(w).Encode(Project{ProjectID: "project-1", Environments: []ProjectEnvironment{{EnvironmentID: "other-environment"}}})
		case "/api/compose.one":
			composeRequests++
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-1", EnvironmentID: "environment-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	app := preparer.AppPlan{Name: "api", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{}}}
	actx := &applyContext{plan: Plan{
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		TargetIdentities: map[string]TargetIdentity{"api": {
			ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-1",
		}},
	}, cache: map[string]*appCache{}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	err := client.hydratePersistedTargetIdentities(context.Background(), actx)
	if err == nil || !strings.Contains(err.Error(), "no longer belongs to project") {
		t.Fatalf("expected environment membership refusal, got %v", err)
	}
	if composeRequests != 0 {
		t.Fatalf("compose was loaded before project membership was verified, requests=%d", composeRequests)
	}
}

func TestLegacyPlanPreservesV1Alpha1RoutedStateOrder(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Source: "data", Target: "/data"}}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
	sync := syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
		ResourceType: "volume",
		ResourceRef:  "volume:web -> /data",
		Strategy:     syncplan.StrategyDockerVolumeArchive,
	}}}}}
	cutover := gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}}
	legacy := LegacyPlanFromArtifactsV1Alpha1(prepare, sync, cutover)
	current := PlanFromArtifactsV1Alpha2(prepare, sync, cutover)

	indexOf := func(plan Plan, kind StepKind) int {
		for index, step := range plan.Steps {
			if step.Kind == kind {
				return index
			}
		}
		return -1
	}
	legacyResume := indexOf(legacy, StepResumeTarget)
	legacyActivate := indexOf(legacy, StepActivateRoutes)
	legacyStop := indexOf(legacy, StepStopCoolifyProxy)
	if legacyResume < 0 || legacyActivate <= legacyResume || legacyStop <= legacyActivate {
		t.Fatalf("unexpected legacy routed state order: %v", stepKinds(legacy.Steps))
	}
	currentStop := indexOf(current, StepStopCoolifyProxy)
	currentResume := indexOf(current, StepResumeTarget)
	currentActivate := indexOf(current, StepActivateRoutes)
	if currentStop < 0 || currentResume <= currentStop || currentActivate <= currentResume {
		t.Fatalf("unexpected current routed state order: %v", stepKinds(current.Steps))
	}
}

func TestLegacyPlanPreservesV1Alpha3UnroutedStateOrder(t *testing.T) {
	app := preparer.AppPlan{
		Name: "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			ComposeApp: preparer.DokployComposeApp{Name: "api"},
		}},
	}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Name: "data", Target: "/data"}}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
	sync := syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
		ResourceType: "volume",
		ResourceRef:  "volume:web -> /data",
		Strategy:     syncplan.StrategyDockerVolumeArchive,
	}}}}}
	legacy := LegacyPlanFromArtifactsV1Alpha3(prepare, sync, gateway.Result{})
	current := PlanFromArtifacts(prepare, sync, gateway.Result{})

	legacyKinds := stepKinds(legacy.Steps)
	syncIndex := slices.Index(legacyKinds, StepSyncVolume)
	pushIndex := slices.Index(legacyKinds, StepPushImage)
	resumeIndex := slices.Index(legacyKinds, StepResumeSource)
	if syncIndex < 0 || resumeIndex != syncIndex+1 || pushIndex != resumeIndex+1 {
		t.Fatalf("legacy v1alpha3 order changed: %v", legacyKinds)
	}
	if slices.Contains(stepKinds(current.Steps), StepResumeSource) {
		t.Fatalf("current staged plan resumed an unrouted source writer: %v", stepKinds(current.Steps))
	}
}

func TestNewClientFromEnvRejectsRemoteHTTP(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://dokploy.example")
	t.Setenv(EnvToken, "secret")
	if _, err := NewClientFromEnv(); err == nil || !strings.Contains(err.Error(), "non-loopback http") {
		t.Fatalf("expected remote http URL to be rejected, got %v", err)
	}
}

func TestNormalizeTokenBaseURLPreservesPathAndRejectsRequestComponents(t *testing.T) {
	got, err := NormalizeTokenBaseURL("https://DOKPLOY.example:443/tenant-a/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://dokploy.example/tenant-a" {
		t.Fatalf("unexpected normalized URL %q", got)
	}
	for _, raw := range []string{
		"https://user@dokploy.example",
		"https://dokploy.example?tenant=a",
		"https://dokploy.example#tenant-a",
	} {
		if _, err := NormalizeTokenBaseURL(raw); err == nil {
			t.Fatalf("expected credentialed URL %q to be rejected", raw)
		}
	}
}

func TestPlanBundleFilesPinComposeAndEnvContents(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(appDir, "compose.yaml")
	envPath := filepath.Join(appDir, ".env")
	reviewedCompose := []byte("services:\n  api:\n    image: example/api:v1\n")
	reviewedEnv := []byte("TOKEN=reviewed\n")
	if err := os.WriteFile(composePath, reviewedCompose, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, reviewedEnv, 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{
		Name:      "api",
		Directory: "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"},
			EnvFiles:   []preparer.DokployEnvFile{{Path: ".env"}},
		}},
	}
	plan := Plan{
		Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}},
		BundleFiles: map[string][]byte{
			filepath.Clean(composePath): reviewedCompose,
			filepath.Clean(envPath):     reviewedEnv,
		},
	}
	if err := os.WriteFile(composePath, []byte("services:\n  api:\n    image: example/api:v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("TOKEN=changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	compose, err := readComposeFile(plan, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(compose, "example/api:v1") || strings.Contains(compose, "example/api:v2") {
		t.Fatalf("compose did not use captured reviewed contents:\n%s", compose)
	}
	env, err := readEnvContent(plan, "api")
	if err != nil {
		t.Fatal(err)
	}
	if env != "TOKEN=reviewed\n" {
		t.Fatalf("env did not use captured reviewed contents: %q", env)
	}
}

func TestValidatePlanReadyForLiveApplyRequiresApprovedDecisionCodes(t *testing.T) {
	app := preparer.AppPlan{
		Name:      "api",
		Readiness: preparer.ReadinessNeedsDecision,
		Gates: []preparer.Gate{{
			Code:      "prepare.review",
			Message:   "review the prepared app",
			Readiness: preparer.ReadinessNeedsDecision,
		}},
	}
	plan := Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}
	if err := validatePlanReadyForLiveApply(plan); err == nil || !strings.Contains(err.Error(), "unapproved prepare decisions") {
		t.Fatalf("expected an unapproved needs_decision app to block live apply, got %v", err)
	}
	approved := NewPrepareDecision("api", "prepare.review", "", preparer.ReadinessNeedsDecision, "review the prepared app")
	plan.ApprovedPrepareDecisions = map[PrepareDecision]struct{}{approved: {}}
	if err := validatePlanReadyForLiveApply(plan); err != nil {
		t.Fatalf("expected the explicitly approved decision code to pass validation: %v", err)
	}
	plan.Prepare.Apps[0].Gates[0].Code = "prepare.future_requirement"
	if err := validatePlanReadyForLiveApply(plan); err == nil || !strings.Contains(err.Error(), "unapproved prepare decisions") {
		t.Fatalf("expected an unknown needs_decision code to fail closed, got %v", err)
	}
	plan.Prepare.Apps[0].Gates[0].Code = ""
	blank := NewPrepareDecision("api", "", "", preparer.ReadinessNeedsDecision, "review the prepared app")
	plan.ApprovedPrepareDecisions = map[PrepareDecision]struct{}{blank: {}}
	if err := validatePlanReadyForLiveApply(plan); err == nil || !strings.Contains(err.Error(), "unapproved prepare decisions") {
		t.Fatalf("expected a blank needs_decision code to fail closed, got %v", err)
	}
}

func TestValidatePlanReadyForLiveApplySeparatesDecisionIdentity(t *testing.T) {
	gate := preparer.Gate{
		Code:        "prepare.review",
		ResourceRef: "service/api",
		Message:     "review the prepared app",
		Readiness:   preparer.ReadinessNeedsDecision,
	}
	plan := Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{
		{Name: "api", Readiness: preparer.ReadinessNeedsDecision, Gates: []preparer.Gate{gate}},
		{Name: "worker", Readiness: preparer.ReadinessNeedsDecision, Gates: []preparer.Gate{gate}},
	}}}
	apiApproval := NewPrepareDecision("api", gate.Code, gate.ResourceRef, gate.Readiness, gate.Message)
	plan.ApprovedPrepareDecisions = map[PrepareDecision]struct{}{apiApproval: {}}
	if err := validatePlanReadyForLiveApply(plan); err == nil || !strings.Contains(err.Error(), "app worker") {
		t.Fatalf("expected api approval not to authorize worker, got %v", err)
	}

	workerApproval := NewPrepareDecision("worker", gate.Code, gate.ResourceRef, gate.Readiness, gate.Message)
	plan.ApprovedPrepareDecisions[workerApproval] = struct{}{}
	if err := validatePlanReadyForLiveApply(plan); err != nil {
		t.Fatalf("expected exact approvals for both apps to pass validation: %v", err)
	}

	plan.Prepare.Apps[1].Gates[0].ResourceRef = "service/worker"
	if err := validatePlanReadyForLiveApply(plan); err == nil || !strings.Contains(err.Error(), "app worker") {
		t.Fatalf("expected approval for a different resource not to authorize worker, got %v", err)
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

type partialSourcePauseRunner struct {
	fakeDockerRunner
	firstRunning bool
}

func (r *partialSourcePauseRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	r.outputArgs = append(r.outputArgs, append([]string{}, args...))
	switch strings.Join(args, " ") {
	case "inspect --type container source-a":
		return []byte(fmt.Sprintf(`[{"Id":"source-a","Name":"/source-a","State":{"Running":%t,"Status":"running"}}]`, r.firstRunning)), nil
	case "stop source-a":
		r.firstRunning = false
		return []byte("source-a\n"), nil
	case "start source-a":
		r.firstRunning = true
		return []byte("source-a\n"), nil
	case "inspect --type container source-b":
		return []byte(`[{"Id":"source-b","Name":"/source-b","State":{"Running":true,"Status":"running"}}]`), nil
	case "stop source-b":
		return nil, errors.New("injected second source stop failure")
	default:
		return nil, fmt.Errorf("docker output not stubbed: %s", strings.Join(args, " "))
	}
}

func TestApplyResumeFromReconcilesCompletedProxyStop(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container coolify-proxy": []byte(`[{"Id":"proxy-id","Name":"/coolify-proxy","State":{"Running":true,"Status":"running"}}]`),
		"stop proxy-id":                          []byte("proxy-id\n"),
	}}
	client := &Client{Docker: runner}
	plan := Plan{Steps: []Step{
		{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer},
		{Kind: StepStartDokployProxy, Ref: dokployProxyContainer},
	}}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	pausedApps := pausedSources{}
	coolifyProxyStopped := false

	if err := client.primeResumeState(context.Background(), actx, plan.Steps[:1], pausedApps, map[string]struct{}{}, &coolifyProxyStopped); err != nil {
		t.Fatalf("prime resume state: %v", err)
	}
	if !fakeOutputCalled(runner, "stop", "proxy-id") {
		t.Fatalf("retry did not restore the durable proxy stop, calls=%#v", runner.outputArgs)
	}
}

func TestBestEffortProxyResumeUsesStandaloneProgressIndex(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container coolify-proxy": []byte(`[{"Id":"proxy-id","Name":"/coolify-proxy","State":{"Running":false,"Status":"exited"}}]`),
		"start proxy-id":                         []byte("proxy-id\n"),
	}}
	client := &Client{Docker: runner}
	progress := []StepProgress{}
	onProgress := func(item StepProgress) {
		progress = append(progress, item)
	}
	plan := Plan{
		Steps:      []Step{{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer}, {Kind: StepStartDokployProxy, Ref: dokployProxyContainer}},
		OnProgress: &onProgress,
	}
	if err := client.bestEffortResume(context.Background(), &applyContext{}, plan, len(plan.Steps), nil, true, true); err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 {
		t.Fatalf("expected proxy cleanup start and completion, got %#v", progress)
	}
	for _, item := range progress {
		if item.Index != len(plan.Steps) || item.Step.Kind != StepStartCoolifyProxy {
			t.Fatalf("proxy cleanup overwrote a forward step: %#v", item)
		}
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

func TestApplyResumesPausedSourceWhenNoStateTransferCompleted(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": true}}
	var beforeCalls int
	beforeStep := func(StepProgress) error {
		beforeCalls++
		if beforeCalls >= 2 {
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
	resumed := false
	for _, p := range progress {
		if p.Step.Kind == StepResumeSource && p.Status == StepStatusOK {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("source did not resume after a pre-transfer failure: %#v", progress)
	}
	if !runner.running["web-id"] || !fakeOutputCalled(&runner.fakeDockerRunner, "start", "web-id") {
		t.Fatalf("paused source was not restarted: running=%v calls=%v", runner.running, runner.outputArgs)
	}
	if beforeCalls != 2 {
		t.Fatalf("unexpected cleanup persistence attempt before any state transfer, calls=%d", beforeCalls)
	}
}

func TestActivePatchGuardBlocksGitBackedCompose(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres.1.task\n"),
		},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres.1.task psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": []byte(
				`{"patchId":"bort-api-compose","filePath":"docker|compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n" +
					`{"patchId":"bort-raw-compose","filePath":"docker|compose.yml","composeName":"raw-api","sourceType":"raw","repository":"owner/repo","branch":"main"}` + "\n"),
		},
	}
	client := &Client{Docker: runner}
	err := client.validateNoActiveBortOverrides(context.Background(), "compose-api")
	if err == nil || !strings.Contains(err.Error(), "active Bort-owned Dokploy patch") || !strings.Contains(err.Error(), "bort-api-compose") || !strings.Contains(err.Error(), "docker|compose.yml") || strings.Contains(err.Error(), "bort-raw-compose") {
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

func TestActivePatchGuardUsesCurrentLocalServiceTask(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres-backup\ndokploy-postgres.1.current\n"),
			"ps --filter label=com.docker.swarm.service.name=dokploy-postgres --filter status=running --format {{.ID}}": []byte("current-container-id\n"),
		},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres-backup psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": {},
			"exec -i current-container-id psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At":    []byte(`{"patchId":"bort-current","filePath":"compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n"),
		},
	}
	client := &Client{Docker: runner}

	err := client.validateNoActiveBortOverrides(context.Background(), "compose-api")
	if err == nil || !strings.Contains(err.Error(), "bort-current") {
		t.Fatalf("expected current service task patch to block apply, got %v", err)
	}
	if len(runner.runs) != 1 || !slices.Contains(runner.runs[0].Args, "current-container-id") {
		t.Fatalf("active patch guard queried the wrong database container: %#v", runner.runs)
	}
	if len(runner.outputArgs) != 1 || strings.Join(runner.outputArgs[0], " ") != "ps --filter label=com.docker.swarm.service.name=dokploy-postgres --filter status=running --format {{.ID}}" {
		t.Fatalf("active patch guard did not use one exact service-label query: %#v", runner.outputArgs)
	}
}

type stagedBortPatchRunner struct {
	fakeDockerRunner
	checks        int
	patchAt       int
	targetStopped bool
}

func (r *stagedBortPatchRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if key == "inspect --type container target-id" && r.targetStopped {
		r.outputArgs = append(r.outputArgs, append([]string{}, args...))
		return []byte(`[{"Id":"target-id","Name":"/target","State":{"Running":false,"Status":"exited"}}]`), nil
	}
	if key == "stop target-id" {
		r.targetStopped = true
	}
	return r.fakeDockerRunner.Output(ctx, args...)
}

func (r *stagedBortPatchRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	key := strings.Join(args, " ")
	if strings.Contains(key, " psql ") {
		r.checks++
		patchAt := r.patchAt
		if patchAt == 0 {
			patchAt = 2
		}
		if r.checks == patchAt {
			r.runOutputs[key] = []byte(`{"patchId":"bort-api-compose","filePath":"docker|compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n")
		} else {
			r.runOutputs[key] = nil
		}
	}
	return r.fakeDockerRunner.Run(ctx, stdin, stdout, args...)
}

type restartingTargetRunner struct {
	inspectCalls int
	stopCalls    int
}

func (r *restartingTargetRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}":
		return []byte("target-id\n"), nil
	case "inspect --type container target-id":
		r.inspectCalls++
		running := r.inspectCalls <= 2
		status := "exited"
		if running {
			status = "running"
		}
		return []byte(fmt.Sprintf(`[{"Id":"target-id","Name":"/target","State":{"Running":%t,"Status":%q}}]`, running, status)), nil
	case "stop target-id":
		r.stopCalls++
		return []byte("target-id\n"), nil
	default:
		return nil, fmt.Errorf("docker output not stubbed: %s", strings.Join(args, " "))
	}
}

func (r *restartingTargetRunner) Run(context.Context, io.Reader, io.Writer, ...string) error {
	return nil
}

func TestUpdateComposePatchGuardDetectsConcurrentPatch(t *testing.T) {
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/compose.update" {
			http.NotFound(w, r)
			return
		}
		updates++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{fakeDockerRunner: fakeDockerRunner{
		outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	err := client.updateComposeWithPatchGuard(context.Background(), "compose-api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "appeared while Bort was changing compose") {
		t.Fatalf("expected post-update patch detection, got %v", err)
	}
	if updates != 1 || runner.checks != 2 {
		t.Fatalf("expected one update between two patch checks, updates=%d checks=%d", updates, runner.checks)
	}
}

func TestDeployComposeForApplyChecksPatchBeforeDeploy(t *testing.T) {
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
	runner := &stagedBortPatchRunner{patchAt: 3, fakeDockerRunner: fakeDockerRunner{
		outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
		runOutputs: map[string][]byte{},
	}}
	actx := &applyContext{
		cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "active Bort-owned Dokploy patch") {
		t.Fatalf("expected pre-deployment patch refusal, got %v", err)
	}
	if deploys != 0 {
		t.Fatalf("pre-deployment refusal called compose.deploy %d time(s)", deploys)
	}
}

func TestPostDeployPatchGuardStopsTargetCompose(t *testing.T) {
	var deploymentTitle string
	var composeReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			deploymentTitle = request.Title
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			composeReads++
			attemptStatus := "running"
			if composeReads > 1 {
				attemptStatus = "done"
			}
			otherStatus := "running"
			if composeReads > 2 {
				otherStatus = "done"
			}
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-api", AppName: "stack-1", Deployments: []Deployment{
				{Title: deploymentTitle, Status: attemptStatus},
				{Title: "other-deployment", Status: otherStatus},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 4, fakeDockerRunner: fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("target-id\n"),
			"inspect --type container target-id":                                       []byte(`[{"Id":"target-id","Name":"/target","State":{"Running":true,"Status":"running"}}]`),
			"stop target-id":                                                           []byte("target-id\n"),
		},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "compose-api"
	entry.ComposeAppName = "stack-1"

	err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "deployment quiesced and target compose containers were stopped") || !isUnsafeTargetResumeError(err) {
		t.Fatalf("expected guarded deploy failure to stop the target and prevent target recovery, got %v", err)
	}
	if isUnsafeSourceResumeError(err) {
		t.Fatalf("successfully quiesced deployment should permit source recovery, got %v", err)
	}
	if composeReads < 3 {
		t.Fatalf("quiescence ignored another nonterminal deployment, reads=%d", composeReads)
	}
	stopCalls := 0
	targetListCalls := 0
	for _, call := range runner.outputArgs {
		if len(call) == 2 && call[0] == "stop" && call[1] == "target-id" {
			stopCalls++
		}
		if strings.Join(call, " ") == "ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}" {
			targetListCalls++
		}
	}
	if stopCalls != 1 || targetListCalls < 3 {
		t.Fatalf("expected target stop plus repeated and final verification, stops=%d lists=%d calls=%#v", stopCalls, targetListCalls, runner.outputArgs)
	}
	if fakeOutputCalled(&runner.fakeDockerRunner, "start", "target-id") {
		t.Fatalf("successful deploy guard failure restarted target, calls=%#v", runner.outputArgs)
	}
}

func TestDeployComposeForApplyResolvesAmbiguousResponseByTitle(t *testing.T) {
	var deploymentTitle string
	deploys := 0
	composeReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			deploys++
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			deploymentTitle = request.Title
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			composeReads++
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-api", Deployments: []Deployment{{Title: deploymentTitle, Status: "done"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
		outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}}}

	if err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", ""); err != nil {
		t.Fatalf("expected titled terminal deployment to resolve ambiguous response, got %v", err)
	}
	if deploys != 1 || composeReads != 1 || deploymentTitle == "" {
		t.Fatalf("expected one deploy resolved by one titled status read, deploys=%d reads=%d title=%q", deploys, composeReads, deploymentTitle)
	}
}

func TestDeployComposeForApplyFailsClosedWhenAmbiguousResponseCannotBeResolved(t *testing.T) {
	var deploymentTitle string
	composeReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			deploymentTitle = request.Title
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			composeReads++
			http.Error(w, "deployment status unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": {},
		},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}}}

	err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "response was ambiguous") || !strings.Contains(err.Error(), "deployment status unavailable") || !strings.Contains(err.Error(), "quiescence could not be proved") {
		t.Fatalf("expected unresolved ambiguous response evidence, got %v", err)
	}
	if !isUnsafeSourceResumeError(err) || !isUnsafeTargetResumeError(err) {
		t.Fatalf("unresolved ambiguous response must prevent source and target recovery, got %v", err)
	}
	if !mutationResponseMayHaveSucceeded(err) {
		t.Fatalf("unresolved deploy response was not marked mutation-ambiguous: %v", err)
	}
	if composeReads != 2 || deploymentTitle == "" {
		t.Fatalf("expected monitoring and quiescence to inspect the titled attempt, reads=%d title=%q", composeReads, deploymentTitle)
	}
	if !fakeOutputCalled(&runner.fakeDockerRunner, "ps", "-a", "--filter", "label=com.docker.compose.project=stack-1", "--format", "{{.ID}}") {
		t.Fatalf("unresolved ambiguous response did not attempt target quiescence, calls=%#v", runner.outputArgs)
	}
}

func TestDeployComposeForApplyDoesNotMonitorAuthoritativeAPIRejection(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   bool
	}{
		{name: "redirect", status: http.StatusFound},
		{name: "replay redirect", status: http.StatusTemporaryRedirect},
		{name: "truncated error", status: http.StatusConflict, body: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			composeReads := 0
			redirects := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
					if test.status >= 300 && test.status < 400 {
						w.Header().Set("Location", "/login")
					}
					if test.body {
						w.Header().Set("Content-Length", "100")
					}
					w.WriteHeader(test.status)
					if test.body {
						_, _ = w.Write([]byte("deploy rejected"))
					}
				case r.URL.Path == "/login":
					redirects++
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
					composeReads++
					http.NotFound(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
				outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
				runOutputs: map[string][]byte{},
			}}
			client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
			actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}}}

			err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != test.status {
				t.Fatalf("expected authoritative API rejection %d, got %v", test.status, err)
			}
			if mutationResponseMayHaveSucceeded(err) {
				t.Fatalf("authoritative API rejection was marked mutation-ambiguous: %v", err)
			}
			if composeReads != 0 || redirects != 0 || isUnsafeSourceResumeError(err) || isUnsafeTargetResumeError(err) {
				t.Fatalf("authoritative rejection followed redirect or entered deployment recovery, redirects=%d reads=%d err=%v", redirects, composeReads, err)
			}
		})
	}
}

func TestDeployComposeAcceptsCompletedSuccessWithTruncatedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/compose.deploy" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("queued"))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}

	if err := client.DeployCompose(context.Background(), "compose-api", "unique-title"); err != nil {
		t.Fatalf("completed 2xx deploy response was not accepted: %v", err)
	}
}

func TestWaitForGuardedDeploymentRechecksPatchAfterTerminalStatus(t *testing.T) {
	composeReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/compose.one" {
			http.NotFound(w, r)
			return
		}
		composeReads++
		_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-api", Deployments: []Deployment{{Title: "unique-title", Status: "done"}}})
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{
		patchAt: 1,
		fakeDockerRunner: fakeDockerRunner{
			outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
			runOutputs: map[string][]byte{},
		},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}

	deployment, err := client.waitForGuardedDeployment(context.Background(), "compose-api", "unique-title")
	if err == nil || !strings.Contains(err.Error(), "appeared while Bort was changing compose") {
		t.Fatalf("expected final patch check after terminal status, got %v", err)
	}
	if deployment.Title != "unique-title" || deployment.Status != "done" {
		t.Fatalf("final patch check discarded terminal deployment: %+v", deployment)
	}
	if composeReads != 1 || runner.checks != 1 {
		t.Fatalf("expected one terminal status read followed by a final patch check, reads=%d checks=%d", composeReads, runner.checks)
	}
}

func TestDeployComposeForApplyRejectsTerminalFailureStatuses(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    string
		patchAt   int
		wantPatch bool
	}{
		{name: "error", status: "error", patchAt: 100},
		{name: "cancelled", status: "cancelled", patchAt: 100},
		{name: "error with final patch conflict", status: "error", patchAt: 4, wantPatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var deploymentTitle string
			rawUpdated := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
					var request updateComposeRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					rawUpdated = request.SourceType == "raw"
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
					if !rawUpdated {
						t.Fatal("compose deploy was not preceded by a raw compose update")
					}
					var request deployComposeRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					deploymentTitle = request.Title
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
					_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-api", Deployments: []Deployment{{Title: deploymentTitle, Status: test.status, ErrorMessage: "deploy failed", LogPath: "/tmp/deploy.log"}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			runner := &stagedBortPatchRunner{
				patchAt: test.patchAt,
				fakeDockerRunner: fakeDockerRunner{
					outputs: map[string][]byte{
						"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
						"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("target-id\n"),
						"inspect --type container target-id":                                       []byte(`[{"Id":"target-id","Name":"/target","State":{"Running":true,"Status":"running"}}]`),
						"stop target-id":                                                           []byte("target-id\n"),
					},
					runOutputs: map[string][]byte{},
				},
			}
			client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
			actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}}}

			err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("status %q", test.status)) || !strings.Contains(err.Error(), "deploy failed") || !strings.Contains(err.Error(), "/tmp/deploy.log") || !strings.Contains(err.Error(), "Dokploy compose deployment did not complete successfully") || strings.Contains(err.Error(), "post-deploy safety guard failed") || !strings.Contains(err.Error(), "deployment quiesced and target compose containers were stopped") || !isUnsafeTargetResumeError(err) {
				t.Fatalf("expected terminal %s deployment to fail, got %v", test.status, err)
			}
			if got := strings.Contains(err.Error(), "appeared while Bort was changing compose"); got != test.wantPatch {
				t.Fatalf("final patch conflict = %t, want %t: %v", got, test.wantPatch, err)
			}
			if isUnsafeSourceResumeError(err) {
				t.Fatalf("terminal %s deployment should permit source recovery after quiescence, got %v", test.status, err)
			}
			if !fakeOutputCalled(&runner.fakeDockerRunner, "stop", "target-id") {
				t.Fatalf("terminal %s deployment did not stop target compose, calls=%#v", test.status, runner.outputArgs)
			}
		})
	}
}

func TestStopTargetComposeContainersRestopsRestartedTarget(t *testing.T) {
	runner := &restartingTargetRunner{}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{"api": {ComposeAppName: "stack-1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.stopTargetComposeContainers(ctx, actx, "api"); err != nil {
		t.Fatalf("stop restarted target: %v", err)
	}
	if runner.stopCalls != 2 {
		t.Fatalf("expected restarted target to be stopped again, stops=%d", runner.stopCalls)
	}
}

func TestDeploymentMonitoringQuiescenceFailurePreventsSourceRecovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			http.Error(w, "deployment status unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": {},
		},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "compose-api"
	entry.ComposeAppName = "stack-1"

	err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "deployment status unavailable") || !strings.Contains(err.Error(), "deployment monitoring could not prove safety") || strings.Contains(err.Error(), "post-deploy safety guard failed") {
		t.Fatalf("expected deployment polling failure, got %v", err)
	}
	if !isUnsafeSourceResumeError(err) || !isUnsafeTargetResumeError(err) {
		t.Fatalf("unproved deployment quiescence must prevent source and target recovery, got %v", err)
	}
}

func TestRejectedDeployKeepsPostDeploySafetyMarkers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			http.Error(w, "deploy rejected", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("web-id\n"),
			"inspect --type container web-id":                                          []byte(`[{"Id":"web-id","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"fresh-vol","Destination":"/data","RW":true}]}]`),
		},
		outputErrs: map[string]error{"stop web-id": errors.New("Error response from daemon: cannot stop container")},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "compose-api"
	entry.ComposeAppName = "stack-1"
	entry.MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"): {Service: "web", Target: "/data", VolumeName: "migrated-vol"},
	}

	err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "deploy rejected") || !strings.Contains(err.Error(), "also failed to stop unsafe target containers") {
		t.Fatalf("expected rejected deploy with failed safety stop, got %v", err)
	}
	if !isUnsafeSourceResumeError(err) || !isUnsafeTargetResumeError(err) || !mutationResponseMayHaveSucceeded(err) {
		t.Fatalf("rejected deploy must keep the post-deploy safety markers, got %v", err)
	}
}

func TestDeploymentQuiescenceRespectsCallerDeadline(t *testing.T) {
	composeReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			composeReads++
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-api"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres\n"),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": {},
		},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: Plan{}, cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()

	err := client.deployComposeForApply(ctx, actx, "api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") || !strings.Contains(err.Error(), "quiescence could not be proved") {
		t.Fatalf("expected deadline-bounded quiescence failure, got %v", err)
	}
	if !isUnsafeSourceResumeError(err) || !isUnsafeTargetResumeError(err) {
		t.Fatalf("expired quiescence deadline must prevent source and target recovery, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("quiescence exceeded caller deadline: %s", elapsed)
	}
	if composeReads == 0 {
		t.Fatal("deployment monitor did not inspect the accepted attempt")
	}
}

func TestQuiesceGuardedDeploymentAcceptsTerminalStatuses(t *testing.T) {
	for _, status := range []string{"done", "error", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/compose.one" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-api", Deployments: []Deployment{{Title: "unique-title", Status: status}}})
			}))
			defer server.Close()
			runner := &fakeDockerRunner{outputs: map[string][]byte{
				"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": {},
			}}
			client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
			actx := &applyContext{cache: map[string]*appCache{"api": {ComposeID: "compose-api", ComposeAppName: "stack-1"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := client.quiesceGuardedDeployment(ctx, actx, "api", "unique-title"); err != nil {
				t.Fatalf("terminal status %q did not quiesce: %v", status, err)
			}
		})
	}
}

func TestDeployComposeAttemptTitleIsUniquePerAttempt(t *testing.T) {
	plan := Plan{RunName: "run1"}
	first, err := deployComposeAttemptTitle(plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := deployComposeAttemptTitle(plan)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "bort-migrate-run1-"
	if first == second {
		t.Fatalf("deployment attempts reused title %q", first)
	}
	if !strings.HasPrefix(first, prefix) || len(strings.TrimPrefix(first, prefix)) != 32 {
		t.Fatalf("deployment title does not contain a 128-bit identity: %q", first)
	}
	if !strings.HasPrefix(second, prefix) || len(strings.TrimPrefix(second, prefix)) != 32 {
		t.Fatalf("deployment title does not contain a 128-bit identity: %q", second)
	}
}

func TestApplyLeavesPausedSourceStoppedWhenGuardedDeploymentCannotBeQuiesced(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  web:\n    image: example/api:latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]Project{{ProjectID: "p1", Name: "api", Environments: []ProjectEnvironment{{EnvironmentID: "env1", Name: "production"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "c1", Name: "api", AppName: "stack-1"}}, Total: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			http.Error(w, "deployment status unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{
		patchAt: 100,
		fakeDockerRunner: fakeDockerRunner{
			outputs: map[string][]byte{
				"ps --format {{.Names}}":           []byte("dokploy-postgres\n"),
				"image inspect example/api:latest": []byte("[]"),
				"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": {},
				"inspect --type container source-id":                                       []byte(`[{"Id":"source-id","Name":"/source","State":{"Running":false,"Status":"exited"}}]`),
				"start source-id":                                                          []byte("source-id\n"),
			},
			runOutputs: map[string][]byte{},
		},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	app := preparer.AppPlan{Name: "api", Directory: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "source-id"}}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api", ComposePath: "compose.yaml"}}}
	paused := &applyContext{plan: Plan{RunDir: bundleDir}, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "source-id", Stopped: true}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	err := client.Apply(context.Background(), Plan{
		ResumeFrom: 3,
		RunDir:     bundleDir,
		Steps: []Step{
			{Kind: StepCreateProject, App: "api", Ref: "api"},
			{Kind: StepCreateService, App: "api", Ref: "api"},
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepActivateRoutes, App: "api", Ref: "routes"},
		},
		Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}},
	})
	if err == nil || !strings.Contains(err.Error(), "paused source applications remain stopped") || !strings.Contains(err.Error(), "bort status") {
		t.Fatalf("expected fail-closed guarded deployment error, got %v", err)
	}
	if !mutationResponseMayHaveSucceeded(err) {
		t.Fatalf("accepted deployment with unproven quiescence was not marked mutation-ambiguous: %v", err)
	}
	if fakeOutputCalled(&runner.fakeDockerRunner, "start", "source-id") {
		t.Fatalf("unsafe recovery restarted the paused source, calls=%#v", runner.outputArgs)
	}
}

func TestComposeUpdatePatchGuardRechecksAfterHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/compose.update" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "mutation failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{fakeDockerRunner: fakeDockerRunner{
		outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	err := client.updateComposeWithPatchGuard(context.Background(), "compose-api", "services: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), "mutation failed") || !strings.Contains(err.Error(), "appeared while Bort was changing compose") {
		t.Fatalf("expected HTTP mutation and post-mutation patch errors, got %v", err)
	}
	if !mutationResponseMayHaveSucceeded(err) {
		t.Fatalf("HTTP 500 compose update was not marked mutation-ambiguous: %v", err)
	}
	if runner.checks != 2 {
		t.Fatalf("patch checks = %d, want 2", runner.checks)
	}
}

func TestComposeUpdateTransportFailureIsMutationAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/compose.update" {
			http.NotFound(w, r)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer server.Close()
	runner := &stagedBortPatchRunner{patchAt: 100, fakeDockerRunner: fakeDockerRunner{
		outputs:    map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
		runOutputs: map[string][]byte{},
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	err := client.updateComposeWithPatchGuard(context.Background(), "compose-api", "services: {}\n", "")
	if err == nil || !mutationResponseMayHaveSucceeded(err) {
		t.Fatalf("expected ambiguous compose update response, got %v", err)
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
	if mutationResponseMayHaveSucceeded(err) {
		t.Fatalf("pre-mutation active patch rejection was marked ambiguous: %v", err)
	}
}

func TestApplyProgressMarksPreMutationFailureRetryable(t *testing.T) {
	var terminal StepProgress
	onProgress := func(progress StepProgress) {
		if progress.Status == StepStatusError {
			terminal = progress
		}
	}
	client := &Client{Docker: &fakeDockerRunner{}}
	err := client.Apply(context.Background(), Plan{
		Steps:      []Step{{Kind: StepPushImage, App: "api", Ref: "api"}},
		OnProgress: &onProgress,
	})
	if err == nil || !strings.Contains(err.Error(), "missing composeId") {
		t.Fatalf("expected pre-mutation validation failure, got %v", err)
	}
	if terminal.Status != StepStatusError || terminal.MutationAmbiguous {
		t.Fatalf("pre-mutation progress was not explicitly retryable: %#v", terminal)
	}
}

func TestApplyActivateRoutesChecksPatchBeforeDomainMutation(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  web:\n    image: example/web\n    expose:\n      - \"8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	domainRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/domain.") {
			domainRequests++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"image inspect example/web": []byte(`[{}]`),
			"ps --format {{.Names}}":    []byte("dokploy-postgres\n"),
		},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": []byte(`{"patchId":"bort-api-compose","filePath":"docker|compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n"),
		},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	app := preparer.AppPlan{Name: "api", Directory: "api", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}}
	actx := &applyContext{
		plan: Plan{
			Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}},
			Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com", ServiceName: "web", Port: "8080"}}}}},
		},
		cache: map[string]*appCache{"api": {ComposeID: "compose-api"}},
	}

	err := client.applyActivateRoutes(context.Background(), actx, Step{Kind: StepActivateRoutes, App: "api", Ref: "routes"})
	if err == nil || !strings.Contains(err.Error(), "active Bort-owned Dokploy patch") {
		t.Fatalf("expected active patch to block route activation, got %v", err)
	}
	if domainRequests != 0 {
		t.Fatalf("active patch allowed %d domain request(s) before rejection", domainRequests)
	}
}

func TestPlannedInstallGatewayChecksPatchBeforeDomainMutation(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  web:\n    image: example/web\n    expose:\n      - \"8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{Name: "api", Directory: "api", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api", ComposePath: "compose.yaml"}}}}
	cutover := gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com", ServiceName: "web", Port: "8080"}}}}}
	plan := PlanFromArtifacts(preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}, syncplan.Result{}, cutover)
	var firstRouteMutation Step
	for _, step := range plan.Steps {
		if step.Kind == StepInstallGateway || step.Kind == StepActivateRoutes {
			firstRouteMutation = step
			break
		}
	}
	if firstRouteMutation.Kind != StepInstallGateway {
		t.Fatalf("first planned route mutation = %s, want %s", firstRouteMutation.Kind, StepInstallGateway)
	}
	domainRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/domain.") {
			domainRequests++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{"ps --format {{.Names}}": []byte("dokploy-postgres\n")},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": []byte(`{"patchId":"bort-api-compose","filePath":"docker|compose.yml","composeName":"api","sourceType":"github","repository":"owner/repo","branch":"main"}` + "\n"),
		},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{"api": {ComposeID: "compose-api"}}}

	err := client.applyStep(context.Background(), actx, firstRouteMutation)
	if err == nil || !strings.Contains(err.Error(), "active Bort-owned Dokploy patch") {
		t.Fatalf("expected planned install_gateway patch rejection, got %v", err)
	}
	if domainRequests != 0 {
		t.Fatalf("planned install_gateway made %d domain request(s) before rejection", domainRequests)
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
	const query = "ps --filter label=com.docker.swarm.service.name=dokploy-postgres --filter status=running --format {{.ID}}"
	client := &Client{Docker: &fakeDockerRunner{outputErrs: map[string]error{query: errors.New("docker unavailable")}}}
	err := client.validateNoActiveBortOverrides(context.Background(), "compose-api")
	if err == nil || !strings.Contains(err.Error(), "active patch inspection") || !strings.Contains(err.Error(), "docker unavailable") {
		t.Fatalf("expected active patch inspection to fail closed, got %v", err)
	}
}

func TestActivePatchGuardFailsClosedWhenDokployDatabaseIsUnavailable(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps --format {{.Names}}": []byte("unrelated\n"),
	}}
	client := &Client{Docker: runner}

	if err := client.validateNoActiveBortOverrides(context.Background(), "compose-api"); err == nil || !strings.Contains(err.Error(), "has 0 running dokploy-postgres service containers") {
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

func TestSearchComposeSelectsOnlyExactName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/compose.search" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{
			{ComposeID: "near", Name: "api-old"},
			{ComposeID: "exact", Name: "api"},
		}, Total: 2})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}

	compose, err := client.SearchCompose(context.Background(), "api", "env1")
	if err != nil {
		t.Fatalf("SearchCompose: %v", err)
	}
	if compose == nil || compose.ComposeID != "exact" {
		t.Fatalf("expected only exact-name compose, got %#v", compose)
	}
}

func TestSearchComposeConsumesEveryCandidatePage(t *testing.T) {
	items := make([]Compose, 100)
	for i := range items {
		items[i] = Compose{ComposeID: fmt.Sprintf("near-%d", i), Name: fmt.Sprintf("api-%d", i)}
	}
	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/compose.search" {
			http.NotFound(w, r)
			return
		}
		offsets = append(offsets, r.URL.Query().Get("offset"))
		if r.URL.Query().Get("offset") == "100" {
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "exact", Name: "api"}}, Total: 101})
			return
		}
		_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: items, Total: 101})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}

	compose, err := client.SearchCompose(context.Background(), "api", "env1")
	if err != nil {
		t.Fatalf("SearchCompose: %v", err)
	}
	if compose == nil || compose.ComposeID != "exact" || !slices.Equal(offsets, []string{"0", "100"}) {
		t.Fatalf("expected exact compose from second page, got compose=%#v offsets=%v", compose, offsets)
	}
}

func TestSearchComposeUsesOneDeadlineAcrossPages(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := requests.Add(1)
		time.Sleep(70 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: fmt.Sprintf("near-%d", request), Name: "api-old"}}, Total: 2})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: &http.Client{Timeout: 100 * time.Millisecond}}

	_, err := client.SearchCompose(context.Background(), "api", "env1")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected operation-level search timeout, got %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("search made %d requests before its operation deadline, want 2", got)
	}
}

func TestSearchComposeRefusesDuplicateExactNames(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/compose.search" {
			http.NotFound(w, r)
			return
		}
		requests++
		_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{
			{ComposeID: "first", Name: "api"},
			{ComposeID: "second", Name: "api"},
		}, Total: 200})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}

	if _, err := client.SearchCompose(context.Background(), "api", "env1"); err == nil || !strings.Contains(err.Error(), `multiple Dokploy compose apps named "api"`) {
		t.Fatalf("expected duplicate exact-name refusal, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("duplicate exact names fetched unnecessary pages: requests=%d", requests)
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

func TestApplyCreateServiceRefreshesMissingComposeAppName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "c1", Name: "api"}}, Total: 1})
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", Name: "api", AppName: "stack-api"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	bundleDir := t.TempDir()
	composePath := filepath.Join(bundleDir, "api", "compose.yaml")
	if err := os.MkdirAll(filepath.Dir(composePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{
		Name:      "api",
		Directory: "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			ComposeApp: preparer.DokployComposeApp{Name: "api", ComposePath: "compose.yaml"},
		}},
	}
	actx := &applyContext{
		plan:  Plan{Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}},
		cache: map[string]*appCache{"api": {EnvironmentID: "env1"}},
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	if err := client.applyCreateService(context.Background(), actx, Step{Kind: StepCreateService, App: "api", Ref: "api"}); err != nil {
		t.Fatal(err)
	}
	entry := actx.entry("api")
	if entry.ComposeID != "c1" || entry.ComposeAppName != "stack-api" {
		t.Fatalf("compose identity was not refreshed before create completed: %#v", entry)
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

func TestDomainMutationTransportFailuresAreAmbiguous(t *testing.T) {
	for _, test := range []struct {
		name          string
		existing      []Domain
		failedPath    string
		wantAmbiguous bool
	}{
		{name: "lookup", failedPath: "/api/domain.byComposeId"},
		{name: "create", failedPath: "/api/domain.create", wantAmbiguous: true},
		{name: "update", existing: []Domain{{DomainID: "domain-1", Host: "api.example.com", ServiceName: "old", Port: 3000}}, failedPath: "/api/domain.update", wantAmbiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == test.failedPath {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Fatal(err)
					}
					_ = conn.Close()
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
					_ = json.NewEncoder(w).Encode(test.existing)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
			_, err := client.CreateDomain(context.Background(), CreateDomainRequest{
				Host: "api.example.com", ComposeID: "compose-1", ServiceName: "web", Port: 8080,
			})
			if err == nil {
				t.Fatal("expected transport failure")
			}
			if got := mutationResponseMayHaveSucceeded(err); got != test.wantAmbiguous {
				t.Fatalf("mutation ambiguity = %t, want %t: %v", got, test.wantAmbiguous, err)
			}
		})
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
	var deploymentTitle string
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
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode compose.deploy: %v", err)
			}
			deploymentTitle = request.Title
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", AppName: "stack-1", Deployments: []Deployment{{Title: deploymentTitle, Status: "done"}}})
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
	var deploymentTitle string
	deploys := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			deploys++
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode compose.deploy: %v", err)
			}
			deploymentTitle = request.Title
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", AppName: "stack-1", Deployments: []Deployment{{Title: deploymentTitle, Status: "done"}}})
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

func TestResolveRouteForComposeExpandsServiceAliases(t *testing.T) {
	for name, compose := range map[string]string{
		"direct alias": "x-services: &app-services\n  web:\n    image: example/api\n    expose:\n      - \"8080\"\nservices: *app-services\n",
		"merge key":    "x-services: &app-services\n  web:\n    image: example/api\n    expose:\n      - \"8080\"\nservices:\n  <<: *app-services\n",
	} {
		t.Run(name, func(t *testing.T) {
			route, err := resolveRouteForCompose(gateway.Route{Host: "api.example.com", ServiceName: "web", Port: "8080"}, compose, nil, preparer.ComposeSourceRaw)
			if err != nil || route.ServiceName != "web" {
				t.Fatalf("aliased Compose service was not resolved at apply time: route=%#v err=%v", route, err)
			}
		})
	}
}

func TestComposeServiceSummariesExpandsSharedAliasChainsWithoutExponentialCost(t *testing.T) {
	var compose strings.Builder
	compose.WriteString("x-base: &level0\n  image: example/api\n  expose:\n    - \"8080\"\n")
	for i := 1; i < 30; i++ {
		fmt.Fprintf(&compose, "x-level%d: &level%d\n  <<: *level%d\n  back: *level%d\n", i, i, i-1, i-1)
	}
	compose.WriteString("services:\n  api:\n    <<: *level29\n")
	summaries, err := composeServiceSummaries(compose.String())
	if err != nil {
		t.Fatalf("composeServiceSummaries: %v", err)
	}
	summary, ok := summaries["api"]
	if !ok {
		t.Fatalf("expected an api service summary, got %#v", summaries)
	}
	if _, ok := summary.Ports["8080"]; !ok {
		t.Fatalf("port exposed through 30 levels of shared anchors was lost: %#v", summary.Ports)
	}
}
