package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
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
				Code:      "cutover.review",
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
	reviewItem := runDecisionItem{Stage: "cutover", App: "api", Code: "cutover.review", Readiness: preparer.ReadinessNeedsDecision}
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
	if got != "Applied: 3 step(s) recorded · 2 ok · 1 failed" {
		t.Fatalf("unexpected footer: %q", got)
	}
}

func TestCockpitShowsDownstreamDecisionsAsReviewOnly(t *testing.T) {
	run := loadedMigrationRun{
		Run: migrationRun{Name: "reviewed", RunDir: t.TempDir(), Target: "dokploy"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{{
			Name: "api",
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
					Code:      "cutover.review",
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
