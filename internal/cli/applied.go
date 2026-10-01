package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aikins01/bort/internal/target/dokploy"
)

const (
	appliedAPIVersion       = "bort.applied/v1alpha2"
	appliedLegacyAPIVersion = "bort.applied/v1alpha1"
	appliedRecoveryProtocol = "target-authority-v1"
	appliedPlanV1Alpha1     = "v1alpha1"
	appliedPlanV1Alpha2     = "v1alpha2"
	appliedPlanV1Alpha3     = "v1alpha3"
	appliedPlanV1Alpha4     = "v1alpha4"
	appliedPlanCurrent      = appliedPlanV1Alpha4
)

// runApplied is the per-run audit ledger. it captures the outcome of every
// step the live applier walks through so a partial run can be inspected and
// safely retried.
type runApplied struct {
	APIVersion       string                `json:"apiVersion"`
	RunName          string                `json:"runName"`
	BundleDir        string                `json:"bundleDir,omitempty"`
	Target           string                `json:"target,omitempty"`
	TargetOrigin     string                `json:"targetOrigin,omitempty"`
	UpdatedAt        time.Time             `json:"updatedAt"`
	SucceededAt      *time.Time            `json:"succeededAt,omitempty"`
	RecoveryProtocol string                `json:"recoveryProtocol,omitempty"`
	PlanVersion      string                `json:"planVersion,omitempty"`
	Steps            []appliedStep         `json:"steps,omitempty"`
	Apps             map[string]appliedApp `json:"apps,omitempty"`
}

type appliedStep struct {
	Index                     int       `json:"index"`
	Kind                      string    `json:"kind"`
	App                       string    `json:"app"`
	Ref                       string    `json:"ref"`
	Status                    string    `json:"status"`
	UpdatedAt                 time.Time `json:"updatedAt"`
	Error                     string    `json:"error,omitempty"`
	MutationAmbiguous         *bool     `json:"mutationAmbiguous,omitempty"`
	AuthorityRecoveryRequired bool      `json:"authorityRecoveryRequired,omitempty"`
	RequiresNewRun            bool      `json:"requiresNewRun,omitempty"`
}

type appliedApp struct {
	ProjectID      string `json:"projectId,omitempty"`
	EnvironmentID  string `json:"environmentId,omitempty"`
	ComposeID      string `json:"composeId,omitempty"`
	ComposeAppName string `json:"composeAppName,omitempty"`
}

func readRunApplied(path string, run migrationRun) (runApplied, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newRunApplied(run), nil
		}
		return runApplied{}, err
	}
	if len(contents) == 0 {
		return newRunApplied(run), nil
	}
	var applied runApplied
	if err := json.Unmarshal(contents, &applied); err != nil {
		return runApplied{}, fmt.Errorf("read %s: %w", path, err)
	}
	if applied.APIVersion != "" && applied.APIVersion != appliedAPIVersion && applied.APIVersion != appliedLegacyAPIVersion {
		return runApplied{}, fmt.Errorf("%s has unsupported apiVersion %q (want %q or %q)", path, applied.APIVersion, appliedAPIVersion, appliedLegacyAPIVersion)
	}
	switch applied.PlanVersion {
	case "", appliedPlanV1Alpha1, appliedPlanV1Alpha2, appliedPlanV1Alpha3, appliedPlanV1Alpha4:
	default:
		return runApplied{}, fmt.Errorf("%s has unsupported planVersion %q", path, applied.PlanVersion)
	}
	if err := validateRunAppliedIdentity(path, applied, run); err != nil {
		return runApplied{}, err
	}
	if applied.Apps == nil {
		applied.Apps = map[string]appliedApp{}
	}
	return applied, nil
}

func validateRunAppliedIdentity(path string, applied runApplied, run migrationRun) error {
	if applied.RunName != "" && run.Name != "" && applied.RunName != run.Name {
		return fmt.Errorf("%s belongs to run %q, not %q", path, applied.RunName, run.Name)
	}
	if applied.BundleDir != "" && run.BundleDir != "" && filepath.Clean(applied.BundleDir) != filepath.Clean(run.BundleDir) {
		return fmt.Errorf("%s belongs to bundle %q, not %q", path, applied.BundleDir, run.BundleDir)
	}
	if applied.Target != "" && run.Target != "" && applied.Target != run.Target {
		return fmt.Errorf("%s belongs to target %q, not %q", path, applied.Target, run.Target)
	}
	return nil
}

func newRunApplied(run migrationRun) runApplied {
	return runApplied{
		APIVersion:       appliedAPIVersion,
		RunName:          run.Name,
		BundleDir:        run.BundleDir,
		Target:           run.Target,
		UpdatedAt:        time.Now().UTC(),
		RecoveryProtocol: appliedRecoveryProtocol,
		PlanVersion:      appliedPlanCurrent,
		Apps:             map[string]appliedApp{},
	}
}

func writeRunApplied(path string, applied runApplied) error {
	applied = upgradeRunAppliedSchema(applied)
	applied.UpdatedAt = time.Now().UTC()
	return writeJSONArtifact(path, applied)
}

func appliedHasHistory(applied runApplied) bool {
	return len(applied.Steps) > 0 || len(applied.Apps) > 0 || applied.SucceededAt != nil
}

// upgradeRunAppliedSchema pins a ledger with history to the plan order it
// was recorded against; step indexes only line up under that order. an
// empty ledger has nothing to preserve and moves to the current order.
func upgradeRunAppliedSchema(applied runApplied) runApplied {
	applied.PlanVersion = appliedPlanVersion(applied)
	applied.APIVersion = appliedAPIVersion
	return applied
}

func recordAppliedStep(applied runApplied, progress dokploy.StepProgress) runApplied {
	step := appliedStep{
		Index:     progress.Index,
		Kind:      string(progress.Step.Kind),
		App:       progress.Step.App,
		Ref:       progress.Step.Ref,
		Status:    string(progress.Status),
		UpdatedAt: time.Now().UTC(),
	}
	if progress.Err != nil {
		step.Error = progress.Err.Error()
		ambiguous := progress.MutationAmbiguous
		step.MutationAmbiguous = &ambiguous
		step.AuthorityRecoveryRequired = progress.AuthorityRecoveryRequired
		step.RequiresNewRun = progress.RequiresNewRun
	}
	if progress.Step.App != "" && progress.Target != nil {
		if applied.Apps == nil {
			applied.Apps = map[string]appliedApp{}
		}
		app := applied.Apps[progress.Step.App]
		app.ProjectID = progress.Target.ProjectID
		app.EnvironmentID = progress.Target.EnvironmentID
		app.ComposeID = progress.Target.ComposeID
		app.ComposeAppName = progress.Target.ComposeAppName
		applied.Apps[progress.Step.App] = app
	}
	for i := range applied.Steps {
		if applied.Steps[i].Index == progress.Index {
			applied.Steps[i] = step
			return applied
		}
	}
	applied.Steps = append(applied.Steps, step)
	sort.Slice(applied.Steps, func(i, j int) bool { return applied.Steps[i].Index < applied.Steps[j].Index })
	return applied
}

// appliedLedger collects step results from concurrent dokploy.OnProgress
// callbacks and persists them to disk. it is safe for concurrent calls.
type appliedLedger struct {
	mu      sync.Mutex
	path    string
	state   runApplied
	persist func(string, runApplied) error
	err     error
}

func newAppliedLedger(path string, run migrationRun) (*appliedLedger, error) {
	state, err := readRunApplied(path, run)
	if err != nil {
		return nil, err
	}
	return &appliedLedger{path: path, state: state, persist: writeRunApplied}, nil
}

func (l *appliedLedger) Record(progress dokploy.StepProgress) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.state = upgradeRunAppliedSchema(recordAppliedStep(l.state, progress))
	if err := l.persist(l.path, l.state); err != nil {
		l.err = err
		return err
	}
	return nil
}

func (l *appliedLedger) PrepareRetry(index int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	next := l.state
	next.Steps = make([]appliedStep, 0, len(l.state.Steps))
	for _, step := range l.state.Steps {
		if step.Index < index {
			next.Steps = append(next.Steps, step)
		}
	}
	if (index == 0 && !appliedHasHistory(next)) || appliedV1Alpha3UsesCurrentPlan(l.state) {
		next.PlanVersion = appliedPlanCurrent
	}
	if len(next.Steps) == len(l.state.Steps) && next.RecoveryProtocol == appliedRecoveryProtocol && next.APIVersion == appliedAPIVersion && next.PlanVersion == l.state.PlanVersion {
		return nil
	}
	next.SucceededAt = nil
	next.RecoveryProtocol = appliedRecoveryProtocol
	next = upgradeRunAppliedSchema(next)
	if err := l.persist(l.path, next); err != nil {
		l.err = err
		return err
	}
	l.state = next
	return nil
}

func (l *appliedLedger) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *appliedLedger) MarkSucceeded() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	now := time.Now().UTC()
	l.state.SucceededAt = &now
	l.state = upgradeRunAppliedSchema(l.state)
	if err := l.persist(l.path, l.state); err != nil {
		l.err = err
		return err
	}
	return nil
}

func (l *appliedLedger) Snapshot() runApplied {
	l.mu.Lock()
	defer l.mu.Unlock()
	clone := l.state
	clone.Steps = append([]appliedStep{}, l.state.Steps...)
	clone.Apps = map[string]appliedApp{}
	for k, v := range l.state.Apps {
		clone.Apps[k] = v
	}
	return clone
}

func (l *appliedLedger) BindTargetOrigin(origin string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if err := validateAppliedTargetOrigin(l.state, origin); err != nil {
		return err
	}
	if l.state.TargetOrigin == origin && l.state.APIVersion == appliedAPIVersion {
		return nil
	}
	next := upgradeRunAppliedSchema(l.state)
	next.TargetOrigin = origin
	if err := l.persist(l.path, next); err != nil {
		l.err = err
		return err
	}
	l.state = next
	return nil
}

func (l *appliedLedger) ValidateTargetOrigin(origin string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	return validateAppliedTargetOrigin(l.state, origin)
}

func validateAppliedTargetOrigin(applied runApplied, origin string) error {
	if applied.TargetOrigin != "" && applied.TargetOrigin != origin {
		return fmt.Errorf("migration run is bound to Dokploy origin %s, not %s", applied.TargetOrigin, origin)
	}
	if applied.TargetOrigin == "" && (len(applied.Steps) > 0 || len(applied.Apps) > 0 || applied.SucceededAt != nil) {
		return fmt.Errorf("migration run has prior live execution but no persisted Dokploy origin; refusing to bind it to %s because the original target cannot be proved; preserve the existing target and start a fresh migration run", origin)
	}
	return nil
}

func completedApplyPrefix(steps []dokploy.Step, applied runApplied) int {
	prefix := indexedCompletedApplyPrefix(steps, applied)
	if prefix >= len(steps) {
		return prefix
	}
	if applyMayHaveAmbiguousAuthority(applied) {
		return prefix
	}
	if applied.RecoveryProtocol == appliedRecoveryProtocol {
		return prefix
	}
	pauseByApp := map[string]int{}
	transferredApps := map[string]struct{}{}
	for index, step := range steps[:prefix] {
		switch {
		case step.Kind == dokploy.StepPauseSource:
			pauseByApp[step.App] = index
			delete(transferredApps, step.App)
		case step.Kind == dokploy.StepDumpDataStore || step.Kind == dokploy.StepRestoreDataStore || step.Kind == dokploy.StepSyncVolume:
			if _, paused := pauseByApp[step.App]; paused {
				transferredApps[step.App] = struct{}{}
			}
		case step.Kind == dokploy.StepResumeSource:
			delete(pauseByApp, step.App)
			delete(transferredApps, step.App)
		}
	}
	for app := range transferredApps {
		if pauseIndex := pauseByApp[app]; pauseIndex < prefix {
			prefix = pauseIndex
		}
	}
	return prefix
}

// appliedPlanVersion resolves the plan order a ledger's step indexes were
// recorded against. a ledger without history has nothing pinned and
// follows the current order.
func appliedPlanVersion(applied runApplied) string {
	switch {
	case !appliedHasHistory(applied):
		return appliedPlanCurrent
	case applied.APIVersion == appliedLegacyAPIVersion || applied.PlanVersion == appliedPlanV1Alpha1:
		return appliedPlanV1Alpha1
	case applied.PlanVersion == "" && applied.APIVersion == appliedAPIVersion:
		return appliedPlanV1Alpha2
	case applied.PlanVersion == "":
		return appliedPlanCurrent
	default:
		return applied.PlanVersion
	}
}

func livePlanForApplied(run loadedMigrationRun, applied runApplied) dokploy.Plan {
	switch appliedPlanVersion(applied) {
	case appliedPlanV1Alpha1:
		return dokploy.LegacyPlanFromArtifactsV1Alpha1(run.Prepare, run.Sync, run.Cutover)
	case appliedPlanV1Alpha2:
		return dokploy.PlanFromArtifactsV1Alpha2(run.Prepare, run.Sync, run.Cutover)
	case appliedPlanV1Alpha3:
		if appliedV1Alpha3UsesCurrentPlan(applied) {
			return dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
		}
		return dokploy.LegacyPlanFromArtifactsV1Alpha3(run.Prepare, run.Sync, run.Cutover)
	default:
		return dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	}
}

func appliedV1Alpha3UsesCurrentPlan(applied runApplied) bool {
	return appliedPlanVersion(applied) == appliedPlanV1Alpha3 &&
		applied.SucceededAt == nil &&
		!appliedV1Alpha3StateTransferMayHaveRun(applied)
}

// appliedPlanTransfersStateInPlace reports whether the ledger is pinned to
// a plan order that deploys the target before copying state into its
// volumes. Bort no longer continues such runs past the stateful steps.
func appliedPlanTransfersStateInPlace(applied runApplied) bool {
	switch appliedPlanVersion(applied) {
	case appliedPlanV1Alpha1, appliedPlanV1Alpha2:
		return true
	default:
		return false
	}
}

type applyAuthorityAmbiguity int

const (
	applyAuthorityUnambiguous applyAuthorityAmbiguity = iota
	applyTrafficAuthorityAmbiguous
	applyWriterAuthorityAmbiguous
)

func appliedAuthorityAmbiguity(applied runApplied) applyAuthorityAmbiguity {
	if applied.SucceededAt != nil {
		return applyAuthorityUnambiguous
	}
	for _, recorded := range applied.Steps {
		if recorded.AuthorityRecoveryRequired {
			return applyWriterAuthorityAmbiguous
		}
	}
	if appliedPlanVersion(applied) == appliedPlanV1Alpha3 && appliedV1Alpha3StateTransferMayHaveRun(applied) {
		return applyWriterAuthorityAmbiguous
	}
	if applied.APIVersion == appliedAPIVersion && applied.RecoveryProtocol == appliedRecoveryProtocol {
		for _, recorded := range applied.Steps {
			if appliedStepMutatesDokploy(recorded.Kind) &&
				appliedTargetMutationAmbiguous(recorded) {
				return applyWriterAuthorityAmbiguous
			}
		}
		return applyAuthorityUnambiguous
	}
	ambiguity := applyAuthorityUnambiguous
	for _, recorded := range applied.Steps {
		if recorded.Kind == string(dokploy.StepStartDokployProxy) && recorded.Ref == "dokploy-traefik" &&
			appliedStepMayHaveRun(recorded) {
			ambiguity = applyTrafficAuthorityAmbiguous
		}
		if (recorded.Kind == string(dokploy.StepResumeTarget) || recorded.Kind == string(dokploy.StepActivateRoutes)) &&
			recorded.App != "" && appliedStepMayHaveRun(recorded) {
			return applyWriterAuthorityAmbiguous
		}
		if (recorded.Kind == string(dokploy.StepSyncVolume) || recorded.Kind == string(dokploy.StepRestoreDataStore)) &&
			appliedStepMayHaveRun(recorded) {
			return applyWriterAuthorityAmbiguous
		}
		if recorded.Kind == string(dokploy.StepPushImage) && appliedStepInterrupted(recorded) {
			return applyWriterAuthorityAmbiguous
		}
	}
	return ambiguity
}

func appliedV1Alpha3StateTransferMayHaveRun(applied runApplied) bool {
	for _, recorded := range applied.Steps {
		kind := dokploy.StepKind(recorded.Kind)
		if (kind == dokploy.StepRestoreDataStore || kind == dokploy.StepSyncVolume) && appliedStepMayHaveRun(recorded) {
			return true
		}
	}
	return false
}

func appliedStagingTransferApps(applied runApplied) []string {
	apps := map[string]struct{}{}
	for _, recorded := range applied.Steps {
		kind := dokploy.StepKind(recorded.Kind)
		if recorded.App != "" && (kind == dokploy.StepRestoreDataStore || kind == dokploy.StepSyncVolume) && appliedStepMayHaveRun(recorded) {
			apps[recorded.App] = struct{}{}
		}
	}
	result := make([]string, 0, len(apps))
	for app := range apps {
		result = append(result, app)
	}
	sort.Strings(result)
	return result
}

func appliedStepMutatesDokploy(kind string) bool {
	switch dokploy.StepKind(kind) {
	case dokploy.StepCreateProject, dokploy.StepCreateService, dokploy.StepUploadEnv, dokploy.StepPushImage, dokploy.StepInstallGateway, dokploy.StepActivateRoutes:
		return true
	default:
		return false
	}
}

func applyMayHaveAmbiguousAuthority(applied runApplied) bool {
	return appliedAuthorityAmbiguity(applied) != applyAuthorityUnambiguous
}

func appliedStepInterrupted(step appliedStep) bool {
	return step.Status == string(dokploy.StepStatusStarted) || step.Status == string(dokploy.StepStatusError)
}

func appliedTargetMutationAmbiguous(step appliedStep) bool {
	if step.Status == string(dokploy.StepStatusStarted) {
		return true
	}
	if step.Status != string(dokploy.StepStatusError) {
		return false
	}
	return step.MutationAmbiguous == nil || *step.MutationAmbiguous
}

func appliedStepMayHaveRun(step appliedStep) bool {
	return step.Status == string(dokploy.StepStatusStarted) ||
		step.Status == string(dokploy.StepStatusError) ||
		step.Status == string(dokploy.StepStatusOK)
}

func appliedTargetIdentities(applied runApplied) map[string]dokploy.TargetIdentity {
	identities := map[string]dokploy.TargetIdentity{}
	for name, app := range applied.Apps {
		if app.ProjectID == "" && app.EnvironmentID == "" && app.ComposeID == "" && app.ComposeAppName == "" {
			continue
		}
		identities[name] = dokploy.TargetIdentity{
			ProjectID:      app.ProjectID,
			EnvironmentID:  app.EnvironmentID,
			ComposeID:      app.ComposeID,
			ComposeAppName: app.ComposeAppName,
		}
	}
	return identities
}
func validateApplyResumeAuthority(applied runApplied) error {
	switch appliedAuthorityAmbiguity(applied) {
	case applyAuthorityUnambiguous:
		return nil
	case applyTrafficAuthorityAmbiguous:
		return fmt.Errorf("live-apply state cannot prove traffic authority after the shared proxy handoff: inspect the Coolify and Dokploy proxies and routes and establish traffic authority manually")
	default:
		return fmt.Errorf("live-apply state cannot prove whether a target deployment was accepted or target writers changed state: inspect and preserve both sides and establish writer and traffic authority manually")
	}
}

func runMayHaveAmbiguousAuthority(run loadedMigrationRun) bool {
	if run.Run.ResolvedAuthority != "" && run.Run.AuthorityResolvedAt != nil {
		return false
	}
	if run.Run.CommittedAt != nil || run.Run.RolledBackAt != nil || run.Run.PurgedAt != nil {
		return false
	}
	if completedLegacyRunMissingTargetOrigin(run) {
		return true
	}
	missingOwner, ownerErr := targetMutationMissingDurableOwner(run)
	if ownerErr != nil || missingOwner {
		return true
	}
	if completedLegacyRun(run) {
		return false
	}
	return applyMayHaveAmbiguousAuthority(run.Applied)
}

func completedLegacyRunMissingTargetOrigin(run loadedMigrationRun) bool {
	if !completedLegacyRun(run) {
		return false
	}
	plan := dokploy.LegacyPlanFromArtifactsV1Alpha1(run.Prepare, run.Sync, run.Cutover)
	for _, step := range plan.Steps {
		if step.Kind == dokploy.StepDumpDataStore || step.Kind == dokploy.StepRestoreDataStore || step.Kind == dokploy.StepSyncVolume {
			return true
		}
	}
	return run.Applied.TargetOrigin == "" && trafficHandoffStepIndex(plan) >= 0
}

func completedLegacyRun(run loadedMigrationRun) bool {
	if appliedPlanVersion(run.Applied) != appliedPlanV1Alpha1 {
		return false
	}
	if run.Applied.SucceededAt != nil {
		return true
	}
	plan := dokploy.LegacyPlanFromArtifactsV1Alpha1(run.Prepare, run.Sync, run.Cutover)
	return len(plan.Steps) > 0 && completedApplyPrefix(plan.Steps, run.Applied) == len(plan.Steps)
}

func indexedCompletedApplyPrefix(steps []dokploy.Step, applied runApplied) int {
	byIndex := map[int]appliedStep{}
	for _, step := range applied.Steps {
		if step.Index < 0 || step.Index >= len(steps) {
			continue
		}
		byIndex[step.Index] = step
	}
	for index, step := range steps {
		recorded, ok := byIndex[index]
		if !ok || !appliedStepCompleted(recorded) {
			return index
		}
		if !appliedStepMatches(recorded, step) && !legacyProxyCleanupPreservesStop(step, recorded, byIndex, index) {
			return index
		}
	}
	return len(steps)
}

func legacyProxyCleanupPreservesStop(planned dokploy.Step, recorded appliedStep, byIndex map[int]appliedStep, index int) bool {
	if planned.Kind != dokploy.StepStopCoolifyProxy || recorded.Kind != string(dokploy.StepStartCoolifyProxy) || recorded.Ref != planned.Ref {
		return false
	}
	for laterIndex, later := range byIndex {
		if laterIndex > index && appliedStepCompleted(later) {
			return true
		}
	}
	return false
}

func appliedStepCompleted(step appliedStep) bool {
	return step.Status == string(dokploy.StepStatusOK) || step.Status == string(dokploy.StepStatusSkipped)
}

func appliedStepMatches(recorded appliedStep, step dokploy.Step) bool {
	return recorded.Kind == string(step.Kind) && recorded.App == step.App && recorded.Ref == step.Ref
}
