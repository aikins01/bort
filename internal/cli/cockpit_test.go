package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestSafeRunArtifactPathRejectsEscapes(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	path, err := safeRunArtifactPath(runDir, "nested/progress.json")
	if err != nil {
		t.Fatalf("expected nested progress path to be allowed: %v", err)
	}
	if err := containedPath(runDir, path); err != nil {
		t.Fatalf("expected %s to remain inside %s: %v", path, runDir, err)
	}

	for _, artifact := range []string{"", "../progress.json", "nested/../../progress.json", filepath.Join(t.TempDir(), "progress.json")} {
		if _, err := safeRunArtifactPath(runDir, artifact); err == nil {
			t.Fatalf("expected artifact path %q to be rejected", artifact)
		}
	}
}

func TestReadRunProgressIgnoresStaleOrMismatchedProgress(t *testing.T) {
	workDir := t.TempDir()
	runDir := filepath.Join(workDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(runDir, "progress.json")
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	run := migrationRun{Name: "demo-app", RunDir: filepath.ToSlash(runDir), UpdatedAt: now, DryRun: true}
	decision := runDecision{
		Kind: "cutover",
		Items: []runDecisionItem{{
			Stage:       "cutover",
			App:         "api",
			Code:        "route.confirm",
			ResourceRef: "route:api.example.com",
			Message:     "confirm cutover route",
			Readiness:   preparer.ReadinessNeedsDecision,
		}},
	}

	stale := markDecisionDone(emptyRunProgress(run), decision, progressStatusResolved, "old", now.Add(-time.Minute))
	if err := writeRunProgress(progressPath, stale); err != nil {
		t.Fatal(err)
	}
	progress, err := readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Decisions) != 0 {
		t.Fatalf("expected stale progress to be ignored, got %#v", progress.Decisions)
	}

	fresh := markDecisionDone(emptyRunProgress(run), decision, progressStatusResolved, "new", now.Add(time.Minute))
	if err := writeRunProgress(progressPath, fresh); err != nil {
		t.Fatal(err)
	}
	progress, err = readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Decisions["cutover"].Status != progressStatusResolved {
		t.Fatalf("expected fresh matching progress to be loaded, got %#v", progress.Decisions)
	}

	mismatchedName := fresh
	mismatchedName.RunName = "other"
	if err := writeRunProgress(progressPath, mismatchedName); err != nil {
		t.Fatal(err)
	}
	progress, err = readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Decisions) != 0 {
		t.Fatalf("expected wrong-run-name progress to be ignored, got %#v", progress.Decisions)
	}

	mismatchedDir := fresh
	mismatchedDir.RunDir = filepath.ToSlash(filepath.Join(workDir, "other-run"))
	if err := writeRunProgress(progressPath, mismatchedDir); err != nil {
		t.Fatal(err)
	}
	progress, err = readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Decisions) != 0 {
		t.Fatalf("expected wrong-run-dir progress to be ignored, got %#v", progress.Decisions)
	}
}

func TestRecordReviewDecisionPersistsWithoutTTY(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "api",
			Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
			Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "review-progress"})
	run, err := loadMigrationRun("review-progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Decisions.Decisions) == 0 {
		t.Fatal("expected a review decision")
	}
	decision := run.Decisions.Decisions[0]
	if err := recordReviewDecision(run, decision, run.Run.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadMigrationRun("review-progress")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Progress.Decisions[decision.Kind].Status != progressStatusResolved {
		t.Fatalf("expected review progress to persist, got %#v", reloaded.Progress.Decisions)
	}
	refreshed, err := refreshMigrationRun("review-progress")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Progress.Decisions[decision.Kind].Status != progressStatusResolved {
		t.Fatalf("expected review progress to survive plan refresh, got %#v", refreshed.Progress.Decisions)
	}
	for _, open := range openRunDecisions(refreshed) {
		if open.Kind == decision.Kind {
			t.Fatalf("refreshed plan reopened resolved decision %q", decision.Kind)
		}
	}
}

func TestRecordReviewDecisionRejectsChangedPlanGeneration(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{{
			Name:     "api",
			Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
			Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
		}},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "stale-review"})
	stale, err := loadMigrationRun("stale-review")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale.Decisions.Decisions) == 0 {
		t.Fatal("expected a review decision")
	}
	if _, err := refreshMigrationRun("stale-review"); err != nil {
		t.Fatal(err)
	}
	err = recordReviewDecision(stale, stale.Decisions.Decisions[0], time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "plan changed while review was open") {
		t.Fatalf("expected stale review to be rejected, got %v", err)
	}
}

func TestNextWizardDecisionsIncludesDownstreamReview(t *testing.T) {
	run := loadedMigrationRun{Decisions: runDecisions{
		APIVersion: decisionsAPIVersion,
		Decisions: []runDecision{{
			ID:        "cutover",
			Kind:      "cutover",
			Readiness: preparer.ReadinessNeedsDecision,
			Items: []runDecisionItem{{
				Stage:     "cutover",
				App:       "api",
				Code:      "cutover.health_check_required",
				Readiness: preparer.ReadinessNeedsDecision,
			}},
		}},
	}}
	decisions, reviewOnly := nextWizardDecisions(run)
	if !reviewOnly || len(decisions) != 1 || decisions[0].Kind != "cutover" {
		t.Fatalf("expected downstream review decision, got reviewOnly=%t decisions=%#v", reviewOnly, decisions)
	}
}

func TestMarkReviewDecisionDoneKeepsExcludedBlockersOpen(t *testing.T) {
	reviewItem := runDecisionItem{Stage: "cutover", App: "api", Code: "cutover.health_check_required", Readiness: preparer.ReadinessNeedsDecision}
	blockedItem := runDecisionItem{Stage: "cutover", App: "api", Code: "cutover.blocked", Readiness: preparer.ReadinessBlocked}
	run := loadedMigrationRun{
		Decisions: runDecisions{
			APIVersion: decisionsAPIVersion,
			Decisions: []runDecision{{
				ID:        "cutover",
				Kind:      "cutover",
				Readiness: preparer.ReadinessBlocked,
				Items:     []runDecisionItem{reviewItem, blockedItem},
			}},
		},
		Progress: emptyRunProgress(migrationRun{}),
	}
	selected := runDecision{ID: "cutover", Kind: "cutover", Readiness: preparer.ReadinessNeedsDecision, Items: []runDecisionItem{reviewItem}}
	run.Progress = markReviewDecisionDone(run, selected, time.Now().UTC())
	if run.Progress.Decisions["cutover"].Status != progressStatusOpen {
		t.Fatalf("expected partially reviewed decision kind to remain open, got %#v", run.Progress.Decisions["cutover"])
	}
	open := openRunDecisions(run)
	if len(open) != 1 || len(open[0].Items) != 1 || open[0].Items[0].Code != blockedItem.Code {
		t.Fatalf("expected only the excluded blocker to remain open, got %#v", open)
	}
}

func TestAppliedFooterEmptyWhenNoSteps(t *testing.T) {
	if got := appliedFooter(runApplied{}); got != "" {
		t.Fatalf("expected empty footer, got %q", got)
	}
}

func TestAppliedFooterCountsOkAndError(t *testing.T) {
	applied := runApplied{Steps: []appliedStep{
		{Index: 0, Status: "ok"},
		{Index: 1, Status: "error"},
		{Index: 2, Status: "skipped"},
	}}
	got := appliedFooter(applied)
	if got != "Applied: 3 steps recorded · 2 ok · 1 failed" {
		t.Fatalf("unexpected footer: %q", got)
	}
}

func TestAppliedFooterUsesSingularStep(t *testing.T) {
	applied := runApplied{Steps: []appliedStep{{Index: 0, Status: "ok"}}}
	if got := appliedFooter(applied); got != "Applied: 1 step recorded · 1 ok" {
		t.Fatalf("unexpected footer: %q", got)
	}
}

func TestCockpitLabelsAmbiguousLegacyHandoffAndScopesRollback(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	externalDir := filepath.Join(t.TempDir(), "legacy-run")
	if err := os.MkdirAll(externalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	run := ambiguousLegacyRunForTest("legacy-run", externalDir)
	if err := validateApplyResumeAuthority(run.Applied); err == nil || !strings.Contains(err.Error(), "after the shared proxy handoff") {
		t.Fatalf("legacy fixture is not ambiguous through the proxy handoff alone: %v", err)
	}

	if phase := migrationRunPhase(run); phase != "authority-ambiguous" {
		t.Fatalf("expected authority-ambiguous phase, got %q", phase)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{"AUTHORITY UNKNOWN", "Inspect and preserve both sides", "Automatic retry, commit, and rollback are unavailable", "establish authority manually"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("expected cockpit to contain %q, got:\n%s", want, output.String())
		}
	}
	now := time.Now().UTC()
	run.Run.RollbackStartedAt = &now
	if phase := migrationRunPhase(run); phase != "authority-ambiguous-rollback" {
		t.Fatalf("expected interrupted ambiguous rollback phase, got %q", phase)
	}
	output.Reset()
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{"AUTHORITY UNKNOWN", "Target fencing may be partial", "source state may be unchanged", "Inspect and preserve both sides"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("expected interrupted ambiguous rollback cockpit to contain %q, got:\n%s", want, output.String())
		}
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "inspect and preserve both source and target data") || !strings.Contains(next.Reason, "fencing may be partial") {
		t.Fatalf("expected interrupted ambiguous rollback guidance, got %#v", next)
	}
}

func TestCockpitPrioritizesActiveApplyOverAmbiguousStep(t *testing.T) {
	run := writeAmbiguousAuthorityRun(t, "active-ambiguous")
	lock, err := acquireApplyLock(filepath.Join(run.Run.RunDir, "apply.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if phase := migrationRunPhase(run); phase != "applying" {
		t.Fatalf("active ambiguous run phase=%q, want applying", phase)
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "view the active apply") || strings.Contains(next.Action, "recover-authority") {
		t.Fatalf("active apply next step is unsafe: %#v", next)
	}
}

func TestCockpitShowsConflictingHostOwner(t *testing.T) {
	resetDokployTrafficOwner(t)
	ownerRun := migrationRun{Name: "owner-run", RunDir: filepath.Join(t.TempDir(), "owner-run"), CreatedAt: time.Now().UTC(), BundleDigest: "owner-digest"}
	if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(ownerRun, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "waiting-run", RunDir: t.TempDir(), Target: "dokploy", CreatedAt: time.Now().UTC(), BundleDigest: "waiting-digest"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
	}
	if phase := migrationRunPhase(run); phase != "host-owned" {
		t.Fatalf("expected host-owned phase, got %q", phase)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{"HOST OWNED", `owned by migration run "owner-run"`, ownerRun.RunDir, "target authority"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("expected cockpit to contain %q, got:\n%s", want, output.String())
		}
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, `finish migration run "owner-run"`) || !strings.Contains(next.Action, ownerRun.RunDir) || !strings.Contains(next.Reason, "target authority") {
		t.Fatalf("expected owning-run guidance, got %#v", next)
	}
}

func TestCockpitShowsHostOwnerGuidanceForEmptyRun(t *testing.T) {
	resetDokployTrafficOwner(t)
	ownerRun := migrationRun{Name: "owner-run", RunDir: filepath.Join(t.TempDir(), "owner-run"), CreatedAt: time.Now().UTC(), BundleDigest: "owner-digest"}
	if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("owner-token")); err != nil {
		t.Fatal(err)
	}
	run := loadedMigrationRun{Run: migrationRun{Name: "empty-run", RunDir: t.TempDir(), Target: "dokploy", CreatedAt: time.Now().UTC(), BundleDigest: "empty-digest"}}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{"No apps in this run", "HOST OWNED", `migration run "owner-run"`, ownerRun.RunDir} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("empty-run cockpit missing %q:\n%s", want, output.String())
		}
	}
}

func TestCockpitRefusesPlatformOnlyLiveApply(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := loadedMigrationRun{
		Run: migrationRun{Name: "platform-only", RunDir: t.TempDir(), Target: "dokploy"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{
			Name: "coolify-proxy",
			Role: "platform",
		}}},
	}
	if phase := migrationRunPhase(run); phase != "empty" {
		t.Fatalf("platform-only phase=%q, want empty", phase)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "NO APPS") || !strings.Contains(output.String(), "no migratable applications") || strings.Contains(output.String(), "migrate --live") {
		t.Fatalf("platform-only cockpit offered live apply:\n%s", output.String())
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "at least one non-platform application") || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("platform-only next step offered live apply: %#v", next)
	}
	if err := validateLiveApplyReady(run); err == nil || !strings.Contains(err.Error(), "no migratable applications") {
		t.Fatalf("platform-only run passed live validation: %v", err)
	}
}

func TestCockpitKeepsStaticallyUnattestedSourcesInspectionOnly(t *testing.T) {
	newRun := func() loadedMigrationRun {
		return loadedMigrationRun{
			Run: migrationRun{Name: "unattested", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
			Prepare: preparer.Result{
				Source:               "docker",
				SourceDockerEngineID: "engine-reviewed",
				Apps: []preparer.AppPlan{{
					Name:      "api",
					Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
				}},
			},
		}
	}
	for _, test := range []struct {
		name       string
		configure  func(*loadedMigrationRun)
		wantReason string
	}{
		{name: "imported manifest", configure: func(run *loadedMigrationRun) { run.Run.Source = "manifest" }, wantReason: "imported manifest"},
		{name: "imported bundle", configure: func(run *loadedMigrationRun) { run.Run.Source = "" }, wantReason: "imported bundle"},
		{name: "missing engine", configure: func(run *loadedMigrationRun) { run.Prepare.SourceDockerEngineID = "" }, wantReason: "no reviewed Docker engine identity"},
		{name: "missing container", configure: func(run *loadedMigrationRun) { run.Prepare.Apps[0].Resources.SourceServices[0].ContainerID = "" }, wantReason: "no stable container ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDokployTrafficOwner(t)
			run := newRun()
			test.configure(&run)
			next := nextSafeStep(run, nil)
			if next.Phase != "inspection-only" || !strings.Contains(next.Action, "new named migration run") || !strings.Contains(next.Reason, test.wantReason) || strings.Contains(next.Action, "migrate --live") {
				t.Fatalf("unattested source received unsafe guidance: %#v", next)
			}
			if phase := migrationRunPhase(run); phase != "inspection-only" {
				t.Fatalf("phase=%q, want inspection-only", phase)
			}
			var output strings.Builder
			writeAppFirstCockpit(&output, run)
			if !strings.Contains(output.String(), "INSPECTION ONLY") || !strings.Contains(output.String(), test.wantReason) || strings.Contains(output.String(), "migrate --live") {
				t.Fatalf("cockpit hid static source-attestation failure:\n%s", output.String())
			}
		})
	}
}

func TestCockpitBlocksReadyRunWhenRuntimeSourceAttestationFails(t *testing.T) {
	resetDokployTrafficOwner(t)
	previous := verifyLocalSourceRun
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error { return context.DeadlineExceeded }
	t.Cleanup(func() { verifyLocalSourceRun = previous })
	run := loadedMigrationRun{
		Run: migrationRun{Name: "source-changed", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
			}},
		},
	}
	next := nextSafeStep(run, nil)
	if next.Phase != "source-attestation-error" || !strings.Contains(next.Reason, context.DeadlineExceeded.Error()) || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("runtime source-attestation failure received unsafe guidance: %#v", next)
	}
	if phase := migrationRunPhase(run); phase != "source-attestation-error" {
		t.Fatalf("phase=%q, want source-attestation-error", phase)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "SOURCE CHANGED") || !strings.Contains(output.String(), context.DeadlineExceeded.Error()) || strings.Contains(output.String(), "migrate --live") {
		t.Fatalf("cockpit hid runtime source-attestation failure:\n%s", output.String())
	}
}

func TestMigrationSummaryPropagatesCancellationToSourceAttestation(t *testing.T) {
	resetDokployTrafficOwner(t)
	previous := verifyLocalSourceRun
	verifyLocalSourceRun = func(ctx context.Context, _ loadedMigrationRun) error { return ctx.Err() }
	t.Cleanup(func() { verifyLocalSourceRun = previous })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := loadedMigrationRun{
		Run: migrationRun{Name: "canceled", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
			}},
		},
	}

	summary := summarizeMigrationRunContext(ctx, run)
	if summary.Next.Phase != "source-attestation-error" || !strings.Contains(summary.Next.Reason, context.Canceled.Error()) {
		t.Fatalf("canceled source attestation produced unsafe guidance: %#v", summary.Next)
	}
}

func TestLiveMigrationSummaryDefersRuntimeSourceAttestation(t *testing.T) {
	resetDokployTrafficOwner(t)
	previous := verifyLocalSourceRun
	calls := 0
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error {
		calls++
		return nil
	}
	t.Cleanup(func() { verifyLocalSourceRun = previous })
	run := loadedMigrationRun{
		Run: migrationRun{Name: "live", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
			}},
		},
	}

	summary := summarizeMigrationRunForLive(context.Background(), run)
	if calls != 0 || !strings.Contains(summary.Next.Action, "migrate --live") {
		t.Fatalf("live summary probed the source or hid the ready action: calls=%d next=%#v", calls, summary.Next)
	}
	summarizeMigrationRunContext(context.Background(), run)
	if calls != 1 {
		t.Fatalf("read-only summary source probes=%d, want 1", calls)
	}
}

func TestCockpitBlocksMutationGuidanceWhenStartedRunSourceIsUnattested(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name      string
		configure func(*loadedMigrationRun)
	}{
		{
			name: "partial apply",
			configure: func(run *loadedMigrationRun) {
				run.Applied.Steps = []appliedStep{{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)}}
			},
		},
		{
			name: "target live",
			configure: func(run *loadedMigrationRun) {
				run.Run.LiveAppliedAt = &now
			},
		},
		{
			name: "stateful target live",
			configure: func(run *loadedMigrationRun) {
				run.Run.LiveAppliedAt = &now
				run.Sync = syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
					ResourceType: "volume",
					ResourceRef:  "volume:web -> /data",
					Strategy:     syncplan.StrategyDockerVolumeArchive,
				}}}}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDokployTrafficOwner(t)
			run := loadedMigrationRun{
				Run: migrationRun{Name: "started", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
				Prepare: preparer.Result{
					Source:               "docker",
					SourceDockerEngineID: "engine-reviewed",
					Apps: []preparer.AppPlan{{
						Name:      "api",
						Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab"}}},
					}},
				},
			}
			test.configure(&run)
			next := nextSafeStep(run, nil)
			if next.Phase != "source-attestation-error" || !strings.Contains(next.Reason, "no reviewed container name") || !strings.Contains(next.Action, "do not replay") {
				t.Fatalf("started unattested run received unsafe guidance: %#v", next)
			}
			for _, command := range []string{"migrate --live", "commit --apply", "rollback --live"} {
				if strings.Contains(next.Action, command) {
					t.Fatalf("started unattested run offered %q: %#v", command, next)
				}
			}
			if phase := migrationRunPhase(run); phase != "source-attestation-error" {
				t.Fatalf("phase=%q, want source-attestation-error", phase)
			}
			var output strings.Builder
			writeAppFirstCockpit(&output, run)
			if !strings.Contains(output.String(), "SOURCE CHANGED") || !strings.Contains(output.String(), "do not replay") {
				t.Fatalf("cockpit hid started source-attestation failure:\n%s", output.String())
			}
			for _, command := range []string{"migrate --live", "commit --apply", "rollback --live"} {
				if strings.Contains(output.String(), command) {
					t.Fatalf("cockpit offered %q for started unattested run:\n%s", command, output.String())
				}
			}
		})
	}

	run := loadedMigrationRun{
		Run: migrationRun{Name: "active", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab"}}},
			}},
		},
	}
	lock, err := acquireApplyLock(filepath.Join(run.Run.RunDir, "apply.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	next := nextSafeStep(run, nil)
	if next.Phase == "" || !strings.Contains(next.Reason, "no reviewed container name") || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("active unattested run received unsafe attach guidance: %#v", next)
	}
}

func TestCockpitReportsUnreadableHostOwner(t *testing.T) {
	resetDokployTrafficOwner(t)
	path, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := loadedMigrationRun{Run: migrationRun{Name: "blocked", RunDir: t.TempDir(), Target: "dokploy"}}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "LOCK ERROR") || !strings.Contains(output.String(), "decode Dokploy traffic owner") || strings.Contains(output.String(), "fresh migration run") {
		t.Fatalf("cockpit hid the durable-owner read failure:\n%s", output.String())
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "dokploy-traffic-owner.json") || !strings.Contains(next.Reason, "decode Dokploy traffic owner") {
		t.Fatalf("next step hid the durable-owner read failure: %#v", next)
	}
}

func TestCockpitPreservesActiveLifecycleWhenHostOwnerIsUnreadable(t *testing.T) {
	resetDokployTrafficOwner(t)
	path, err := dokployTrafficOwnerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, test := range []struct {
		name      string
		configure func(*migrationRun)
		phase     string
		status    string
		context   string
	}{
		{
			name:      "commit",
			configure: func(run *migrationRun) { run.CommitStartedAt = &now },
			phase:     "committing",
			status:    "COMMITTING",
			context:   "rollback is no longer available",
		},
		{
			name:      "rollback",
			configure: func(run *migrationRun) { run.RollbackStartedAt = &now },
			phase:     "rolling back",
			status:    "ROLLING BACK",
			context:   "traffic or source state may already have changed",
		},
		{
			name: "authority finalization",
			configure: func(run *migrationRun) {
				run.ResolvedAuthority = dokployTrafficSource
				run.AuthorityResolvedAt = &now
			},
			phase:   "authority-finalizing",
			status:  "RECOVERY PENDING",
			context: "lifecycle finalization is incomplete",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			meta := migrationRun{Name: test.name, RunDir: t.TempDir(), Target: "dokploy", LiveAppliedAt: &now}
			test.configure(&meta)
			run := loadedMigrationRun{Run: meta}
			if phase := migrationRunPhase(run); phase != test.phase {
				t.Fatalf("phase=%q, want %q", phase, test.phase)
			}
			next := nextSafeStep(run, nil)
			if !strings.Contains(next.Action, "dokploy-traffic-owner.json") || !strings.Contains(next.Reason, "decode Dokploy traffic owner") {
				t.Fatalf("next step hid lifecycle or owner failure: %#v", next)
			}
			var output strings.Builder
			writeAppFirstCockpit(&output, run)
			if !strings.Contains(output.String(), test.status) || !strings.Contains(output.String(), test.context) || !strings.Contains(output.String(), "dokploy-traffic-owner.json") || strings.Contains(output.String(), "LOCK ERROR") {
				t.Fatalf("cockpit hid lifecycle behind owner failure:\n%s", output.String())
			}
		})
	}
}

func TestCockpitFinalizesCompletedDurableOwnerBeforeTerminalGuidance(t *testing.T) {
	for _, test := range []struct {
		name           string
		configureRun   func(*migrationRun, *time.Time)
		configureOwner func(migrationRun) error
		phase          string
		resumeCommand  string
		releaseOwner   func(migrationRun) error
		terminalPhase  string
	}{
		{
			name: "commit",
			configureRun: func(run *migrationRun, now *time.Time) {
				run.CommitStartedAt = now
				run.CommittedAt = now
			},
			configureOwner: func(run migrationRun) error { return markDokployTrafficTarget(run, "http://127.0.0.1:3030") },
			phase:          "committing",
			resumeCommand:  "commit --apply",
			releaseOwner:   releaseDokployTargetOwner,
			terminalPhase:  "committed",
		},
		{
			name: "rollback",
			configureRun: func(run *migrationRun, now *time.Time) {
				run.RollbackStartedAt = now
				run.RolledBackAt = now
			},
			configureOwner: func(run migrationRun) error { return markDokployTrafficSource(run) },
			phase:          "rolling back",
			resumeCommand:  "rollback --live",
			releaseOwner:   releaseDokployTrafficOwner,
			terminalPhase:  "rolled back",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDokployTrafficOwner(t)
			now := time.Now().UTC()
			meta := migrationRun{Name: test.name, RunDir: filepath.Join(t.TempDir(), test.name), Target: "dokploy", CreatedAt: now, BundleDigest: test.name + "-digest"}
			test.configureRun(&meta, &now)
			meta = persistRunFixture(t, meta)
			if err := claimDokployHostOwnership(meta, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
				t.Fatal(err)
			}
			if err := test.configureOwner(meta); err != nil {
				t.Fatal(err)
			}
			run := loadedMigrationRun{Run: meta, Applied: newRunApplied(meta)}
			if phase := migrationRunPhase(run); phase != test.phase {
				t.Fatalf("phase=%q, want %q", phase, test.phase)
			}
			next := nextSafeStep(run, nil)
			if !strings.Contains(next.Action, test.resumeCommand) || !strings.Contains(next.Reason, "not released") {
				t.Fatalf("next step did not expose owner finalization: %#v", next)
			}
			if err := test.releaseOwner(meta); err != nil {
				t.Fatal(err)
			}
			if phase := migrationRunPhase(run); phase != test.terminalPhase {
				t.Fatalf("released phase=%q, want %q", phase, test.terminalPhase)
			}
		})
	}
}

func TestCockpitManualTargetAuthorityDefersToHostOperationLock(t *testing.T) {
	resetDokployTrafficOwner(t)
	previous := verifyLocalSourceRun
	verifyLocalSourceRun = func(context.Context, loadedMigrationRun) error { return nil }
	t.Cleanup(func() { verifyLocalSourceRun = previous })
	now := time.Now().UTC()
	rollbackStarted := now.Add(-time.Minute)
	meta := migrationRun{Name: "manual-target", RunDir: filepath.Join(t.TempDir(), "manual-target"), Target: "dokploy", Source: "docker", CreatedAt: now, BundleDigest: "manual-digest", RollbackStartedAt: &rollbackStarted, LiveAppliedAt: &now, ResolvedAuthority: dokployTrafficTarget, AuthorityResolvedAt: &now}
	if err := claimDokployHostOwnership(meta, "http://127.0.0.1:3030", dokployCredentialID("token")); err != nil {
		t.Fatal(err)
	}
	if err := markDokployTrafficTarget(meta, "http://127.0.0.1:3030"); err != nil {
		t.Fatal(err)
	}
	run := loadedMigrationRun{
		Run:     meta,
		Prepare: preparer.Result{Source: "docker", SourceDockerEngineID: "engine-reviewed", Apps: []preparer.AppPlan{{Name: "api", Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}}}}},
		Applied: newRunApplied(meta),
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "commit --apply") || strings.Contains(next.Action, "recover-authority") {
		t.Fatalf("next step treated the resolved run as an unfinished rollback: %#v", next)
	}
	if phase := migrationRunPhase(run); phase != "applied" {
		t.Fatalf("phase=%q, want applied before the host lock is held", phase)
	}
	hostLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer hostLock.Release()
	next = nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "wait for the active Dokploy host operation") || strings.Contains(next.Action, "commit --apply") {
		t.Fatalf("next step ignored the host-wide operation lock: %#v", next)
	}
	if phase := migrationRunPhase(run); phase != "host-busy" {
		t.Fatalf("phase=%q disagrees with next step %q", phase, next.Action)
	}
}

func TestCockpitBlocksCompletedOrRecoveredRunWithoutProvableOwner(t *testing.T) {
	for _, test := range []struct {
		name         string
		configure    func(*testing.T, migrationRun)
		wantReason   string
		manualTarget bool
	}{
		{name: "missing", configure: func(*testing.T, migrationRun) {}, wantReason: "no durable Dokploy host owner"},
		{name: "different", configure: func(t *testing.T, _ migrationRun) {
			other := migrationRun{Name: "other", RunDir: filepath.Join(t.TempDir(), "other"), CreatedAt: time.Now().UTC(), BundleDigest: "other-digest"}
			if err := claimDokployHostOwnership(other, "http://127.0.0.1:3030", dokployCredentialID("other-token")); err != nil {
				t.Fatal(err)
			}
			if err := markDokployTrafficTarget(other, "http://127.0.0.1:3030"); err != nil {
				t.Fatal(err)
			}
		}, wantReason: "ownership belongs to"},
		{name: "unreadable", configure: func(t *testing.T, _ migrationRun) {
			path, err := dokployTrafficOwnerPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantReason: "decode Dokploy traffic owner"},
		{name: "manual target missing", configure: func(*testing.T, migrationRun) {}, wantReason: "no durable host-wide Dokploy traffic owner", manualTarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDokployTrafficOwner(t)
			now := time.Now().UTC()
			meta := migrationRun{Name: test.name, RunDir: filepath.Join(t.TempDir(), test.name), Target: "dokploy", CreatedAt: now, BundleDigest: test.name + "-digest", CommitStartedAt: &now, CommittedAt: &now}
			if test.manualTarget {
				meta.CommitStartedAt = nil
				meta.CommittedAt = nil
				meta.ResolvedAuthority = dokployTrafficTarget
				meta.AuthorityResolvedAt = &now
				meta.LiveAppliedAt = &now
			}
			test.configure(t, meta)
			run := loadedMigrationRun{Run: meta, Applied: newRunApplied(meta)}
			if phase := migrationRunPhase(run); phase != "host-owner-error" {
				t.Fatalf("phase=%q, want host-owner-error", phase)
			}
			next := nextSafeStep(run, nil)
			if !strings.Contains(next.Action, "dokploy-traffic-owner.json") || !strings.Contains(next.Reason, test.wantReason) || strings.Contains(next.Action, "commit --apply") || strings.Contains(next.Action, "cleanup") {
				t.Fatalf("next step hid owner finalization failure: %#v", next)
			}
		})
	}
}

func TestCockpitRefusesAppliedActionsWithoutMatchingCurrentOwner(t *testing.T) {
	for _, tc := range []struct {
		name         string
		installOwner func(*testing.T)
	}{
		{name: "missing owner", installOwner: func(t *testing.T) {}},
		{name: "different owner", installOwner: func(t *testing.T) {
			ownerRun := migrationRun{Name: "other-run", RunDir: filepath.Join(t.TempDir(), "other-run"), CreatedAt: time.Now().UTC(), BundleDigest: "other-digest"}
			if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("other-token")); err != nil {
				t.Fatal(err)
			}
			if err := markDokployTrafficTarget(ownerRun, "http://127.0.0.1:3030"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDokployTrafficOwner(t)
			runMeta := migrationRun{Name: "applied-run", RunDir: filepath.Join(t.TempDir(), "applied-run"), Target: "dokploy", CreatedAt: time.Now().UTC(), BundleDigest: "applied-digest"}
			now := time.Now().UTC()
			run := loadedMigrationRun{
				Run:     runMeta,
				Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
				Applied: newRunApplied(runMeta),
			}
			run.Run.LiveAppliedAt = &now
			run.Applied.SucceededAt = &now
			tc.installOwner(t)

			if phase := migrationRunPhase(run); phase != "authority-ambiguous" {
				t.Fatalf("applied run without matching owner phase=%q, want authority-ambiguous", phase)
			}
			var output strings.Builder
			writeAppFirstCockpit(&output, run)
			if !strings.Contains(output.String(), "AUTHORITY UNKNOWN") || !strings.Contains(output.String(), "establish authority manually") || strings.Contains(output.String(), "commit --apply") || strings.Contains(output.String(), "rollback --live") {
				t.Fatalf("cockpit offered actions without a matching owner:\n%s", output.String())
			}
			next := nextSafeStep(run, nil)
			if !strings.Contains(next.Action, "establish writer and traffic authority manually") || strings.Contains(next.Action, "commit --apply") || strings.Contains(next.Action, "rollback --live") {
				t.Fatalf("next offered actions without a matching owner: %#v", next)
			}
		})
	}
}

func TestCockpitRefusesResumeAfterSuccessfulTargetMutationLosesOwner(t *testing.T) {
	resetDokployTrafficOwner(t)
	runMeta := migrationRun{Name: "owner-lost", RunDir: t.TempDir(), Target: "dokploy", CreatedAt: time.Now().UTC(), BundleDigest: "owner-lost-digest"}
	run := loadedMigrationRun{
		Run:     runMeta,
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Applied: newRunApplied(runMeta),
	}
	plan := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	if len(plan.Steps) == 0 {
		t.Fatal("expected target mutation step")
	}
	step := plan.Steps[0]
	run.Applied.Steps = []appliedStep{{Index: 0, Kind: string(step.Kind), App: step.App, Ref: step.Ref, Status: string(dokploy.StepStatusOK)}}
	if phase := migrationRunPhase(run); phase != "authority-ambiguous" {
		t.Fatalf("owner-lost partial run phase=%q, want authority-ambiguous", phase)
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "establish writer and traffic authority manually") || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("owner-lost partial run offered unsafe resume: %#v", next)
	}
}

func TestCockpitShowsDownstreamDecisionsAsReviewOnly(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "reviewed", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{Source: "docker", SourceDockerEngineID: "engine-reviewed", Apps: []preparer.AppPlan{{
			Name:      "api",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
		}}},
		Decisions: runDecisions{
			APIVersion: decisionsAPIVersion,
			Decisions: []runDecision{{
				ID:        "cutover",
				Kind:      "cutover",
				Readiness: preparer.ReadinessNeedsDecision,
				Items: []runDecisionItem{{
					Stage:     "cutover",
					App:       "api",
					Code:      "cutover.health_check_required",
					Readiness: preparer.ReadinessNeedsDecision,
				}},
			}},
		},
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{
		"READY",
		"Review-only decisions: 1 open (non-blocking before live apply)",
		"cutover needs_decision",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("expected cockpit to contain %q, got:\n%s", want, output.String())
		}
	}
}

func TestCockpitDirectsStatefulPlanToManualMigration(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "stateful", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
			}},
		},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	if phase := migrationRunPhase(run); phase != "ready" {
		t.Fatalf("staged stateful plan phase = %q, want ready", phase)
	}
	if next := nextSafeStep(run, nil); !strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("staged stateful run did not offer live apply: %#v", next)
	}
	run.Applied = runApplied{APIVersion: appliedAPIVersion, PlanVersion: appliedPlanV1Alpha2, Apps: map[string]appliedApp{"api": {}}}
	if phase := migrationRunPhase(run); phase != "manual-state" {
		t.Fatalf("in-place stateful ledger phase = %q, want manual-state", phase)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "MANUAL STATE") || !strings.Contains(output.String(), "cannot continue this run") || !strings.Contains(output.String(), "outside Bort") || strings.Contains(output.String(), "bort migrate --live") {
		t.Fatalf("stateful cockpit offered an unusable live action:\n%s", output.String())
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "do not rerun this blocked run") || !strings.Contains(next.Action, "outside Bort") || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("stateful next step offered an unusable live action: %#v", next)
	}
	run.Applied.Steps = []appliedStep{{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)}}
	if phase := migrationRunPhase(run); phase != "manual-state" {
		t.Fatalf("pre-pause stateful prefix phase = %q, want manual-state", phase)
	}
	if next := nextSafeStep(run, nil); strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("pre-pause stateful prefix offered an unusable live action: %#v", next)
	}
	run.Applied.Steps = append(run.Applied.Steps, appliedStep{Index: 1, Kind: string(dokploy.StepPauseSource), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)})
	if phase := migrationRunPhase(run); phase != "source-recovery" {
		t.Fatalf("paused stateful prefix phase = %q, want source-recovery", phase)
	}
	if next := nextSafeStep(run, nil); !strings.Contains(next.Action, "migrate --live") || !strings.Contains(next.Reason, "no state transfer") {
		t.Fatalf("paused stateful prefix did not offer cleanup-only recovery: %#v", next)
	}
	run.Applied.Steps[1].Kind = string(dokploy.StepResumeSource)
	if phase := migrationRunPhase(run); phase != "manual-state" {
		t.Fatalf("cleaned stateful prefix phase = %q, want manual-state", phase)
	}
}

func TestCockpitBlocksPlanLiveApplyWouldRefuse(t *testing.T) {
	app := preparer.AppPlan{
		Name:            "api",
		Resources:       preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api"}}},
	}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "bind", Source: "/srv/data", Target: "/data", SourceContainerID: "0123456789ab", SourceContainerName: "api"}}
	run := loadedMigrationRun{
		Run: migrationRun{Name: "bind", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps:                 []preparer.AppPlan{app},
		},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
	}
	if phase := migrationRunPhase(run); phase != "plan-blocked" {
		t.Fatalf("bind-mount staged plan phase = %q, want plan-blocked", phase)
	}
	next := nextSafeStep(run, nil)
	if !strings.Contains(next.Reason, "only named volumes are transferred before deploy") || !strings.Contains(next.Action, "re-plan") || strings.Contains(next.Action, "migrate --live") {
		t.Fatalf("bind-mount staged plan offered an unusable next step: %#v", next)
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "PLAN BLOCKED") || strings.Contains(output.String(), "READY") || strings.Contains(output.String(), "migrate --live") {
		t.Fatalf("bind-mount staged plan cockpit offered live apply:\n%s", output.String())
	}
	run.Applied = runApplied{APIVersion: appliedAPIVersion, PlanVersion: appliedPlanCurrent, TargetOrigin: "http://127.0.0.1:3000"}
	if phase := migrationRunPhase(run); phase != "plan-blocked" {
		t.Fatalf("bound zero-step blocked plan phase = %q, want plan-blocked", phase)
	}
	next = nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "cannot be re-planned") || strings.Contains(next.Action, "bort migrate") {
		t.Fatalf("bound zero-step blocked plan offered re-planning: %#v", next)
	}
	run.Applied.Steps = []appliedStep{{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)}}
	if phase := migrationRunPhase(run); phase != "partial" {
		t.Fatalf("partial run with a currently unstageable bundle phase = %q, want partial", phase)
	}
	if next := nextSafeStep(run, nil); !strings.Contains(next.Action, "resume the interrupted apply") || strings.Contains(next.Action, "re-plan") {
		t.Fatalf("partial run with a currently unstageable bundle lost its resume step: %#v", next)
	}
	run.Applied.Steps = append(run.Applied.Steps, appliedStep{Index: 1, Kind: string(dokploy.StepPauseSource), App: "api", Ref: "api", Status: string(dokploy.StepStatusError), Error: "staged restore preflight of db for app api failed, so pause_source did not stop the source", RequiresNewRun: true})
	next = nextSafeStep(run, nil)
	if strings.Contains(next.Action, "resume the interrupted apply") || !strings.Contains(next.Action, "recover-authority --authority source") || !strings.Contains(next.Action, "no state was transferred") || !strings.Contains(next.Reason, "did not stop the source") {
		t.Fatalf("pause_source preflight refusal offered resume instead of releasing the run: %#v", next)
	}
	if phase := migrationRunPhase(run); phase != "new-run-required" {
		t.Fatalf("pause_source preflight refusal phase = %q, want new-run-required", phase)
	}
	output.Reset()
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "NEW RUN REQUIRED") || strings.Contains(output.String(), "migrate --live") || strings.Contains(output.String(), "resume") || !strings.Contains(output.String(), "recover-authority --authority source") {
		t.Fatalf("pause_source preflight refusal cockpit still offered resume:\n%s", output.String())
	}
	refusal := run.Applied.Steps
	refusedProgress := dokploy.StepProgress{Index: 1, Step: dokploy.Step{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"}, Status: dokploy.StepStatusError, Err: errors.New("staged restore preflight of db for app api failed, so pause_source did not stop the source"), RequiresNewRun: true}
	resumeProgress := dokploy.StepProgress{Index: 1, Step: dokploy.Step{Kind: dokploy.StepResumeSource, App: "api", Ref: "api"}}
	replayed := runApplied{APIVersion: appliedAPIVersion, PlanVersion: appliedPlanCurrent, TargetOrigin: "http://127.0.0.1:3000"}
	replayed = recordAppliedStep(replayed, dokploy.StepProgress{Index: 0, Step: dokploy.Step{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"}, Status: dokploy.StepStatusOK})
	replayed = recordAppliedStep(replayed, refusedProgress)
	resumeProgress.Status = dokploy.StepStatusOK
	replayed = recordAppliedStep(replayed, resumeProgress)
	replayed = recordAppliedStep(replayed, refusedProgress)
	run.Applied = replayed
	if next := nextSafeStep(run, nil); !strings.Contains(next.Action, "no state was transferred") || !strings.Contains(next.Action, "recover-authority --authority source") {
		t.Fatalf("refusal re-recorded after restoring an earlier partial pause did not release the run: %#v", next)
	}
	replayed = recordAppliedStep(replayed, refusedProgress)
	resumeProgress.Status, resumeProgress.Err = dokploy.StepStatusError, errors.New("start source container web-id: timeout")
	run.Applied = recordAppliedStep(replayed, resumeProgress)
	if phase := migrationRunPhase(run); phase != "partial" {
		t.Fatalf("failed cleanup resume after a refusal phase = %q, want partial so the retry restores the source", phase)
	}
	if next := nextSafeStep(run, nil); !strings.Contains(next.Action, "resume the interrupted apply") {
		t.Fatalf("failed cleanup resume after a refusal must retry to restore the source: %#v", next)
	}
	run.Applied.Steps = append([]appliedStep{
		{Index: 0, Kind: string(dokploy.StepPauseSource), App: "web", Ref: "web", Status: string(dokploy.StepStatusOK)},
		{Index: 0, Kind: string(dokploy.StepResumeSource), App: "web", Ref: "web", Status: string(dokploy.StepStatusOK)},
	}, refusal...)
	if next := nextSafeStep(run, nil); !strings.Contains(next.Action, "no state was transferred") || !strings.Contains(next.Action, "recover-authority --authority source") {
		t.Fatalf("refusal after another app's restored pause did not release the run: %#v", next)
	}
	run.Applied.Steps = append([]appliedStep{
		{Index: 0, Kind: string(dokploy.StepPauseSource), App: "web", Ref: "web", Status: string(dokploy.StepStatusOK)},
		{Index: 1, Kind: string(dokploy.StepPushImage), App: "web", Ref: "web", Status: string(dokploy.StepStatusOK)},
	}, refusal...)
	if phase := migrationRunPhase(run); phase != "new-run-required" {
		t.Fatalf("refusal after another app's handoff phase = %q, want new-run-required", phase)
	}
	next = nextSafeStep(run, nil)
	if strings.Contains(next.Action, "resume the interrupted apply") || strings.Contains(next.Action, "no state was transferred") || strings.Contains(next.Action, "--authority") || !strings.Contains(next.Action, "restore the earlier apps' source writers and traffic manually") || !strings.Contains(next.Action, "create a fresh migration run") {
		t.Fatalf("refusal after another app's handoff released or resumed the run: %#v", next)
	}
	if !strings.Contains(next.Reason, "earlier app's source may already be paused or handed off") || !strings.Contains(next.Reason, "did not stop the source") {
		t.Fatalf("refusal after another app's handoff lost its reason: %#v", next)
	}
	output.Reset()
	writeAppFirstCockpit(&output, run)
	if !strings.Contains(output.String(), "NEW RUN REQUIRED") || strings.Contains(output.String(), "migrate --live") || strings.Contains(output.String(), "resume") {
		t.Fatalf("refusal after another app's handoff cockpit offered resume:\n%s", output.String())
	}
	resetDokployTrafficOwner(t)
	if err := claimDokployHostOwnership(run.Run, "http://127.0.0.1:3000", "cred-1"); err != nil {
		t.Fatal(err)
	}
	next = nextSafeStep(run, nil)
	if !strings.Contains(next.Action, "--authority source") || strings.Contains(next.Action, "--authority target") || !strings.Contains(next.Action, "create a fresh migration run") {
		t.Fatalf("refusal after another app's handoff with a claimed host must offer only source authority before a new run: %#v", next)
	}
	run.Applied.RecoveryProtocol = appliedRecoveryProtocol
	for name, moved := range map[string][]dokploy.StepKind{
		"staged restore": {dokploy.StepDumpDataStore, dokploy.StepRestoreDataStore},
		"staged sync":    {dokploy.StepSyncVolume},
	} {
		earlier := []appliedStep{{Index: 0, Kind: string(dokploy.StepPauseSource), App: "web", Ref: "web", Status: string(dokploy.StepStatusOK)}}
		for _, kind := range append(moved, dokploy.StepResumeSource, dokploy.StepPushImage) {
			earlier = append(earlier, appliedStep{Index: len(earlier), Kind: string(kind), App: "web", Ref: "web", Status: string(dokploy.StepStatusOK)})
		}
		refusedAPI := refusal[len(refusal)-1]
		refusedAPI.Index = len(earlier)
		run.Applied.Steps = append(earlier, refusedAPI)
		if phase := migrationRunPhase(run); phase != "new-run-required" {
			t.Fatalf("refusal after another app's %s and resumed source phase = %q, want new-run-required", name, phase)
		}
		next = nextSafeStep(run, nil)
		if strings.Contains(next.Action, "no state was transferred") || !strings.Contains(next.Action, "restore the earlier apps' source writers and traffic manually") || !strings.Contains(next.Action, "--authority source") || !strings.Contains(next.Reason, "earlier app's source may already be paused or handed off") {
			t.Fatalf("refusal after another app's %s and resumed source released the run: %#v", name, next)
		}
	}
}

func TestStagedTransferRefusalGatesPlannedPostgresDataDirOnApplyHistory(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /pg/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n"
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{
		Name:            "api",
		Directory:       "api",
		Resources:       preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api", ComposePath: "compose.yaml"}}},
	}
	app.Resources.DataStores = []preparer.DataStoreResource{{Kind: "postgres", Service: "db", Strategy: "migrate"}}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data", SourceContainerID: "0123456789ab", SourceContainerName: "api"}}
	run := loadedMigrationRun{
		Run: migrationRun{Name: "pgdata", RunDir: t.TempDir(), Target: "dokploy", Source: "docker"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			BundleDir:            bundleDir,
			Apps:                 []preparer.AppPlan{app},
		},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{ResourceType: "data_store", ResourceRef: "data-store:db"}}}}},
	}
	err := stagedTransferRefusal(run)
	if !errors.Is(err, dokploy.ErrNotImplemented) || !strings.Contains(err.Error(), "/pg/data") {
		t.Fatalf("a run without apply history must be refused before binding for a literal PGDATA off the staged volume, got %v", err)
	}
	if phase := migrationRunPhase(run); phase != "plan-blocked" {
		t.Fatalf("unstarted run with an unstaged PGDATA phase = %q, want plan-blocked", phase)
	}
	run.Applied = runApplied{APIVersion: appliedAPIVersion, PlanVersion: appliedPlanCurrent, TargetOrigin: "http://127.0.0.1:3000", Steps: []appliedStep{{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)}}}
	if err := stagedTransferRefusal(run); err != nil {
		t.Fatalf("a run with apply history must reach Apply so its pause_source preflight can restart an owned source, got %v", err)
	}
	if phase := migrationRunPhase(run); phase != "partial" {
		t.Fatalf("started run with an unstaged PGDATA phase = %q, want partial", phase)
	}
}

func TestWizardDoesNotRecommendLiveApplyForInPlaceStatefulRun(t *testing.T) {
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "stateful", RunDir: t.TempDir(), Target: "dokploy"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
		Applied: runApplied{APIVersion: appliedAPIVersion, PlanVersion: appliedPlanV1Alpha2, Apps: map[string]appliedApp{"api": {}}},
	}
	var output strings.Builder
	if err := runWizard(context.Background(), run, strings.NewReader(""), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "MANUAL STATE") || strings.Contains(output.String(), "migrate --live") {
		t.Fatalf("stateful wizard offered an unusable live action:\n%s", output.String())
	}
}

func TestWizardDoesNotRecommendLiveApplyWhileHostIsOwned(t *testing.T) {
	resetDokployTrafficOwner(t)
	run := loadedMigrationRun{
		Run: migrationRun{Name: "waiting-run", RunDir: t.TempDir(), Target: "dokploy", Source: "docker", CreatedAt: time.Now().UTC(), BundleDigest: "waiting-digest"},
		Prepare: preparer.Result{
			Source:               "docker",
			SourceDockerEngineID: "engine-reviewed",
			Apps: []preparer.AppPlan{{
				Name:      "api",
				Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{{ContainerID: "0123456789ab", ContainerName: "api"}}},
			}},
		},
	}
	var unowned strings.Builder
	if err := runWizard(context.Background(), run, strings.NewReader(""), &unowned, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unowned.String(), "Live apply is explicit") {
		t.Fatalf("wizard did not offer live apply for an unowned host:\n%s", unowned.String())
	}
	ownerRun := migrationRun{Name: "owner-run", RunDir: filepath.Join(t.TempDir(), "owner-run"), CreatedAt: time.Now().UTC(), BundleDigest: "owner-digest"}
	if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("owner-token")); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := runWizard(context.Background(), run, strings.NewReader(""), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "HOST OWNED") || strings.Contains(output.String(), "Live apply is explicit") {
		t.Fatalf("wizard offered live apply while another run owned the host:\n%s", output.String())
	}
}

func TestCockpitShowsDownstreamBlockersAsBlocking(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "blocked", RunDir: t.TempDir(), Target: "dokploy"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{
			Name: "api",
		}}},
		Decisions: runDecisions{
			APIVersion: decisionsAPIVersion,
			Decisions: []runDecision{{
				ID:        "cutover",
				Kind:      "cutover",
				Readiness: preparer.ReadinessBlocked,
				Items: []runDecisionItem{{
					Stage:     "cutover",
					App:       "api",
					Code:      "cutover.blocked",
					Readiness: preparer.ReadinessBlocked,
				}},
			}},
		},
	}
	var output strings.Builder
	writeAppFirstCockpit(&output, run)
	for _, want := range []string{"PLANNING", "Downstream blockers: 1 open", "cutover blocked"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("expected cockpit to contain %q, got:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "Review-only decisions") {
		t.Fatalf("downstream blocker was mislabeled as review-only:\n%s", output.String())
	}
}

func TestCockpitAndNextSurfaceApplyLockProbeErrors(t *testing.T) {
	for _, fixture := range []struct {
		name string
		link func(string, string) error
	}{
		{name: "symlink", link: os.Symlink},
		{name: "hard link", link: os.Link},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			workDir := t.TempDir()
			t.Chdir(workDir)
			bundleDir := filepath.Join(workDir, "bort-bundle")
			writeTestBundle(t, bundleDir, manifest.Manifest{
				Source: manifest.Source{Platform: "docker"},
				Apps:   []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}}},
			})
			runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "lock-error"})
			run, err := loadMigrationRun("lock-error")
			if err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(run.Run.RunDir, "lock-target")
			if err := os.WriteFile(targetPath, []byte("preserve\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fixture.link(targetPath, filepath.Join(run.Run.RunDir, "apply.lock")); err != nil {
				t.Skipf("%s fixture is unavailable: %v", fixture.name, err)
			}

			var output strings.Builder
			writeAppFirstCockpit(&output, run)
			if !strings.Contains(output.String(), "LOCK ERROR") || strings.Contains(output.String(), " READY") {
				t.Fatalf("expected cockpit to surface the apply-lock probe error, got:\n%s", output.String())
			}
			next := nextSafeStep(run, nil)
			if !strings.Contains(next.Action, "inspect the live-apply lock") || strings.Contains(next.Action, "migrate --live") || next.Reason == "" {
				t.Fatalf("expected next step to require lock inspection, got %#v", next)
			}
		})
	}
}
