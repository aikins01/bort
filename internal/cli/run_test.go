package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000"}}},
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
		"Next safe step: run `bort migrate --live --run demo-app`",
		"Dry run only: no target resources, sync operations, route changes, ownership commits, or source cleanup were executed.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected migrate output to contain %q, got:\n%s", want, output)
		}
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

func TestLegacySelfContainedRunWithoutBundleDigestRemainsLoadable(t *testing.T) {
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

func TestEnsureDokployClientShellQuotesRepairURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	baseURL := server.URL + "?source=a&mode=b"
	t.Setenv(dokploy.EnvBaseURL, baseURL)
	t.Setenv(dokploy.EnvToken, "secret")

	_, err := ensureDokployClient(context.Background(), "dokploy", strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--dokploy-url '"+baseURL+"'") {
		t.Fatalf("expected shell-quoted repair URL, got %v", err)
	}
}

func TestApplyLiveMigrationRejectsEmptyPlan(t *testing.T) {
	runDir := t.TempDir()
	bundleDir := filepath.Join(runDir, "bundle")
	if err := os.Mkdir(bundleDir, 0o700); err != nil {
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
			run:  migrationRun{Name: "selected-run", LiveAppliedAt: &now},
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
			step := nextSafeStep(loadedMigrationRun{Run: tt.run}, nil)
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
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"})

	var statusOut bytes.Buffer
	var statusErr bytes.Buffer
	if err := runStatus(context.Background(), []string{"--run", "demo-app"}, &statusOut, &statusErr); err != nil {
		t.Fatalf("status failed: %v\nstderr:\n%s", err, statusErr.String())
	}
	for _, want := range []string{"local bundle → dokploy", "api", "READY", "bort migrate --live --run demo-app"} {
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
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "lifecycle"})
	run, err := loadMigrationRun("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	operationLock, err := acquireRunOperationLock(run.Run.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	defer operationLock.Release()

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
	if !strings.Contains(cockpit.String(), "COMMITTED") {
		t.Fatalf("expected committed cockpit, got:\n%s", cockpit.String())
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
