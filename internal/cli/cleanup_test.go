package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestLifecycleAcceptanceTraceWithFakeDokploy(t *testing.T) {
	composeCreated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]dokploy.Project{{
				ProjectID: "project-api",
				Name:      "api",
				Environments: []dokploy.ProjectEnvironment{{
					EnvironmentID: "environment-production",
					Name:          "production",
				}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			items := []map[string]string{}
			if composeCreated {
				items = append(items, map[string]string{"composeId": "compose-api", "name": "api", "appName": "compose-api", "environmentId": "environment-production"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.create":
			composeCreated = true
			_ = json.NewEncoder(w).Encode(dokploy.Compose{ComposeID: "compose-api", Name: "api", AppName: "compose-api", EnvironmentID: "environment-production"})
		case r.Method == http.MethodPost && (r.URL.Path == "/api/compose.update" || r.URL.Path == "/api/compose.deploy"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "test-token")

	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerPath := filepath.Join(binDir, "docker")
	dockerStub := "#!/bin/sh\nif [ \"$1\" = inspect ]; then\n  echo 'Error: No such object' >&2\n  exit 1\nfi\nif [ \"$1\" = ps ]; then\n  echo dokploy-postgres\nfi\nexit 0\n"
	if err := os.WriteFile(dockerPath, []byte(dockerStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	manifestPath := filepath.Join(workDir, "manifest.json")
	if err := writeJSONArtifact(manifestPath, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Services: []manifest.Service{{
				ID:    "source-api",
				Name:  "api",
				Image: "example/api:latest",
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	runCommand(t, runMigrate, []string{"--manifest", manifestPath, "--run", "acceptance", "--observation-window", "0", "--rollback-window", "0"})
	planned, err := loadMigrationRun("acceptance")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.ToSlash(planned.Run.BundleDir), ".bort/runs/acceptance/bundle") {
		t.Fatalf("expected a self-contained bundle, got %s", planned.Run.BundleDir)
	}
	for _, decision := range openSetupDecisions(planned) {
		if err := recordReviewDecision(planned, decision, planned.Run.UpdatedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	runCommand(t, runMigrate, []string{"--live", "--run", "acceptance"})
	live, err := loadMigrationRun("acceptance")
	if err != nil {
		t.Fatal(err)
	}
	if live.Run.LiveAppliedAt == nil || requireLiveApplySucceeded(live) != nil {
		t.Fatalf("expected completed live lifecycle, metadata=%#v applied=%#v", live.Run, live.Applied)
	}
	var cockpit bytes.Buffer
	writeAppFirstCockpit(&cockpit, live)
	if !strings.Contains(cockpit.String(), "TARGET LIVE") {
		t.Fatalf("expected target-live cockpit, got:\n%s", cockpit.String())
	}

	runCommand(t, runCommit, []string{"--apply", "--run", "acceptance"})
	committed, err := loadMigrationRun("acceptance")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Run.CommittedAt == nil {
		t.Fatal("expected committed lifecycle timestamp")
	}
	cockpit.Reset()
	writeAppFirstCockpit(&cockpit, committed)
	if !strings.Contains(cockpit.String(), "COMMITTED") {
		t.Fatalf("expected committed cockpit, got:\n%s", cockpit.String())
	}

	var cleanupOut bytes.Buffer
	if err := runCleanup(context.Background(), []string{"--run", "acceptance"}, &cleanupOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cleanupOut.String(), "Source containers inventoried for later purge") {
		t.Fatalf("expected cleanup inventory, got:\n%s", cleanupOut.String())
	}
	var purgeOut bytes.Buffer
	if err := runCleanup(context.Background(), []string{"purge", "--run", "acceptance", "--all-apps"}, &purgeOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(purgeOut.String(), "Source containers scheduled for purge") {
		t.Fatalf("expected purge inventory, got:\n%s", purgeOut.String())
	}
	if err := runCleanup(context.Background(), []string{"purge", "--run", "acceptance", "--apply", "--all-apps", "--confirm", "purge acceptance"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	completed, err := loadMigrationRun("acceptance")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.PurgedAt == nil {
		t.Fatal("expected full purge to mark the lifecycle complete")
	}
	cockpit.Reset()
	writeAppFirstCockpit(&cockpit, completed)
	if !strings.Contains(cockpit.String(), "COMPLETE") {
		t.Fatalf("expected complete cockpit, got:\n%s", cockpit.String())
	}
}

func TestRunCleanupInventoriesLeftoversAndSafeMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.all":
			_ = json.NewEncoder(w).Encode([]dokploy.Project{{ProjectID: "p-proxy", Name: "proxy"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/project.one":
			if r.URL.Query().Get("projectId") != "p-proxy" {
				t.Fatalf("unexpected project.one query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(dokploy.Project{ProjectID: "p-proxy", Name: "proxy"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/domain.byComposeId":
			t.Fatalf("did not expect domain lookup for empty project")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")

	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Git: &manifest.GitSource{
				Repository: "https://github.com/example/api",
				Branch:     "main",
				Provider:   "github",
				SourceType: "App\\Models\\GithubApp",
				SourceID:   "42",
			},
			Services: []manifest.Service{{
				ID:       "cid123",
				Name:     "web",
				Image:    "example/api:latest",
				Mounts:   []manifest.Mount{{Type: "volume", Name: "api-data", Target: "/data"}},
				Networks: []manifest.ServiceNetwork{{Name: "api-net"}},
			}},
			Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "web"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "cleanup-run", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCleanup(context.Background(), []string{"--run", "cleanup-run"}, &stdout, &stderr); err != nil {
		t.Fatalf("cleanup failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Cleanup plan: .bort/runs/cleanup-run -> dokploy",
		"[present] proxy (p-proxy): safe metadata cleanup candidate: empty project with zero domains visible",
		"[absent] source: no Dokploy project with this stale platform name is visible",
		"Source containers inventoried for later purge:",
		"api/web web",
		"Source-control credentials left untouched:",
		"api https://github.com/example/api (github/coolify_github_app)",
		"Source volumes and bind mounts preserved:",
		"api api-data -> /data (volume)",
		"Source networks preserved:",
		"api api-net",
		"Target artifacts kept by default:",
		"Dry run only: run `bort cleanup --apply --run cleanup-run`",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected cleanup output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Source containers, volumes, networks, and target apps were removed") {
		t.Fatalf("cleanup dry run claimed destructive removal:\n%s", output)
	}
}

func TestRunCleanupPurgeInventoriesDestructiveSourceResources(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Git: &manifest.GitSource{
				Repository: "https://github.com/example/api",
				Branch:     "main",
				Provider:   "github",
				SourceType: "App\\Models\\GithubApp",
				SourceID:   "42",
			},
			Services: []manifest.Service{{
				ID:    "cid123",
				Name:  "web",
				Image: "example/api:latest",
				Mounts: []manifest.Mount{
					{Type: "volume", Name: "api-data", Target: "/data"},
					{Type: "bind", Source: "/data/coolify/applications/app-1/storage", Target: "/storage"},
				},
				Networks: []manifest.ServiceNetwork{{Name: "api-net"}},
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "purge-run", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--source-dir", "/data/coolify/services/service-1"}, &stdout, &stderr); err != nil {
		t.Fatalf("cleanup purge failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Cleanup purge plan: .bort/runs/purge-run -> dokploy",
		"Mode: dry run",
		"Source containers that must be absent before apply:",
		"api/web web",
		"Named source volumes that must be absent before apply:",
		"api api-data -> /data",
		"Host source paths that must be absent before apply:",
		"api /data/coolify/applications/app-1/storage (bind_mount)",
		"explicit /data/coolify/services/service-1 (explicit)",
		"Source networks that must be absent before apply:",
		"api api-net",
		"Source-control credentials left untouched:",
		"Bort will not automatically delete any selected resource in this scope",
		"Dry run only: rerun with an explicit --app, --project, or --all-apps scope before applying a purge.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected cleanup purge output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Applied destructive source purge") {
		t.Fatalf("cleanup purge dry run claimed destructive removal:\n%s", output)
	}
}

func TestRunCleanupPurgeApplyRequiresExplicitScopeAndConfirmation(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerPath := filepath.Join(binDir, "docker")
	dockerStub := "#!/bin/sh\nif [ \"$1\" = inspect ]; then\n  echo 'Error: No such object' >&2\n  exit 1\nfi\nif [ \"$1\" = volume ] && [ \"$2\" = inspect ]; then\n  echo 'Error response from daemon: no such volume' >&2\n  exit 1\nfi\nexit 0\n"
	if err := os.WriteFile(dockerPath, []byte(dockerStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Services: []manifest.Service{{
				ID:     "cid123",
				Name:   "web",
				Image:  "example/api:latest",
				Mounts: []manifest.Mount{{Type: "volume", Name: "api-data", Target: "/data"}},
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "purge-run", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--apply", "--confirm", "purge purge-run"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "requires an explicit scope") {
		t.Fatalf("expected explicit scope error, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	err = runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--apply", "--app", "api"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "before a successful live apply") {
		t.Fatalf("expected lifecycle eligibility before confirmation, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	run, err := loadMigrationRun("purge-run")
	if err != nil {
		t.Fatal(err)
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	applied := newRunApplied(run.Run)
	succeededAt := time.Now().UTC()
	applied.SucceededAt = &succeededAt
	for index, step := range steps {
		applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK)})
	}
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	err = runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--apply", "--app", "api"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "before `bort commit --apply --run purge-run`") {
		t.Fatalf("expected target acceptance before confirmation, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}

	operationLock, err := acquireRunOperationLock(run.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	markErr := markRunCommittedLocked(run.Run)
	operationLock.Release()
	if markErr != nil {
		t.Fatal(markErr)
	}
	stdout.Reset()
	stderr.Reset()
	err = runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--apply", "--app", "api"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--confirm \"purge purge-run\"") {
		t.Fatalf("expected confirmation after lifecycle eligibility, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	err = runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--apply", "--app", "api", "--confirm", "wrong"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "confirmation mismatch") {
		t.Fatalf("expected confirmation mismatch after lifecycle eligibility, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestRunCleanupRejectsMisplacedPurgeSubcommand(t *testing.T) {
	err := runCleanup(context.Background(), []string{"--run", "prod", "--apply", "purge", "--all-apps"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "purge must immediately follow cleanup") {
		t.Fatalf("expected misplaced purge to be rejected before metadata apply, got %v", err)
	}
}

func TestRunCleanupPurgePreservesResourcesSharedWithUnselectedApps(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{{
					ID:   "api-id",
					Name: "api-web",
					Mounts: []manifest.Mount{
						{Type: "volume", Name: "shared-data", Target: "/data"},
						{Type: "bind", Source: "/data/coolify/applications/shared/storage", Target: "/storage"},
					},
					Networks: []manifest.ServiceNetwork{{Name: "shared-net"}, {Name: "coolify"}},
				}},
			},
			{
				Name: "worker",
				Services: []manifest.Service{{
					ID:   "api-id",
					Name: "api-web",
					Mounts: []manifest.Mount{
						{Type: "volume", Name: "shared-data", Target: "/data"},
						{Type: "bind", Source: "/data/coolify/applications/shared/storage", Target: "/storage"},
					},
					Networks: []manifest.ServiceNetwork{{Name: "shared-net"}},
				}},
			},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "purge-run", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCleanup(context.Background(), []string{"purge", "--run", "purge-run", "--app", "api"}, &stdout, &stderr); err != nil {
		t.Fatalf("cleanup purge failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"source container api-web for api is also referenced by unselected app(s): worker",
		"named volume shared-data for api is also referenced by unselected app(s): worker",
		"bind mount source /data/coolify/applications/shared/storage for api is also referenced by unselected app(s): worker",
		"source network shared-net for api is also referenced by unselected app(s): worker",
		"source network coolify for api is a platform network and requires --include-platform for purge",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected cleanup purge output to contain %q, got:\n%s", want, output)
		}
	}
	for _, notWant := range []string{
		"Source containers scheduled for purge:",
		"Named source volumes that must be absent before apply:",
		"Host source paths that must be absent before apply:",
		"Source networks scheduled for purge:",
	} {
		if strings.Contains(output, notWant) {
			t.Fatalf("did not expect shared resource section %q, got:\n%s", notWant, output)
		}
	}
}

func TestCleanupPurgeFailsClosedWhenTopologyIsUnavailable(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{ID: "api-id", Name: "api"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "purge-run"})
	run, err := loadMigrationRun("purge-run")
	if err != nil {
		t.Fatal(err)
	}
	topologyPath := filepath.Join(run.Prepare.BundleDir, filepath.FromSlash(run.Prepare.Apps[0].Directory), "topology.json")
	topology, err := os.ReadFile(topologyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topologyPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = planCleanupPurge(run, "dokploy", cleanupPurgeFilters{AllApps: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "inspect source networks for api before purge") {
		t.Fatalf("expected malformed topology to block purge planning, got %v", err)
	}

	result := planCleanup(context.Background(), run, "dokploy")
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "inspect source networks for api") {
		t.Fatalf("expected read-only cleanup to report the topology warning, got %#v", result.Warnings)
	}

	if err := os.WriteFile(topologyPath, topology, 0o600); err != nil {
		t.Fatal(err)
	}
	run.Prepare.Apps[0].Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "api", ContainerName: "api"},
		{ServiceName: "worker", ContainerID: "worker-id", ContainerName: "worker"},
	}
	incomplete, err := planCleanupPurge(run, "dokploy", cleanupPurgeFilters{AllApps: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.CompletesLifecycle || !strings.Contains(strings.Join(incomplete.Warnings, "\n"), "has no stable container ID") || len(incomplete.SourceContainers) != 1 || incomplete.SourceContainers[0].ContainerID != "worker-id" {
		t.Fatalf("expected unresolved source coordinates to keep lifecycle incomplete, got %#v", incomplete)
	}
}

func TestCleanupPurgeAllAppsCompletenessRequiresIncludedPlatformApps(t *testing.T) {
	run := loadedMigrationRun{Prepare: preparer.Result{Apps: []preparer.AppPlan{
		{Name: "api"},
		{Name: "proxy", Role: "platform"},
	}}}
	withoutPlatform := cleanupPurgeFilters{AllApps: true}
	if cleanupPurgeCoversAllRunApps(run, withoutPlatform) {
		t.Fatal("expected excluded platform apps to keep the lifecycle incomplete")
	}
	selected, err := cleanupPurgeSelectedApps(run, withoutPlatform)
	if err != nil || len(selected) != 1 || selected[0].Name != "api" {
		t.Fatalf("unexpected --all-apps selection without platform apps: selected=%#v err=%v", selected, err)
	}

	withPlatform := cleanupPurgeFilters{AllApps: true, IncludePlatform: true}
	if !cleanupPurgeCoversAllRunApps(run, withPlatform) {
		t.Fatal("expected --all-apps --include-platform to cover every run app")
	}
	selected, err = cleanupPurgeSelectedApps(run, withPlatform)
	if err != nil || len(selected) != 2 {
		t.Fatalf("unexpected --all-apps --include-platform selection: selected=%#v err=%v", selected, err)
	}
	if cleanupPurgeCoversAllRunApps(run, cleanupPurgeFilters{IncludePlatform: true}) {
		t.Fatal("expected explicit app scope never to mark a full purge")
	}
}

func TestCleanupPurgeManualCompletionDoesNotRecordLifecycleEligibility(t *testing.T) {
	result := cleanupPurgeResult{
		CompletesLifecycle: true,
		SourceContainers:   []cleanupSourceContainer{{ContainerID: "cid123"}},
		SourceVolumes:      []cleanupSourceVolume{{Type: "volume", Name: "api-data", Action: "require_absent_before_purge_apply"}},
	}
	setCleanupPurgeManualCompletion(&result)
	if !result.ManualCompletion || result.CompletesLifecycle {
		t.Fatalf("expected verification-only purge to remain ineligible for lifecycle completion, got %#v", result)
	}
}

func TestCleanupContainerOwnersMatchReviewedShortAndCanonicalIDs(t *testing.T) {
	shortID := "123456789abc"
	fullID := shortID + strings.Repeat("d", 52)
	owners := cleanupPurgeOwners{ContainerIDs: map[string][]string{fullID: {"worker"}}}
	outside := cleanupContainerOwnersOutsideSelected(owners, cleanupSourceContainer{ContainerID: shortID}, map[string]struct{}{"api": {}})
	if len(outside) != 1 || outside[0] != "worker" {
		t.Fatalf("expected canonical ID owner to keep reviewed short-ID container shared, got %#v", outside)
	}
}

func TestCleanupPurgePlatformNetworkRequiresExplicitInclusion(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name: "api",
			Services: []manifest.Service{{
				ID:       "api-id",
				Name:     "api",
				Networks: []manifest.ServiceNetwork{{Name: "coolify"}},
			}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "platform-network"})
	run, err := loadMigrationRun("platform-network")
	if err != nil {
		t.Fatal(err)
	}

	withoutPlatform, err := planCleanupPurge(run, "dokploy", cleanupPurgeFilters{AllApps: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withoutPlatform.CompletesLifecycle || len(withoutPlatform.SourceNetworks) != 0 {
		t.Fatalf("expected platform network exclusion to keep lifecycle incomplete, got %#v", withoutPlatform)
	}
	withPlatform, err := planCleanupPurge(run, "dokploy", cleanupPurgeFilters{AllApps: true, IncludePlatform: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !withPlatform.CompletesLifecycle || len(withPlatform.SourceNetworks) != 1 || withPlatform.SourceNetworks[0].Name != "coolify" {
		t.Fatalf("expected included platform network to be scheduled, got %#v", withPlatform)
	}
}

func TestCleanupPurgeProjectFilterDoesNotAliasAppName(t *testing.T) {
	app := preparer.AppPlan{
		Name:         "api",
		ProjectGroup: &preparer.ProjectGroup{Name: "source-project"},
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			Project: preparer.DokployProject{Name: "target-project"},
		}},
	}
	if cleanupPurgeProjectMatches(app, "api") {
		t.Fatal("expected --project not to match an app name")
	}
	for _, project := range []string{"source-project", "target-project"} {
		if !cleanupPurgeProjectMatches(app, project) {
			t.Fatalf("expected --project to match %q", project)
		}
	}
}

func TestCleanupPurgeRejectsAmbiguousAppSelector(t *testing.T) {
	run := loadedMigrationRun{Prepare: preparer.Result{Apps: []preparer.AppPlan{
		{Name: "My App"},
		{Name: "my-app"},
	}}}
	_, err := cleanupPurgeSelectedApps(run, cleanupPurgeFilters{Apps: []string{"my-app"}})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "My App, my-app") {
		t.Fatalf("expected ambiguous app selector to fail closed, got %v", err)
	}
}

func TestCleanupPurgeIncompleteResourceCoordinatesBlockLifecycleCompletion(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web"}}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "files", Type: "volume", Target: "/data"}}
	if unresolved := cleanupUnresolvedContainersForApp(app); len(unresolved) != 2 {
		t.Fatalf("expected unresolved source service and volume coordinates, got %#v", unresolved)
	}
}

func TestCleanupPurgeContainerNameDoesNotInheritStableIDFromMatchingRecord(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "cid123", ContainerName: "api-web"}}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "files", SourceContainerName: "api-web"}}
	if unresolved := cleanupUnresolvedContainersForApp(app); len(unresolved) != 1 || unresolved[0] != "files" {
		t.Fatalf("expected name-only coordinate to remain unresolved, got %#v", unresolved)
	}
}

func TestApplyCleanupPurgeIdentitiesUsesIdentifiedContainers(t *testing.T) {
	result := cleanupPurgeResult{SourceContainers: []cleanupSourceContainer{
		{App: "api", Service: "web", ContainerID: "cid123", ContainerName: "api-web"},
		{App: "api", Service: "web", ContainerName: "api-web"},
	}}
	identified := dokploy.SourcePurgeOptions{Containers: []dokploy.SourcePurgeContainer{
		{App: "api", Service: "web", ContainerID: "cid123", ContainerName: "api-web"},
	}}
	applyCleanupPurgeIdentities(&result, identified)
	options := cleanupPurgeOptions(result)
	if len(result.SourceContainers) != 1 || len(options.Containers) != 1 || options.Containers[0].ContainerID != "cid123" {
		t.Fatalf("expected backup and execution to use the identified container, result=%#v options=%#v", result.SourceContainers, options.Containers)
	}
}

func TestCleanupStaleProjectNameCollisions(t *testing.T) {
	run := loadedMigrationRun{Prepare: preparer.Result{Apps: []preparer.AppPlan{
		{Name: "api", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{Project: preparer.DokployProject{Name: "proxy"}}}},
		{Name: "worker", TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{Project: preparer.DokployProject{Name: "app"}}}},
	}}}
	collisions := cleanupStaleProjectNameCollisions(run, []string{"coolify-proxy", "proxy", "source"})
	if len(collisions) != 1 || collisions[0] != "proxy" {
		t.Fatalf("unexpected collisions: %#v", collisions)
	}
}

func TestRunCleanupCommandsDoNotUseMtimeFallback(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "mtime-only", Services: []manifest.Service{{Name: "mtime-only", Image: "example/mtime:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "mtime-only"})
	if err := os.Remove(filepath.Join(workDir, ".bort", "state.json")); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{nil, {"purge"}} {
		if err := runCleanup(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no current migration run") {
			t.Fatalf("expected cleanup %v to reject an mtime-only run, got %v", args, err)
		}
	}
}

func TestCleanupContainersIncludeVolumeAndDataStoreSources(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "files", SourceContainerID: "volume-container", SourceContainerName: "files"}}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db", SourceContainerID: "database-container", SourceContainerName: "db"}}
	containers, err := cleanupContainersForApp(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 2 || containers[0].ContainerID != "volume-container" || containers[1].ContainerID != "database-container" {
		t.Fatalf("expected all source container coordinates in purge inventory, got %#v", containers)
	}
}

func TestCleanupContainersRejectConflictingCoordinatesForSameID(t *testing.T) {
	for _, test := range []struct {
		name   string
		second preparer.VolumeResource
		want   string
	}{
		{name: "name", second: preparer.VolumeResource{Service: "web", SourceContainerID: "cid123", SourceContainerName: "replacement"}, want: "conflicting names"},
		{name: "service", second: preparer.VolumeResource{Service: "worker", SourceContainerID: "cid123", SourceContainerName: "web"}, want: "conflicting services"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := preparer.AppPlan{Name: "api"}
			app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "cid123", ContainerName: "web"}}
			app.Resources.Volumes = []preparer.VolumeResource{test.second}
			if _, err := cleanupContainersForApp(app); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s conflict, got %v", test.name, err)
			}
		})
	}
}

func TestCleanupContainersDeduplicateReviewedShortAndCanonicalIDs(t *testing.T) {
	shortID := "123456789abc"
	fullID := shortID + strings.Repeat("d", 52)
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: shortID, ContainerName: "web"}}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", SourceContainerID: fullID, SourceContainerName: "web"}}
	containers, err := cleanupContainersForApp(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].ContainerID != fullID {
		t.Fatalf("expected one canonical container coordinate, got %#v", containers)
	}
}

func TestCleanupPathContainsAllowsDotDotPrefixedChildNames(t *testing.T) {
	parent := filepath.Join("data", "coolify", "applications", "api")
	if !cleanupPathContains(parent, filepath.Join(parent, "..data")) {
		t.Fatal("expected a child name beginning with two dots to remain inside its parent")
	}
	if cleanupPathContains(parent, filepath.Join(parent, "..", "other")) {
		t.Fatal("expected parent traversal to remain outside")
	}
}

func TestCleanupRecoveryCommandsPreserveExternalRunAndPurgeScope(t *testing.T) {
	externalRunDir := filepath.Join(t.TempDir(), "external-run")
	result := cleanupPurgeResult{
		RunName:   "external-run",
		RunDir:    externalRunDir,
		DryRun:    true,
		BackupDir: "/private/purge backups",
		Filters:   cleanupPurgeFilters{Apps: []string{"api"}, IncludePlatform: true},
		SourcePaths: []cleanupSourcePath{{
			Path:   "/data/coolify/services/service-1",
			Source: "explicit",
		}},
	}
	command := cleanupPurgeApplyCommand(result)
	for _, want := range []string{"cleanup purge --apply", "--app api", "--include-platform", "--backup-dir '/private/purge backups'", "--source-dir /data/coolify/services/service-1", "--confirm 'purge external-run'", "--run " + shellQuote(externalRunDir)} {
		if !strings.Contains(command, want) {
			t.Fatalf("expected purge recovery command to contain %q, got %q", want, command)
		}
	}
	if strings.Contains(command, "<app>") || strings.Contains(command, "--all-apps") {
		t.Fatalf("purge recovery command replaced its reviewed scope: %q", command)
	}

	var output strings.Builder
	writeCleanupText(&output, cleanupResult{RunName: "external-run", RunDir: externalRunDir, DryRun: true})
	if !strings.Contains(output.String(), "cleanup --apply --run "+shellQuote(externalRunDir)) {
		t.Fatalf("metadata cleanup recovery command lost external run path:\n%s", output.String())
	}
}

func TestWriteCleanupPurgeTextWarnsAfterPartialApply(t *testing.T) {
	result := cleanupPurgeResult{
		RunName: "partial",
		RunDir:  filepath.Join(".bort", "runs", "partial"),
		DryRun:  false,
		PurgeResult: &dokploy.SourcePurgeResult{Containers: []dokploy.SourcePurgeResourceResult{
			{Ref: "web", Status: "removed"},
			{Ref: "worker", Status: "error"},
		}},
	}
	var output strings.Builder
	writeCleanupPurgeText(&output, result)
	for _, want := range []string{"Mode: incomplete", "[removed] web", "[error] worker", "Earlier resources may already have been removed"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("expected partial purge output to contain %q, got:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "Dry run only") {
		t.Fatalf("partial purge was mislabeled as a dry run:\n%s", output.String())
	}
}

func TestWriteCleanupPurgeTextReportsIncompleteScopedApply(t *testing.T) {
	var output strings.Builder
	writeCleanupPurgeText(&output, cleanupPurgeResult{Applied: true, CompletesLifecycle: false})
	if !strings.Contains(output.String(), "migration lifecycle remains incomplete") {
		t.Fatalf("expected scoped apply to report incomplete lifecycle, got:\n%s", output.String())
	}
}

func TestWriteCleanupPurgeTextRequiresRecordedLifecycleCompletion(t *testing.T) {
	var output strings.Builder
	writeCleanupPurgeText(&output, cleanupPurgeResult{Applied: true, CompletesLifecycle: true})
	if strings.Contains(output.String(), "completed the migration lifecycle") || !strings.Contains(output.String(), "migration lifecycle remains incomplete") {
		t.Fatalf("expected unrecorded lifecycle completion to remain incomplete, got:\n%s", output.String())
	}
}

func TestCleanupPurgeApplyCommandRejectsUnscopedReview(t *testing.T) {
	if command := cleanupPurgeApplyCommand(cleanupPurgeResult{RunName: "unscoped"}); command != "" {
		t.Fatalf("expected no apply command for an unscoped review, got %q", command)
	}
}

func TestUpdateCleanupPurgeBackupPersistsPartialResults(t *testing.T) {
	backupDir := t.TempDir()
	result := cleanupPurgeResult{RunName: "partial", DryRun: false, PurgeResult: &dokploy.SourcePurgeResult{
		Containers: []dokploy.SourcePurgeResourceResult{{Ref: "web", Status: "removed"}},
	}}
	path, err := writeCleanupPurgeBackup(backupDir, cleanupPurgeResult{RunName: result.RunName, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	recorder := cleanupPurgeBackupRecorder{path: path, result: &result, persist: updateCleanupPurgeBackup}
	if err := recorder.Start(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record(*result.PurgeResult); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted cleanupPurgeResult
	if err := json.Unmarshal(contents, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.DryRun || persisted.ExecutionStartedAt == nil || persisted.PurgeResult == nil || len(persisted.PurgeResult.Containers) != 1 || persisted.PurgeResult.Containers[0].Status != "removed" {
		t.Fatalf("expected partial results in private backup, got %#v", persisted)
	}
}

func TestCleanupPurgeBackupRecorderKeepsPersistenceFailureSticky(t *testing.T) {
	result := cleanupPurgeResult{RunName: "partial"}
	writes := 0
	recorder := cleanupPurgeBackupRecorder{
		path:   filepath.Join(t.TempDir(), "purge.json"),
		result: &result,
		persist: func(string, cleanupPurgeResult) error {
			writes++
			return errors.New("disk unavailable")
		},
	}
	if err := recorder.Start(); err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("expected initial persistence failure, got %v", err)
	}
	if err := recorder.Record(dokploy.SourcePurgeResult{}); err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("expected sticky persistence failure, got %v", err)
	}
	if writes != 1 {
		t.Fatalf("expected persistence not to retry after failure, got %d writes", writes)
	}
}
