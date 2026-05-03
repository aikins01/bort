package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	run := migrationRun{Name: "marketmap", RunDir: filepath.ToSlash(runDir), UpdatedAt: now, DryRun: true}
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
