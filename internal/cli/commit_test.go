package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestRunCommitWritesTextPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), []string{"--bundle", dir, "--target", "dokploy"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Commit plan: " + dir + " -> dokploy",
		"[yellow] api",
		"readiness: needs_decision",
		"cutover readiness: needs_decision",
		"rollback window: 3600s",
		"needs_decision accept dokploy.domain:api.example.com; retire source.route:api.example.com service=api port=3000",
		"warn commit.rollback_window_closed: confirm rollback window for api.example.com is closed or explicitly waived before retiring source route",
		"Dry run only: no target ownership was committed and no source resources were retired.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected commit output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRunCommitWritesJSONPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), []string{"--bundle", dir, "--format", "json", "--rollback-window", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var result commitplan.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("commit json did not decode: %v\n%s", err, stdout.String())
	}
	if result.APIVersion != commitplan.APIVersion || !result.DryRun || result.Target != "dokploy" || len(result.Apps) != 1 {
		t.Fatalf("unexpected commit json: %#v", result)
	}
	if len(result.Apps[0].Routes) != 1 || len(result.Apps[0].Steps) != 4 {
		t.Fatalf("expected commit route and steps, got %#v", result.Apps[0])
	}
	if result.Apps[0].RollbackWindowSeconds != 0 {
		t.Fatalf("expected explicit zero rollback window, got %#v", result.Apps[0])
	}
	for _, gate := range result.Apps[0].Gates {
		if gate.Code == "commit.rollback_window_closed" {
			t.Fatalf("did not expect rollback window gate for explicit zero window: %#v", result.Apps[0].Gates)
		}
	}
}

func TestRunCommitApplyRejectsIgnoredPlanningFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCommit(context.Background(), []string{"--apply", "--app", "api"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not accept --app") {
		t.Fatalf("expected commit apply to reject ignored app scope, got %v", err)
	}
}

func TestRunCommitApplyRejectsPositionalArguments(t *testing.T) {
	err := runCommit(context.Background(), []string{"--apply", "purge"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not accept positional argument") {
		t.Fatalf("expected positional argument to be rejected before source retirement, got %v", err)
	}
}

func TestRunCommitRejectsEmptyExplicitRun(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		state.CurrentRun = ".bort/runs/current"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--run="}, {"--apply", "--run="}} {
		var stdout bytes.Buffer
		err := runCommit(context.Background(), args, &stdout, io.Discard)
		if err == nil || err.Error() != "commit requires a non-empty --run value" {
			t.Fatalf("args=%v: expected empty run rejection, got %v", args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("args=%v: empty run produced output before rejection: %q", args, stdout.String())
		}
	}
}

func TestRunCommitRejectsEmptyCutoverArtifact(t *testing.T) {
	var stdout bytes.Buffer
	err := runCommit(context.Background(), []string{"--from-cutover="}, &stdout, io.Discard)
	if err == nil || err.Error() != "commit requires a non-empty --from-cutover value" {
		t.Fatalf("expected empty cutover artifact rejection, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("empty cutover artifact produced output before rejection: %q", stdout.String())
	}

	err = runCommit(context.Background(), []string{"--apply", "--from-cutover="}, io.Discard, io.Discard)
	if err == nil || err.Error() != "commit --apply does not accept --from-cutover; select the run with --run" {
		t.Fatalf("expected apply mode to reject the cutover artifact flag, got %v", err)
	}
}

func TestRunCommitDefaultsToCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "reviewed-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed-app", Services: []manifest.Service{{Name: "reviewed-app", Image: "example/reviewed:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "reviewed-run"})
	reviewed, err := loadMigrationRun("reviewed-run")
	if err != nil {
		t.Fatal(err)
	}
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "stale-app", Services: []manifest.Service{{Name: "stale-app", Image: "example/stale:latest"}}}},
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Commit plan: "+reviewed.Run.BundleDir+" -> dokploy") || !strings.Contains(stdout.String(), "reviewed-app") {
		t.Fatalf("expected commit to use the current run artifact, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "stale-app") {
		t.Fatalf("commit planned from the default bundle instead of the current run:\n%s", stdout.String())
	}
}

func TestRunCommitHonorsExplicitRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	explicitBundle := filepath.Join(workDir, "explicit-bundle")
	currentBundle := filepath.Join(workDir, "current-bundle")
	writeTestBundle(t, explicitBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "explicit-app", Services: []manifest.Service{{Name: "explicit-app", Image: "example/explicit:latest"}}}},
	})
	writeTestBundle(t, currentBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "current-app", Services: []manifest.Service{{Name: "current-app", Image: "example/current:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", explicitBundle, "--run", "explicit-run"})
	runCommand(t, runMigrate, []string{"--bundle", currentBundle, "--run", "current-run"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCommit(context.Background(), []string{"--run", "explicit-run"}, &stdout, &stderr); err != nil {
		t.Fatalf("commit failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "explicit-app") || strings.Contains(stdout.String(), "current-app") {
		t.Fatalf("expected explicit run to override current run, got:\n%s", stdout.String())
	}
}

func TestRunCommitUsesDefaultBundleWithoutCurrentRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	reviewedBundle := filepath.Join(workDir, "reviewed-bundle")
	writeTestBundle(t, reviewedBundle, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "reviewed-app", Services: []manifest.Service{{Name: "reviewed-app", Image: "example/reviewed:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", reviewedBundle, "--run", "reviewed-run"})
	writeTestBundle(t, filepath.Join(workDir, "bort-bundle"), manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "default-app", Services: []manifest.Service{{Name: "default-app", Image: "example/default:latest"}}}},
	})
	if err := mutateBortState(defaultStatePath(), func(state *bortState) bool {
		state.CurrentRun = ""
		return true
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runCommit(context.Background(), nil, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "default-app") || strings.Contains(stdout.String(), "reviewed-app") {
		t.Fatalf("expected bare commit to plan from the default bundle without a current run, got:\n%s", stdout.String())
	}
}

func TestNewRunRequiresSuccessfulOutcomeBeforeCommit(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "missing-outcome"})
	run, err := loadMigrationRun("missing-outcome")
	if err != nil {
		t.Fatal(err)
	}
	if !run.Run.ApplyOutcomeRequired {
		t.Fatal("expected new run metadata to require a successful apply outcome")
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	applied := newRunApplied(run.Run)
	for index, step := range steps {
		applied.Steps = append(applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunApplied(appliedPath, applied); err != nil {
		t.Fatal(err)
	}
	run, err = loadMigrationRun("missing-outcome")
	if err != nil {
		t.Fatal(err)
	}
	var cockpit bytes.Buffer
	writeAppFirstCockpit(&cockpit, run)
	if strings.Contains(cockpit.String(), "TARGET LIVE") {
		t.Fatalf("complete steps without a durable outcome were shown as target live:\n%s", cockpit.String())
	}
	if err := applyCommitFromArgs(context.Background(), "missing-outcome", io.Discard); err == nil || !strings.Contains(err.Error(), "no successful live-apply outcome") {
		t.Fatalf("expected commit to reject complete steps without a durable outcome, got %v", err)
	}
}

func TestLegacyCompleteLedgerDoesNotRequireSuccessfulOutcomeMarker(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "legacy-run"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	for index, step := range steps {
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		t.Fatalf("expected a legacy complete ledger to remain accepted: %v", err)
	}
}

func TestLiveApplyRecoveryErrorsPreserveExternalRunDirectory(t *testing.T) {
	externalRunDir := filepath.Join(t.TempDir(), "selected-run")
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "selected-run", RunDir: externalRunDir, ApplyOutcomeRequired: true},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	want := liveApplyCommand(run)
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected missing-outcome recovery command %q, got %v", want, err)
	}
	run.Run.ApplyOutcomeRequired = false
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected missing-step recovery command %q, got %v", want, err)
	}
}

func TestRequireLiveApplySucceededAcceptsSkippedPlatformSteps(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{
			{Name: "proxy", Role: "platform"},
			{Name: "api"},
		}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{
			Name:   "api",
			Routes: []gateway.Route{{Host: "api.example.com"}},
		}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	succeededAt := time.Now().UTC()
	run.Applied.SucceededAt = &succeededAt
	for index, step := range steps {
		status := string(dokploy.StepStatusOK)
		if step.App == "proxy" {
			status = string(dokploy.StepStatusSkipped)
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: status,
		})
	}
	if err := requireLiveApplySucceeded(run); err != nil {
		t.Fatalf("expected skipped platform steps to count as completed: %v", err)
	}
}

func TestRequireLiveApplySucceededRejectsReorderedLedgerIndexes(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}, {Name: "worker"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{
			{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}},
			{Name: "worker", Routes: []gateway.Route{{Host: "worker.example.com"}}},
		}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  len(steps) - 1 - index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), "plan index") {
		t.Fatalf("expected reordered completed ledger steps to be rejected, got %v", err)
	}
}

func TestRequireLiveApplySucceededRejectsLaterFailedLedgerEntry(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	for index, step := range steps {
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{Index: index, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK), UpdatedAt: time.Unix(1, 0)})
	}
	failed := steps[0]
	run.Applied.Steps = append(run.Applied.Steps, appliedStep{Index: len(steps), Kind: string(failed.Kind), App: failed.App, Ref: failed.Ref, Status: string(dokploy.StepStatusError), UpdatedAt: time.Unix(2, 0)})
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), string(failed.Kind)) {
		t.Fatalf("expected later failed ledger entry to invalidate historical success, got %v", err)
	}
}

func TestRequireLiveApplySucceededForAppsIgnoresUnselectedApps(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}, {Name: "worker"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{
			{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}},
			{Name: "worker", Routes: []gateway.Route{{Host: "worker.example.com"}}},
		}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	succeededAt := time.Now().UTC()
	run.Applied.SucceededAt = &succeededAt
	for index, step := range steps {
		if step.App != "" && step.App != "api" {
			continue
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceededForApps(run, map[string]struct{}{"api": {}}); err != nil {
		t.Fatalf("expected selected app's completed ledger steps to count as successful live apply: %v", err)
	}
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("expected all-app guard to still reject missing worker steps, got %v", err)
	}
}

func TestRequireLiveApplySucceededForAppsSkippingAllowsMissingSkippedKinds(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "run-1"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Cutover: gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	}
	steps := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover).Steps
	if len(steps) == 0 {
		t.Fatal("expected live plan steps")
	}
	succeededAt := time.Now().UTC()
	run.Applied.SucceededAt = &succeededAt
	skipKind := steps[0].Kind
	for index, step := range steps {
		if step.Kind == skipKind {
			continue
		}
		run.Applied.Steps = append(run.Applied.Steps, appliedStep{
			Index:  index,
			Kind:   string(step.Kind),
			App:    step.App,
			Ref:    step.Ref,
			Status: string(dokploy.StepStatusOK),
		})
	}
	if err := requireLiveApplySucceededForAppsSkipping(run, nil, map[dokploy.StepKind]struct{}{skipKind: {}}); err != nil {
		t.Fatalf("expected missing skipped step kind to be ignored: %v", err)
	}
	if err := requireLiveApplySucceeded(run); err == nil || !strings.Contains(err.Error(), string(skipKind)) {
		t.Fatalf("expected regular guard to reject missing %s step, got %v", skipKind, err)
	}
}
