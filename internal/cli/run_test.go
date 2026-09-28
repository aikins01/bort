package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
	syncplan "github.com/aikins01/bort/internal/sync"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRunMigrateCreatesLocalRunArtifactsAndSummary(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker", DockerEngineID: "engine-reviewed"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{ID: "0123456789ab", Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000"}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate failed: %v\nstderr:\n%s", err, stderr.String())
	}

	runDir := filepath.Join(workDir, ".bort", "runs", "demo-app")
	for _, name := range []string{"run.json", "prepare.json", "sync.json", "cutover.json", "rollback.json", "commit.json", "decisions.json", "progress.json", "applied.json"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}

	run := readJSONFile[migrationRun](t, filepath.Join(runDir, "run.json"))
	if run.APIVersion != runAPIVersion || !run.DryRun || run.Name != "demo-app" || run.SourceBundleDir != bundleDir || run.Target != "dokploy" {
		t.Fatalf("unexpected run metadata: %#v", run)
	}
	if err := containedPath(runDir, run.BundleDir); err != nil {
		t.Fatalf("expected a self-contained reviewed bundle: %v", err)
	}
	if run.BundleDigest == "" {
		t.Fatal("expected the reviewed bundle digest to be recorded")
	}
	prepareResult := readJSONFile[preparer.Result](t, filepath.Join(runDir, "prepare.json"))
	syncResult := readJSONFile[syncplan.Result](t, filepath.Join(runDir, "sync.json"))
	cutoverResult := readJSONFile[gateway.Result](t, filepath.Join(runDir, "cutover.json"))
	rollbackResult := readJSONFile[rollbackplan.Result](t, filepath.Join(runDir, "rollback.json"))
	commitResult := readJSONFile[commitplan.Result](t, filepath.Join(runDir, "commit.json"))
	decisions := readJSONFile[runDecisions](t, filepath.Join(runDir, "decisions.json"))
	if prepareResult.APIVersion != preparer.APIVersion || syncResult.APIVersion != syncplan.APIVersion || cutoverResult.APIVersion != gateway.APIVersion || rollbackResult.APIVersion != rollbackplan.APIVersion || commitResult.APIVersion != commitplan.APIVersion {
		t.Fatalf("unexpected artifact api versions")
	}
	if !syncResult.DryRun || !cutoverResult.DryRun || !rollbackResult.DryRun || !commitResult.DryRun {
		t.Fatalf("expected all downstream artifacts to be dry-run")
	}
	if decisions.APIVersion != decisionsAPIVersion || !decisions.DryRun || len(decisions.Decisions) == 0 || decisions.Decisions[0].ID != "cutover" {
		t.Fatalf("unexpected decisions artifact: %#v", decisions)
	}
	loaded, err := loadMigrationRun("demo-app")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLiveApplyReady(loaded); err != nil {
		t.Fatalf("expected downstream review decision to remain visible without blocking live apply: %v", err)
	}
	if err := verifyReviewedMigrationBundle(loaded); err != nil {
		t.Fatalf("expected the recorded bundle digest to match: %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"Migration run created: .bort/runs/demo-app",
		"Overall: needs_decision (yellow)",
		"Apps: 1 total, 0 green, 1 yellow, 0 red",
		"Routes: 1 cutover, 1 rollback, 1 commit",
		"Decisions: 3 open",
		"Open decisions:",
		"Next safe step: create a new named migration run from a local Docker scan on this source host",
		"Dry run only: no target resources, sync operations, route changes, ownership commits, or source cleanup were executed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected migrate output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "migrate --live") {
		t.Fatalf("imported bundle run must not recommend live apply, got:\n%s", output)
	}
}

func TestRunMigrateLiveUsesCurrentRunInsteadOfDefaultBundle(t *testing.T) {
	t.Setenv("BORT_DOKPLOY_URL", "")
	t.Setenv("BORT_DOKPLOY_TOKEN", "")
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, ".bort", "runs", "coolify-local", "bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "coolify-local", "--observation-window", "0", "--rollback-window", "0"})
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "stale", Services: []manifest.Service{{Name: "stale", Image: "example/stale:latest"}}}},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runMigrate(context.Background(), []string{"--live"}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected live mode to stop at missing dokploy credentials")
	}
	if strings.Contains(stdout.String(), "stale") {
		t.Fatalf("expected current run to be used instead of stale default bundle, stdout=%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "no dokploy credentials available") {
		t.Fatalf("expected the reviewed run to reach target setup, got err=%v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	if !strings.Contains(err.Error(), "init-target dokploy --dokploy-url http://127.0.0.1:3030") || !strings.Contains(err.Error(), "replace 3030") {
		t.Fatalf("generated credential guidance omitted existing-Dokploy bootstrap: %v", err)
	}
	if !strings.Contains(err.Error(), "--auth-secret-backup /absolute/path/to/encrypted-or-off-host/dokploy-auth-secret") {
		t.Fatalf("generated install command omitted required authentication-secret escrow: %v", err)
	}
	if !strings.Contains(stdout.String(), "Migration run loaded: .bort/runs/coolify-local") {
		t.Fatalf("expected live migrate to load the current run, got stdout=%s", stdout.String())
	}
	if active, activeErr := runOperationActive("coolify-local"); activeErr != nil || active {
		t.Fatalf("live apply failure left the run operation lock held: active=%t err=%v", active, activeErr)
	}
	if _, loadErr := loadMigrationRun("coolify-local"); loadErr != nil {
		t.Fatalf("live apply failure left unreadable run metadata: %v", loadErr)
	}
}

func TestRunMigrateLivePreservesReviewedArtifactsAfterBundleChanges(t *testing.T) {
	t.Setenv("BORT_DOKPLOY_URL", "")
	t.Setenv("BORT_DOKPLOY_TOKEN", "")
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "reviewed",
			Services: []manifest.Service{{Name: "reviewed", Image: "example/reviewed:latest"}},
			Routes:   []manifest.Route{{Host: "reviewed.example.com", ServiceName: "reviewed", Port: "3000"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "reviewed-plan"})
	reviewed, err := loadMigrationRun("reviewed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Run.SourceBundleDir != bundleDir {
		t.Fatalf("expected source bundle %s, got %s", bundleDir, reviewed.Run.SourceBundleDir)
	}
	if err := containedPath(reviewed.Run.RunDir, reviewed.Run.BundleDir); err != nil {
		t.Fatalf("expected reviewed bundle inside the run directory: %v", err)
	}
	composePath := filepath.Join(reviewed.Run.BundleDir, filepath.FromSlash(reviewed.Prepare.Apps[0].Directory), reviewed.Prepare.Apps[0].Resources.App.ComposePath)
	composeBefore, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	preparePath := filepath.Join(workDir, ".bort", "runs", "reviewed-plan", "prepare.json")
	before, err := os.ReadFile(preparePath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "reviewed",
			Services: []manifest.Service{{Name: "reviewed", Image: "example/replacement:latest"}},
			Routes:   []manifest.Route{{Host: "reviewed.example.com", ServiceName: "reviewed", Port: "3000"}},
		}},
	})

	err = runMigrate(context.Background(), []string{"--live", "--run", "reviewed-plan"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no dokploy credentials available") {
		t.Fatalf("expected live apply to stop at missing dokploy credentials, got %v", err)
	}
	if !strings.Contains(err.Error(), "--auth-secret-backup /absolute/path/to/encrypted-or-off-host/dokploy-auth-secret") {
		t.Fatalf("generated install command omitted required authentication-secret escrow: %v", err)
	}
	after, err := os.ReadFile(preparePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("live apply rewrote the reviewed prepare artifact after the source bundle changed")
	}
	composeAfter, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(composeAfter, composeBefore) {
		t.Fatal("live apply changed the compose file in the self-contained reviewed bundle")
	}
}

func TestApplyLiveMigrationRejectsChangedReviewedBundle(t *testing.T) {
	for _, test := range []struct {
		name         string
		partialApply bool
		guidance     string
	}{
		{name: "before apply", guidance: "to re-plan before live apply"},
		{name: "partial apply resume", partialApply: true, guidance: "cannot be re-planned; reconcile any applied target changes, then create a new run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()
			t.Setenv(dokploy.EnvBaseURL, server.URL)
			t.Setenv(dokploy.EnvToken, "token")

			workDir := t.TempDir()
			t.Chdir(workDir)
			bundleDir := filepath.Join(workDir, "bort-bundle")
			writeTestBundle(t, bundleDir, manifest.Manifest{
				Source: manifest.Source{Platform: "docker"},
				Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:v1"}}}},
			})
			runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "changed-bundle"})
			run, err := loadMigrationRun("changed-bundle")
			if err != nil {
				t.Fatal(err)
			}
			if test.partialApply {
				steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
				if len(steps) == 0 {
					t.Fatal("expected live apply steps")
				}
				applied := newRunApplied(run.Run)
				applied.Steps = []appliedStep{{Index: 0, Kind: string(steps[0].Kind), App: steps[0].App, Ref: steps[0].Ref, Status: string(dokploy.StepStatusOK)}}
				if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), applied); err != nil {
					t.Fatal(err)
				}
				run, err = loadMigrationRun("changed-bundle")
				if err != nil {
					t.Fatal(err)
				}
			}
			composePath := filepath.Join(run.Run.BundleDir, filepath.FromSlash(run.Prepare.Apps[0].Directory), run.Prepare.Apps[0].Resources.App.ComposePath)
			compose, err := os.ReadFile(composePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(composePath, append(compose, []byte("\nchanged: true\n")...), 0o600); err != nil {
				t.Fatal(err)
			}

			err = applyLiveMigrationLocked(context.Background(), run, io.Discard, nil)
			if err == nil || !strings.Contains(err.Error(), "changed after planning") || !strings.Contains(err.Error(), test.guidance) {
				t.Fatalf("expected changed reviewed bundle to block live apply with valid recovery guidance, got %v", err)
			}
			if requests != 0 {
				t.Fatalf("changed reviewed bundle contacted dokploy %d time(s)", requests)
			}
		})
	}
}

func TestLegacySelfContainedRunWithoutBundleDigestIsUpgradedBeforeLiveApply(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "legacy-contained"})
	run, err := loadMigrationRun("legacy-contained")
	if err != nil {
		t.Fatal(err)
	}
	run.Run.BundleDigest = ""
	if err := writeJSONArtifact(filepath.Join(run.Run.RunDir, "run.json"), run.Run); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadMigrationRun("legacy-contained")
	if err != nil {
		t.Fatalf("legacy run without a bundle digest did not load: %v", err)
	}
	if loaded.Run.BundleDigest != "" {
		t.Fatalf("legacy run unexpectedly acquired a digest while loading: %q", loaded.Run.BundleDigest)
	}
	if err := verifyReviewedMigrationBundle(loaded); err == nil || !strings.Contains(err.Error(), "predates reviewed bundle digests") || !strings.Contains(err.Error(), "to re-plan before live apply") {
		t.Fatalf("legacy run without a bundle digest did not require a safe re-plan before live apply: %v", err)
	}
	originalBundleDir := loaded.Run.BundleDir
	originalArtifacts := loaded.Run.Artifacts
	operationLock, err := acquireRunOperationLock(loaded.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, upgradeErr := ensureSelfContainedLiveRunLocked(loaded)
	operationLock.Release()
	if upgradeErr != nil {
		t.Fatal(upgradeErr)
	}
	if upgraded.Run.BundleDigest == "" {
		t.Fatal("legacy self-contained run did not acquire a reviewed bundle digest")
	}
	if upgraded.Run.BundleDir != originalBundleDir {
		t.Fatalf("legacy self-contained bundle was replaced: before=%q after=%q", originalBundleDir, upgraded.Run.BundleDir)
	}
	if upgraded.Run.Artifacts.Prepare == originalArtifacts.Prepare {
		t.Fatalf("legacy digest upgrade did not publish a new artifact generation: %#v", upgraded.Run.Artifacts)
	}
	if err := verifyReviewedMigrationBundle(upgraded); err != nil {
		t.Fatalf("upgraded reviewed bundle did not verify: %v", err)
	}
	reloaded, err := loadMigrationRun("legacy-contained")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Run.BundleDigest != upgraded.Run.BundleDigest || reloaded.Run.Artifacts != upgraded.Run.Artifacts {
		t.Fatalf("legacy digest upgrade was not persisted: upgraded=%#v reloaded=%#v", upgraded.Run, reloaded.Run)
	}
}

func TestLegacySelfContainedAppliedRunRetainsResumeStateDuringDigestUpgrade(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "legacy-applied"})
	run, err := loadMigrationRun("legacy-applied")
	if err != nil {
		t.Fatal(err)
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	if len(steps) == 0 {
		t.Fatal("expected legacy live steps")
	}
	run.Run.BundleDigest = ""
	run.Applied.Steps = []appliedStep{{
		Index:  0,
		Kind:   string(steps[0].Kind),
		App:    steps[0].App,
		Ref:    steps[0].Ref,
		Status: string(dokploy.StepStatusOK),
	}}
	if err := writeRunApplied(runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied), run.Applied); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(filepath.Join(run.Run.RunDir, "run.json"), run.Run); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadMigrationRun("legacy-applied")
	if err != nil {
		t.Fatal(err)
	}
	operationLock, err := acquireRunOperationLock(loaded.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, upgradeErr := ensureSelfContainedLiveRunLocked(loaded)
	operationLock.Release()
	if upgradeErr != nil {
		t.Fatal(upgradeErr)
	}
	if upgraded.Run.BundleDigest == "" || len(upgraded.Applied.Steps) != 1 || upgraded.Applied.Steps[0] != run.Applied.Steps[0] {
		t.Fatalf("legacy digest upgrade lost applied resume state: run=%#v applied=%#v", upgraded.Run, upgraded.Applied)
	}
	if err := verifyReviewedMigrationBundle(upgraded); err != nil {
		t.Fatalf("upgraded applied run bundle did not verify: %v", err)
	}
	reloaded, err := loadMigrationRun("legacy-applied")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Run.BundleDigest != upgraded.Run.BundleDigest || len(reloaded.Applied.Steps) != 1 || reloaded.Applied.Steps[0] != run.Applied.Steps[0] {
		t.Fatalf("persisted legacy digest upgrade lost applied resume state: run=%#v applied=%#v", reloaded.Run, reloaded.Applied)
	}
}

func TestLoadMigrationRunDoesNotApplyLaterWorkspaceState(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "api",
			Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "reviewed-state"})
	run, err := loadMigrationRun("reviewed-state")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Prepare.Apps) != 1 || len(run.Prepare.Apps[0].Resources.DataStores) == 0 {
		t.Fatalf("expected a reviewed data-store requirement, got %#v", run.Prepare.Apps)
	}
	preparePath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Prepare)
	if err != nil {
		t.Fatal(err)
	}
	reviewed := readJSONFile[preparer.Result](t, preparePath)
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		*state = setAppDataStrategy(*state, "api", "postgres", dataStrategyMigrate)
		return true
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadMigrationRun("reviewed-state")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Prepare, reviewed) {
		t.Fatalf("workspace state changed the reviewed prepare artifact in memory: reviewed=%#v reloaded=%#v", reviewed, reloaded.Prepare)
	}
}

func TestValidateLiveApplyReadyBlocksEveryPrepareRequirement(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "selected-run", RunDir: t.TempDir()},
		Decisions: runDecisions{
			APIVersion: decisionsAPIVersion,
			Decisions: []runDecision{{
				ID:        "prepare-review",
				Kind:      "route_review",
				Readiness: preparer.ReadinessNeedsDecision,
				Action:    "review the prepared route",
				Items: []runDecisionItem{{
					Stage:     "prepare",
					Readiness: preparer.ReadinessNeedsDecision,
				}},
			}},
		},
	}
	if err := validateLiveApplyReady(run); err == nil || !strings.Contains(err.Error(), "live apply is blocked") || !strings.Contains(err.Error(), runScopedCommand(run, "status")) {
		t.Fatalf("expected prepare-stage needs_decision to block live apply with run-scoped review guidance, got %v", err)
	}

	reviewOnlyItems := []runDecisionItem{
		{Stage: "cutover", Code: "cutover.sync_verification_required"},
		{Stage: "cutover", Code: "cutover.health_check_required"},
		{Stage: "rollback", Code: "rollback.trigger_required"},
		{Stage: "rollback", Code: "rollback.source_health_required"},
		{Stage: "commit", Code: "commit.target_acceptance_required"},
		{Stage: "commit", Code: "commit.target_route_acceptance_required"},
		{Stage: "commit", Code: "commit.rollback_window_closed"},
	}
	for _, item := range reviewOnlyItems {
		item.Readiness = preparer.ReadinessNeedsDecision
		run.Decisions.Decisions[0].Items[0] = item
		if err := validateLiveApplyReady(run); err != nil {
			t.Fatalf("expected %s not to block live apply, got %v", item.Code, err)
		}
	}
	for _, stage := range []string{"cutover", "rollback", "commit"} {
		run.Decisions.Decisions[0].Items[0] = runDecisionItem{Stage: stage, Code: stage + ".future_requirement", Readiness: preparer.ReadinessNeedsDecision}
		if err := validateLiveApplyReady(run); err == nil {
			t.Fatalf("expected unknown %s-stage needs_decision to block live apply", stage)
		}
		if decisions := openDownstreamBlockingDecisions(run); len(decisions) != 1 {
			t.Fatalf("expected unknown %s-stage needs_decision to be surfaced as a downstream blocker, got %#v", stage, decisions)
		}
	}
	run.Decisions.Decisions[0].Items[0] = runDecisionItem{Code: "cutover.sync_verification_required", Readiness: preparer.ReadinessNeedsDecision}
	if err := validateLiveApplyReady(run); err == nil {
		t.Fatal("expected needs_decision with an unknown stage to block live apply")
	}
	if decisions := openReviewDecisions(run); len(decisions) != 0 {
		t.Fatalf("expected needs_decision with an unknown stage to stay out of review-only decisions, got %#v", decisions)
	}
	run.Decisions.Decisions[0].Items[0] = reviewOnlyItems[0]
	for _, readiness := range []preparer.Readiness{preparer.ReadinessBlocked, preparer.ReadinessNeedsInput} {
		run.Decisions.Decisions[0].Readiness = readiness
		run.Decisions.Decisions[0].Items[0].Readiness = readiness
		if err := validateLiveApplyReady(run); err == nil {
			t.Fatalf("expected downstream %s item to block live apply", readiness)
		}
	}
	next := nextSafeStep(run, nil)
	if strings.Contains(next.Action, "migrate --live") || next.DecisionID != "prepare-review" {
		t.Fatalf("expected next to surface the downstream blocker, got %#v", next)
	}
}

func TestApprovedPrepareDecisionsRequireResolvedOrSkippedProgress(t *testing.T) {
	item := runDecisionItem{
		Stage:     "prepare",
		App:       "api",
		Code:      "prepare.review",
		Message:   "review the prepared app",
		Readiness: preparer.ReadinessNeedsDecision,
	}
	decision := runDecision{ID: "prepare-review", Kind: "prepare-review", Items: []runDecisionItem{item}}
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{name: "unresolved"},
		{name: "resolved", status: progressStatusResolved, want: true},
		{name: "skipped", status: progressStatusSkipped, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := loadedMigrationRun{
				Decisions: runDecisions{APIVersion: decisionsAPIVersion, Decisions: []runDecision{decision}},
				Progress:  runProgress{Decisions: map[string]decisionProgress{}},
			}
			if tt.status != "" {
				run.Progress = markDecisionDone(run.Progress, decision, tt.status, "reviewed", time.Now().UTC())
			}
			approved := dokploy.NewPrepareDecision(item.App, item.Code, item.ResourceRef, item.Readiness, item.Message)
			_, ok := approvedPrepareDecisions(run)[approved]
			if ok != tt.want {
				t.Fatalf("approved = %t, want %t", ok, tt.want)
			}
		})
	}
}

func TestEnsureBoundDokployClientRejectsOriginBeforeCredentialedRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "bound", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindTargetOrigin("http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	_, err = ensureBoundDokployClient(context.Background(), migrationRun{Name: "bound"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{})
	if err == nil || !strings.Contains(err.Error(), "bound to Dokploy origin") {
		t.Fatalf("expected origin mismatch, got %v", err)
	}
	if requests != 0 {
		t.Fatalf("origin mismatch sent credentials before refusal, requests=%d", requests)
	}
}

func TestEnsureBoundDokployClientRejectsNonLoopbackBeforeCredentialedRequest(t *testing.T) {
	previousVerifier := verifyLocalDokployClient
	verifyLocalDokployClient = defaultVerifyLocalDokployClient
	t.Cleanup(func() { verifyLocalDokployClient = previousVerifier })
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	nonLoopbackURL := strings.Replace(server.URL, "127.0.0.1", "0.0.0.0", 1)
	if nonLoopbackURL == server.URL {
		t.Fatalf("test server URL %q did not bind 127.0.0.1", server.URL)
	}
	t.Setenv(dokploy.EnvBaseURL, nonLoopbackURL)
	t.Setenv(dokploy.EnvToken, "secret")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "non-loopback", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ensureBoundDokployClient(context.Background(), migrationRun{Name: "non-loopback"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{})
	if err == nil || !strings.Contains(err.Error(), "non-loopback http URL") {
		t.Fatalf("expected non-loopback refusal, got %v", err)
	}
	if requests != 0 {
		t.Fatalf("non-loopback target received %d credentialed request(s)", requests)
	}
	if origin := ledger.Snapshot().TargetOrigin; origin != "" {
		t.Fatalf("non-loopback target permanently bound the run to %q", origin)
	}
}

func TestEnsureBoundDokployClientDoesNotBindUnreachableOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "unreachable", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ensureBoundDokployClient(context.Background(), migrationRun{Name: "unreachable"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{})
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("expected reachability error, got %v", err)
	}
	if origin := ledger.Snapshot().TargetOrigin; origin != "" {
		t.Fatalf("unreachable target permanently bound the run to %q", origin)
	}
}

func TestEnsureBoundDokployClientQuotesRepairURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	baseURL := server.URL + "/a;b&c"
	t.Setenv(dokploy.EnvBaseURL, baseURL)
	t.Setenv(dokploy.EnvToken, "secret")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "quoted", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ensureBoundDokployClient(context.Background(), migrationRun{Name: "quoted"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{})
	if err == nil {
		t.Fatal("expected reachability error")
	}
	if !strings.Contains(err.Error(), "--dokploy-url '"+baseURL+"'") {
		t.Fatalf("repair command does not shell-quote the URL: %v", err)
	}
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if !strings.Contains(err.Error(), "--install-port "+port+" ") {
		t.Fatalf("repair command does not install on the URL's port %s: %v", port, err)
	}
}

func TestEnsureBoundDokployClientWithoutTerminalDoesNotOfferInstallForBoundRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "bound", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindTargetOrigin(server.URL); err != nil {
		t.Fatal(err)
	}
	_, err = ensureBoundDokployClient(context.Background(), migrationRun{Name: "bound"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{})
	if err == nil || !strings.Contains(err.Error(), "not reachable") || !strings.Contains(err.Error(), "restore the exact original target credential") {
		t.Fatalf("expected bound-run reachability error with credential-restore guidance, got %v", err)
	}
	if strings.Contains(err.Error(), "init-target") {
		t.Fatalf("bound run was offered a fresh install: %v", err)
	}
}

func TestEnsureBoundDokployClientDoesNotOfferInstallWhileHostLockIsHeld(t *testing.T) {
	resetDokployTrafficOwner(t)
	stdinIsTerminal = func(io.Reader) bool { return true }
	defer func() { stdinIsTerminal = defaultStdinIsTerminal }()
	t.Setenv(dokploy.EnvBaseURL, "http://127.0.0.1:3030")
	t.Setenv(dokploy.EnvToken, "test-token")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "lock-held", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindTargetOrigin("http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	held, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var retained *applyLock
	_, err = ensureBoundDokployClient(context.Background(), migrationRun{Name: "lock-held"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{retain: &retained})
	if !errors.Is(err, errDokployLiveOperationActive) {
		t.Fatalf("expected the held host lock to surface directly, got %v", err)
	}
	if retained != nil {
		retained.Release()
		t.Fatal("lock-blocked apply retained a host lock")
	}
}

func TestEnsureBoundDokployClientReloadsCredentialsAfterHostLock(t *testing.T) {
	resetDokployTrafficOwner(t)
	firstRequests := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer first.Close()
	secondRequests := 0
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	t.Setenv(dokploy.EnvBaseURL, first.URL)
	t.Setenv(dokploy.EnvToken, "first-token")
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "credential-race", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	var retained *applyLock
	client, err := ensureBoundDokployClient(context.Background(), migrationRun{Name: "credential-race"}, "dokploy", strings.NewReader(""), io.Discard, io.Discard, ledger, dokploySetupLockOptions{
		retain: &retained,
		revalidate: func() error {
			t.Setenv(dokploy.EnvBaseURL, second.URL)
			t.Setenv(dokploy.EnvToken, "second-token")
			return nil
		},
	})
	if retained != nil {
		defer retained.Release()
	}
	if err != nil {
		t.Fatal(err)
	}
	if client.BaseURL != second.URL || ledger.Snapshot().TargetOrigin != second.URL {
		t.Fatalf("apply used credentials read before the host lock: client=%q origin=%q", client.BaseURL, ledger.Snapshot().TargetOrigin)
	}
	if firstRequests != 0 || secondRequests != 1 {
		t.Fatalf("unexpected credentialed requests: first=%d second=%d", firstRequests, secondRequests)
	}
}

func TestApplyLiveMigrationRejectsEmptyPlan(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	runDir := filepath.Join(workDir, ".bort", "runs", "empty")
	bundleDir := filepath.Join(runDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundleDigest, err := digestMigrationBundle(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	run := loadedMigrationRun{
		Run: migrationRun{
			Name:         "empty",
			RunDir:       runDir,
			BundleDir:    bundleDir,
			BundleDigest: hex.EncodeToString(bundleDigest[:]),
			Target:       "dokploy",
			DryRun:       true,
			Artifacts:    defaultRunArtifacts(),
		},
		Prepare: preparer.Result{BundleDir: bundleDir},
	}
	if err := applyLiveMigrationLocked(context.Background(), run, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "no executable steps") {
		t.Fatalf("expected empty live plan to fail before recording success, got %v", err)
	}
}

func TestApplyLiveMigrationRefusesUnstageablePlanBeforeBindingTarget(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	runDir := filepath.Join(workDir, ".bort", "runs", "bind")
	bundleDir := filepath.Join(runDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundleDigest, err := digestMigrationBundle(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	previous := verifyLocalSourceRun
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error { return nil }
	t.Cleanup(func() { verifyLocalSourceRun = previous })
	app := preparer.AppPlan{
		Name:            "api",
		Resources:       preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api"}}},
	}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "bind", Source: "/srv/data", Target: "/data", SourceContainerID: "0123456789ab"}}
	run := loadedMigrationRun{
		Run: migrationRun{
			Name:         "bind",
			RunDir:       runDir,
			BundleDir:    bundleDir,
			BundleDigest: hex.EncodeToString(bundleDigest[:]),
			Target:       "dokploy",
			Source:       "docker",
			Artifacts:    defaultRunArtifacts(),
		},
		Prepare: preparer.Result{BundleDir: bundleDir, Source: "docker", SourceDockerEngineID: "engine-reviewed", Apps: []preparer.AppPlan{app}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	err = applyLiveMigrationLocked(context.Background(), run, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "before binding") || !strings.Contains(err.Error(), "only named volumes are transferred before deploy") {
		t.Fatalf("expected staged-transfer refusal before target binding, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, run.Run.Artifacts.Applied)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refusal must not create the applied ledger, stat err=%v", statErr)
	}
}

func TestOpenSetupDecisionsRecomputesFilteredMetadata(t *testing.T) {
	run := loadedMigrationRun{
		Decisions: runDecisions{
			APIVersion: decisionsAPIVersion,
			Decisions: []runDecision{{
				ID:        "blockers",
				Kind:      "blockers",
				Readiness: preparer.ReadinessBlocked,
				Action:    "stale action",
				Reason:    "stale reason",
				Apps:      []string{"api", "worker"},
				Codes:     []string{"cutover.blocked", "prepare.review"},
				Count:     2,
				Items: []runDecisionItem{
					{Stage: "prepare", App: "api", Code: "prepare.review", Readiness: preparer.ReadinessNeedsDecision},
					{Stage: "cutover", App: "worker", Code: "cutover.blocked", Readiness: preparer.ReadinessBlocked},
				},
			}},
		},
	}
	decisions := openSetupDecisions(run)
	if len(decisions) != 1 {
		t.Fatalf("expected one setup decision, got %#v", decisions)
	}
	decision := decisions[0]
	if decision.Count != 1 || decision.Readiness != preparer.ReadinessNeedsDecision || len(decision.Apps) != 1 || decision.Apps[0] != "api" || len(decision.Codes) != 1 || decision.Codes[0] != "prepare.review" {
		t.Fatalf("expected metadata recomputed from the prepare item, got %#v", decision)
	}
	if decision.Action != "fix 1 blocking issue(s) across 1 app(s)" || decision.Reason != "1 item(s), 1 app(s): prepare.review" {
		t.Fatalf("expected action and reason recomputed from the prepare item, got %#v", decision)
	}
}

func TestNextSafeStepScopesLifecycleCommandsToSelectedRun(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		run  migrationRun
		want string
	}{
		{
			name: "target live",
			run:  migrationRun{Name: "selected-run", Source: "docker", LiveAppliedAt: &now},
			want: "bort commit --apply --run selected-run",
		},
		{
			name: "committed",
			run:  migrationRun{Name: "selected-run", CommittedAt: &now},
			want: "bort cleanup --run selected-run",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := loadedMigrationRun{Run: tt.run}
			if tt.run.LiveAppliedAt != nil {
				run.Prepare = preparer.Result{
					Source:               "docker",
					SourceDockerEngineID: "engine-reviewed",
					Apps: []preparer.AppPlan{{
						Name:      "api",
						Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
					}},
				}
			}
			step := nextSafeStep(run, nil)
			if !strings.Contains(step.Action, tt.want) {
				t.Fatalf("expected next step to contain %q, got %q", tt.want, step.Action)
			}
		})
	}
}

func TestRunScopedCommandPreservesExternalRunDirectory(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	externalRunDir := filepath.Join(t.TempDir(), "selected-run")
	run := loadedMigrationRun{Run: migrationRun{Name: "selected-run", RunDir: externalRunDir}}
	got := runScopedCommand(run, "migrate --live")
	want := bortCommand("migrate --live --run " + shellQuote(externalRunDir))
	if got != want {
		t.Fatalf("expected external run command %q, got %q", want, got)
	}
}

func TestAmbiguousLegacyHandoffRequiresManualAuthorityRecovery(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	localDir := filepath.Join(workDir, ".bort", "runs", "named-run")
	externalDir := filepath.Join(t.TempDir(), "external-run")
	for _, runDir := range []string{localDir, externalDir} {
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name string
		run  loadedMigrationRun
	}{
		{name: "named", run: ambiguousLegacyRunForTest("named-run", localDir)},
		{name: "external", run: ambiguousLegacyRunForTest("external-run", externalDir)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := nextSafeStep(test.run, nil)
			if !strings.Contains(next.Action, "establish writer and traffic authority manually") || !strings.Contains(next.Action, "fresh migration run") || !strings.Contains(next.Reason, "cannot durably fence") {
				t.Fatalf("expected manual authority recovery guidance, got %#v", next)
			}
			if strings.Contains(next.Action, "rollback") || strings.Contains(next.Action, "--run") {
				t.Fatalf("ambiguous legacy guidance offered an unsafe automated recovery command: %#v", next)
			}
		})
	}
}

func ambiguousLegacyRunForTest(name, runDir string) loadedMigrationRun {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	cutover := gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}}
	steps := dokploy.PlanFromArtifacts(prepare, syncplan.Result{}, cutover).Steps
	applied := runApplied{RunName: name, Target: "dokploy"}
	for index, step := range steps {
		status := dokploy.StepStatusOK
		switch step.Kind {
		case dokploy.StepStartDokployProxy:
			status = dokploy.StepStatusStarted
		case dokploy.StepActivateRoutes, dokploy.StepResumeTarget:
			status = dokploy.StepStatusSkipped
		}
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: status})
		if status == dokploy.StepStatusStarted {
			break
		}
	}
	return loadedMigrationRun{
		Run:     migrationRun{Name: name, RunDir: runDir, Target: "dokploy"},
		Prepare: prepare,
		Sync:    syncplan.Result{},
		Cutover: cutover,
		Applied: applied,
	}
}

func TestRunMigrateLiveRequiresReviewedRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runMigrate(context.Background(), []string{"--live"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "no current migration run") {
		t.Fatalf("expected bare live apply to require a reviewed run, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if _, statErr := os.Stat(filepath.Join(workDir, ".bort", "runs")); !os.IsNotExist(statErr) {
		t.Fatalf("live apply created a run from the default bundle: %v", statErr)
	}
}

func TestRunMigrateRejectsPositionalArgument(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	err := runMigrate(context.Background(), []string{"--live", "intended-run"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `migrate does not accept positional argument "intended-run"`) {
		t.Fatalf("expected positional argument rejection, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, ".bort")); !os.IsNotExist(statErr) {
		t.Fatalf("positional argument rejection reached migration run setup: %v", statErr)
	}
}

func TestRunMigrateLiveDoesNotUseMtimeFallback(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "latest-only", Services: []manifest.Service{{Name: "latest-only", Image: "example/latest:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "latest-only"})
	if err := os.Remove(filepath.Join(workDir, ".bort", "state.json")); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runMigrate(context.Background(), []string{"--live"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "no current migration run") {
		t.Fatalf("expected live mode to reject an mtime-only run, got err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestApplyRunActiveTracksHeldLock(t *testing.T) {
	runDir := t.TempDir()
	lock, err := acquireApplyLock(filepath.Join(runDir, "apply.lock"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := applyRunActive(runDir)
	if err != nil || !active {
		lock.Release()
		t.Fatalf("expected held apply lock to report active: active=%t err=%v", active, err)
	}
	lock.Release()
	active, err = applyRunActive(runDir)
	if err != nil || active {
		t.Fatalf("expected released apply lock to report inactive: active=%t err=%v", active, err)
	}
}

func TestApplyLockActiveSurfacesOpenErrors(t *testing.T) {
	active, err := applyLockActive(filepath.Join(t.TempDir(), "missing.lock"))
	if err != nil || active {
		t.Fatalf("expected a missing lock file to report inactive: active=%t err=%v", active, err)
	}
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("lock probe fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if active, err := applyLockActive(filepath.Join(parentFile, "apply.lock")); err == nil || active {
		t.Fatalf("expected an unexpected lock open error to be surfaced: active=%t err=%v", active, err)
	}
}

func TestAcquireApplyLockToleratesBriefProbeHold(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "apply.lock")
	probe, err := tryAcquireApplyLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * applyLockProbeInterval)
		probe.Release()
	}()
	lock, err := acquireApplyLock(lockPath)
	if err != nil {
		t.Fatalf("acquire during a brief probe hold: %v", err)
	}
	lock.Release()
	held, err := tryAcquireApplyLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	started := time.Now()
	if _, err := acquireApplyLock(lockPath); !errors.Is(err, errApplyAlreadyRunning) {
		t.Fatalf("expected a held lock to still be reported busy, got %v", err)
	}
	if time.Since(started) < applyLockProbeGrace {
		t.Fatal("expected the acquirer to wait through the probe grace before reporting busy")
	}
}

func TestDokployLiveOperationProbeDoesNotCreateHostState(t *testing.T) {
	original := dokployLiveOperationLockPath
	t.Cleanup(func() { dokployLiveOperationLockPath = original })
	parent := filepath.Join(t.TempDir(), "missing")
	dokployLiveOperationLockPath = func() (string, error) {
		return filepath.Join(parent, "dokploy-live.lock"), nil
	}
	active, err := dokployLiveOperationActive()
	if err != nil || active {
		t.Fatalf("expected missing host state to report an inactive lock: active=%t err=%v", active, err)
	}
	if _, statErr := os.Stat(parent); !os.IsNotExist(statErr) {
		t.Fatalf("read-only host lock probe created %s: %v", parent, statErr)
	}
}

func TestApplyLockRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "run.json")
	want := []byte("preserve reviewed metadata\n")
	if err := os.WriteFile(targetPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "operation.lock")
	if err := os.Symlink(targetPath, lockPath); err != nil {
		t.Skipf("symlink fixture is unavailable: %v", err)
	}
	lock, err := acquireApplyLock(lockPath)
	if lock != nil {
		lock.Release()
	}
	if err == nil {
		t.Fatal("expected a symlink lock path to be rejected")
	}
	if active, err := applyLockActive(lockPath); err == nil || active {
		t.Fatalf("expected a symlink lock probe error: active=%t err=%v", active, err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("lock acquisition changed symlink target: got %q want %q", got, want)
	}
}

func TestApplyLockRejectsHardLinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "run.json")
	want := []byte("preserve reviewed metadata\n")
	if err := os.WriteFile(targetPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "operation.lock")
	if err := os.Link(targetPath, lockPath); err != nil {
		t.Skipf("hard-link fixture is unavailable: %v", err)
	}
	lock, err := acquireApplyLock(lockPath)
	if lock != nil {
		lock.Release()
	}
	if err == nil {
		t.Fatal("expected a hard-linked lock path to be rejected")
	}
	if active, err := applyLockActive(lockPath); err == nil || active {
		t.Fatalf("expected a hard-linked lock probe error: active=%t err=%v", active, err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("lock acquisition changed hard-link target: got %q want %q", got, want)
	}
}

func TestRunOperationLockTracksContentionAndRelease(t *testing.T) {
	runDir := t.TempDir()
	lock, err := acquireRunOperationLock(runDir)
	if err != nil {
		t.Fatal(err)
	}
	active, err := runOperationActive(runDir)
	if err != nil || !active {
		lock.Release()
		t.Fatalf("expected held run operation lock to report active: active=%t err=%v", active, err)
	}
	if second, err := acquireRunOperationLock(runDir); !errors.Is(err, errRunOperationActive) {
		if second != nil {
			second.Release()
		}
		lock.Release()
		t.Fatalf("expected second acquisition to fail with operation contention, got %v", err)
	}
	lock.Release()
	active, err = runOperationActive(runDir)
	if err != nil || active {
		t.Fatalf("expected released run operation lock to report inactive: active=%t err=%v", active, err)
	}
	reacquired, err := acquireRunOperationLock(runDir)
	if err != nil {
		t.Fatalf("expected lock reacquisition after release: %v", err)
	}
	reacquired.Release()
}

func TestDokployLiveOperationLockSpansWorkspaces(t *testing.T) {
	t.Chdir(t.TempDir())
	first, err := defaultDokployLiveOperationLockPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	second, err := defaultDokployLiveOperationLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(first) || first != second {
		t.Fatalf("expected one absolute host lock path independent of the workspace, got %q and %q", first, second)
	}
}

func TestBortStateMutationsSerializeReadModifyWrite(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".bort", "state.json")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- mutateBortState(statePath, func(state *bortState) bool {
			close(firstEntered)
			<-releaseFirst
			*state = setAppEnv(*state, "api", map[string]string{"TOKEN": "value"})
			return true
		})
	}()
	<-firstEntered

	secondStarted := make(chan struct{})
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- mutateBortState(statePath, func(state *bortState) bool {
			close(secondEntered)
			*state = setAppDataStrategy(*state, "api", "postgres", dataStrategyMigrate)
			return true
		})
	}()
	<-secondStarted
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second state mutation entered before the first released its lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	state, err := readBortState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Apps["api"].Env["TOKEN"] != "value" || state.Apps["api"].Data["postgres"].Strategy != dataStrategyMigrate {
		t.Fatalf("expected both serialized state updates, got %#v", state.Apps["api"])
	}
}

func TestRunOperationLockBlocksMutationsButNotStatus(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "locked"})
	lock, err := acquireRunOperationLock("locked")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	for name, command := range map[string]func() error{
		"refresh": func() error {
			return runMigrate(context.Background(), []string{"--run", "locked"}, io.Discard, io.Discard)
		},
		"commit": func() error {
			return runCommit(context.Background(), []string{"--apply", "--run", "locked"}, io.Discard, io.Discard)
		},
		"cleanup": func() error {
			return runCleanup(context.Background(), []string{"--apply", "--run", "locked"}, io.Discard, io.Discard)
		},
		"purge": func() error {
			return runCleanup(context.Background(), []string{"purge", "--apply", "--run", "locked", "--app", "api"}, io.Discard, io.Discard)
		},
	} {
		if err := command(); !errors.Is(err, errRunOperationActive) {
			t.Fatalf("expected held operation lock to block %s, got %v", name, err)
		}
	}
	if err := runStatus(context.Background(), []string{"--run", "locked"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("read-only status was blocked by operation lock: %v", err)
	}
}

func TestRunMigrateLiveAttachesToHeldApplyDespiteOperationLock(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "attaching"})
	operationLock, err := acquireRunOperationLock("attaching")
	if err != nil {
		t.Fatal(err)
	}
	defer operationLock.Release()
	applyLock, err := acquireApplyLock(filepath.Join(workDir, ".bort", "runs", "attaching", "apply.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer applyLock.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	err = runMigrate(ctx, []string{"--live", "--run", "attaching"}, io.Discard, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled attach, got %v", err)
	}
	if !strings.Contains(stderr.String(), "attaching to progress") {
		t.Fatalf("expected second live invocation to attach, got:\n%s", stderr.String())
	}
}

func TestFinalizeAttachedLiveMigrationRecordsLifecycle(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "attached-complete"})
	run, err := loadMigrationRun("attached-complete")
	if err != nil {
		t.Fatal(err)
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	applied := newRunApplied(run.Run)
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
	applyLock, err := acquireApplyLock(filepath.Join(run.Run.RunDir, "apply.lock"))
	if err != nil {
		t.Fatal(err)
	}
	attachCtx, cancelAttach := context.WithCancel(context.Background())
	cancelAttach()
	if err := applyLiveMigrationLocked(attachCtx, run, io.Discard, nil); !errors.Is(err, context.Canceled) {
		applyLock.Release()
		t.Fatalf("expected raced attach to wait for the producer's successful outcome, got %v", err)
	}
	applyLock.Release()
	if err := finalizeAttachedLiveMigration(context.Background(), run.Run.RunDir); err == nil || !strings.Contains(err.Error(), "no successful live-apply outcome") {
		t.Fatalf("expected complete steps without a successful outcome to remain incomplete, got %v", err)
	}
	withoutOutcome, err := loadMigrationRun("attached-complete")
	if err != nil {
		t.Fatal(err)
	}
	if withoutOutcome.Run.LiveAppliedAt != nil {
		t.Fatal("complete steps without a successful outcome marked the run live")
	}
	succeededAt := time.Now().UTC()
	applied.SucceededAt = &succeededAt
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	ownerPath, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerPath, []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := finalizeAttachedLiveMigration(context.Background(), run.Run.RunDir); err == nil || !strings.Contains(err.Error(), "cannot prove writer or traffic authority") {
		t.Fatalf("expected malformed durable owner to block attached finalization, got %v", err)
	}
	withMalformedOwner, err := loadMigrationRun("attached-complete")
	if err != nil {
		t.Fatal(err)
	}
	if withMalformedOwner.Run.LiveAppliedAt != nil {
		t.Fatal("malformed durable owner marked the attached run live")
	}
	if err := os.Remove(ownerPath); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := finalizeAttachedLiveMigration(context.Background(), run.Run.RunDir); err != nil {
		t.Fatal(err)
	}
	completed, err := loadMigrationRun("attached-complete")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.LiveAppliedAt == nil {
		t.Fatal("expected attached completion to record live lifecycle")
	}
}

func TestApplyLiveMigrationRefusesStatefulRunBeforeRecovery(t *testing.T) {
	resetDokployTrafficOwner(t)
	if runtime.GOOS == "windows" {
		t.Skip("docker PATH shim requires a POSIX shell")
	}
	workDir := t.TempDir()
	t.Chdir(workDir)
	if err := os.Mkdir(filepath.Join(workDir, ".bort"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/project.all" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "[]")
	}))
	defer server.Close()
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")

	shimDir := t.TempDir()
	dockerPath := filepath.Join(shimDir, "docker")
	dockerShim := `#!/bin/sh
if [ "$*" = "inspect --type container source-id" ]; then
  printf '%s\n' '[{"Id":"source-id","Name":"/source","State":{"Running":true,"Status":"running"}}]'
  exit 0
fi
printf 'unexpected docker command: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(dockerPath, []byte(dockerShim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runDir := t.TempDir()
	bundleDir := filepath.Join(runDir, "bundle")
	if err := os.Mkdir(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundleDigest, err := digestMigrationBundle(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "source-id"}}
	run := loadedMigrationRun{
		Run: migrationRun{
			Name:         "complete-without-outcome",
			RunDir:       runDir,
			BundleDir:    bundleDir,
			BundleDigest: hex.EncodeToString(bundleDigest[:]),
			Target:       "dokploy",
			DryRun:       true,
			Artifacts:    defaultRunArtifacts(),
		},
		Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	steps := dokploy.PlanFromArtifactsV1Alpha2(run.Prepare, run.Sync, run.Cutover).Steps
	applied := newRunApplied(run.Run)
	applied.PlanVersion = appliedPlanV1Alpha2
	applied.TargetOrigin = server.URL
	for index, step := range steps {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	run.Applied = applied
	appliedPath := runArtifactPath(runDir, run.Run.Artifacts.Applied)
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, server.URL, dokployCredentialID("secret")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, server.URL); err != nil {
		t.Fatal(err)
	}

	err = applyLiveMigrationLocked(context.Background(), run, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "live apply refused for stateful app(s) api") {
		t.Fatalf("expected stateful live apply refusal, got %v", err)
	}
	finalApplied := readJSONFile[runApplied](t, appliedPath)
	if finalApplied.SucceededAt != nil {
		t.Fatal("completed ledger with ambiguous source authority was marked successful")
	}
}

func TestApplyLiveMigrationResumesHistoricalPauseBeforeStatefulRefusal(t *testing.T) {
	resetDokployTrafficOwner(t)
	if runtime.GOOS == "windows" {
		t.Skip("docker PATH shim requires a POSIX shell")
	}
	runDir := t.TempDir()
	tracePath := filepath.Join(runDir, "docker.trace")
	shimDir := t.TempDir()
	dockerShim := `#!/bin/sh
printf '%s\n' "$*" >> "$TRACE"
if [ "$*" = "inspect --type container source-id" ]; then
  if [ -f "$SOURCE_RUNNING" ]; then
    printf '%s\n' '[{"Id":"source-id","Name":"/source","State":{"Running":true,"Status":"running"}}]'
  else
    printf '%s\n' '[{"Id":"source-id","Name":"/source","State":{"Running":false,"Status":"exited"}}]'
  fi
  exit 0
fi
printf 'unexpected docker command: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(shimDir, "docker"), []byte(dockerShim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TRACE", tracePath)
	sourceRunningPath := filepath.Join(runDir, "source-running")
	t.Setenv("SOURCE_RUNNING", sourceRunningPath)
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "source-id"}}
	run := loadedMigrationRun{
		Run: migrationRun{
			Name:      "historical-pause",
			RunDir:    runDir,
			BundleDir: filepath.Join(runDir, "bundle"),
			Target:    "dokploy",
			Artifacts: defaultRunArtifacts(),
		},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	plan := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	pauseIndex := -1
	applied := runApplied{
		APIVersion: appliedLegacyAPIVersion,
		RunName:    run.Run.Name,
		BundleDir:  run.Run.BundleDir,
		Target:     run.Run.Target,
		Apps:       map[string]appliedApp{},
	}
	for index, step := range plan.Steps {
		if step.Kind == dokploy.StepPauseSource {
			pauseIndex = index
			applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK)})
			break
		}
		applied.Steps = append(applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK)})
	}
	if pauseIndex < 0 {
		t.Fatal("stateful fixture did not produce a source pause")
	}
	run.Applied = applied
	appliedPath := runArtifactPath(runDir, run.Run.Artifacts.Applied)
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	if err := validateLiveApplyReady(run); err != nil {
		t.Fatalf("safe source cleanup was blocked before recovery: %v", err)
	}
	var stderr strings.Builder
	err := applyLiveMigrationLocked(context.Background(), run, &stderr, nil)
	if err == nil || !strings.Contains(err.Error(), "live apply refused for stateful app(s) api") || !strings.Contains(err.Error(), "docker start source-id") || strings.Contains(err.Error(), "previously paused source was resumed") {
		t.Fatalf("expected manual start instruction for a stopped source without pause ownership, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "source-pause.json")); !os.IsNotExist(err) {
		t.Fatalf("pause ownership was recorded for a stopped source Bort cannot prove it stopped: %v", err)
	}
	if err := os.WriteFile(sourceRunningPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	err = applyLiveMigrationLocked(context.Background(), run, &stderr, nil)
	if err == nil || !strings.Contains(err.Error(), "live apply refused for stateful app(s) api") || !strings.Contains(err.Error(), "previously paused source was resumed") {
		t.Fatalf("expected cleanup followed by stateful refusal, got %v", err)
	}
	if !strings.Contains(stderr.String(), "no state transfer was attempted") {
		t.Fatalf("expected cleanup-only recovery status, got %q", stderr.String())
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(trace), "start source-id") {
		t.Fatalf("Bort started a source container it never proved it stopped:\n%s", trace)
	}
	finalApplied := readJSONFile[runApplied](t, appliedPath)
	var cleanup appliedStep
	for _, step := range finalApplied.Steps {
		if step.Index == pauseIndex {
			cleanup = step
			break
		}
	}
	if cleanup.Kind != string(dokploy.StepResumeSource) || cleanup.Status != string(dokploy.StepStatusOK) {
		t.Fatalf("source cleanup was not durably recorded at pause index %d: %#v", pauseIndex, cleanup)
	}
}

func TestInterruptedStatefulSourceCleanupAllowsReadOnlyDump(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db", Kind: "postgres", Strategy: "migrate"}}
	run := loadedMigrationRun{
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "data_store",
			ResourceRef:  "data-store:db",
		}}}}},
	}
	plan := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	for _, dumpStatus := range []dokploy.StepStatus{dokploy.StepStatusStarted, dokploy.StepStatusOK} {
		t.Run(string(dumpStatus), func(t *testing.T) {
			applied := runApplied{APIVersion: appliedLegacyAPIVersion}
			pauseIndex := -1
			for index, step := range plan.Steps {
				if step.Kind == dokploy.StepPauseSource {
					pauseIndex = index
				}
				status := dokploy.StepStatusOK
				if step.Kind == dokploy.StepDumpDataStore {
					status = dumpStatus
				}
				applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: status})
				if step.Kind == dokploy.StepDumpDataStore {
					break
				}
			}
			run.Applied = applied
			cleanup := interruptedStatefulSourceCleanup(run)
			if len(cleanup) != 1 || cleanup[0].app != "api" || cleanup[0].index != pauseIndex {
				t.Fatalf("read-only dump blocked source recovery: %#v", cleanup)
			}
		})
	}
}

func TestInterruptedStatefulSourceCleanupRefusesCompletedImagePush(t *testing.T) {
	app := preparer.AppPlan{
		Name: "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			ComposeApp: preparer.DokployComposeApp{Name: "api"},
		}},
	}
	run := loadedMigrationRun{
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	run.Applied = runApplied{APIVersion: appliedAPIVersion, PlanVersion: appliedPlanV1Alpha2}
	plan := dokploy.PlanFromArtifactsV1Alpha2(run.Prepare, run.Sync, run.Cutover)
	pushCompleted := false
	pauseCompleted := false
	for index, step := range plan.Steps {
		run.Applied = recordAppliedStep(run.Applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
		pushCompleted = pushCompleted || step.Kind == dokploy.StepPushImage
		if step.Kind == dokploy.StepPauseSource {
			pauseCompleted = true
			break
		}
	}
	if !pushCompleted || !pauseCompleted {
		t.Fatalf("fixture did not complete image push and source pause: %v", plan.Steps)
	}
	if cleanup := interruptedStatefulSourceCleanup(run); len(cleanup) != 0 {
		t.Fatalf("completed image push allowed automatic source recovery: %#v", cleanup)
	}
}

func TestCompletedPrefixReconcilesProxyBeforeTargetAuthority(t *testing.T) {
	run, appliedPath := prepareCompletedPrefixProxyRun(t)
	if err := applyLiveMigrationLocked(context.Background(), run, io.Discard, nil); err != nil {
		t.Fatalf("completed-prefix recovery failed: %v", err)
	}
	for _, path := range []string{"coolify-stopped", "dokploy-started"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("proxy reconciliation did not create %s: %v", path, err)
		}
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		t.Fatal(err)
	}
	if !found || owner.Authority != dokployTrafficTarget {
		t.Fatalf("target authority was not recorded after reconciliation: %#v found=%t", owner, found)
	}
	finalApplied := readJSONFile[runApplied](t, appliedPath)
	if finalApplied.SucceededAt == nil {
		t.Fatal("completed-prefix recovery did not record success")
	}
}

func TestCompletedPrefixKeepsAuthorityWhenProxyStartFails(t *testing.T) {
	run, appliedPath := prepareCompletedPrefixProxyRun(t)
	if err := os.WriteFile("fail-start", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := applyLiveMigrationLocked(context.Background(), run, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "start proxy container dokploy-traefik") {
		t.Fatalf("expected completed-prefix recovery to fail on the Dokploy proxy start, got %v", err)
	}
	if _, statErr := os.Stat("dokploy-started"); statErr == nil {
		t.Fatal("docker stub recorded a proxy start despite failing")
	}
	owner, found, err := readDokployTrafficOwner()
	if err != nil {
		t.Fatal(err)
	}
	if found && owner.Authority == dokployTrafficTarget {
		t.Fatalf("target authority was recorded although the Dokploy proxy never started: %#v", owner)
	}
	if finalApplied := readJSONFile[runApplied](t, appliedPath); finalApplied.SucceededAt != nil {
		t.Fatal("completed-prefix recovery recorded success although the Dokploy proxy never started")
	}
}

func prepareCompletedPrefixProxyRun(t *testing.T) (loadedMigrationRun, string) {
	t.Helper()
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{{
			Name:     "api",
			Services: []manifest.Service{{ID: "source-id", Name: "web", Image: "example/api:latest"}},
			Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "web", Port: "3000"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "proxy-reconcile", "--observation-window", "0", "--rollback-window", "0"})
	run, err := loadMigrationRun("proxy-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	target := dokploy.TargetIdentity{ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-1", ComposeAppName: "stack-api"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/project.all":
			_, _ = io.WriteString(w, "[]")
		case "/api/project.one":
			_ = json.NewEncoder(w).Encode(dokploy.Project{ProjectID: target.ProjectID, Name: "api", Environments: []dokploy.ProjectEnvironment{{EnvironmentID: target.EnvironmentID, Name: "production"}}})
		case "/api/compose.one":
			_ = json.NewEncoder(w).Encode(dokploy.Compose{ComposeID: target.ComposeID, Name: "api", AppName: target.ComposeAppName, EnvironmentID: target.EnvironmentID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(dokploy.EnvBaseURL, server.URL)
	t.Setenv(dokploy.EnvToken, "secret")
	binDir := t.TempDir()
	dockerStub := `#!/bin/sh
if [ "$1" = inspect ] && [ "$2" = --type ]; then
  running=false
  policy=no
  case "$4" in
    coolify-proxy) [ ! -f coolify-stopped ] && running=true ;;
    dokploy-traefik) [ -f dokploy-started ] && running=true ;;
  esac
  [ ! -f "$4-unless" ] || policy=unless-stopped
  echo '[{"Id":"'"$4"'-id","Name":"/'"$4"'","State":{"Running":'"$running"',"Status":"running"},"HostConfig":{"RestartPolicy":{"Name":"'"$policy"'"}}}]'
elif [ "$1" = stop ]; then
  touch coolify-stopped
elif [ "$1" = start ]; then
  if [ -f fail-start ]; then
    echo "Error response from daemon: driver failed programming external connectivity" >&2
    exit 1
  fi
  touch dokploy-started
elif [ "$1" = update ] && [ "$2" = --restart=unless-stopped ]; then
  touch "${3%-id}-unless"
else
  echo "unexpected docker command: $*" >&2
  exit 1
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	plan := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	applied := newRunApplied(run.Run)
	applied.TargetOrigin = server.URL
	for index, step := range plan.Steps {
		progress := dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK}
		if step.App != "" {
			progress.Target = &target
		}
		applied = recordAppliedStep(applied, progress)
	}
	appliedPath := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	if err := claimDokployHostOwnership(run.Run, server.URL, dokployCredentialID("secret")); err != nil {
		t.Fatal(err)
	}
	return run, appliedPath
}

func TestValidateStatefulLiveApplyRefusesInPlaceTransferLedgersOnly(t *testing.T) {
	run := loadedMigrationRun{
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	for name, applied := range map[string]runApplied{
		"fresh":                 newRunApplied(migrationRun{}),
		"legacy-api-unexecuted": {APIVersion: appliedLegacyAPIVersion},
		"v1alpha2-unexecuted":   {APIVersion: appliedAPIVersion, PlanVersion: appliedPlanV1Alpha2},
	} {
		run.Applied = applied
		if err := validateStatefulLiveApply(run); err != nil {
			t.Fatalf("%s ledger must stage state instead of refusing, got %v", name, err)
		}
	}
	history := []appliedStep{{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)}}
	for name, applied := range map[string]runApplied{
		"legacy-api-history": {APIVersion: appliedLegacyAPIVersion, Steps: history},
		"v1alpha1-history":   {APIVersion: appliedAPIVersion, PlanVersion: appliedPlanV1Alpha1, Steps: history},
		"v1alpha2-history":   {APIVersion: appliedAPIVersion, PlanVersion: appliedPlanV1Alpha2, Steps: history},
	} {
		run.Applied = applied
		err := validateStatefulLiveApply(run)
		if err == nil || !strings.Contains(err.Error(), "live apply refused for stateful app(s) api") || !strings.Contains(err.Error(), "older plan version") || !strings.Contains(err.Error(), "outside Bort") {
			t.Fatalf("%s ledger: expected actionable stateful refusal, got %v", name, err)
		}
	}
}

func TestAttachProgressIncludesSourceCleanupAtPauseIndex(t *testing.T) {
	now := time.Now().UTC()
	steps := []dokploy.Step{{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"}}
	applied := runApplied{Steps: []appliedStep{{
		Index:     0,
		Kind:      string(dokploy.StepResumeSource),
		App:       "api",
		Ref:       "api",
		Status:    string(dokploy.StepStatusError),
		UpdatedAt: now,
		Error:     "source remains stopped",
	}}}

	entries := attachProgressEntries(steps, applied, time.Time{})
	if len(entries) != 1 {
		t.Fatalf("expected one attached cleanup entry, got %#v", entries)
	}
	progress := entries[0].progress
	if progress.Index != 0 || progress.Step.Kind != dokploy.StepResumeSource || progress.Status != dokploy.StepStatusError || progress.Err == nil || !strings.Contains(progress.Err.Error(), "remains stopped") {
		t.Fatalf("unexpected attached cleanup progress: %#v", progress)
	}
	failed, ok := latestAttachFailure(steps, applied, now)
	if !ok || failed.Kind != string(dokploy.StepResumeSource) || !strings.Contains(failed.Error, "remains stopped") {
		t.Fatalf("source cleanup failure was not visible to attach: %#v ok=%t", failed, ok)
	}
}

func TestAttachProgressIncludesStandaloneProxyCleanup(t *testing.T) {
	now := time.Now().UTC()
	steps := []dokploy.Step{{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"}}
	applied := runApplied{Steps: []appliedStep{{
		Index:     len(steps),
		Kind:      string(dokploy.StepStartCoolifyProxy),
		Ref:       "coolify-proxy",
		Status:    string(dokploy.StepStatusError),
		UpdatedAt: now,
		Error:     "proxy remains stopped",
	}}}

	entries := attachProgressEntries(steps, applied, time.Time{})
	if len(entries) != 1 {
		t.Fatalf("expected one attached proxy cleanup entry, got %#v", entries)
	}
	progress := entries[0].progress
	if progress.Index != len(steps) || progress.Total != len(steps)+1 || progress.Step.Kind != dokploy.StepStartCoolifyProxy || progress.Status != dokploy.StepStatusError || progress.Err == nil || !strings.Contains(progress.Err.Error(), "remains stopped") {
		t.Fatalf("unexpected attached proxy cleanup progress: %#v", progress)
	}
	var output bytes.Buffer
	writeAttachTextProgress(&output, progress, progress.Total, "recorded")
	if !strings.Contains(output.String(), "2/2 step(s) recorded") {
		t.Fatalf("standalone cleanup rendered invalid progress: %q", output.String())
	}
	failed, ok := latestAttachFailure(steps, applied, now)
	if !ok || failed.Kind != string(dokploy.StepStartCoolifyProxy) || !strings.Contains(failed.Error, "remains stopped") {
		t.Fatalf("proxy cleanup failure was not visible to attach: %#v ok=%t", failed, ok)
	}
}

func TestAttachUsesPersistedLegacyPlanVersion(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "legacy-attach", RunDir: t.TempDir(), Target: "dokploy"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	legacy := dokploy.LegacyPlanFromArtifactsV1Alpha1(run.Prepare, run.Sync, run.Cutover)
	applied := runApplied{
		APIVersion:  appliedAPIVersion,
		PlanVersion: appliedPlanV1Alpha1,
		RunName:     run.Run.Name,
		Target:      run.Run.Target,
		Apps:        map[string]appliedApp{},
	}
	for index, step := range legacy.Steps {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	now := time.Now().UTC()
	applied.SucceededAt = &now
	appliedPath := filepath.Join(run.Run.RunDir, "applied.json")
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	if err := attachLiveMigration(context.Background(), run, appliedPath, filepath.Join(run.Run.RunDir, "apply.lock"), io.Discard, nil); err != nil {
		t.Fatalf("attach rejected completed persisted legacy plan: %v", err)
	}
}

func TestRunRefreshPublishesVersionedArtifactsTogether(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "atomic-refresh"})
	before, err := loadMigrationRun("atomic-refresh")
	if err != nil {
		t.Fatal(err)
	}
	oldArtifacts := []string{
		before.Run.Artifacts.Prepare,
		before.Run.Artifacts.Sync,
		before.Run.Artifacts.Cutover,
		before.Run.Artifacts.Rollback,
		before.Run.Artifacts.Commit,
		before.Run.Artifacts.Decisions,
		before.Run.Artifacts.Progress,
		before.Run.Artifacts.Applied,
	}

	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:v2"}}}},
	})
	runCommand(t, runMigrate, []string{"--run", "atomic-refresh"})
	after, err := loadMigrationRun("atomic-refresh")
	if err != nil {
		t.Fatal(err)
	}
	newArtifacts := []string{
		after.Run.Artifacts.Prepare,
		after.Run.Artifacts.Sync,
		after.Run.Artifacts.Cutover,
		after.Run.Artifacts.Rollback,
		after.Run.Artifacts.Commit,
		after.Run.Artifacts.Decisions,
		after.Run.Artifacts.Progress,
		after.Run.Artifacts.Applied,
	}
	artifactDir := filepath.Dir(filepath.FromSlash(newArtifacts[0]))
	if artifactDir == "." {
		t.Fatalf("expected refreshed artifacts in a versioned directory, got %#v", after.Run.Artifacts)
	}
	for index, name := range newArtifacts {
		if name == oldArtifacts[index] || filepath.Dir(filepath.FromSlash(name)) != artifactDir {
			t.Fatalf("expected one new versioned artifact set, old=%#v new=%#v", oldArtifacts, newArtifacts)
		}
		if _, err := os.Stat(runArtifactPath(after.Run.RunDir, name)); err != nil {
			t.Fatalf("published artifact %s does not exist: %v", name, err)
		}
	}
	for _, name := range oldArtifacts {
		if _, err := os.Stat(runArtifactPath(before.Run.RunDir, name)); err != nil {
			t.Fatalf("previous reviewed artifact %s was not preserved: %v", name, err)
		}
	}
}

func TestRunMigrateManifestCreatesSelfContainedCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	manifestPath := filepath.Join(workDir, "manifest.json")
	if err := writeJSONArtifact(manifestPath, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--manifest", manifestPath, "--run", "direct"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate from manifest failed: %v\nstderr:\n%s", err, stderr.String())
	}
	run := readJSONFile[migrationRun](t, filepath.Join(workDir, ".bort", "runs", "direct", "run.json"))
	if run.Source != "manifest" || run.ManifestPath == manifestPath || !strings.HasPrefix(filepath.Base(run.ManifestPath), "manifest-") || !run.ApplyOutcomeRequired {
		t.Fatalf("unexpected direct migration metadata: %#v", run)
	}
	if err := containedPath(run.RunDir, run.ManifestPath); err != nil {
		t.Fatalf("expected a private manifest generation: %v", err)
	}
	if err := containedPath(run.RunDir, run.BundleDir); err != nil || !strings.HasPrefix(filepath.Base(run.BundleDir), "bundle-") {
		t.Fatalf("expected a private self-contained bundle snapshot, path=%s err=%v", run.BundleDir, err)
	}
	entries, err := os.ReadDir(run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "source-bundle-") {
			t.Fatalf("temporary source bundle was not removed: %s", entry.Name())
		}
	}
	state := readJSONFile[bortState](t, filepath.Join(workDir, ".bort", "state.json"))
	if state.CurrentRun != filepath.ToSlash(filepath.Join(".bort", "runs", "direct")) {
		t.Fatalf("expected direct run to become current, got %#v", state)
	}
	for _, want := range []string{"Existing manifest → dokploy", "api"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, stdout.String())
		}
	}
}

func TestMigrationBundleSnapshotValidationRejectsSourceChanges(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "source")
	snapshotDir := filepath.Join(t.TempDir(), "snapshot")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "compose.yaml")
	snapshotPath := filepath.Join(snapshotDir, "compose.yaml")
	if err := os.WriteFile(sourcePath, []byte("image: example/api:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, []byte("image: example/api:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceDigest, err := digestMigrationBundle(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("image: example/api:v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationBundleSnapshot(sourceDir, snapshotDir, sourceDigest); err == nil || !strings.Contains(err.Error(), "changed while it was being snapshotted") {
		t.Fatalf("expected a changing source bundle to be rejected, got %v", err)
	}
}

func TestRunSourceRefreshPublishesNewBundleAndArtifactsTogether(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	manifestPath := filepath.Join(workDir, "manifest.json")
	writeManifest := func(app, image string) {
		t.Helper()
		if err := writeJSONArtifact(manifestPath, manifest.Manifest{
			Source: manifest.Source{Platform: "docker"},
			Apps:   []manifest.App{{Name: app, Services: []manifest.Service{{Name: app, Image: image}}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest("reviewed", "example/reviewed:v1")
	runCommand(t, runMigrate, []string{"--manifest", manifestPath, "--run", "source-refresh"})
	before, err := loadMigrationRun("source-refresh")
	if err != nil {
		t.Fatal(err)
	}
	oldManifest, err := os.ReadFile(before.Run.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	oldIndexPath := filepath.Join(before.Run.BundleDir, "index.json")
	oldIndex, err := os.ReadFile(oldIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	oldArtifacts := []string{
		before.Run.Artifacts.Prepare,
		before.Run.Artifacts.Sync,
		before.Run.Artifacts.Cutover,
		before.Run.Artifacts.Rollback,
		before.Run.Artifacts.Commit,
		before.Run.Artifacts.Decisions,
		before.Run.Artifacts.Progress,
		before.Run.Artifacts.Applied,
	}

	writeManifest("replacement", "example/replacement:v2")
	runCommand(t, runMigrate, []string{"--manifest", manifestPath, "--run", "source-refresh"})
	after, err := loadMigrationRun("source-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if after.Run.BundleDir == before.Run.BundleDir {
		t.Fatalf("source refresh reused the reviewed bundle path %s", after.Run.BundleDir)
	}
	if after.Run.ManifestPath == before.Run.ManifestPath || !strings.HasPrefix(filepath.Base(after.Run.ManifestPath), "manifest-") {
		t.Fatalf("source refresh did not publish a new manifest generation: before=%s after=%s", before.Run.ManifestPath, after.Run.ManifestPath)
	}
	if err := containedPath(after.Run.RunDir, after.Run.ManifestPath); err != nil {
		t.Fatalf("expected refreshed manifest inside the run: %v", err)
	}
	if err := containedPath(after.Run.RunDir, after.Run.BundleDir); err != nil || !strings.HasPrefix(filepath.Base(after.Run.BundleDir), "bundle-") {
		t.Fatalf("expected refreshed source bundle to be a private snapshot, path=%s err=%v", after.Run.BundleDir, err)
	}
	if len(after.Prepare.Apps) != 1 || after.Prepare.Apps[0].Name != "replacement" {
		t.Fatalf("expected refreshed artifacts to use replacement source, got %#v", after.Prepare.Apps)
	}
	preservedIndex, err := os.ReadFile(oldIndexPath)
	if err != nil {
		t.Fatalf("old reviewed bundle was removed: %v", err)
	}
	if !bytes.Equal(preservedIndex, oldIndex) {
		t.Fatal("old reviewed bundle changed during source refresh")
	}
	preservedManifest, err := os.ReadFile(before.Run.ManifestPath)
	if err != nil {
		t.Fatalf("old reviewed manifest was removed: %v", err)
	}
	if !bytes.Equal(preservedManifest, oldManifest) {
		t.Fatal("old reviewed manifest changed during source refresh")
	}
	for _, name := range oldArtifacts {
		if _, err := os.Stat(runArtifactPath(before.Run.RunDir, name)); err != nil {
			t.Fatalf("old reviewed artifact %s was removed: %v", name, err)
		}
	}
	for _, name := range []string{
		after.Run.Artifacts.Prepare,
		after.Run.Artifacts.Sync,
		after.Run.Artifacts.Cutover,
		after.Run.Artifacts.Rollback,
		after.Run.Artifacts.Commit,
		after.Run.Artifacts.Decisions,
		after.Run.Artifacts.Progress,
		after.Run.Artifacts.Applied,
	} {
		if _, err := os.Stat(runArtifactPath(after.Run.RunDir, name)); err != nil {
			t.Fatalf("published artifact %s does not exist: %v", name, err)
		}
	}
	if after.Run.Artifacts.Prepare == before.Run.Artifacts.Prepare {
		t.Fatalf("source refresh did not switch to versioned artifacts: before=%#v after=%#v", before.Run.Artifacts, after.Run.Artifacts)
	}
	entries, err := os.ReadDir(after.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "source-bundle-") {
			t.Fatalf("temporary refreshed source bundle was not removed: %s", entry.Name())
		}
	}
}

func TestLegacyRunBundleIsSnapshottedBeforeLiveResume(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "legacy-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:v1"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "legacy-resume"})
	legacy, err := loadMigrationRun("legacy-resume")
	if err != nil {
		t.Fatal(err)
	}
	oldArtifacts := legacy.Run.Artifacts
	legacy.Run.ApplyOutcomeRequired = false
	legacy.Run.SourceBundleDir = ""
	legacy.Run.BundleDir = bundleDir
	legacy.Run.BundleDigest = ""
	legacy.Prepare.BundleDir = bundleDir
	legacy.Sync.BundleDir = bundleDir
	legacy.Cutover.BundleDir = bundleDir
	legacy.Rollback.BundleDir = bundleDir
	legacy.Commit.BundleDir = bundleDir
	legacy.Decisions.BundleDir = bundleDir
	steps := dokploy.PlanFromArtifacts(legacy.Prepare, legacy.Sync, legacy.Cutover).Steps
	if len(steps) == 0 {
		t.Fatal("expected legacy live steps")
	}
	legacy.Applied = newRunApplied(legacy.Run)
	legacy.Applied.Steps = []appliedStep{{
		Index:  0,
		Kind:   string(steps[0].Kind),
		App:    steps[0].App,
		Ref:    steps[0].Ref,
		Status: string(dokploy.StepStatusOK),
	}}
	runDir := legacy.Run.RunDir
	for path, value := range map[string]any{
		oldArtifacts.Prepare:   legacy.Prepare,
		oldArtifacts.Sync:      legacy.Sync,
		oldArtifacts.Cutover:   legacy.Cutover,
		oldArtifacts.Rollback:  legacy.Rollback,
		oldArtifacts.Commit:    legacy.Commit,
		oldArtifacts.Decisions: legacy.Decisions,
	} {
		if err := writeJSONArtifact(runArtifactPath(runDir, path), value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRunApplied(runArtifactPath(runDir, oldArtifacts.Applied), legacy.Applied); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(filepath.Join(runDir, "run.json"), legacy.Run); err != nil {
		t.Fatal(err)
	}
	legacy, err = loadMigrationRun("legacy-resume")
	if err != nil {
		t.Fatal(err)
	}
	operationLock, err := acquireRunOperationLock(runDir)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, upgradeErr := ensureSelfContainedLiveRunLocked(legacy)
	operationLock.Release()
	if upgradeErr != nil {
		t.Fatal(upgradeErr)
	}
	if !upgraded.Run.ApplyOutcomeRequired || upgraded.Run.SourceBundleDir != bundleDir || upgraded.Run.BundleDir == bundleDir {
		t.Fatalf("unexpected upgraded legacy metadata: %#v", upgraded.Run)
	}
	if upgraded.Run.BundleDigest == "" {
		t.Fatal("expected upgraded legacy run to record its reviewed bundle digest")
	}
	if err := verifyReviewedMigrationBundle(upgraded); err != nil {
		t.Fatalf("expected upgraded legacy bundle digest to match: %v", err)
	}
	if err := containedPath(upgraded.Run.RunDir, upgraded.Run.BundleDir); err != nil {
		t.Fatalf("legacy run bundle was not snapshotted into the run: %v", err)
	}
	if upgraded.Run.Artifacts.Prepare == oldArtifacts.Prepare || len(upgraded.Applied.Steps) != 1 {
		t.Fatalf("legacy resume state was not moved to a new generation: artifacts=%#v applied=%#v", upgraded.Run.Artifacts, upgraded.Applied)
	}
	indexPath := filepath.Join(upgraded.Run.BundleDir, "index.json")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "replacement", Services: []manifest.Service{{Name: "replacement", Image: "example/replacement:v2"}}}},
	})
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexAfter, indexBefore) {
		t.Fatal("upgraded legacy run still consumed its external mutable bundle")
	}
	reloaded, err := loadMigrationRun("legacy-resume")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Run.BundleDir != upgraded.Run.BundleDir || len(reloaded.Applied.Steps) != 1 {
		t.Fatalf("upgraded legacy run did not reload coherently: %#v", reloaded)
	}
}

func TestRunPlanBecomesImmutableAfterLiveExecutionStarts(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "immutable"})
	run, err := loadMigrationRun("immutable")
	if err != nil {
		t.Fatal(err)
	}
	operationLock, err := acquireRunOperationLock(run.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	markErr := markRunLiveAppliedLocked(run.Run)
	operationLock.Release()
	if markErr != nil {
		t.Fatal(markErr)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "immutable"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "reviewed plan is immutable") {
		t.Fatalf("expected applied run plan to be immutable, got err=%v", err)
	}
}

func TestRunSourceBundleBecomesImmutableAfterLiveExecutionStarts(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	manifestPath := filepath.Join(workDir, "manifest.json")
	if err := writeJSONArtifact(manifestPath, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed", Services: []manifest.Service{{Name: "reviewed", Image: "example/reviewed:latest"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	runCommand(t, runMigrate, []string{"--manifest", manifestPath, "--run", "source-immutable"})
	run, err := loadMigrationRun("source-immutable")
	if err != nil {
		t.Fatal(err)
	}
	operationLock, err := acquireRunOperationLock(run.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	markErr := markRunLiveAppliedLocked(run.Run)
	operationLock.Release()
	if markErr != nil {
		t.Fatal(markErr)
	}
	indexPath := filepath.Join(run.Run.BundleDir, "index.json")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONArtifact(manifestPath, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "replacement", Services: []manifest.Service{{Name: "replacement", Image: "example/replacement:latest"}}}},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = runMigrate(context.Background(), []string{"--manifest", manifestPath, "--run", "source-immutable"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "reviewed plan is immutable") {
		t.Fatalf("expected source-created run to be immutable, got err=%v", err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("source-created live bundle was overwritten")
	}
}

func TestRunPlanBecomesImmutableAfterLiveStartLedger(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "partial"})
	run, err := loadMigrationRun("partial")
	if err != nil {
		t.Fatal(err)
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	if len(steps) == 0 {
		t.Fatal("expected live apply steps")
	}
	applied := newRunApplied(run.Run)
	applied.Steps = []appliedStep{{Index: 0, Kind: string(steps[0].Kind), App: steps[0].App, Ref: steps[0].Ref, Status: string(dokploy.StepStatusStarted)}}
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "partial"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "reviewed plan is immutable") {
		t.Fatalf("expected partial live ledger to make the plan immutable, got err=%v", err)
	}
}

func TestRunPlanBecomesImmutableAfterTargetOriginBinding(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "target-bound"})
	run, err := loadMigrationRun("target-bound")
	if err != nil {
		t.Fatal(err)
	}
	applied := newRunApplied(run.Run)
	applied.TargetOrigin = "http://127.0.0.1:3030"
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "target-bound"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "reviewed plan is immutable") {
		t.Fatalf("expected target-bound run plan to be immutable, got err=%v", err)
	}
}

func TestRunPlanRefusesToOverwriteUnreadableMetadata(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "damaged"})
	metadataPath := filepath.Join(workDir, ".bort", "runs", "damaged", "run.json")
	if err := os.WriteFile(metadataPath, []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "damaged"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "metadata cannot be read") {
		t.Fatalf("expected unreadable run metadata to block rewrite, got err=%v", err)
	}
	contents, readErr := os.ReadFile(metadataPath)
	if readErr != nil || string(contents) != "{\n" {
		t.Fatalf("damaged run metadata was overwritten: contents=%q err=%v", contents, readErr)
	}
}

func TestRunStatusAndNextReadExistingRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker", DockerEngineID: "engine-reviewed"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{ID: "0123456789ab", Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"})
	markRunLocallyScanned(t, "demo-app", "docker")

	var statusOut bytes.Buffer
	var statusErr bytes.Buffer
	if err := runStatus(context.Background(), []string{"--run", "demo-app"}, &statusOut, &statusErr); err != nil {
		t.Fatalf("status failed: %v\nstderr:\n%s", err, statusErr.String())
	}
	for _, want := range []string{"Local Docker → dokploy", "api", "READY", "bort migrate --live --run demo-app"} {
		if !strings.Contains(statusOut.String(), want) {
			t.Fatalf("expected status output to contain %q, got:\n%s", want, statusOut.String())
		}
	}

	var nextOut bytes.Buffer
	var nextErr bytes.Buffer
	if err := runNext(context.Background(), []string{"demo-app"}, &nextOut, &nextErr); err != nil {
		t.Fatalf("next failed: %v\nstderr:\n%s", err, nextErr.String())
	}
	for _, want := range []string{"Next safe step: run `bort migrate --live --run demo-app`", "Run: demo-app", "Dry run only: no live migration action is executed by this command."} {
		if !strings.Contains(nextOut.String(), want) {
			t.Fatalf("expected next output to contain %q, got:\n%s", want, nextOut.String())
		}
	}

	hostLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer hostLock.Release()
	statusOut.Reset()
	if err := runStatus(context.Background(), []string{"--run", "demo-app"}, &statusOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut.String(), "HOST BUSY") || strings.Contains(statusOut.String(), " READY") {
		t.Fatalf("status did not surface the host-wide operation lock:\n%s", statusOut.String())
	}
	nextOut.Reset()
	if err := runNext(context.Background(), []string{"demo-app"}, &nextOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nextOut.String(), "wait for the active Dokploy host operation") || strings.Contains(nextOut.String(), "migrate --live") {
		t.Fatalf("next did not surface the host-wide operation lock:\n%s", nextOut.String())
	}
}

func TestNonLinuxReadyRunIsInspectionOnly(t *testing.T) {
	previous := dokployLiveOperationsSupported
	dokployLiveOperationsSupported = func() bool { return false }
	t.Cleanup(func() { dokployLiveOperationsSupported = previous })
	now := time.Now().UTC()
	for _, tc := range []struct {
		run   loadedMigrationRun
		phase string
	}{
		{run: loadedMigrationRun{Run: migrationRun{Name: "ready", RunDir: t.TempDir(), Target: "dokploy"}}, phase: "empty"},
		{run: loadedMigrationRun{Run: migrationRun{Name: "applied", RunDir: t.TempDir(), Target: "dokploy", LiveAppliedAt: &now}}, phase: "applied"},
		{run: loadedMigrationRun{Run: migrationRun{Name: "rollback", RunDir: t.TempDir(), Target: "dokploy", LiveAppliedAt: &now, RollbackStartedAt: &now}}, phase: "rolling back"},
	} {
		if phase := migrationRunPhase(tc.run); phase != tc.phase {
			t.Fatalf("unsupported platform run %q phase=%q, want %q", tc.run.Run.Name, phase, tc.phase)
		}
		next := nextSafeStep(tc.run, nil)
		wantAction := "Linux source host"
		if tc.phase == "empty" {
			wantAction = "at least one non-platform application"
		}
		if !strings.Contains(next.Action, wantAction) || strings.Contains(next.Action, "migrate --live") || strings.Contains(next.Action, "commit --apply") || strings.Contains(next.Action, "rollback --live") {
			t.Fatalf("unsupported platform run %q offered a live action: %#v", tc.run.Run.Name, next)
		}
	}
	migratable := loadedMigrationRun{
		Run: migrationRun{Name: "migratable", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
			}},
		},
	}
	if phase := migrationRunPhase(migratable); phase != "inspection-only" {
		t.Fatalf("unsupported platform migratable run phase=%q, want inspection-only", phase)
	}
	if next := nextSafeStep(migratable, nil); !strings.Contains(next.Action, "Linux source host") || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("unsupported platform migratable run offered a live action: %#v", next)
	}
	var migratableOutput strings.Builder
	writeAppFirstCockpit(&migratableOutput, migratable)
	if !strings.Contains(migratableOutput.String(), "INSPECTION ONLY") || !strings.Contains(migratableOutput.String(), "Dokploy live actions are unavailable on "+runtime.GOOS) || strings.Contains(migratableOutput.String(), "migrate --live") {
		t.Fatalf("unsupported platform cockpit hid inspection-only guidance:\n%s", migratableOutput.String())
	}
	for _, tc := range []struct {
		run          loadedMigrationRun
		phase        string
		wantAction   string
		wantGuidance string
	}{
		{run: loadedMigrationRun{Run: migrationRun{Name: "committed", RunDir: t.TempDir(), Target: "dokploy", CommittedAt: &now}}, phase: "committed", wantAction: "cleanup", wantGuidance: "audit leftovers"},
		{run: loadedMigrationRun{Run: migrationRun{Name: "rolled-back", RunDir: t.TempDir(), Target: "dokploy", RolledBackAt: &now}}, phase: "rolled back", wantAction: "new named migration run", wantGuidance: "To migrate again"},
		{run: loadedMigrationRun{Run: migrationRun{Name: "purged", RunDir: t.TempDir(), Target: "dokploy", PurgedAt: &now}}, phase: "purged", wantAction: "migration complete", wantGuidance: "Migration complete"},
	} {
		if phase := migrationRunPhase(tc.run); phase != tc.phase {
			t.Fatalf("unsupported platform terminal run %q phase=%q, want %q", tc.run.Run.Name, phase, tc.phase)
		}
		next := nextSafeStep(tc.run, nil)
		if !strings.Contains(next.Action, tc.wantAction) {
			t.Fatalf("unsupported platform terminal run %q hid safe next action: %#v", tc.run.Run.Name, next)
		}
		var output strings.Builder
		writeAppFirstCockpit(&output, tc.run)
		if !strings.Contains(output.String(), tc.wantGuidance) {
			t.Fatalf("unsupported platform terminal run %q hid safe cockpit guidance:\n%s", tc.run.Run.Name, output.String())
		}
	}
	if _, err := acquireDokployLiveOperationLock(); err == nil || !strings.Contains(err.Error(), "unavailable on this platform") {
		t.Fatalf("unsupported platform acquired live-operation lock: %v", err)
	}

	runDir := t.TempDir()
	lockTarget := filepath.Join(runDir, "lock-target")
	if err := os.WriteFile(lockTarget, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lockTarget, filepath.Join(runDir, "apply.lock")); err != nil {
		t.Skipf("symlink fixture is unavailable: %v", err)
	}
	lockErrorRun := loadedMigrationRun{Run: migrationRun{Name: "lock-error", RunDir: runDir, Target: "dokploy"}}
	if phase := migrationRunPhase(lockErrorRun); phase != "lock-error" {
		t.Fatalf("unsupported platform hid lock error behind phase %q", phase)
	}
	next := nextSafeStep(lockErrorRun, nil)
	if !strings.Contains(next.Action, "inspect the live-apply lock") || !strings.Contains(next.Action, "Linux source host") {
		t.Fatalf("unsupported platform hid apply-lock recovery: %#v", next)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, lockErrorRun)
	if !strings.Contains(output.String(), "LOCK ERROR") || !strings.Contains(output.String(), "apply.lock") || strings.Contains(output.String(), "ready for inspection") {
		t.Fatalf("unsupported cockpit contradicted lock-error state:\n%s", output.String())
	}
}

func TestDefaultVerifyLocalSourceRunAttestsEngineAndContainers(t *testing.T) {
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	script := `#!/bin/sh
if [ "$1" = info ]; then
  printf '%s\n' "$ENGINE_ID"
elif [ "$1" = inspect ] && [ "$2" = --type ] && [ "$3" = container ]; then
  printf '[{"Id":"%s","Name":"/%s","State":{"Running":true,"Status":"running"}}]\n' "$CONTAINER_ID" "$CONTAINER_NAME"
else
  echo "unexpected docker command: $*" >&2
  exit 1
fi
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ENGINE_ID", "engine-reviewed")
	t.Setenv("CONTAINER_ID", "0123456789abcdef")
	t.Setenv("CONTAINER_NAME", "reviewed-web")
	run := loadedMigrationRun{Run: migrationRun{Source: "coolify-local"}, Prepare: preparer.Result{
		Source:               "coolify-local",
		SourceDockerEngineID: "engine-reviewed",
		Apps: []preparer.AppPlan{{
			Name: "api",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{
				ServiceName:   "web",
				ContainerID:   "0123456789ab",
				ContainerName: "reviewed-web",
			}}},
		}},
	}}
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err != nil {
		t.Fatalf("expected matching source attestation, got %v", err)
	}
	t.Setenv("ENGINE_ID", "engine-other")
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected Docker engine mismatch refusal, got %v", err)
	}
	t.Setenv("ENGINE_ID", "engine-reviewed")
	t.Setenv("CONTAINER_ID", "fedcba9876543210")
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "resolved to ID") {
		t.Fatalf("expected source container mismatch refusal, got %v", err)
	}
	run.Run.Source = "manifest"
	t.Setenv("CONTAINER_ID", "0123456789abcdef")
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "imported manifest") {
		t.Fatalf("expected imported manifest refusal despite embedded local attestation, got %v", err)
	}
	run.Run.Source = ""
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "imported bundle") {
		t.Fatalf("expected imported bundle refusal despite embedded local attestation, got %v", err)
	}
	run.Run.Source = "coolify"
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), `source "coolify", not a local Docker scan`) {
		t.Fatalf("expected remote Coolify source refusal, got %v", err)
	}
	run.Run.Source = "docker"
	run.Prepare.Source = "imported"
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "not a locally attested") {
		t.Fatalf("expected imported source refusal, got %v", err)
	}
	run.Prepare.Source = "coolify-local-forged"
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "not a locally attested") {
		t.Fatalf("expected unknown Coolify source variant refusal, got %v", err)
	}
	run.Applied.TargetOrigin = "http://127.0.0.1:3030"
	if err := defaultVerifyLocalSourceRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "already started live execution and cannot be refreshed") {
		t.Fatalf("expected applied imported source to require manual recovery, got %v", err)
	}
}

func TestHostBusyPreservesRollbackLifecycleGuidance(t *testing.T) {
	for _, test := range []struct {
		name       string
		phase      string
		wantStatus string
		wantText   string
		configure  func(*migrationRun, time.Time)
	}{
		{
			name:       "rollback",
			phase:      "rolling back",
			wantStatus: "ROLLING BACK",
			wantText:   "Rollback is in progress",
			configure:  func(run *migrationRun, now time.Time) { run.RollbackStartedAt = &now },
		},
		{
			name:       "commit",
			phase:      "committing",
			wantStatus: "COMMITTING",
			wantText:   "Source retirement is in progress",
			configure:  func(run *migrationRun, now time.Time) { run.CommitStartedAt = &now },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDokployTrafficOwner(t)
			now := time.Now().UTC()
			meta := migrationRun{Name: test.name, RunDir: t.TempDir(), Target: "dokploy", CreatedAt: now, BundleDigest: test.name + "-digest", LiveAppliedAt: &now}
			test.configure(&meta, now)
			if err := claimDokployHostOwnership(meta, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
				t.Fatal(err)
			}
			if err := markDokployTrafficTarget(meta, "http://127.0.0.1:3030"); err != nil {
				t.Fatal(err)
			}
			run := loadedMigrationRun{Run: meta}
			lock, err := acquireDokployLiveOperationLock()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			if phase := migrationRunPhase(run); phase != test.phase {
				t.Fatalf("busy host phase=%q, want %q", phase, test.phase)
			}
			next := nextSafeStep(run, nil)
			if !strings.Contains(next.Action, "wait for the active Dokploy host operation") || strings.Contains(next.Action, "--live") || strings.Contains(next.Action, "commit --apply") {
				t.Fatalf("busy host offered mutation recovery: %#v", next)
			}
			var output strings.Builder
			writeAppFirstCockpit(&output, run)
			if !strings.Contains(output.String(), test.wantStatus) || !strings.Contains(output.String(), test.wantText) || !strings.Contains(output.String(), "wait for it to finish") || strings.Contains(output.String(), "rollback --live") || strings.Contains(output.String(), "commit --apply") {
				t.Fatalf("busy host hid lifecycle or offered a retry:\n%s", output.String())
			}
		})
	}
}

func TestRunStatusAndNextRejectAmbiguousRunArguments(t *testing.T) {
	tests := []struct {
		name string
		run  cliRunner
		args []string
		want string
	}{
		{
			name: "status multiple positional references",
			run:  runStatus,
			args: []string{"intended-run", "typo"},
			want: `status does not accept positional argument "typo" after run reference "intended-run"`,
		},
		{
			name: "next multiple positional references",
			run:  runNext,
			args: []string{"intended-run", "typo"},
			want: `next does not accept positional argument "typo" after run reference "intended-run"`,
		},
		{
			name: "status flag and positional reference",
			run:  runStatus,
			args: []string{"--run", "intended-run", "typo"},
			want: `status does not accept positional argument "typo" with --run`,
		},
		{
			name: "next flag and positional reference",
			run:  runNext,
			args: []string{"--run", "intended-run", "typo"},
			want: `next does not accept positional argument "typo" with --run`,
		},
		{
			name: "status empty flag and positional reference",
			run:  runStatus,
			args: []string{"--run=", "intended-run"},
			want: `status does not accept positional argument "intended-run" with --run`,
		},
		{
			name: "next empty flag and positional reference",
			run:  runNext,
			args: []string{"--run=", "intended-run"},
			want: `next does not accept positional argument "intended-run" with --run`,
		},
		{
			name: "status empty flag",
			run:  runStatus,
			args: []string{"--run="},
			want: "status requires a non-empty --run value",
		},
		{
			name: "next empty flag",
			run:  runNext,
			args: []string{"--run="},
			want: "next requires a non-empty --run value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := tt.run(context.Background(), tt.args, &stdout, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("ambiguous arguments produced output before rejection: %q", stdout.String())
			}
		})
	}
}

func TestRunStatusUsesCurrentRunBeforeNewerMtime(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	mtimeBundle := filepath.Join(workDir, "mtime-bundle")
	currentBundle := filepath.Join(workDir, "current-bundle")
	writeTestBundle(t, mtimeBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "mtime-only", Services: []manifest.Service{{Name: "mtime-only", Image: "example/mtime:latest"}}}},
	})
	writeTestBundle(t, currentBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "selected-current", Services: []manifest.Service{{Name: "selected-current", Image: "example/current:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", mtimeBundle, "--run", "mtime-run"})
	runCommand(t, runMigrate, []string{"--bundle", currentBundle, "--run", "current-run"})
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(workDir, ".bort", "runs", "mtime-run", "run.json"), future, future); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runStatus(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("status failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "selected-current") {
		t.Fatalf("expected status to use the persisted current run, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "mtime-only") {
		t.Fatalf("status selected a newer-mtime run instead of the current run:\n%s", stdout.String())
	}
}

func TestRunStatusAndNextDoNotUseMtimeFallback(t *testing.T) {
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

	for _, test := range []struct {
		name string
		run  cliRunner
	}{
		{name: "status", run: runStatus},
		{name: "next", run: runNext},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := test.run(context.Background(), nil, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "no current migration run") {
				t.Fatalf("expected %s to reject an mtime-only run, got err=%v stdout=%s stderr=%s", test.name, err, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunLifecycleTimestampsDriveCockpitLabels(t *testing.T) {
	resetDokployTrafficOwner(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker", DockerEngineID: "engine-reviewed"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{ID: "0123456789ab", Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "lifecycle"})
	markRunLocallyScanned(t, "lifecycle", "docker")
	run, err := loadMigrationRun("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	operationLock, err := acquireRunOperationLock(run.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	defer operationLock.Release()

	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(run.Run, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	if err := markRunLiveAppliedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.LiveAppliedAt == nil || run.Run.CommittedAt != nil || run.Run.PurgedAt != nil {
		t.Fatalf("unexpected live-applied lifecycle metadata: %#v", run.Run)
	}
	liveAppliedAt := *run.Run.LiveAppliedAt
	var cockpit bytes.Buffer
	writeAppFirstCockpit(&cockpit, run)
	if !strings.Contains(cockpit.String(), "TARGET LIVE") {
		t.Fatalf("expected target-live cockpit, got:\n%s", cockpit.String())
	}

	if err := markRunCommittedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.LiveAppliedAt == nil || !run.Run.LiveAppliedAt.Equal(liveAppliedAt) || run.Run.CommittedAt == nil || run.Run.PurgedAt != nil {
		t.Fatalf("unexpected committed lifecycle metadata: %#v", run.Run)
	}
	cockpit.Reset()
	writeAppFirstCockpit(&cockpit, run)
	if !strings.Contains(cockpit.String(), "COMMITTING") || !strings.Contains(cockpit.String(), "finish releasing commit host ownership") {
		t.Fatalf("expected commit-finalization cockpit, got:\n%s", cockpit.String())
	}
	if err := releaseDokployTargetOwner(run.Run); err != nil {
		t.Fatal(err)
	}
	cockpit.Reset()
	writeAppFirstCockpit(&cockpit, run)
	if !strings.Contains(cockpit.String(), "COMMITTED") {
		t.Fatalf("expected committed cockpit after owner release, got:\n%s", cockpit.String())
	}

	if err := markRunPurgedLocked(run.Run); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.LiveAppliedAt == nil || run.Run.CommittedAt == nil || run.Run.PurgedAt == nil {
		t.Fatalf("expected all lifecycle timestamps to persist: %#v", run.Run)
	}
	cockpit.Reset()
	writeAppFirstCockpit(&cockpit, run)
	if !strings.Contains(cockpit.String(), "COMPLETE") {
		t.Fatalf("expected complete cockpit, got:\n%s", cockpit.String())
	}
}

func TestRunMigratePreservesPrepareBlockersBeforeDecisions(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "blocked-app"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Overall: blocked (red)",
		"Open decisions:",
		"deploy_artifacts blocked: fix deploy artifacts for 1 app(s) (2 item(s))",
		"Next safe step: fix deploy artifacts for 1 app(s)",
		"Next decision: deploy_artifacts",
		"Next artifact: .bort/runs/blocked-app/decisions.json",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected blocked run output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunMigrateExcludesPlatformAppsFromGuidedSummary(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local"},
		Apps: []manifest.App{
			{Name: "api", Metadata: map[string]string{"migrationRole": "candidate"}, Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
			{Name: "coolify-proxy", Metadata: map[string]string{"migrationRole": "platform"}, Services: []manifest.Service{{Name: "traefik", Image: "traefik:v3", Environment: []manifest.EnvVar{{Name: "PROXY_TOKEN"}}}}},
		},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runMigrate(context.Background(), []string{"--bundle", bundleDir, "--run", "platform-filter", "--observation-window", "0", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"Apps: 1 total", "Platform/internal apps excluded: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected platform-filtered output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "coolify-proxy/env.values_required") {
		t.Fatalf("did not expect platform gates in guided summary:\n%s", output)
	}
}

func writeTestBundle(t *testing.T, bundleDir string, m manifest.Manifest) {
	t.Helper()
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir}); err != nil {
		t.Fatal(err)
	}
}
