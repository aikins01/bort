package cli

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestAppliedLedgerRecordsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "run-1", BundleDir: "bundle", Target: "dokploy"})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	if err := ledger.Record(dokploy.StepProgress{
		Index:  0,
		Total:  2,
		Step:   dokploy.Step{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"},
		Status: dokploy.StepStatusOK,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := ledger.Record(dokploy.StepProgress{
		Index:  1,
		Total:  2,
		Step:   dokploy.Step{Kind: dokploy.StepCreateService, App: "api", Ref: "api"},
		Status: dokploy.StepStatusError,
		Err:    errors.New("boom"),
	}); err != nil {
		t.Fatalf("record err: %v", err)
	}

	reloaded, err := readRunApplied(path, migrationRun{Name: "run-1"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.RunName != "run-1" {
		t.Fatalf("expected runName run-1, got %q", reloaded.RunName)
	}
	if len(reloaded.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d: %#v", len(reloaded.Steps), reloaded.Steps)
	}
	if reloaded.Steps[0].Status != "ok" || reloaded.Steps[0].Kind != string(dokploy.StepCreateProject) {
		t.Fatalf("unexpected step 0: %#v", reloaded.Steps[0])
	}
	if reloaded.Steps[1].Status != "error" || reloaded.Steps[1].Error != "boom" {
		t.Fatalf("unexpected step 1: %#v", reloaded.Steps[1])
	}
}

func TestAppliedLedgerOverwritesByIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "run-2"})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	first := dokploy.StepProgress{Index: 3, Step: dokploy.Step{Kind: dokploy.StepUploadEnv, App: "api"}, Status: dokploy.StepStatusError}
	second := dokploy.StepProgress{Index: 3, Step: dokploy.Step{Kind: dokploy.StepUploadEnv, App: "api"}, Status: dokploy.StepStatusOK}
	if err := ledger.Record(first); err != nil {
		t.Fatalf("record first: %v", err)
	}
	if err := ledger.Record(second); err != nil {
		t.Fatalf("record second: %v", err)
	}
	snap := ledger.Snapshot()
	if len(snap.Steps) != 1 {
		t.Fatalf("expected single step, got %d: %#v", len(snap.Steps), snap.Steps)
	}
	if snap.Steps[0].Status != "ok" {
		t.Fatalf("expected retry to overwrite to ok, got %#v", snap.Steps[0])
	}
}

func TestAppliedLedgerRetainsPersistenceFailure(t *testing.T) {
	ledger := &appliedLedger{
		state: newRunApplied(migrationRun{Name: "run-failure"}),
		persist: func(string, runApplied) error {
			return errors.New("persist failed")
		},
	}
	progress := dokploy.StepProgress{Index: 0, Step: dokploy.Step{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"}, Status: dokploy.StepStatusStarted}
	if err := ledger.Record(progress); err == nil || err.Error() != "persist failed" {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	if err := ledger.Err(); err == nil || err.Error() != "persist failed" {
		t.Fatalf("expected retained persistence failure, got %v", err)
	}
	if err := ledger.Record(progress); err == nil || err.Error() != "persist failed" {
		t.Fatalf("expected later records to stop at the retained failure, got %v", err)
	}
	if err := ledger.MarkSucceeded(); err == nil || err.Error() != "persist failed" {
		t.Fatalf("expected success marking to stop at the retained failure, got %v", err)
	}
}

func TestAppliedLedgerPersistsSuccessfulOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	run := migrationRun{Name: "run-success", BundleDir: "bundle", Target: "dokploy"}
	ledger, err := newAppliedLedger(path, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkSucceeded(); err != nil {
		t.Fatal(err)
	}
	applied, err := readRunApplied(path, run)
	if err != nil {
		t.Fatal(err)
	}
	if applied.SucceededAt == nil {
		t.Fatal("expected a durable successful live-apply outcome")
	}
}

func TestReadRunAppliedRejectsUnknownAPIVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied.json")
	if err := writeJSONArtifact(path, runApplied{APIVersion: "bort.applied/v999", RunName: "run-3"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readRunApplied(path, migrationRun{Name: "run-3"}); err == nil {
		t.Fatalf("expected error on unsupported apiVersion")
	}
}

func TestReadRunAppliedRejectsMismatchedRunIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied.json")
	if err := writeJSONArtifact(path, runApplied{APIVersion: appliedAPIVersion, RunName: "other", BundleDir: "bundle", Target: "dokploy"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readRunApplied(path, migrationRun{Name: "current", BundleDir: "bundle", Target: "dokploy"}); err == nil {
		t.Fatalf("expected mismatched run identity to be rejected")
	}
}

func TestCompletedApplyPrefixStopsAtFirstIncompleteOrMismatchedStep(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"},
		{Kind: dokploy.StepCreateService, App: "api", Ref: "api"},
		{Kind: dokploy.StepPushImage, App: "api", Ref: "api"},
		{Kind: dokploy.StepInstallGateway, App: "api", Ref: "api.example.com"},
	}
	applied := runApplied{Steps: []appliedStep{
		{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
		{Index: 1, Kind: string(dokploy.StepCreateService), App: "api", Ref: "api", Status: string(dokploy.StepStatusSkipped)},
		{Index: 2, Kind: string(dokploy.StepPushImage), App: "api", Ref: "api", Status: string(dokploy.StepStatusError)},
		{Index: 4, Kind: string(dokploy.StepResumeSource), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
	}}
	if got := completedApplyPrefix(steps, applied); got != 2 {
		t.Fatalf("expected prefix 2, got %d", got)
	}

	applied.Steps[2].Status = string(dokploy.StepStatusOK)
	applied.Steps = append(applied.Steps, appliedStep{Index: 3, Kind: string(dokploy.StepInstallGateway), App: "api", Ref: "wrong.example.com", Status: string(dokploy.StepStatusOK)})
	if got := completedApplyPrefix(steps, applied); got != 3 {
		t.Fatalf("expected prefix 3 for mismatched gateway, got %d", got)
	}
}

func TestCompletedApplyPrefixRequiresContiguousIndexedSteps(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepCreateProject, App: "api", Ref: "api"},
		{Kind: dokploy.StepCreateService, App: "api", Ref: "api"},
	}
	applied := runApplied{Steps: []appliedStep{
		{Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
		{Index: 1, Kind: string(dokploy.StepCreateProject), App: "removed", Ref: "removed", Status: string(dokploy.StepStatusOK)},
		{Index: 2, Kind: string(dokploy.StepCreateService), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
	}}
	if got := completedApplyPrefix(steps, applied); got != 1 {
		t.Fatalf("expected prefix to stop at mismatched index 1, got %d", got)
	}
}
