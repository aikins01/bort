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
