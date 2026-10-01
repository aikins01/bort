package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
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

func TestAppliedLedgerRewindRemovesStaleSuffixBeforeReplay(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"},
		{Kind: dokploy.StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepResumeTarget, App: "api", Ref: "api"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	for index, step := range steps {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  0,
		Step:   dokploy.Step{Kind: dokploy.StepResumeSource, App: "api", Ref: "api"},
		Status: dokploy.StepStatusOK,
	})
	ledger := &appliedLedger{state: applied, persist: func(string, runApplied) error { return nil }}

	resumeFrom := completedApplyPrefix(steps, ledger.Snapshot())
	if resumeFrom != 0 {
		t.Fatalf("expected cleanup to rewind to source pause, got %d", resumeFrom)
	}
	if err := ledger.PrepareRetry(resumeFrom); err != nil {
		t.Fatalf("rewind ledger: %v", err)
	}
	if err := ledger.Record(dokploy.StepProgress{Index: 0, Step: steps[0], Status: dokploy.StepStatusOK}); err != nil {
		t.Fatalf("record replayed pause: %v", err)
	}
	if got := completedApplyPrefix(steps, ledger.Snapshot()); got != 1 {
		t.Fatalf("expected a second retry to stop before the stale transfer suffix, got prefix %d", got)
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

func TestAppliedLedgerBindsTargetOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	ledger, err := newAppliedLedger(path, migrationRun{Name: "origin", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindTargetOrigin("https://dokploy.example/api"); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Snapshot().TargetOrigin; got != "https://dokploy.example/api" {
		t.Fatalf("unexpected normalized origin %q", got)
	}
	if err := ledger.BindTargetOrigin("https://dokploy.example/api"); err != nil {
		t.Fatalf("equivalent origin was rejected: %v", err)
	}
	if err := ledger.BindTargetOrigin("https://dokploy.example/"); err == nil || !strings.Contains(err.Error(), "bound to Dokploy origin") {
		t.Fatalf("expected path-routed target mismatch, got %v", err)
	}
	if err := ledger.BindTargetOrigin("https://other.example"); err == nil || !strings.Contains(err.Error(), "bound to Dokploy origin") {
		t.Fatalf("expected target-origin mismatch, got %v", err)
	}
}

func TestAppliedLedgerRefusesToInventOriginForPriorExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	run := migrationRun{Name: "legacy-origin", Target: "dokploy"}
	if err := writeJSONArtifact(path, runApplied{
		APIVersion: appliedLegacyAPIVersion,
		RunName:    run.Name,
		Target:     run.Target,
		Steps: []appliedStep{{
			Index: 0, Kind: string(dokploy.StepCreateProject), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := newAppliedLedger(path, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindTargetOrigin("https://dokploy.example"); err == nil || !strings.Contains(err.Error(), "original target cannot be proved") {
		t.Fatalf("expected missing legacy origin to fail closed, got %v", err)
	}
}

func TestLegacyProxyOnlyAmbiguityReportsTrafficRecovery(t *testing.T) {
	applied := runApplied{
		APIVersion: appliedLegacyAPIVersion,
		Steps: []appliedStep{{
			Kind:   string(dokploy.StepStartDokployProxy),
			Ref:    "dokploy-traefik",
			Status: string(dokploy.StepStatusStarted),
		}},
	}
	err := validateApplyResumeAuthority(applied)
	if err == nil || !strings.Contains(err.Error(), "cannot prove traffic authority") || !strings.Contains(err.Error(), "inspect the Coolify and Dokploy proxies") {
		t.Fatalf("expected proxy-specific recovery guidance, got %v", err)
	}
	if strings.Contains(err.Error(), "target data") || strings.Contains(err.Error(), "writer authority") {
		t.Fatalf("proxy-only interruption reported writer or data ambiguity: %v", err)
	}
}

func TestWriteRunAppliedUpgradesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	legacy := runApplied{
		APIVersion: appliedLegacyAPIVersion,
		Steps: []appliedStep{{
			Index: 0, Kind: string(dokploy.StepSyncVolume), App: "api", Status: string(dokploy.StepStatusError),
		}},
	}
	if err := writeRunApplied(path, legacy); err != nil {
		t.Fatal(err)
	}
	upgraded, err := readRunApplied(path, migrationRun{})
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.APIVersion != appliedAPIVersion || upgraded.PlanVersion != appliedPlanV1Alpha1 {
		t.Fatalf("legacy ledger did not retain its plan version during upgrade: %#v", upgraded)
	}
}

func TestPrepareRetryUsesCurrentPlanForUnexecutedLegacyLedger(t *testing.T) {
	persisted := runApplied{}
	ledger := &appliedLedger{
		state: runApplied{
			APIVersion:       appliedAPIVersion,
			PlanVersion:      appliedPlanV1Alpha1,
			RecoveryProtocol: appliedRecoveryProtocol,
			Apps:             map[string]appliedApp{},
		},
		persist: func(_ string, applied runApplied) error {
			persisted = applied
			return nil
		},
	}
	if err := ledger.PrepareRetry(0); err != nil {
		t.Fatal(err)
	}
	if persisted.PlanVersion != appliedPlanCurrent || ledger.Snapshot().PlanVersion != appliedPlanCurrent {
		t.Fatalf("unexecuted legacy ledger retained stale plan version: persisted=%#v snapshot=%#v", persisted, ledger.Snapshot())
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
	if err := writeJSONArtifact(path, runApplied{APIVersion: appliedAPIVersion, PlanVersion: "v1alpha9", RunName: "run-3"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readRunApplied(path, migrationRun{Name: "run-3"}); err == nil || !strings.Contains(err.Error(), "planVersion") {
		t.Fatalf("expected error on unsupported planVersion, got %v", err)
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

func TestCompletedApplyPrefixIgnoresStandaloneProxyCompensation(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepActivateRoutes, App: "api", Ref: "routes"},
		{Kind: dokploy.StepStartDokployProxy, Ref: "dokploy-traefik"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	for index, step := range steps[:2] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 2, Step: steps[2], Status: dokploy.StepStatusError})
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  len(steps),
		Step:   dokploy.Step{Kind: dokploy.StepStartCoolifyProxy, Ref: "coolify-proxy"},
		Status: dokploy.StepStatusOK,
	})
	if got := completedApplyPrefix(steps, applied); got != 2 {
		t.Fatalf("proxy compensation rewound completed route activation: prefix=%d", got)
	}
}

func TestCompletedApplyPrefixAcceptsLegacyProxyCompensationWhenLaterProgressProvesStop(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepActivateRoutes, App: "api", Ref: "routes"},
		{Kind: dokploy.StepStartDokployProxy, Ref: "dokploy-traefik"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 0, Step: steps[0], Status: dokploy.StepStatusOK})
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 1, Step: steps[1], Status: dokploy.StepStatusOK})
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 2, Step: steps[2], Status: dokploy.StepStatusError})
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  0,
		Step:   dokploy.Step{Kind: dokploy.StepStartCoolifyProxy, Ref: "coolify-proxy"},
		Status: dokploy.StepStatusOK,
	})
	if got := completedApplyPrefix(steps, applied); got != 2 {
		t.Fatalf("legacy proxy compensation rewound completed route activation: prefix=%d", got)
	}
}

func TestCompletedApplyPrefixRejectsUnprovedLegacyProxyCompensation(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepActivateRoutes, App: "api", Ref: "routes"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  0,
		Step:   dokploy.Step{Kind: dokploy.StepStartCoolifyProxy, Ref: "coolify-proxy"},
		Status: dokploy.StepStatusOK,
	})
	if got := completedApplyPrefix(steps, applied); got != 0 {
		t.Fatalf("unproved proxy stop was accepted: prefix=%d", got)
	}
}

func TestCompletedApplyPrefixReplaysStateTransferAfterSourceCleanup(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"},
		{Kind: dokploy.StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepInstallGateway, App: "api", Ref: "api.example.com"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	for index, step := range steps[:2] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  0,
		Step:   dokploy.Step{Kind: dokploy.StepResumeSource, App: "api", Ref: "api"},
		Status: dokploy.StepStatusStarted,
	})

	if got := completedApplyPrefix(steps, applied); got != 0 {
		t.Fatalf("expected retry to replay from source pause after cleanup, got prefix %d", got)
	}
}

func TestCompletedApplyPrefixUsesCurrentCleanupMarkerBoundary(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepPauseSource, App: "alpha", Ref: "alpha"},
		{Kind: dokploy.StepSyncVolume, App: "alpha", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepPauseSource, App: "beta", Ref: "beta"},
		{Kind: dokploy.StepSyncVolume, App: "beta", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepResumeTarget, App: "alpha", Ref: "alpha"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	for index, step := range steps[:5] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 5, Step: steps[5], Status: dokploy.StepStatusError})
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  2,
		Step:   dokploy.Step{Kind: dokploy.StepResumeSource, App: "beta", Ref: "beta"},
		Status: dokploy.StepStatusOK,
	})

	if got := completedApplyPrefix(steps, applied); got != 2 {
		t.Fatalf("expected beta's cleanup marker to be the replay boundary without rewinding alpha, got %d", got)
	}
}

func TestCompletedApplyPrefixPreservesAmbiguousLegacySourceCleanup(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepCreateService, App: "api", Ref: "api"},
		{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"},
		{Kind: dokploy.StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepInstallGateway, App: "api", Ref: "api.example.com"},
	}
	applied := runApplied{}
	for index, step := range steps[:3] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 3, Step: steps[3], Status: dokploy.StepStatusError, Err: errors.New("injected gateway failure")})
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  len(steps),
		Step:   dokploy.Step{Kind: dokploy.StepResumeSource, App: "api", Ref: "api"},
		Status: dokploy.StepStatusOK,
	})

	if !applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("expected legacy cleanup after state transfer to preserve possible target writes")
	}
	if got := completedApplyPrefix(steps, applied); got != 3 {
		t.Fatalf("expected ambiguous legacy cleanup to stop at the failed step without rewinding state, got prefix %d", got)
	}
}

func TestCompletedApplyPrefixPreservesIncompleteLegacyLogicalTransfer(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepCreateService, App: "api", Ref: "api"},
		{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"},
		{Kind: dokploy.StepDumpDataStore, App: "api", Ref: "data-store:db"},
		{Kind: dokploy.StepRestoreDataStore, App: "api", Ref: "data-store:db"},
		{Kind: dokploy.StepInstallGateway, App: "api", Ref: "api.example.com"},
	}
	applied := runApplied{}
	for index, step := range steps[:4] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{
		Index:  4,
		Step:   steps[4],
		Status: dokploy.StepStatusError,
		Err:    errors.New("injected gateway failure"),
	})

	if !applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("expected incomplete legacy restore to preserve possible target writes")
	}
	if got := completedApplyPrefix(steps, applied); got != 4 {
		t.Fatalf("expected incomplete legacy logical transfer to stop at the failed step without rewinding state, got prefix %d", got)
	}
}

func TestCompletedApplyPrefixPreservesMultipleAmbiguousLegacyTransfers(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepCreateService, App: "alpha", Ref: "alpha"},
		{Kind: dokploy.StepPauseSource, App: "alpha", Ref: "alpha"},
		{Kind: dokploy.StepSyncVolume, App: "alpha", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepPauseSource, App: "beta", Ref: "beta"},
		{Kind: dokploy.StepSyncVolume, App: "beta", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepInstallGateway, App: "alpha", Ref: "alpha.example.com"},
	}
	applied := runApplied{}
	for index, step := range steps[:5] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 5, Step: steps[5], Status: dokploy.StepStatusError})

	if !applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("expected incomplete legacy transfers to preserve possible target writes")
	}
	if got := completedApplyPrefix(steps, applied); got != 5 {
		t.Fatalf("expected ambiguous legacy transfers to stop at the failed step without rewinding state, got %d", got)
	}
}

func TestCompletedApplyPrefixDoesNotReplayCurrentProtocolWithoutSourceCleanup(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"},
		{Kind: dokploy.StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepStartDokployProxy, Ref: "dokploy-traefik"},
	}
	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
	for index, step := range steps[:3] {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
	}
	applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 3, Step: steps[3], Status: dokploy.StepStatusStarted})

	if got := completedApplyPrefix(steps, applied); got != 3 {
		t.Fatalf("expected current recovery protocol to resume target handoff without replaying source state, got %d", got)
	}
}

func TestLegacyCutoverHandoffIsAmbiguous(t *testing.T) {
	steps := []dokploy.Step{
		{Kind: dokploy.StepPauseSource, App: "api", Ref: "api"},
		{Kind: dokploy.StepStopCoolifyProxy, Ref: "coolify-proxy"},
		{Kind: dokploy.StepStartDokployProxy, Ref: "dokploy-traefik"},
	}
	for _, tc := range []struct {
		name     string
		api      string
		protocol string
		status   dokploy.StepStatus
		want     bool
	}{
		{name: "legacy started", status: dokploy.StepStatusStarted, want: true},
		{name: "legacy error", status: dokploy.StepStatusError, want: true},
		{name: "current started", api: appliedAPIVersion, protocol: appliedRecoveryProtocol, status: dokploy.StepStatusStarted, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied := runApplied{APIVersion: tc.api, RecoveryProtocol: tc.protocol}
			for index, step := range steps[:2] {
				applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
			}
			applied = recordAppliedStep(applied, dokploy.StepProgress{Index: 2, Step: steps[2], Status: tc.status})

			if got := applyMayHaveAmbiguousAuthority(applied); got != tc.want {
				t.Fatalf("applyMayHaveAmbiguousAuthority() = %t, want %t", got, tc.want)
			}
			if !tc.want {
				return
			}
			if err := validateApplyResumeAuthority(applied); err == nil || !strings.Contains(err.Error(), "shared proxy handoff") || !strings.Contains(err.Error(), "establish traffic authority manually") {
				t.Fatalf("expected actionable traffic-authority refusal, got %v", err)
			}
			if got := completedApplyPrefix(steps, applied); got != 2 {
				t.Fatalf("expected ambiguous legacy handoff to keep the completed prefix, got %d", got)
			}
		})
	}
}

func TestLegacyCutoverHandoffSurvivesCurrentPlanReordering(t *testing.T) {
	applied := runApplied{Steps: []appliedStep{{
		Index:  1,
		Kind:   string(dokploy.StepStartDokployProxy),
		Ref:    "dokploy-traefik",
		Status: string(dokploy.StepStatusStarted),
	}}}
	if !applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("expected legacy proxy ambiguity to survive current plan reordering")
	}
}

func TestLegacyTargetResumeBeforeProxyHandoffIsAmbiguous(t *testing.T) {
	applied := runApplied{Steps: []appliedStep{
		{Index: 0, Kind: string(dokploy.StepPauseSource), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
		{Index: 1, Kind: string(dokploy.StepResumeTarget), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
	}}
	if !applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("expected legacy target resume to require authority reconciliation")
	}
	if err := validateApplyResumeAuthority(applied); err == nil {
		t.Fatal("expected legacy target resume to refuse automatic replay")
	}
}

func TestLegacyStateTransferWithoutTargetResumeIsAmbiguous(t *testing.T) {
	applied := runApplied{Steps: []appliedStep{
		{Index: 0, Kind: string(dokploy.StepPauseSource), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
		{Index: 1, Kind: string(dokploy.StepSyncVolume), App: "api", Ref: "volume:web -> /data", Status: string(dokploy.StepStatusOK)},
		{Index: 2, Kind: string(dokploy.StepInstallGateway), App: "api", Ref: "api.example.com", Status: string(dokploy.StepStatusError)},
	}}
	if !applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("expected old failure cleanup after state transfer to require authority reconciliation")
	}
	if err := validateApplyResumeAuthority(applied); err == nil {
		t.Fatal("expected legacy state transfer to refuse automatic replay")
	}
}

func TestCompletedLegacyApplyRetainsRollbackEligibility(t *testing.T) {
	now := time.Now().UTC()
	applied := runApplied{
		SucceededAt: &now,
		Steps: []appliedStep{
			{Index: 0, Kind: string(dokploy.StepSyncVolume), App: "api", Ref: "volume:web -> /data", Status: string(dokploy.StepStatusOK)},
			{Index: 1, Kind: string(dokploy.StepResumeTarget), App: "api", Ref: "api", Status: string(dokploy.StepStatusOK)},
			{Index: 2, Kind: string(dokploy.StepStartDokployProxy), Ref: "dokploy-traefik", Status: string(dokploy.StepStatusOK)},
		},
	}
	if applyMayHaveAmbiguousAuthority(applied) {
		t.Fatal("completed legacy apply was classified as authority-ambiguous")
	}
	if err := validateApplyResumeAuthority(applied); err != nil {
		t.Fatalf("completed legacy apply lost rollback eligibility: %v", err)
	}
}

func TestCurrentDeploymentErrorUsesPersistedAmbiguity(t *testing.T) {
	retryable := false
	ambiguous := true
	for _, kind := range []dokploy.StepKind{
		dokploy.StepCreateProject,
		dokploy.StepCreateService,
		dokploy.StepUploadEnv,
		dokploy.StepPushImage,
		dokploy.StepInstallGateway,
		dokploy.StepActivateRoutes,
	} {
		t.Run(string(kind), func(t *testing.T) {
			base := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol}
			for _, test := range []struct {
				name      string
				status    dokploy.StepStatus
				ambiguous *bool
				wantError bool
			}{
				{name: "interrupted before terminal record", status: dokploy.StepStatusStarted, wantError: true},
				{name: "old error without discriminator", status: dokploy.StepStatusError, wantError: true},
				{name: "ambiguous mutation response", status: dokploy.StepStatusError, ambiguous: &ambiguous, wantError: true},
				{name: "authoritative or pre-mutation error", status: dokploy.StepStatusError, ambiguous: &retryable},
				{name: "completed", status: dokploy.StepStatusOK},
			} {
				t.Run(test.name, func(t *testing.T) {
					applied := base
					applied.Steps = []appliedStep{{
						Index: 0, Kind: string(kind), App: "api", Ref: "api", Status: string(test.status), MutationAmbiguous: test.ambiguous,
					}}
					err := validateApplyResumeAuthority(applied)
					if test.wantError && (err == nil || !strings.Contains(err.Error(), "cannot prove whether a target deployment was accepted")) {
						t.Fatalf("expected ambiguous deployment refusal, got %v", err)
					}
					if !test.wantError && err != nil {
						t.Fatalf("retryable deployment failure remained ambiguous: %v", err)
					}
				})
			}
		})
	}
}

func TestRecordAppliedStepPersistsExplicitMutationAmbiguity(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		applied := recordAppliedStep(runApplied{}, dokploy.StepProgress{
			Index:             1,
			Step:              dokploy.Step{Kind: dokploy.StepPushImage, App: "api", Ref: "api"},
			Status:            dokploy.StepStatusError,
			Err:               errors.New("deploy failed"),
			MutationAmbiguous: ambiguous,
		})
		if len(applied.Steps) != 1 || applied.Steps[0].MutationAmbiguous == nil || *applied.Steps[0].MutationAmbiguous != ambiguous {
			t.Fatalf("persisted mutation ambiguity = %#v, want %t", applied.Steps, ambiguous)
		}
	}
}

func TestRecordAppliedStepPersistsTargetIdentity(t *testing.T) {
	applied := recordAppliedStep(runApplied{}, dokploy.StepProgress{
		Index:  1,
		Step:   dokploy.Step{Kind: dokploy.StepCreateService, App: "api", Ref: "api"},
		Status: dokploy.StepStatusOK,
		Target: &dokploy.TargetIdentity{ProjectID: "project-1", EnvironmentID: "environment-1", ComposeID: "compose-1", ComposeAppName: "stack-api"},
	})
	identity := applied.Apps["api"]
	if identity.ProjectID != "project-1" || identity.EnvironmentID != "environment-1" || identity.ComposeID != "compose-1" || identity.ComposeAppName != "stack-api" {
		t.Fatalf("unexpected persisted target identity: %#v", identity)
	}
}

func TestPersistedV1Alpha3StateTransferUsesLegacyPlanAndRequiresAuthorityRecovery(t *testing.T) {
	app := preparer.AppPlan{
		Name: "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			ComposeApp: preparer.DokployComposeApp{Name: "api"},
		}},
	}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Name: "data", Target: "/data"}}
	run := loadedMigrationRun{
		Run:     migrationRun{Name: "legacy-staged", RunDir: t.TempDir(), Target: "dokploy"},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Sync: syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{
			ResourceType: "volume",
			ResourceRef:  "volume:web -> /data",
			Strategy:     syncplan.StrategyDockerVolumeArchive,
		}}}}},
		Cutover: gateway.Result{},
	}
	legacy := dokploy.LegacyPlanFromArtifactsV1Alpha3(run.Prepare, run.Sync, run.Cutover)
	preTransfer := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol, PlanVersion: appliedPlanV1Alpha3, RunName: run.Run.Name, Target: run.Run.Target}
	for index, step := range legacy.Steps {
		preTransfer = recordAppliedStep(preTransfer, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
		if step.Kind == dokploy.StepPauseSource {
			break
		}
	}
	safePlan := livePlanForApplied(run, preTransfer)
	if slices.ContainsFunc(safePlan.Steps, func(step dokploy.Step) bool { return step.Kind == dokploy.StepResumeSource }) {
		t.Fatalf("pre-transfer v1alpha3 run kept the unsafe legacy source resume: %#v", safePlan.Steps)
	}
	safePrefix := completedApplyPrefix(safePlan.Steps, preTransfer)
	if safePrefix >= len(safePlan.Steps) || safePlan.Steps[safePrefix].Kind != dokploy.StepSyncVolume {
		t.Fatalf("pre-transfer v1alpha3 run did not retain its completed prefix under the safe plan: prefix=%d steps=%#v", safePrefix, safePlan.Steps)
	}
	path := filepath.Join(run.Run.RunDir, "applied.json")
	if err := writeRunApplied(path, preTransfer); err != nil {
		t.Fatal(err)
	}
	ledger, err := newAppliedLedger(path, run.Run)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.PrepareRetry(safePrefix); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Snapshot().PlanVersion; got != appliedPlanCurrent {
		t.Fatalf("prepared legacy ledger planVersion=%q, want %q", got, appliedPlanCurrent)
	}
	if err := ledger.Record(dokploy.StepProgress{Index: safePrefix, Step: safePlan.Steps[safePrefix], Status: dokploy.StepStatusStarted}); err != nil {
		t.Fatal(err)
	}
	restarted, err := readRunApplied(path, run.Run)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.PlanVersion != appliedPlanCurrent || len(restarted.Steps) != len(preTransfer.Steps)+1 || restarted.Steps[len(restarted.Steps)-1].Kind != string(dokploy.StepSyncVolume) {
		t.Fatalf("restarted ledger lost its current-plan transfer: %#v", restarted)
	}
	restartedPlan := livePlanForApplied(run, restarted)
	if slices.ContainsFunc(restartedPlan.Steps, func(step dokploy.Step) bool { return step.Kind == dokploy.StepResumeSource }) {
		t.Fatalf("repinned ledger reverted to the legacy source resume after restart: %#v", restartedPlan.Steps)
	}
	if err := validateApplyResumeAuthority(restarted); err != nil {
		t.Fatalf("repinned current-plan transfer became ambiguous after restart: %v", err)
	}

	applied := runApplied{APIVersion: appliedAPIVersion, RecoveryProtocol: appliedRecoveryProtocol, PlanVersion: appliedPlanV1Alpha3, RunName: run.Run.Name, Target: run.Run.Target}
	for index, step := range legacy.Steps {
		applied = recordAppliedStep(applied, dokploy.StepProgress{Index: index, Step: step, Status: dokploy.StepStatusOK})
		if step.Kind == dokploy.StepSyncVolume {
			break
		}
	}
	if err := writeRunApplied(path, applied); err != nil {
		t.Fatal(err)
	}
	loaded, err := readRunApplied(path, run.Run)
	if err != nil {
		t.Fatal(err)
	}
	plan := livePlanForApplied(run, loaded)
	if !slices.Equal(plan.Steps, legacy.Steps) {
		t.Fatalf("v1alpha3 ledger did not reconstruct its historical plan: got %#v want %#v", plan.Steps, legacy.Steps)
	}
	prefix := completedApplyPrefix(plan.Steps, loaded)
	if prefix >= len(plan.Steps) || plan.Steps[prefix].Kind != dokploy.StepResumeSource {
		t.Fatalf("v1alpha3 resume lost its legacy step indexes: prefix=%d steps=%#v", prefix, plan.Steps)
	}
	if err := validateApplyResumeAuthority(loaded); err == nil || !strings.Contains(err.Error(), "establish writer and traffic authority manually") {
		t.Fatalf("v1alpha3 transfer without a durable pin was allowed to resume: %v", err)
	}
}
