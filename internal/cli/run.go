package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/planfile"
	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
	"github.com/aikins01/bort/internal/source/localdocker"
	syncplan "github.com/aikins01/bort/internal/sync"
	"github.com/aikins01/bort/internal/target/dokploy"
)

const (
	runAPIVersion       = "bort.run/v1alpha1"
	decisionsAPIVersion = "bort.decisions/v1alpha1"
)

var (
	errRunOperationActive             = errors.New("migration run operation already in progress")
	errDokployLiveOperationActive     = errors.New("another Dokploy live operation is already running on this host")
	errDokployInstallRecoveryRequired = errors.New("an interrupted Dokploy installation requires recovery; rerun the same init-target --install command before other host mutations")
)

type migrationRun struct {
	APIVersion                string       `json:"apiVersion"`
	Name                      string       `json:"name"`
	RunDir                    string       `json:"runDir"`
	CreatedAt                 time.Time    `json:"createdAt"`
	UpdatedAt                 time.Time    `json:"updatedAt"`
	LiveAppliedAt             *time.Time   `json:"liveAppliedAt,omitempty"`
	CommitStartedAt           *time.Time   `json:"commitStartedAt,omitempty"`
	CommittedAt               *time.Time   `json:"committedAt,omitempty"`
	RollbackStartedAt         *time.Time   `json:"rollbackStartedAt,omitempty"`
	RolledBackAt              *time.Time   `json:"rolledBackAt,omitempty"`
	PurgedAt                  *time.Time   `json:"purgedAt,omitempty"`
	ResolvedAuthority         string       `json:"resolvedAuthority,omitempty"`
	AuthorityResolvedAt       *time.Time   `json:"authorityResolvedAt,omitempty"`
	AuthorityFinalizedAt      *time.Time   `json:"authorityFinalizedAt,omitempty"`
	HostOwnerReleaseStartedAt *time.Time   `json:"hostOwnerReleaseStartedAt,omitempty"`
	ApplyOutcomeRequired      bool         `json:"applyOutcomeRequired,omitempty"`
	Source                    string       `json:"source,omitempty"`
	BundleDir                 string       `json:"bundleDir"`
	BundleDigest              string       `json:"bundleDigest,omitempty"`
	SourceBundleDir           string       `json:"sourceBundleDir,omitempty"`
	ManifestPath              string       `json:"manifest,omitempty"`
	Target                    string       `json:"target"`
	AppName                   string       `json:"app,omitempty"`
	DryRun                    bool         `json:"dryRun"`
	ObservationWindowSeconds  int          `json:"observationWindowSeconds"`
	RollbackWindowSeconds     int          `json:"rollbackWindowSeconds"`
	Artifacts                 runArtifacts `json:"artifacts"`
}

type runArtifacts struct {
	Prepare   string `json:"prepare"`
	Sync      string `json:"sync"`
	Cutover   string `json:"cutover"`
	Rollback  string `json:"rollback"`
	Commit    string `json:"commit"`
	Decisions string `json:"decisions"`
	Progress  string `json:"progress,omitempty"`
	Applied   string `json:"applied,omitempty"`
}

type loadedMigrationRun struct {
	Run       migrationRun
	Prepare   preparer.Result
	Sync      syncplan.Result
	Cutover   gateway.Result
	Rollback  rollbackplan.Result
	Commit    commitplan.Result
	Decisions runDecisions
	Progress  runProgress
	Applied   runApplied
}

type runDecisions struct {
	APIVersion  string        `json:"apiVersion"`
	RunName     string        `json:"runName"`
	RunDir      string        `json:"runDir"`
	GeneratedAt time.Time     `json:"generatedAt"`
	BundleDir   string        `json:"bundleDir"`
	Target      string        `json:"target"`
	AppName     string        `json:"app,omitempty"`
	DryRun      bool          `json:"dryRun"`
	Decisions   []runDecision `json:"decisions"`
}

type runDecision struct {
	ID        string             `json:"id"`
	Kind      string             `json:"kind"`
	Status    string             `json:"status"`
	Readiness preparer.Readiness `json:"readiness"`
	Action    string             `json:"action"`
	Reason    string             `json:"reason"`
	Apps      []string           `json:"apps"`
	Codes     []string           `json:"codes"`
	Count     int                `json:"count"`
	Items     []runDecisionItem  `json:"items"`
}

type runDecisionItem struct {
	Stage       string             `json:"stage"`
	App         string             `json:"app"`
	Code        string             `json:"code"`
	ResourceRef string             `json:"resourceRef,omitempty"`
	Message     string             `json:"message"`
	Readiness   preparer.Readiness `json:"readiness"`
	Artifact    string             `json:"artifact"`
	Evidence    []string           `json:"evidence,omitempty"`
}

type migrationRunSummary struct {
	Run            migrationRun
	Status         preparer.Status
	Readiness      preparer.Readiness
	Apps           int
	PlatformApps   int
	StatusCounts   runStatusCounts
	GateCounts     runReadinessCounts
	CutoverRoutes  int
	RollbackRoutes int
	CommitRoutes   int
	StateSteps     int
	PauseSteps     int
	FirstGates     []runGateSummary
	Decisions      []runDecision
	Progress       string
	Next           runNextStep
}

type runStatusCounts struct {
	Green  int
	Yellow int
	Red    int
}

type runReadinessCounts struct {
	Ready         int
	NeedsDecision int
	NeedsInput    int
	Blocked       int
}

type runGateSummary struct {
	Stage       string
	Artifact    string
	App         string
	Code        string
	Message     string
	Readiness   preparer.Readiness
	ResourceRef string
	Evidence    []string
}

type runNextStep struct {
	Action     string
	Reason     string
	Artifact   string
	DecisionID string
	Phase      string
}

type migrationRunOptions struct {
	BundleDir                string
	Target                   string
	AppName                  string
	RunRef                   string
	Source                   string
	ManifestPath             string
	ObservationWindowSeconds int
	RollbackWindowSeconds    int
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var runRef string
	var sourceName string
	var manifestPath string
	var live bool
	observationWindowSeconds := gateway.DefaultObservationWindowSeconds
	rollbackWindowSeconds := gateway.DefaultRollbackWindowSeconds

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to include in the run")
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.StringVar(&sourceName, "source", "", "scan a source directly: docker, coolify, coolify-local, or manifest")
	fs.StringVar(&manifestPath, "manifest", "", "existing manifest path (implies --source manifest)")
	fs.BoolVar(&live, "live", false, "execute against the target platform (default dry-run only)")
	fs.IntVar(&observationWindowSeconds, "observation-window", gateway.DefaultObservationWindowSeconds, "observation window in seconds")
	fs.IntVar(&rollbackWindowSeconds, "rollback-window", gateway.DefaultRollbackWindowSeconds, "rollback window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("migrate does not accept positional argument %q", fs.Arg(0))
	}
	bundleFlagSet := flagSet(fs, "bundle")
	if live {
		for _, name := range []string{"app", "bundle", "manifest", "observation-window", "rollback-window", "source", "target"} {
			if flagSet(fs, name) {
				return fmt.Errorf("--live applies an existing planned run and does not accept --%s; create or update the dry-run first, then apply it with --run or the current-run default", name)
			}
		}
	}
	if strings.TrimSpace(manifestPath) != "" && strings.TrimSpace(sourceName) == "" {
		sourceName = "manifest"
	}
	if strings.TrimSpace(sourceName) != "" {
		if bundleFlagSet {
			return fmt.Errorf("migrate accepts either --source/--manifest or --bundle, not both")
		}
		if appName != "" {
			return fmt.Errorf("--app is only supported with --bundle; source scans create a complete run")
		}
		if live {
			return fmt.Errorf("--live cannot create a run from --source; run the dry-run command first, review it with `%s`, then apply", bortCommand(""))
		}
		if sourceName == "manifest" && strings.TrimSpace(manifestPath) == "" {
			return fmt.Errorf("--manifest is required with --source manifest")
		}
		if sourceName != "manifest" && strings.TrimSpace(manifestPath) != "" {
			return fmt.Errorf("--manifest can only be used with --source manifest")
		}
		if strings.TrimSpace(runRef) == "" {
			runRef = defaultGuideRunName(sourceName, time.Now().UTC())
		}
		loadedRun, err := createMigrationRunFromSource(ctx, guidedSetup{
			Source:       sourceName,
			Target:       target,
			RunName:      runRef,
			ManifestPath: manifestPath,
		}, observationWindowSeconds, rollbackWindowSeconds)
		if err != nil {
			rerunArgs := []string{"migrate"}
			for _, arg := range args {
				rerunArgs = append(rerunArgs, shellQuote(arg))
			}
			return wrapGuideScanFailure(err, strings.Join(rerunArgs, " "))
		}
		if err := rememberCurrentRun(loadedRun.Run); err != nil {
			return err
		}
		writeAppFirstCockpitContext(ctx, stdout, loadedRun)
		return nil
	}

	opts := migrationRunOptions{
		BundleDir:                bundleDir,
		Target:                   target,
		AppName:                  appName,
		RunRef:                   runRef,
		ObservationWindowSeconds: observationWindowSeconds,
		RollbackWindowSeconds:    rollbackWindowSeconds,
	}
	if live {
		activeRef, ok, err := activeApplyRunRefForMigrate(opts, bundleFlagSet)
		if err != nil {
			return err
		}
		if ok {
			loadedRun, err := loadMigrationRun(activeRef)
			if err != nil {
				return err
			}
			if err := rememberCurrentRun(loadedRun.Run); err != nil {
				return err
			}
			summary := summarizeMigrationRunForLive(ctx, loadedRun)
			writeLiveMigrationRunText(stdout, "Migration run loaded", summary)
			return attachLiveMigrationRun(ctx, loadedRun, stderr, nil)
		}
		resolved, err := resolveRunRef(opts.RunRef, false)
		if err != nil {
			return err
		}
		if !migrationRunMetadataExists(resolved) {
			return fmt.Errorf("migration run %q does not exist; run `%s` to create or select a run before --live", resolved, bortCommand(""))
		}
		operationLock, err := acquireRunOperationLock(resolved)
		if err != nil {
			applyActive, applyActiveErr := applyRunActive(resolved)
			if applyActiveErr != nil {
				return fmt.Errorf("check live migration for run %q: %w", resolved, applyActiveErr)
			}
			if errors.Is(err, errRunOperationActive) && applyActive {
				loadedRun, loadErr := loadMigrationRun(resolved)
				if loadErr != nil {
					return loadErr
				}
				writeLiveMigrationRunText(stdout, "Migration run loaded", summarizeMigrationRunForLive(ctx, loadedRun))
				return attachLiveMigrationRun(ctx, loadedRun, stderr, nil)
			}
			return fmt.Errorf("start live migration for run %q: %w", resolved, err)
		}
		defer operationLock.Release()
		loadedRun, err := loadMigrationRun(resolved)
		if err != nil {
			return err
		}
		if err := rememberCurrentRun(loadedRun.Run); err != nil {
			return err
		}
		writeLiveMigrationRunText(stdout, "Migration run loaded", summarizeMigrationRunForLive(ctx, loadedRun))
		if err := validateLiveApplyReady(loadedRun); err != nil {
			return err
		}
		return applyLiveMigrationLocked(ctx, loadedRun, stderr, nil)
	}

	loadedRun, err := migrationRunForMigrateCommand(opts, bundleFlagSet)
	if err != nil {
		return err
	}
	if err := rememberCurrentRun(loadedRun.Run); err != nil {
		return err
	}

	summary := summarizeMigrationRunContext(ctx, loadedRun)
	writeMigrationRunText(stdout, "Migration run created", summary)
	return nil
}

func migrationRunForMigrateCommand(opts migrationRunOptions, bundleFlagSet bool) (loadedMigrationRun, error) {
	if strings.TrimSpace(opts.RunRef) != "" && !bundleFlagSet && migrationRunMetadataExists(opts.RunRef) {
		return refreshMigrationRunSafely(opts.RunRef)
	}
	return createMigrationRun(opts)
}

func activeApplyRunRefForMigrate(opts migrationRunOptions, bundleFlagSet bool) (string, bool, error) {
	if strings.TrimSpace(opts.RunRef) != "" && migrationRunMetadataExists(opts.RunRef) {
		active, err := applyRunActive(opts.RunRef)
		if err != nil {
			return "", false, err
		}
		if active {
			return opts.RunRef, true, nil
		}
	}
	if strings.TrimSpace(opts.RunRef) == "" && !bundleFlagSet {
		current, err := resolveRunRef("", false)
		if err != nil {
			return "", false, err
		}
		active, err := applyRunActive(current)
		if err != nil {
			return "", false, err
		}
		if active {
			return current, true, nil
		}
	}
	return "", false, nil
}

func refreshMigrationRunSafely(runRef string) (loadedMigrationRun, error) {
	operationLock, err := acquireRunOperationLock(runRef)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	defer operationLock.Release()
	return refreshMigrationRunSafelyLocked(runRef)
}

func refreshMigrationRunSafelyLocked(runRef string) (loadedMigrationRun, error) {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	run, err := readRunMetadata(filepath.Join(runDir, "run.json"))
	if err != nil {
		return loadedMigrationRun{}, err
	}
	liveExecutionStarted, err := runHasStartedLiveExecution(run)
	if err != nil {
		return loadedMigrationRun{}, fmt.Errorf("read applied migration progress: %w", err)
	}
	if run.LiveAppliedAt != nil || run.CommitStartedAt != nil || run.CommittedAt != nil || run.RollbackStartedAt != nil || run.RolledBackAt != nil || run.PurgedAt != nil || liveExecutionStarted {
		return loadMigrationRun(runRef)
	}
	return refreshMigrationRunLocked(runRef)
}

func validateLiveApplyReady(run loadedMigrationRun) error {
	if run.Run.RolledBackAt != nil {
		return fmt.Errorf("live apply refused: run %q was rolled back; start a fresh migration run to migrate again", run.Run.Name)
	}
	if run.Run.RollbackStartedAt != nil {
		return fmt.Errorf("live apply refused: rollback started for run %q; %s", run.Run.Name, incompleteRollbackRecovery(run))
	}
	if run.Run.CommitStartedAt != nil {
		return fmt.Errorf("live apply refused: source retirement started for run %q; %s", run.Run.Name, finishStartedAcceptanceAction(run))
	}
	if run.Run.ResolvedAuthority != "" {
		return fmt.Errorf("live apply refused: run %q has a manual %s-authority resolution; finish that recovery instead of replaying the ambiguous apply", run.Run.Name, run.Run.ResolvedAuthority)
	}
	if err := validateStatefulLiveApply(run); err != nil {
		if len(interruptedStatefulSourceCleanup(run)) > 0 {
			return nil
		}
		return err
	}
	if decisions := liveApplyBlockingDecisions(run); len(decisions) > 0 {
		decision := decisions[0]
		action := strings.TrimSpace(decision.Action)
		if action == "" {
			action = "resolve the open migration decision"
		}
		return fmt.Errorf("live apply is blocked by %d unresolved requirement(s); next safe step: %s (run `%s` to review this run)", len(decisions), action, runScopedCommand(run, "status"))
	}
	if hasOnlyPlatformRunApps(run) {
		return fmt.Errorf("live apply refused: run %q has no migratable applications; platform-role entries are excluded from live apply", run.Run.Name)
	}
	return nil
}

func stagedTransferRefusal(run loadedMigrationRun) error {
	return stagedTransferRefusalForPlan(livePlanForApplied(run, run.Applied), run.Applied)
}

func stagedTransferRefusalForPlan(plan dokploy.Plan, applied runApplied) error {
	if err := dokploy.ValidateStagedTransfer(plan); err != nil {
		return err
	}
	if appliedHasHistory(applied) {
		return nil
	}
	return dokploy.ValidatePlannedPostgresDataDirs(plan)
}

// validateStatefulLiveApply refuses to continue a stateful run recorded
// under a plan order that copied state into the deployed target: Dokploy
// cannot durably keep a queued or future deployment from restarting
// target writers mid-copy. current plans transfer state into Bort-owned
// staging volumes before the target exists and need no such guard.
func validateStatefulLiveApply(run loadedMigrationRun) error {
	if !appliedPlanTransfersStateInPlace(run.Applied) {
		return nil
	}
	platformApps := map[string]struct{}{}
	for _, app := range run.Prepare.Apps {
		if strings.EqualFold(strings.TrimSpace(app.Role), "platform") {
			platformApps[app.Name] = struct{}{}
		}
	}
	apps := []string{}
	seen := map[string]struct{}{}
	for _, step := range livePlanForApplied(run, run.Applied).Steps {
		if step.Kind != dokploy.StepPauseSource {
			continue
		}
		if _, platform := platformApps[step.App]; platform {
			continue
		}
		if _, exists := seen[step.App]; exists {
			continue
		}
		seen[step.App] = struct{}{}
		apps = append(apps, step.App)
	}
	if len(apps) == 0 {
		return nil
	}
	return fmt.Errorf("live apply refused for stateful app(s) %s: this run was applied with an older plan version that copied state into the deployed target, and Dokploy cannot durably prevent a queued or future deployment from restarting target writers during that copy; Bort cannot continue this run, so complete target setup, state transfer, traffic cutover, and source retirement outside Bort, or create a new run (new runs stage state before the target is deployed)", strings.Join(apps, ", "))
}

type sourceCleanupStep struct {
	app   string
	index int
}

func interruptedStatefulSourceCleanup(run loadedMigrationRun) []sourceCleanupStep {
	plan := livePlanForApplied(run, run.Applied)
	byIndex := map[int]appliedStep{}
	for _, recorded := range run.Applied.Steps {
		if recorded.Index < 0 || recorded.Index >= len(plan.Steps) {
			return nil
		}
		byIndex[recorded.Index] = recorded
	}
	pending := map[string]sourceCleanupStep{}
	for index, step := range plan.Steps {
		recorded, ok := byIndex[index]
		if !ok {
			continue
		}
		if recorded.Kind == string(dokploy.StepResumeSource) && step.Kind == dokploy.StepPauseSource && recorded.App == step.App && recorded.Ref == step.Ref {
			if appliedStepCompleted(recorded) {
				delete(pending, step.App)
			} else if appliedStepMayHaveRun(recorded) {
				pending[step.App] = sourceCleanupStep{app: step.App, index: index}
			}
			continue
		}
		if !appliedStepMatches(recorded, step) {
			return nil
		}
		if !appliedStepMayHaveRun(recorded) {
			continue
		}
		switch step.Kind {
		case dokploy.StepPushImage, dokploy.StepRestoreDataStore, dokploy.StepSyncVolume,
			dokploy.StepResumeTarget, dokploy.StepInstallGateway, dokploy.StepActivateRoutes,
			dokploy.StepStopCoolifyProxy, dokploy.StepStartDokployProxy:
			return nil
		case dokploy.StepPauseSource:
			pending[step.App] = sourceCleanupStep{app: step.App, index: index}
		case dokploy.StepResumeSource:
			if appliedStepCompleted(recorded) {
				delete(pending, step.App)
			}
		}
	}
	cleanup := make([]sourceCleanupStep, 0, len(pending))
	for _, step := range pending {
		cleanup = append(cleanup, step)
	}
	sort.Slice(cleanup, func(i, j int) bool { return cleanup[i].index < cleanup[j].index })
	return cleanup
}

func recoverInterruptedStatefulSources(ctx context.Context, run loadedMigrationRun, stderr io.Writer, onProgress func(dokploy.StepProgress), refusal error) error {
	cleanup := interruptedStatefulSourceCleanup(run)
	if len(cleanup) == 0 {
		return refusal
	}
	if err := verifyLocalSourceRun(ctx, run); err != nil {
		return errors.Join(refusal, fmt.Errorf("refusing interrupted source cleanup on an unverified Docker host: %w", err))
	}
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		return errors.Join(refusal, err)
	}
	lockPath, err := safeRunArtifactPath(run.Run.RunDir, "apply.lock")
	if err != nil {
		return errors.Join(refusal, err)
	}
	lock, err := acquireApplyLock(lockPath)
	if err != nil {
		return errors.Join(refusal, fmt.Errorf("lock interrupted source cleanup: %w", err))
	}
	defer lock.Release()
	hostLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		return errors.Join(refusal, fmt.Errorf("lock Dokploy live operations for interrupted source cleanup: %w", err))
	}
	defer hostLock.Release()
	if err := ensureDokploySourceCleanupAvailable(run.Run); err != nil {
		return errors.Join(refusal, err)
	}
	ledger, err := newAppliedLedger(appliedPath, run.Run)
	if err != nil {
		return errors.Join(refusal, err)
	}
	apps := map[string]struct{}{}
	appNames := make([]string, 0, len(cleanup))
	steps := make([]dokploy.Step, 0, len(cleanup))
	for _, item := range cleanup {
		apps[item.app] = struct{}{}
		appNames = append(appNames, item.app)
		steps = append(steps, dokploy.Step{Kind: dokploy.StepResumeSource, App: item.app, Ref: item.app})
	}
	prepare := run.Prepare
	prepare.Apps = nil
	for _, app := range run.Prepare.Apps {
		if _, ok := apps[app.Name]; !ok {
			continue
		}
		app.Readiness = ""
		app.Gates = nil
		prepare.Apps = append(prepare.Apps, app)
	}
	remap := func(progress dokploy.StepProgress) dokploy.StepProgress {
		progress.Index = cleanup[progress.Index].index
		return progress
	}
	beforeStep := func(progress dokploy.StepProgress) error {
		return ledger.Record(remap(progress))
	}
	progress := func(item dokploy.StepProgress) {
		item = remap(item)
		if item.Status == dokploy.StepStatusOK || item.Status == dokploy.StepStatusError || item.Status == dokploy.StepStatusSkipped {
			if recordErr := ledger.Record(item); recordErr != nil {
				fmt.Fprintf(stderr, "warning: failed to record source cleanup for %s: %v\n", item.Step.App, recordErr)
			}
		}
		if onProgress != nil {
			onProgress(item)
		}
	}
	plan := dokploy.Plan{Steps: steps, Prepare: prepare, RunDir: run.Run.RunDir, RecoveryCommand: runScopedCommand(run, "status"), BeforeStep: &beforeStep, OnProgress: &progress}
	client := &dokploy.Client{}
	if err := client.AdoptHistoricalSourcePause(ctx, plan, appNames); err != nil {
		return errors.Join(refusal, fmt.Errorf("record historical source pause ownership: %w", err))
	}
	if err := client.Apply(ctx, plan); err != nil {
		return errors.Join(refusal, fmt.Errorf("resume source after interrupted historical pause: %w", err))
	}
	if err := ledger.Err(); err != nil {
		return errors.Join(refusal, fmt.Errorf("persist interrupted source cleanup: %w", err))
	}
	fmt.Fprintln(stderr, "stateful recovery: resumed and recorded the source pause cleanup; no state transfer was attempted")
	return fmt.Errorf("%w; the previously paused source was resumed and recorded, but state transfer remains disabled", refusal)
}

func liveApplyBlockingDecisions(run loadedMigrationRun) []runDecision {
	return openFilteredDecisions(run, func(item runDecisionItem) bool {
		if item.Stage == "prepare" || item.Readiness == preparer.ReadinessBlocked || item.Readiness == preparer.ReadinessNeedsInput {
			return true
		}
		if item.Readiness != preparer.ReadinessNeedsDecision {
			return false
		}
		return !liveApplyReviewOnlyDecision(item)
	})
}

func liveApplyReviewOnlyDecision(item runDecisionItem) bool {
	switch item.Stage {
	case "cutover":
		return item.Code == "cutover.sync_verification_required" || item.Code == "cutover.health_check_required"
	case "rollback":
		return item.Code == "rollback.trigger_required" || item.Code == "rollback.source_health_required"
	case "commit":
		return item.Code == "commit.target_acceptance_required" || item.Code == "commit.target_route_acceptance_required" || item.Code == "commit.rollback_window_closed"
	default:
		return false
	}
}

func applyRunActive(runRef string) (bool, error) {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return false, err
	}
	return applyLockActive(filepath.Join(runDir, "apply.lock"))
}

func acquireRunOperationLock(runRef string) (*applyLock, error) {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return nil, err
	}
	lockPath, err := safeRunArtifactPath(runDir, "operation.lock")
	if err != nil {
		return nil, err
	}
	lock, err := acquireApplyLock(lockPath)
	if errors.Is(err, errApplyAlreadyRunning) {
		return nil, errRunOperationActive
	}
	return lock, err
}

func acquireDokployLiveOperationLock() (*applyLock, error) {
	return acquireDokployLiveOperationLockWithRecovery(false)
}

func acquireDokployInstallRecoveryLock() (*applyLock, error) {
	return acquireDokployLiveOperationLockWithRecovery(true)
}

func acquireDokployLiveOperationLockWithRecovery(allowInstallRecovery bool) (*applyLock, error) {
	if !dokployLiveOperationsSupported() {
		return nil, fmt.Errorf("Dokploy live operations are unavailable on this platform; continue on the Linux source host")
	}
	lockPath, err := dokployLiveOperationLockPath()
	if err != nil {
		return nil, err
	}
	if err := prepareDokployLiveOperationLockPath(lockPath); err != nil {
		return nil, err
	}
	lock, err := acquireApplyLock(lockPath)
	if errors.Is(err, errApplyAlreadyRunning) {
		return nil, errDokployLiveOperationActive
	}
	if err != nil {
		return nil, err
	}
	if !allowInstallRecovery {
		if err := dokployInstallationRecoveryBlocker(); err != nil {
			lock.Release()
			return nil, err
		}
	}
	return lock, err
}

func dokployInstallationRecoveryBlocker() error {
	command, found, err := dokployInstallationRecoveryCommand()
	if err != nil {
		return fmt.Errorf("verify Dokploy installation recovery state: %w", err)
	}
	if !found {
		return nil
	}
	return fmt.Errorf("%w: run `%s`", errDokployInstallRecoveryRequired, command)
}

func dokployLiveOperationActive() (bool, error) {
	if !dokployLiveOperationsSupported() {
		return false, nil
	}
	lockPath, err := dokployLiveOperationLockPath()
	if err != nil {
		return false, err
	}
	active, err := applyLockActive(lockPath)
	if err != nil || active {
		return active, err
	}
	return false, dokployInstallationRecoveryBlocker()
}

func runOperationActive(runRef string) (bool, error) {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return false, err
	}
	return applyLockActive(filepath.Join(runDir, "operation.lock"))
}

func migrationRunMetadataExists(runRef string) bool {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(runDir, "run.json"))
	return err == nil
}

func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func attachLiveMigrationRun(ctx context.Context, run loadedMigrationRun, stderr io.Writer, onProgress func(dokploy.StepProgress)) error {
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		return err
	}
	lockPath, err := safeRunArtifactPath(run.Run.RunDir, "apply.lock")
	if err != nil {
		return err
	}
	if err := attachLiveMigration(ctx, run, appliedPath, lockPath, stderr, onProgress); err != nil {
		return err
	}
	return finalizeAttachedLiveMigration(ctx, run.Run.RunDir)
}

func applyLiveMigrationLocked(ctx context.Context, run loadedMigrationRun, stderr io.Writer, onProgress func(dokploy.StepProgress)) error {
	if run.Run.Target != "dokploy" {
		return fmt.Errorf("--live is only supported for target dokploy, got %q", run.Run.Target)
	}
	if err := validateStatefulLiveApply(run); err != nil {
		return recoverInterruptedStatefulSources(ctx, run, stderr, onProgress, err)
	}
	var err error
	run, err = ensureSelfContainedLiveRunLocked(run)
	if err != nil {
		return err
	}
	appliedPath, err := safeRunArtifactPath(run.Run.RunDir, run.Run.Artifacts.Applied)
	if err != nil {
		return err
	}
	lockPath, err := safeRunArtifactPath(run.Run.RunDir, "apply.lock")
	if err != nil {
		return err
	}
	lock, err := acquireApplyLock(lockPath)
	if err != nil {
		if err == errApplyAlreadyRunning {
			if err := attachLiveMigration(ctx, run, appliedPath, lockPath, stderr, onProgress); err != nil {
				return err
			}
			current, err := loadMigrationRun(run.Run.RunDir)
			if err != nil {
				return err
			}
			if err := requireLiveApplySucceeded(current); err != nil {
				return err
			}
			return markRunLiveAppliedLocked(current.Run)
		}
		return fmt.Errorf("lock live migration run: %w", err)
	}
	defer lock.Release()
	bundleFiles, err := captureReviewedMigrationBundle(run)
	if err != nil {
		return err
	}
	if err := validateLiveApplyReady(run); err != nil {
		return err
	}
	plan := livePlanForApplied(run, run.Applied)
	if len(plan.Steps) == 0 {
		return fmt.Errorf("live migration plan has no executable steps")
	}
	if err := verifyLocalSourceRun(ctx, run); err != nil {
		return fmt.Errorf("refusing live migration on an unverified Docker source: %w", err)
	}
	plan.BundleFiles = bundleFiles
	plan.ApprovedPrepareDecisions = approvedPrepareDecisions(run)
	if err := stagedTransferRefusalForPlan(plan, run.Applied); err != nil {
		return fmt.Errorf("live apply refused before binding run %q to a Dokploy target: %w", run.Run.Name, err)
	}
	ledger, err := newAppliedLedger(appliedPath, run.Run)
	if err != nil {
		return err
	}
	plan.RunName = run.Run.Name
	plan.RunDir = run.Run.RunDir
	plan.RecoveryCommand = runScopedCommand(run, "status")
	if plan.RunID, err = dokployTrafficRunID(run.Run); err != nil {
		return err
	}
	applied := ledger.Snapshot()
	if err := validateApplyResumeAuthority(applied); err != nil {
		return fmt.Errorf("%w; %s", err, authorityRecoveryInstruction(run))
	}
	var targetLock *applyLock
	client, err := ensureBoundDokployClient(ctx, run.Run, run.Run.Target, os.Stdin, stderr, stderr, ledger, dokploySetupLockOptions{
		retain: &targetLock,
		revalidate: func() error {
			return ensureDokployTrafficRunAvailable(run.Run, plan)
		},
	})
	if targetLock != nil {
		defer targetLock.Release()
	}
	if err != nil {
		if err == errDokploySetupSkipped {
			return nil
		}
		return err
	}
	if targetLock == nil {
		return fmt.Errorf("dokploy client became ready without retaining the host operation lock")
	}
	applied = ledger.Snapshot()
	plan.TargetIdentities = appliedTargetIdentities(applied)
	resumeFrom := completedApplyPrefix(plan.Steps, applied)
	if err := validateDokployTrafficResume(run.Run, plan, applied, client.BaseURL, dokployCredentialID(client.Token), resumeFrom); err != nil {
		return err
	}
	if resumeFrom < len(plan.Steps) {
		if err := ledger.PrepareRetry(resumeFrom); err != nil {
			return fmt.Errorf("prepare applied ledger for retry: %w", err)
		}
	}
	plan.ResumeFrom = resumeFrom
	ownershipIndex := dokployOwnershipStepIndex(plan)
	beforeStep := func(p dokploy.StepProgress) error {
		if p.Index == ownershipIndex {
			if err := claimDokployHostOwnership(run.Run, client.BaseURL, dokployCredentialID(client.Token)); err != nil {
				return err
			}
		}
		return ledger.Record(p)
	}
	plan.BeforeStep = &beforeStep
	fn := func(p dokploy.StepProgress) {
		if p.Status == dokploy.StepStatusOK || p.Status == dokploy.StepStatusError || p.Status == dokploy.StepStatusSkipped {
			if recordErr := ledger.Record(p); recordErr != nil {
				fmt.Fprintf(stderr, "warning: failed to record applied step %s: %v\n", p.Step.Kind, recordErr)
			}
		}
		if onProgress != nil {
			onProgress(p)
		}
	}
	plan.OnProgress = &fn
	fmt.Fprintf(stderr, "live mode: %s reachable; planned %s\n", client.BaseURL, pluralize(len(plan.Steps), "Dokploy step", "Dokploy steps"))
	if resumeFrom > 0 && resumeFrom < len(plan.Steps) {
		fmt.Fprintf(stderr, "live mode: resuming after %s completed\n", pluralize(resumeFrom, "step", "steps"))
	}
	if resumeFrom == len(plan.Steps) && len(plan.Steps) > 0 {
		fmt.Fprintln(stderr, "live mode: all planned Dokploy steps are already recorded as complete")
	}
	if err := client.Apply(ctx, plan); err != nil {
		return err
	}
	if trafficHandoffStepIndex(plan) >= 0 {
		if err := client.ReconcileTargetTrafficAuthority(ctx); err != nil {
			return fmt.Errorf("verify Dokploy target traffic authority: %w", err)
		}
	}
	if err := markDokployTrafficTarget(run.Run, client.BaseURL); err != nil {
		return fmt.Errorf("record Dokploy target authority: %w", err)
	}
	if err := ledger.Err(); err != nil {
		return fmt.Errorf("live apply completed but applied ledger could not be persisted: %w", err)
	}
	if err := client.FinalizeSourceResumes(plan); err != nil {
		return fmt.Errorf("live apply completed but source resume ownership could not be cleared: %w", err)
	}
	if err := ledger.MarkSucceeded(); err != nil {
		return fmt.Errorf("live apply completed but its successful outcome could not be persisted: %w", err)
	}
	return markRunLiveAppliedLocked(run.Run)
}

func ensureBoundDokployClient(ctx context.Context, run migrationRun, target string, stdin io.Reader, stdout, stderr io.Writer, ledger *appliedLedger, lockOptions dokploySetupLockOptions) (*dokploy.Client, error) {
	client, acquired, lookupErr := lookupDokployClientForApply(target, lockOptions)
	if lookupErr == nil {
		if err := ledger.ValidateTargetOrigin(client.BaseURL); err != nil {
			if acquired != nil {
				acquired.Release()
			}
			return nil, fmt.Errorf("bind migration run to Dokploy target: %w", err)
		}
		if err := verifyLocalDokployClient(ctx, client); err != nil {
			if acquired != nil {
				acquired.Release()
			}
			return nil, err
		}
		pingErr := client.Ping(ctx)
		if pingErr == nil {
			if err := ledger.BindTargetOrigin(client.BaseURL); err != nil {
				if acquired != nil {
					acquired.Release()
				}
				return nil, fmt.Errorf("bind migration run to Dokploy target: %w", err)
			}
			if acquired != nil {
				*lockOptions.retain = acquired
			}
			return client, nil
		}
		if acquired != nil {
			acquired.Release()
		}
		if err := validateInlineDokploySetup(run, ledger.Snapshot(), client.BaseURL); err != nil {
			return nil, fmt.Errorf("dokploy is not reachable at %s: %w; %v", client.BaseURL, pingErr, err)
		}
		if target != "dokploy" || !stdinIsTerminal(stdin) {
			return nil, fmt.Errorf("dokploy is not reachable at %s: %w. run `%s`", client.BaseURL, pingErr, dokployInstallRepairCommand(client.BaseURL))
		}
		if err := promptInstallAndBootstrapDokploy(ctx, stdin, stdout, stderr, client.BaseURL, pingErr, lockOptions); err != nil {
			return nil, err
		}
	} else {
		var preflightErr *dokployLiveOperationPreflightError
		if errors.As(lookupErr, &preflightErr) || target != "dokploy" || !stdinIsTerminal(stdin) {
			return nil, lookupErr
		}
		if err := validateInlineDokploySetup(run, ledger.Snapshot(), ""); err != nil {
			return nil, err
		}
		if err := promptInstallAndBootstrapDokploy(ctx, stdin, stdout, stderr, "", lookupErr, lockOptions); err != nil {
			return nil, err
		}
	}
	client, err := lookupDokployClient(target)
	if err != nil {
		return nil, err
	}
	if err := ledger.ValidateTargetOrigin(client.BaseURL); err != nil {
		return nil, fmt.Errorf("bind migration run to Dokploy target: %w", err)
	}
	if err := verifyLocalDokployClient(ctx, client); err != nil {
		return nil, err
	}
	if err := client.Ping(ctx); err != nil {
		return nil, fmt.Errorf("dokploy ping failed after install/bootstrap: %w", err)
	}
	if err := ledger.BindTargetOrigin(client.BaseURL); err != nil {
		return nil, fmt.Errorf("bind migration run to Dokploy target: %w", err)
	}
	return client, nil
}

func dokployInstallRepairCommand(baseURL string) string {
	command := "init-target dokploy --install --auth-secret-backup /absolute/path/to/encrypted-or-off-host/dokploy-auth-secret"
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Port() != "" {
		command += " --install-port " + shellQuote(parsed.Port())
	}
	return bortCommand(command + " --dokploy-url " + shellQuote(baseURL))
}

var verifyLocalDokployClient = defaultVerifyLocalDokployClient

var verifyLocalSourceRun = defaultVerifyLocalSourceRun
var verifyLocalSourceEngine = defaultVerifyLocalSourceEngine

func defaultVerifyLocalSourceRun(ctx context.Context, run loadedMigrationRun) error {
	if err := defaultVerifyLocalSourceEngine(ctx, run); err != nil {
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return dokploy.VerifySourceContainers(verifyCtx, run.Prepare.Apps)
}

func defaultVerifyLocalSourceEngine(ctx context.Context, run loadedMigrationRun) error {
	if err := validateLocalSourceEngineAttestation(run); err != nil {
		return err
	}
	expected := strings.TrimSpace(run.Prepare.SourceDockerEngineID)
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	actual, err := localdocker.DockerEngineID(verifyCtx)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("reviewed Docker engine %q does not match local engine %q", expected, actual)
	}
	return nil
}

func validateLocalSourceEngineAttestation(run loadedMigrationRun) error {
	recovery := "refresh it from a local Docker scan before live work"
	if sourceAttestationCannotRefresh(run) {
		recovery = "this run already started live execution and cannot be refreshed; verify and recover or retire its reviewed sources manually"
	}
	switch strings.ToLower(strings.TrimSpace(run.Run.Source)) {
	case "docker", "local-docker", "coolify-local":
	case "manifest":
		return fmt.Errorf("run was created from an imported manifest, not a local Docker scan; %s", recovery)
	case "":
		return fmt.Errorf("run was created from an imported bundle, not a local Docker scan; %s", recovery)
	default:
		return fmt.Errorf("run was created from source %q, not a local Docker scan; %s", run.Run.Source, recovery)
	}
	source := strings.ToLower(strings.TrimSpace(run.Prepare.Source))
	switch source {
	case "docker", "coolify-local", "coolify-local-traefik", "coolify-local-caddy":
	default:
		return fmt.Errorf("run source %q is not a locally attested Docker scan; %s", run.Prepare.Source, recovery)
	}
	expected := strings.TrimSpace(run.Prepare.SourceDockerEngineID)
	if expected == "" {
		return fmt.Errorf("run has no reviewed Docker engine identity; %s", recovery)
	}
	return nil
}

func validateLocalSourceAttestation(run loadedMigrationRun) error {
	if err := validateLocalSourceEngineAttestation(run); err != nil {
		return err
	}
	return dokploy.ValidateSourceContainerAttestations(run.Prepare.Apps)
}

func sourceAttestationCannotRefresh(run loadedMigrationRun) bool {
	return run.Applied.TargetOrigin != "" || len(run.Applied.Steps) > 0 || run.Run.LiveAppliedAt != nil || run.Run.CommitStartedAt != nil || run.Run.CommittedAt != nil || run.Run.RollbackStartedAt != nil || run.Run.RolledBackAt != nil || run.Run.PurgedAt != nil || run.Run.ResolvedAuthority != ""
}

func sourceAttestationBlockerWithProbe(ctx context.Context, run loadedMigrationRun, probe bool) (runNextStep, bool) {
	if err := validateLocalSourceAttestation(run); err != nil {
		if sourceAttestationCannotRefresh(run) {
			return runNextStep{
				Action: "inspect and preserve both source and target state, then recover or retire the reviewed sources manually; do not replay this run against replacement containers",
				Reason: err.Error(),
				Phase:  "source-attestation-error",
			}, true
		}
		return runNextStep{
			Action: "create a new named migration run from a local Docker scan on this source host",
			Reason: err.Error(),
			Phase:  "inspection-only",
		}, true
	}
	if !probe {
		return runNextStep{}, false
	}
	if err := verifyLocalSourceRun(ctx, run); err != nil {
		action := "restore the reviewed local Docker daemon and source containers, then check this run again; if they were replaced, create a new named migration run from a fresh local scan"
		if sourceAttestationCannotRefresh(run) {
			action = "inspect and preserve both source and target state, restore the reviewed Docker host and containers if safe, then recover or retire this run manually; do not replay it against replacement containers"
		}
		if run.Run.ResolvedAuthority == dokployTrafficTarget && run.Run.LiveAppliedAt != nil {
			action = fmt.Sprintf("manually verify that the source is retired, then run `%s` to record manual retirement and release host ownership without mutating the unattested source", authorityRecoverySourceRetiredCommand(run))
		}
		return runNextStep{
			Action: action,
			Reason: "source attestation failed: " + err.Error(),
			Phase:  "source-attestation-error",
		}, true
	}
	return runNextStep{}, false
}

func defaultVerifyLocalDokployClient(ctx context.Context, client *dokploy.Client) error {
	if err := client.VerifySameDockerHost(ctx); err != nil {
		return fmt.Errorf("refusing same-VPS migration through %s: %w", client.BaseURL, err)
	}
	return nil
}

type dokployLiveOperationPreflightError struct {
	err error
}

func (e *dokployLiveOperationPreflightError) Error() string { return e.err.Error() }

func (e *dokployLiveOperationPreflightError) Unwrap() error { return e.err }

func lookupDokployClientForApply(target string, lockOptions dokploySetupLockOptions) (*dokploy.Client, *applyLock, error) {
	client, err := lookupDokployClient(target)
	if err != nil || lockOptions.retain == nil || *lockOptions.retain != nil {
		return client, nil, err
	}
	acquired, err := acquireDokployLiveOperationLock()
	if err != nil {
		return nil, nil, &dokployLiveOperationPreflightError{err: fmt.Errorf("lock Dokploy live operations: %w", err)}
	}
	if lockOptions.revalidate != nil {
		if err := lockOptions.revalidate(); err != nil {
			acquired.Release()
			return nil, nil, &dokployLiveOperationPreflightError{err: err}
		}
	}
	client, err = lookupDokployClient(target)
	if err != nil {
		acquired.Release()
		return nil, nil, err
	}
	return client, acquired, nil
}

func approvedPrepareDecisions(run loadedMigrationRun) map[dokploy.PrepareDecision]struct{} {
	approved := map[dokploy.PrepareDecision]struct{}{}
	for _, decision := range run.Decisions.Decisions {
		progress, ok := run.Progress.Decisions[decision.Kind]
		if !ok {
			continue
		}
		for _, item := range decision.Items {
			if item.Stage != "prepare" || item.Readiness != preparer.ReadinessNeedsDecision || strings.TrimSpace(item.Code) == "" {
				continue
			}
			itemProgress, ok := progress.Items[progressItemKey(item)]
			if ok && (itemProgress.Status == progressStatusResolved || itemProgress.Status == progressStatusSkipped) {
				decision := dokploy.NewPrepareDecision(item.App, item.Code, item.ResourceRef, item.Readiness, item.Message)
				approved[decision] = struct{}{}
			}
		}
	}
	return approved
}

func ensureSelfContainedLiveRunLocked(run loadedMigrationRun) (loadedMigrationRun, error) {
	containedErr := containedPath(run.Run.RunDir, run.Prepare.BundleDir)
	if containedErr == nil && strings.TrimSpace(run.Run.BundleDigest) != "" {
		return run, nil
	}
	if containedErr != nil && run.Run.ApplyOutcomeRequired {
		return loadedMigrationRun{}, fmt.Errorf("run %q does not have a self-contained reviewed bundle; run `%s` to refresh it before live apply: %w", run.Run.Name, runScopedCommand(run, "migrate"), containedErr)
	}

	runDir := filepath.FromSlash(run.Run.RunDir)
	bundleDir := run.Prepare.BundleDir
	removeBundle := false
	defer func() {
		if removeBundle {
			_ = os.RemoveAll(bundleDir)
		}
	}()
	if containedErr != nil {
		var err error
		bundleDir, err = snapshotMigrationBundle(run.Prepare.BundleDir, runDir)
		if err != nil {
			return loadedMigrationRun{}, fmt.Errorf("snapshot legacy run bundle: %w", err)
		}
		removeBundle = true
	}
	bundleDigest, err := digestMigrationBundle(bundleDir)
	if err != nil {
		return loadedMigrationRun{}, fmt.Errorf("digest legacy run bundle: %w", err)
	}
	artifacts, artifactDir, err := nextRunArtifacts(runDir, run.Run)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	removeArtifacts := true
	defer func() {
		if removeArtifacts {
			_ = os.RemoveAll(artifactDir)
		}
	}()

	now := time.Now().UTC()
	if strings.TrimSpace(run.Run.Source) == "" && strings.TrimSpace(run.Run.SourceBundleDir) == "" {
		run.Run.SourceBundleDir = run.Run.BundleDir
	}
	run.Run.BundleDir = bundleDir
	run.Run.BundleDigest = hex.EncodeToString(bundleDigest[:])
	run.Run.ApplyOutcomeRequired = true
	run.Run.UpdatedAt = now
	run.Run.Artifacts = artifacts
	run.Prepare.BundleDir = bundleDir
	run.Sync.BundleDir = bundleDir
	run.Cutover.BundleDir = bundleDir
	run.Rollback.BundleDir = bundleDir
	run.Commit.BundleDir = bundleDir
	run.Decisions.RunName = run.Run.Name
	run.Decisions.RunDir = run.Run.RunDir
	run.Decisions.BundleDir = bundleDir
	for decisionIndex := range run.Decisions.Decisions {
		for itemIndex := range run.Decisions.Decisions[decisionIndex].Items {
			item := &run.Decisions.Decisions[decisionIndex].Items[itemIndex]
			switch item.Stage {
			case "prepare":
				item.Artifact = runArtifactPath(runDir, artifacts.Prepare)
			case "sync":
				item.Artifact = runArtifactPath(runDir, artifacts.Sync)
			case "cutover":
				item.Artifact = runArtifactPath(runDir, artifacts.Cutover)
			case "rollback":
				item.Artifact = runArtifactPath(runDir, artifacts.Rollback)
			case "commit":
				item.Artifact = runArtifactPath(runDir, artifacts.Commit)
			}
		}
	}
	run.Progress.RunName = run.Run.Name
	run.Progress.RunDir = run.Run.RunDir
	run.Progress.UpdatedAt = now
	run.Applied.RunName = run.Run.Name
	run.Applied.BundleDir = bundleDir
	run.Applied.Target = run.Run.Target

	if err := writeJSONArtifact(runArtifactPath(runDir, artifacts.Prepare), run.Prepare); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, artifacts.Sync), run.Sync); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, artifacts.Cutover), run.Cutover); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, artifacts.Rollback), run.Rollback); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, artifacts.Commit), run.Commit); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, artifacts.Decisions), run.Decisions); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeRunProgress(runArtifactPath(runDir, artifacts.Progress), run.Progress); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeRunApplied(runArtifactPath(runDir, artifacts.Applied), run.Applied); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(filepath.Join(runDir, "run.json"), run.Run); err != nil {
		return loadedMigrationRun{}, err
	}
	removeBundle = false
	removeArtifacts = false
	return run, nil
}

const liveFinalizePollInterval = 50 * time.Millisecond

func finalizeAttachedLiveMigration(ctx context.Context, runRef string) error {
	ticker := time.NewTicker(liveFinalizePollInterval)
	defer ticker.Stop()
	for {
		run, err := loadMigrationRun(runRef)
		if err != nil {
			return err
		}
		if run.Run.LiveAppliedAt != nil {
			return nil
		}
		operationLock, err := acquireRunOperationLock(runRef)
		if err == nil {
			current, loadErr := loadMigrationRun(runRef)
			if loadErr == nil {
				loadErr = requireLiveApplySucceeded(current)
			}
			if loadErr == nil {
				loadErr = markRunLiveAppliedLocked(current.Run)
			}
			operationLock.Release()
			return loadErr
		}
		if !errors.Is(err, errRunOperationActive) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

const liveAttachPollInterval = 2 * time.Second

func attachLiveMigration(ctx context.Context, run loadedMigrationRun, appliedPath, lockPath string, stderr io.Writer, onProgress func(dokploy.StepProgress)) error {
	pid := readApplyLockPID(lockPath)
	startedAt := time.Now().UTC()
	var lastSeen time.Time
	initialApplied, initialAppliedOK := readRunApplied(appliedPath, run.Run)
	plan := livePlanForApplied(run, initialApplied)
	if initialAppliedOK == nil && initialApplied.SucceededAt != nil && completedApplyPrefix(plan.Steps, initialApplied) >= len(plan.Steps) && len(plan.Steps) > 0 {
		entries := attachProgressEntries(plan.Steps, initialApplied, time.Time{})
		emitAttachProgress(onProgress, entries)
		if onProgress == nil {
			fmt.Fprintf(stderr, "live mode: apply already complete (%s recorded)\n", pluralize(len(plan.Steps), "step", "steps"))
		}
		return nil
	}
	if pid > 0 {
		fmt.Fprintf(stderr, "live mode: another apply is already running (pid %d); attaching to progress\n", pid)
	} else {
		fmt.Fprintln(stderr, "live mode: another apply is already running; attaching to progress")
	}
	if initialAppliedOK == nil {
		entries := attachProgressEntries(plan.Steps, initialApplied, time.Time{})
		emitAttachProgress(onProgress, entries)
		if latest, ok := latestAttachProgressEntry(entries); ok {
			lastSeen = latest.updatedAt
			if onProgress == nil {
				writeAttachTextProgress(stderr, latest.progress, latest.progress.Total, "already recorded")
			}
		} else if onProgress == nil {
			fmt.Fprintf(stderr, "live mode: 0 of %s already recorded\n", pluralize(len(plan.Steps), "step", "steps"))
		}
	}

	ticker := time.NewTicker(liveAttachPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			applied, err := readRunApplied(appliedPath, run.Run)
			if err != nil {
				fmt.Fprintf(stderr, "warning: failed to read applied progress: %v\n", err)
				continue
			}
			plan = livePlanForApplied(run, applied)
			entries := attachProgressEntries(plan.Steps, applied, lastSeen)
			if len(entries) > 0 {
				emitAttachProgress(onProgress, entries)
				latest, _ := latestAttachProgressEntry(entries)
				lastSeen = latest.updatedAt
				if onProgress == nil {
					writeAttachTextProgress(stderr, latest.progress, latest.progress.Total, "recorded")
				}
			}
			if applied.SucceededAt != nil && completedApplyPrefix(plan.Steps, applied) >= len(plan.Steps) {
				return nil
			}
			active, err := applyLockActive(lockPath)
			if err != nil {
				return fmt.Errorf("check live migration lock: %w", err)
			}
			if active {
				continue
			}
			return attachExitResult(run, plan.Steps, applied, startedAt)
		}
	}
}

func readApplyLockPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 0 {
		return 0
	}
	return pid
}

type attachProgressEntry struct {
	progress  dokploy.StepProgress
	updatedAt time.Time
}

func attachProgressEntries(steps []dokploy.Step, applied runApplied, after time.Time) []attachProgressEntry {
	entries := []attachProgressEntry{}
	for _, recorded := range applied.Steps {
		if recorded.Index < 0 || recorded.UpdatedAt.IsZero() || !recorded.UpdatedAt.After(after) {
			continue
		}
		if !attachStatusRecorded(recorded.Status) {
			continue
		}
		step := dokploy.Step{Kind: dokploy.StepKind(recorded.Kind), App: recorded.App, Ref: recorded.Ref}
		if recorded.Index < len(steps) {
			step = steps[recorded.Index]
			if !appliedStepMatches(recorded, step) {
				if !appliedCleanupMatches(recorded, step) {
					continue
				}
				step = dokploy.Step{Kind: dokploy.StepKind(recorded.Kind), App: recorded.App, Ref: recorded.Ref}
			}
		} else if !standaloneAppliedCleanup(recorded, len(steps)) {
			continue
		}
		total := len(steps)
		if standaloneAppliedCleanup(recorded, len(steps)) {
			total++
		}
		progress := dokploy.StepProgress{Index: recorded.Index, Total: total, Step: step, Status: dokploy.StepStatus(recorded.Status)}
		if recorded.Error != "" {
			progress.Err = errors.New(recorded.Error)
		}
		entries = append(entries, attachProgressEntry{progress: progress, updatedAt: recorded.UpdatedAt})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].updatedAt.Before(entries[j].updatedAt) })
	return entries
}

func attachStatusRecorded(status string) bool {
	return status == string(dokploy.StepStatusOK) || status == string(dokploy.StepStatusSkipped) || status == string(dokploy.StepStatusError)
}

func appliedCleanupMatches(recorded appliedStep, planned dokploy.Step) bool {
	sameIdentity := recorded.App == planned.App && recorded.Ref == planned.Ref
	return sameIdentity &&
		(planned.Kind == dokploy.StepPauseSource && recorded.Kind == string(dokploy.StepResumeSource) ||
			planned.Kind == dokploy.StepStopCoolifyProxy && recorded.Kind == string(dokploy.StepStartCoolifyProxy))
}

func standaloneAppliedCleanup(recorded appliedStep, stepCount int) bool {
	return recorded.Index == stepCount && recorded.Kind == string(dokploy.StepStartCoolifyProxy) && recorded.Ref == "coolify-proxy"
}

func emitAttachProgress(onProgress func(dokploy.StepProgress), entries []attachProgressEntry) {
	if onProgress == nil {
		return
	}
	for _, entry := range entries {
		onProgress(entry.progress)
	}
}

func latestAttachProgressEntry(entries []attachProgressEntry) (attachProgressEntry, bool) {
	if len(entries) == 0 {
		return attachProgressEntry{}, false
	}
	return entries[len(entries)-1], true
}

func writeAttachTextProgress(stderr io.Writer, progress dokploy.StepProgress, total int, label string) {
	fmt.Fprintf(stderr, "live mode: %d of %s %s; latest %s %s/%s %s\n", progress.Index+1, pluralize(total, "step", "steps"), label, progress.Status, progress.Step.App, progress.Step.Ref, progress.Step.Kind)
}

func attachExitResult(run loadedMigrationRun, steps []dokploy.Step, applied runApplied, attachedAt time.Time) error {
	completed := completedApplyPrefix(steps, applied)
	if completed >= len(steps) {
		if applied.SucceededAt != nil {
			return nil
		}
		return fmt.Errorf("live migration recorded %s but no successful live-apply outcome; rerun `%s` to continue", pluralize(len(steps), "step", "steps"), liveApplyCommand(run))
	}
	if failed, ok := latestAttachFailure(steps, applied, attachedAt); ok {
		return fmt.Errorf("live migration exited after recording %d of %s; latest failure: %s %s/%s: %s", completed, pluralize(len(steps), "step", "steps"), failed.Kind, failed.App, failed.Ref, failed.Error)
	}
	return fmt.Errorf("live migration exited after recording %d of %s; rerun `%s` to continue", completed, pluralize(len(steps), "step", "steps"), liveApplyCommand(run))
}

func latestAttachFailure(steps []dokploy.Step, applied runApplied, attachedAt time.Time) (appliedStep, bool) {
	var latest appliedStep
	found := false
	for _, step := range applied.Steps {
		if step.Status != string(dokploy.StepStatusError) || step.UpdatedAt.Before(attachedAt.Add(-5*time.Second)) {
			continue
		}
		if step.Index < 0 || step.Index > len(steps) {
			continue
		}
		if step.Index == len(steps) {
			if !standaloneAppliedCleanup(step, len(steps)) {
				continue
			}
		} else if !appliedStepMatches(step, steps[step.Index]) && !appliedCleanupMatches(step, steps[step.Index]) {
			continue
		}
		if !found || step.UpdatedAt.After(latest.UpdatedAt) {
			latest = step
			found = true
		}
	}
	return latest, found
}

func lookupDokployClient(target string) (*dokploy.Client, error) {
	client, envErr := dokploy.NewClientFromEnv()
	if envErr == nil {
		return client, nil
	}
	if strings.TrimSpace(os.Getenv(dokploy.EnvBaseURL)) != "" || strings.TrimSpace(os.Getenv(dokploy.EnvToken)) != "" {
		return nil, envErr
	}
	state, err := readBortState(defaultStatePath())
	if err != nil {
		return nil, err
	}
	creds, ok := state.Targets[target]
	if ok && creds.URL != "" && creds.Token != "" {
		baseURL, err := dokploy.NormalizeTokenBaseURL(creds.URL)
		if err != nil {
			return nil, err
		}
		return &dokploy.Client{BaseURL: baseURL, Token: creds.Token, HTTPClient: &http.Client{Timeout: 30 * time.Second}}, nil
	}
	return nil, fmt.Errorf("no dokploy credentials available: set %s and %s; for an existing Dokploy run `%s` (replace 3030 if it uses another published port), or if Dokploy is absent run `%s`", dokploy.EnvBaseURL, dokploy.EnvToken, bortCommand("init-target dokploy --dokploy-url http://127.0.0.1:3030"), bortCommand("init-target dokploy --install --auth-secret-backup /absolute/path/to/encrypted-or-off-host/dokploy-auth-secret"))
}

var stdinIsTerminal = defaultStdinIsTerminal

func defaultStdinIsTerminal(stdin io.Reader) bool {
	file, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	return isInteractiveTerminal(file)
}

func existingMutableMigrationRun(runDir, runName string) (migrationRun, error) {
	immutableError := func() error {
		return fmt.Errorf("run %q has started live execution and its reviewed plan is immutable; create a new run instead", runName)
	}
	metadataPath := filepath.Join(runDir, "run.json")
	if _, err := os.Stat(metadataPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return migrationRun{}, err
		}
		appliedPath := filepath.Join(runDir, defaultRunArtifacts().Applied)
		if _, err := os.Stat(appliedPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return migrationRun{}, nil
			}
			return migrationRun{}, err
		}
		probe := migrationRun{Name: runName, RunDir: filepath.ToSlash(filepath.Clean(runDir)), Artifacts: defaultRunArtifacts()}
		applied, err := readRunApplied(appliedPath, probe)
		if err != nil {
			return migrationRun{}, fmt.Errorf("refusing to rewrite run %q because its apply ledger cannot be verified: %w", runName, err)
		}
		if applied.TargetOrigin != "" || len(applied.Steps) > 0 {
			return migrationRun{}, immutableError()
		}
		return migrationRun{}, nil
	}

	existing, err := readRunMetadata(metadataPath)
	if err != nil {
		return migrationRun{}, fmt.Errorf("refusing to rewrite run %q because its metadata cannot be read: %w", runName, err)
	}
	existing.RunDir = filepath.ToSlash(filepath.Clean(runDir))
	existing.Artifacts = existing.Artifacts.withDefaults()
	if existing.LiveAppliedAt != nil || existing.CommitStartedAt != nil || existing.CommittedAt != nil || existing.RollbackStartedAt != nil || existing.RolledBackAt != nil || existing.PurgedAt != nil {
		return migrationRun{}, immutableError()
	}
	appliedPath, err := safeRunArtifactPath(runDir, existing.Artifacts.Applied)
	if err != nil {
		return migrationRun{}, fmt.Errorf("refusing to rewrite run %q because its apply ledger path is invalid: %w", runName, err)
	}
	applied, err := readRunApplied(appliedPath, existing)
	if err != nil {
		return migrationRun{}, fmt.Errorf("refusing to rewrite run %q because its apply ledger cannot be verified: %w", runName, err)
	}
	if applied.TargetOrigin != "" || len(applied.Steps) > 0 {
		return migrationRun{}, immutableError()
	}
	return existing, nil
}

func createMigrationRun(opts migrationRunOptions) (loadedMigrationRun, error) {
	if opts.BundleDir == "" {
		opts.BundleDir = "bort-bundle"
	}
	if opts.Target == "" {
		opts.Target = "dokploy"
	}

	now := time.Now().UTC()
	runDir, runName := newRunDir(opts.RunRef, opts.BundleDir, opts.AppName, now)
	if err := ensurePrivateRunDir(runDir); err != nil {
		return loadedMigrationRun{}, err
	}
	operationLock, err := acquireRunOperationLock(runDir)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	defer operationLock.Release()
	return createMigrationRunLocked(opts, runDir, runName, now)
}

func snapshotMigrationBundle(sourceDir, runDir string) (string, error) {
	info, err := os.Lstat(sourceDir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("migration bundle %s must be a directory and cannot be a symlink", sourceDir)
	}
	sourceAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", err
	}
	runAbs, err := filepath.Abs(runDir)
	if err != nil {
		return "", err
	}
	relRun, err := filepath.Rel(sourceAbs, runAbs)
	if err != nil {
		return "", err
	}
	if relRun == "." || (relRun != ".." && !strings.HasPrefix(relRun, ".."+string(os.PathSeparator))) {
		return "", fmt.Errorf("migration bundle %s cannot contain run directory %s", sourceDir, runDir)
	}
	sourceDigest, err := digestMigrationBundle(sourceDir)
	if err != nil {
		return "", err
	}

	snapshotDir, err := os.MkdirTemp(runDir, "bundle-")
	if err != nil {
		return "", err
	}
	removeSnapshot := true
	defer func() {
		if removeSnapshot {
			_ = os.RemoveAll(snapshotDir)
		}
	}()
	if err := os.Chmod(snapshotDir, 0o700); err != nil {
		return "", err
	}
	if err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("migration bundle entry %s cannot be a symlink", path)
		}
		target := filepath.Join(snapshotDir, rel)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, 0o700)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("migration bundle entry %s must be a regular file", path)
		}
		contents, err := readFileNoFollow(path)
		if err != nil {
			return err
		}
		return writeFileAtomic(target, contents, 0o600)
	}); err != nil {
		return "", err
	}
	if err := validateMigrationBundleSnapshot(sourceDir, snapshotDir, sourceDigest); err != nil {
		return "", err
	}
	removeSnapshot = false
	return filepath.Clean(snapshotDir), nil
}

func validateMigrationBundleSnapshot(sourceDir, snapshotDir string, sourceDigest [sha256.Size]byte) error {
	afterDigest, err := digestMigrationBundle(sourceDir)
	if err != nil {
		return err
	}
	snapshotDigest, err := digestMigrationBundle(snapshotDir)
	if err != nil {
		return err
	}
	if afterDigest != sourceDigest || snapshotDigest != sourceDigest {
		return fmt.Errorf("migration bundle %s changed while it was being snapshotted; retry after its files are stable", sourceDir)
	}
	return nil
}

func digestMigrationBundle(root string) ([sha256.Size]byte, error) {
	digest, _, err := readMigrationBundle(root, false)
	return digest, err
}

func captureMigrationBundle(root string) ([sha256.Size]byte, map[string][]byte, error) {
	return readMigrationBundle(root, true)
}

func readMigrationBundle(root string, capture bool) ([sha256.Size]byte, map[string][]byte, error) {
	digest := sha256.New()
	var files map[string][]byte
	if capture {
		files = map[string][]byte{}
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("migration bundle entry %s cannot be a symlink", path)
		}
		if rel == "." {
			if !entry.IsDir() {
				return fmt.Errorf("migration bundle %s must be a directory", root)
			}
			return nil
		}
		if err := writeMigrationBundleDigestField(digest, []byte(filepath.ToSlash(rel))); err != nil {
			return err
		}
		if entry.IsDir() {
			return writeMigrationBundleDigestField(digest, []byte("directory"))
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("migration bundle entry %s must be a regular file", path)
		}
		contents, err := readFileNoFollow(path)
		if err != nil {
			return err
		}
		if capture {
			files[filepath.Clean(path)] = contents
		}
		if err := writeMigrationBundleDigestField(digest, []byte("file")); err != nil {
			return err
		}
		return writeMigrationBundleDigestField(digest, contents)
	})
	var result [sha256.Size]byte
	if err != nil {
		return result, nil, err
	}
	copy(result[:], digest.Sum(nil))
	return result, files, nil
}

func verifyReviewedMigrationBundle(run loadedMigrationRun) error {
	_, err := captureReviewedMigrationBundle(run)
	return err
}

func captureReviewedMigrationBundle(run loadedMigrationRun) (map[string][]byte, error) {
	recovery := reviewedMigrationBundleRecovery(run)
	var expected [sha256.Size]byte
	if strings.TrimSpace(run.Run.BundleDigest) == "" {
		return nil, fmt.Errorf("run %q predates reviewed bundle digests; %s", run.Run.Name, recovery)
	}
	expectedBytes, err := hex.DecodeString(run.Run.BundleDigest)
	if err != nil || len(expectedBytes) != sha256.Size {
		return nil, fmt.Errorf("reviewed migration bundle digest for run %q is invalid; %s", run.Run.Name, recovery)
	}
	copy(expected[:], expectedBytes)
	actual, files, err := captureMigrationBundle(run.Run.BundleDir)
	if err != nil {
		return nil, fmt.Errorf("capture reviewed migration bundle for run %q: %w; %s", run.Run.Name, err, recovery)
	}
	if actual != expected {
		return nil, fmt.Errorf("reviewed migration bundle for run %q changed after planning; %s", run.Run.Name, recovery)
	}
	return files, nil
}

func reviewedMigrationBundleRecovery(run loadedMigrationRun) string {
	if len(run.Applied.Steps) > 0 || run.Applied.TargetOrigin != "" || run.Run.LiveAppliedAt != nil {
		return "this run has started live execution and cannot be re-planned; reconcile any applied target changes, then create a new run"
	}
	return fmt.Sprintf("run `%s` to re-plan before live apply", runScopedCommand(run, "migrate"))
}

func writeMigrationBundleDigestField(w io.Writer, value []byte) error {
	if _, err := fmt.Fprintf(w, "%d:", len(value)); err != nil {
		return err
	}
	_, err := w.Write(value)
	return err
}

func nextRunArtifacts(runDir string, existing migrationRun) (runArtifacts, string, error) {
	artifacts := defaultRunArtifacts()
	if existing.APIVersion == "" {
		return artifacts, "", nil
	}
	dir, err := os.MkdirTemp(runDir, "plan-")
	if err != nil {
		return runArtifacts{}, "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return runArtifacts{}, "", err
	}
	prefix := filepath.Base(dir)
	artifacts.Prepare = filepath.ToSlash(filepath.Join(prefix, artifacts.Prepare))
	artifacts.Sync = filepath.ToSlash(filepath.Join(prefix, artifacts.Sync))
	artifacts.Cutover = filepath.ToSlash(filepath.Join(prefix, artifacts.Cutover))
	artifacts.Rollback = filepath.ToSlash(filepath.Join(prefix, artifacts.Rollback))
	artifacts.Commit = filepath.ToSlash(filepath.Join(prefix, artifacts.Commit))
	artifacts.Decisions = filepath.ToSlash(filepath.Join(prefix, artifacts.Decisions))
	artifacts.Progress = filepath.ToSlash(filepath.Join(prefix, artifacts.Progress))
	artifacts.Applied = filepath.ToSlash(filepath.Join(prefix, artifacts.Applied))
	return artifacts, dir, nil
}

func createMigrationRunLocked(opts migrationRunOptions, runDir, runName string, now time.Time) (loadedMigrationRun, error) {
	existingRun, err := existingMutableMigrationRun(runDir, runName)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	bundleDir, err := snapshotMigrationBundle(opts.BundleDir, runDir)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	sourceBundleDir := ""
	removeSnapshot := true
	if strings.TrimSpace(opts.Source) == "" {
		sourceBundleDir = opts.BundleDir
	}
	defer func() {
		if removeSnapshot {
			_ = os.RemoveAll(bundleDir)
		}
	}()

	createdAt := now
	if !existingRun.CreatedAt.IsZero() {
		createdAt = existingRun.CreatedAt
	}

	state, err := readBortState(defaultStatePath())
	if err != nil {
		return loadedMigrationRun{}, err
	}
	if _, err := applyStateEnvToBundle(state, bundleDir); err != nil {
		return loadedMigrationRun{}, err
	}
	bundleDigest, err := digestMigrationBundle(bundleDir)
	if err != nil {
		return loadedMigrationRun{}, err
	}

	preparePlan, err := preparer.Plan(preparer.Options{BundleDir: bundleDir, Target: opts.Target, AppName: opts.AppName})
	if err != nil {
		return loadedMigrationRun{}, err
	}
	applyStateOverridesToPrepare(state, &preparePlan)
	syncResult := syncplan.PlanFromPrepare(preparePlan)
	cutoverResult, err := gateway.PlanFromPrepareAndSync(preparePlan, syncResult, opts.ObservationWindowSeconds, opts.RollbackWindowSeconds)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	rollbackResult, err := rollbackplan.PlanFromCutover(cutoverResult, opts.ObservationWindowSeconds)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	commitResult, err := commitplan.PlanFromCutover(cutoverResult)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	artifacts, artifactDir, err := nextRunArtifacts(runDir, existingRun)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	removeArtifacts := artifactDir != ""
	defer func() {
		if removeArtifacts {
			_ = os.RemoveAll(artifactDir)
		}
	}()

	run := migrationRun{
		APIVersion:                runAPIVersion,
		Name:                      runName,
		RunDir:                    filepath.ToSlash(filepath.Clean(runDir)),
		CreatedAt:                 createdAt,
		UpdatedAt:                 now,
		LiveAppliedAt:             existingRun.LiveAppliedAt,
		CommitStartedAt:           existingRun.CommitStartedAt,
		CommittedAt:               existingRun.CommittedAt,
		RollbackStartedAt:         existingRun.RollbackStartedAt,
		RolledBackAt:              existingRun.RolledBackAt,
		PurgedAt:                  existingRun.PurgedAt,
		ResolvedAuthority:         existingRun.ResolvedAuthority,
		AuthorityResolvedAt:       existingRun.AuthorityResolvedAt,
		AuthorityFinalizedAt:      existingRun.AuthorityFinalizedAt,
		HostOwnerReleaseStartedAt: existingRun.HostOwnerReleaseStartedAt,
		ApplyOutcomeRequired:      true,
		Source:                    opts.Source,
		BundleDir:                 bundleDir,
		BundleDigest:              hex.EncodeToString(bundleDigest[:]),
		SourceBundleDir:           sourceBundleDir,
		ManifestPath:              opts.ManifestPath,
		Target:                    opts.Target,
		AppName:                   opts.AppName,
		DryRun:                    true,
		ObservationWindowSeconds:  opts.ObservationWindowSeconds,
		RollbackWindowSeconds:     opts.RollbackWindowSeconds,
		Artifacts:                 artifacts,
	}

	if err := writeJSONArtifact(runArtifactPath(runDir, run.Artifacts.Prepare), preparePlan); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, run.Artifacts.Sync), syncResult); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, run.Artifacts.Cutover), cutoverResult); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, run.Artifacts.Rollback), rollbackResult); err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeJSONArtifact(runArtifactPath(runDir, run.Artifacts.Commit), commitResult); err != nil {
		return loadedMigrationRun{}, err
	}
	loadedRun := loadedMigrationRun{Run: run, Prepare: preparePlan, Sync: syncResult, Cutover: cutoverResult, Rollback: rollbackResult, Commit: commitResult}
	loadedRun.Decisions = generateRunDecisions(loadedRun, now)
	if err := writeJSONArtifact(runArtifactPath(runDir, run.Artifacts.Decisions), loadedRun.Decisions); err != nil {
		return loadedMigrationRun{}, err
	}
	progressPath, err := safeRunArtifactPath(runDir, run.Artifacts.Progress)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	progress := emptyRunProgress(run)
	if existingRun.APIVersion != "" {
		existingProgressPath, err := safeRunArtifactPath(runDir, existingRun.Artifacts.Progress)
		if err != nil {
			return loadedMigrationRun{}, err
		}
		progress, err = readRunProgress(existingProgressPath, existingRun)
		if err != nil {
			return loadedMigrationRun{}, err
		}
	}
	progress.RunName = run.Name
	progress.RunDir = run.RunDir
	progress.UpdatedAt = now
	if err := writeRunProgress(progressPath, progress); err != nil {
		return loadedMigrationRun{}, err
	}
	loadedRun.Progress = progress
	appliedPath, err := safeRunArtifactPath(runDir, run.Artifacts.Applied)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	applied, err := readRunApplied(appliedPath, run)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	if err := writeRunApplied(appliedPath, applied); err != nil {
		return loadedMigrationRun{}, err
	}
	loadedRun.Applied = applied
	if err := writeJSONArtifact(filepath.Join(runDir, "run.json"), run); err != nil {
		return loadedMigrationRun{}, err
	}

	removeSnapshot = false
	removeArtifacts = false
	return loadedRun, nil
}

func refreshMigrationRun(runRef string) (loadedMigrationRun, error) {
	operationLock, err := acquireRunOperationLock(runRef)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	defer operationLock.Release()
	return refreshMigrationRunLocked(runRef)
}

func refreshMigrationRunLocked(runRef string) (loadedMigrationRun, error) {
	return refreshMigrationRunLockedWithInputs(runRef, "", "")
}

func refreshMigrationRunLockedWithInputs(runRef, bundleOverride, manifestOverride string) (loadedMigrationRun, error) {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	existing, err := readRunMetadata(filepath.Join(runDir, "run.json"))
	if err != nil {
		return loadedMigrationRun{}, err
	}
	existing.Artifacts = existing.Artifacts.withDefaults()
	manifestPath := existing.ManifestPath
	if strings.TrimSpace(manifestOverride) != "" {
		manifestPath = manifestOverride
	}
	if manifestPath == "" && strings.TrimSpace(existing.Source) != "" {
		manifestPath = runArtifactPath(runDir, "manifest.json")
	}
	bundleDir := strings.TrimSpace(bundleOverride)
	if bundleDir == "" {
		bundleDir = existing.BundleDir
	}
	if strings.TrimSpace(bundleOverride) == "" && strings.TrimSpace(existing.Source) == "" && strings.TrimSpace(existing.SourceBundleDir) != "" {
		bundleDir = existing.SourceBundleDir
	}
	return createMigrationRunLocked(migrationRunOptions{
		BundleDir:                bundleDir,
		Target:                   existing.Target,
		AppName:                  existing.AppName,
		RunRef:                   runDir,
		Source:                   existing.Source,
		ManifestPath:             manifestPath,
		ObservationWindowSeconds: existing.ObservationWindowSeconds,
		RollbackWindowSeconds:    existing.RollbackWindowSeconds,
	}, runDir, runNameFromDir(runDir), time.Now().UTC())
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		handled, err := writeStandaloneDokployInstallationRecovery(stdout)
		if err != nil || handled {
			return err
		}
	}
	run, err := loadRunFromArgs("status", args, stderr)
	if err != nil {
		return err
	}
	writeAppFirstCockpitContext(ctx, stdout, run)
	return nil
}

func runNext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		handled, err := writeStandaloneDokployInstallationRecovery(stdout)
		if err != nil || handled {
			return err
		}
	}
	run, err := loadRunFromArgs("next", args, stderr)
	if err != nil {
		return err
	}
	writeRunNextText(stdout, summarizeMigrationRunContext(ctx, run))
	return nil
}

func writeStandaloneDokployInstallationRecovery(w io.Writer) (bool, error) {
	if !dokployLiveOperationsSupported() {
		return false, nil
	}
	command, found, err := dokployInstallationRecoveryCommand()
	if err != nil {
		return false, fmt.Errorf("read interrupted Dokploy installation recovery command: %w", err)
	}
	if !found {
		return false, nil
	}
	st := newStyler(w)
	fmt.Fprintln(w, st.glyph("Dokploy installation recovery required", sevBad))
	fmt.Fprintf(w, "Next safe step: run `%s` to reconcile the interrupted installation\n", command)
	fmt.Fprintln(w, "Reason: the durable installation-recovery marker blocks other host mutations")
	return true, nil
}

func loadRunFromArgs(command string, args []string, stderr io.Writer) (loadedMigrationRun, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var runRef string
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	if err := fs.Parse(args); err != nil {
		return loadedMigrationRun{}, err
	}
	runFlagSet := flagSet(fs, "run")
	if runFlagSet && fs.NArg() > 0 {
		return loadedMigrationRun{}, fmt.Errorf("%s does not accept positional argument %q with --run", command, fs.Arg(0))
	}
	if runFlagSet && strings.TrimSpace(runRef) == "" {
		return loadedMigrationRun{}, fmt.Errorf("%s requires a non-empty --run value", command)
	}
	if fs.NArg() > 1 {
		return loadedMigrationRun{}, fmt.Errorf("%s does not accept positional argument %q after run reference %q", command, fs.Arg(1), fs.Arg(0))
	}
	if fs.NArg() == 1 {
		runRef = fs.Arg(0)
	}
	resolved, err := resolveRunRef(runRef, false)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	return loadMigrationRun(resolved)
}

func loadMigrationRun(runRef string) (loadedMigrationRun, error) {
	runDir, err := existingRunDir(runRef)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	run, err := readRunMetadata(filepath.Join(runDir, "run.json"))
	if err != nil {
		return loadedMigrationRun{}, err
	}
	run.RunDir = filepath.ToSlash(filepath.Clean(runDir))
	if run.Name == "" {
		run.Name = runNameFromDir(runDir)
	}
	run.Artifacts = run.Artifacts.withDefaults()

	expect := artifactExpectations{BundleDir: run.BundleDir, Target: run.Target, AppName: run.AppName}
	preparePlan, err := readPrepareArtifact(runArtifactPath(runDir, run.Artifacts.Prepare), expect)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	syncResult, err := readSyncArtifact(runArtifactPath(runDir, run.Artifacts.Sync), expect)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	cutoverResult, err := readCutoverArtifact(runArtifactPath(runDir, run.Artifacts.Cutover), expect)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	rollbackResult, err := readRollbackArtifact(runArtifactPath(runDir, run.Artifacts.Rollback), expect)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	commitResult, err := readCommitArtifact(runArtifactPath(runDir, run.Artifacts.Commit), expect)
	if err != nil {
		return loadedMigrationRun{}, err
	}

	loadedRun := loadedMigrationRun{Run: run, Prepare: preparePlan, Sync: syncResult, Cutover: cutoverResult, Rollback: rollbackResult, Commit: commitResult}
	decisions, err := readRunDecisions(runArtifactPath(runDir, run.Artifacts.Decisions), run)
	if err != nil {
		decisions = generateRunDecisions(loadedRun, run.UpdatedAt)
	}
	loadedRun.Decisions = decisions
	progressPath, err := safeRunArtifactPath(runDir, run.Artifacts.Progress)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	progress, err := readRunProgress(progressPath, run)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	loadedRun.Progress = progress
	appliedPath, err := safeRunArtifactPath(runDir, run.Artifacts.Applied)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	applied, err := readRunApplied(appliedPath, run)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	loadedRun.Applied = applied
	return loadedRun, nil
}

func summarizeMigrationRun(run loadedMigrationRun) migrationRunSummary {
	return summarizeMigrationRunContext(context.Background(), run)
}

func summarizeMigrationRunContext(ctx context.Context, run loadedMigrationRun) migrationRunSummary {
	return summarizeMigrationRunContextWithSourceProbe(ctx, run, true)
}

func summarizeMigrationRunForLive(ctx context.Context, run loadedMigrationRun) migrationRunSummary {
	return summarizeMigrationRunContextWithSourceProbe(ctx, run, false)
}

func summarizeMigrationRunContextWithSourceProbe(ctx context.Context, run loadedMigrationRun, probeSource bool) migrationRunSummary {
	summary := migrationRunSummary{Run: run.Run, Readiness: preparer.ReadinessReadyToCreate, Status: preparer.StatusGreen}

	for _, app := range run.Commit.Apps {
		if isPlatformRunApp(app.Role) {
			summary.PlatformApps++
			continue
		}
		summary.Apps++
		summary.Readiness = preparer.WorseReadiness(summary.Readiness, app.Readiness)
		summary.Status = preparer.WorseStatus(summary.Status, app.Status)
		summary.StatusCounts.add(app.Status)
		for _, gate := range app.Gates {
			summary.GateCounts.add(gate.Readiness)
		}
		for _, route := range app.Routes {
			if route.Host != "" || route.CurrentRef != "" || route.TargetRef != "" {
				summary.CommitRoutes++
			}
		}
	}

	if summary.Apps == 0 {
		for _, app := range run.Prepare.Apps {
			if isPlatformRunApp(app.Role) {
				continue
			}
			summary.Apps++
			summary.Readiness = preparer.WorseReadiness(summary.Readiness, app.Readiness)
			summary.Status = preparer.WorseStatus(summary.Status, app.Status)
			summary.StatusCounts.add(app.Status)
		}
	}

	for _, app := range run.Cutover.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		summary.CutoverRoutes += len(app.Routes)
	}
	for _, app := range run.Rollback.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		summary.RollbackRoutes += len(app.Routes)
	}
	for _, app := range run.Sync.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, step := range app.Steps {
			if step.Phase == syncplan.PhaseStateSync {
				summary.StateSteps++
			}
			if step.Pause != syncplan.PauseNone {
				summary.PauseSteps++
			}
		}
	}

	blockingDecisions := liveApplyBlockingDecisions(run)
	summary.Decisions = openRunDecisions(run)
	summary.Progress = progressSummary(run.Progress)
	summary.FirstGates = firstUnresolvedRunGates(run, 3)
	summary.Next = nextSafeStepContextWithSourceProbe(ctx, run, blockingDecisions, probeSource)
	return summary
}

func firstUnresolvedRunGates(run loadedMigrationRun, limit int) []runGateSummary {
	for _, readiness := range []preparer.Readiness{preparer.ReadinessBlocked, preparer.ReadinessNeedsInput, preparer.ReadinessNeedsDecision} {
		gates := gatesWithReadiness(run, readiness)
		if len(gates) > limit {
			return gates[:limit]
		}
		if len(gates) > 0 {
			return gates
		}
	}
	return nil
}

func nextSafeStep(run loadedMigrationRun, decisions []runDecision) runNextStep {
	return nextSafeStepContext(context.Background(), run, decisions)
}

func nextSafeStepContext(ctx context.Context, run loadedMigrationRun, decisions []runDecision) runNextStep {
	return nextSafeStepContextWithSourceProbe(ctx, run, decisions, true)
}

func nextSafeStepContextWithSourceProbe(ctx context.Context, run loadedMigrationRun, decisions []runDecision, probeSource bool) runNextStep {
	sourceBlocker := func() (runNextStep, bool) {
		return sourceAttestationBlockerWithProbe(ctx, run, probeSource)
	}
	if dokployLiveOperationsSupported() && !authorityRecoveryPending(run) {
		ownerFinalizationPending, ownerFinalizationErr := completedDokployOwnerFinalization(run)
		if ownerFinalizationErr != nil {
			return runNextStep{Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before treating this run as complete", Reason: ownerFinalizationErr.Error()}
		}
		if ownerFinalizationPending {
			if run.Run.RolledBackAt != nil {
				return runNextStep{Action: fmt.Sprintf("run `%s` to finish releasing rollback host ownership", runScopedCommand(run, "rollback --live")), Reason: "rollback is recorded, but its durable source-authority owner is not released"}
			}
			return runNextStep{Action: fmt.Sprintf("run `%s` to finish releasing commit host ownership", runScopedCommand(run, "commit --apply")), Reason: "commit is recorded, but its durable target-authority owner is not released"}
		}
	}
	if run.Run.PurgedAt != nil {
		return runNextStep{Action: "migration complete", Reason: "selected source leftovers were purged after target acceptance"}
	}
	if run.Run.CommittedAt != nil {
		return runNextStep{Action: fmt.Sprintf("run `%s` to audit remaining metadata and source leftovers", runScopedCommand(run, "cleanup")), Reason: "the target is accepted and source app containers are retired"}
	}
	if run.Run.RolledBackAt != nil && !authorityRecoveryPending(run) {
		return runNextStep{Action: "create a new named migration run with `bort migrate --run <new-name>` and current `--source` or `--bundle` inputs", Reason: "the rollback returned traffic to the source; the Dokploy target resources remain on the server"}
	}
	if !dokployLiveOperationsSupported() {
		return unsupportedPlatformNextStep(run)
	}
	owner, hostOwned, hostOwnerErr := conflictingDokployHostOwner(run.Run)
	applyActive, applyActiveErr := false, error(nil)
	if run.Run.LiveAppliedAt == nil {
		applyActive, applyActiveErr = applyRunActive(run.Run.RunDir)
	}
	hostOperationActive, hostOperationErr := dokployLiveOperationActive()
	if authorityRecoveryPending(run) {
		if hostOwnerErr != nil {
			return runNextStep{Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before finishing authority recovery", Reason: "manual authority is recorded but lifecycle finalization is incomplete: " + hostOwnerErr.Error()}
		}
		if applyActiveErr != nil {
			return runNextStep{Action: fmt.Sprintf("inspect the live-apply lock for run %s before finishing authority recovery", shellQuote(run.Run.Name)), Reason: "manual authority is recorded but lifecycle finalization is incomplete: " + applyActiveErr.Error()}
		}
		if blocked, ok := dokployHostOperationBlocker(hostOperationActive, hostOperationErr); ok {
			blocked.Reason = "manual authority is recorded but lifecycle finalization is incomplete: " + blocked.Reason
			return blocked
		}
		return runNextStep{
			Action: fmt.Sprintf("run `%s` to finish the recorded manual %s-authority recovery", pendingAuthorityRecoveryCommand(run), run.Run.ResolvedAuthority),
			Reason: "the authority decision is durable, but its lifecycle and host-owner finalization did not complete",
		}
	}
	trafficOwnerErr := validateCurrentDokployTrafficOwner(run, rollbackInProgress(run))
	if rollbackInProgress(run) {
		if hostOwnerErr != nil {
			return runNextStep{Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before continuing rollback", Reason: "rollback started and traffic or source state may already have changed: " + hostOwnerErr.Error()}
		}
		if applyActiveErr != nil {
			return runNextStep{Action: fmt.Sprintf("inspect the live-apply lock for run %s before continuing rollback", shellQuote(run.Run.Name)), Reason: "rollback started and traffic or source state may already have changed: " + applyActiveErr.Error()}
		}
		if blocked, ok := dokployHostOperationBlocker(hostOperationActive, hostOperationErr); ok {
			blocked.Reason = "rollback started and traffic or source state may already have changed: " + blocked.Reason
			return blocked
		}
		if runMayHaveAmbiguousAuthority(run) {
			if authorityRecoveryAvailable(run) {
				return authorityRecoveryNextStep(run, "authority-ambiguous recovery did not complete; target fencing may be partial and source state may be unchanged")
			}
			return runNextStep{
				Action: "inspect and preserve both source and target data, establish writer and traffic authority manually, then create a fresh migration run",
				Reason: "authority-ambiguous recovery did not complete; target fencing may be partial, source state may be unchanged, and Dokploy cannot durably fence later starts or deployments",
			}
		}
		if trafficOwnerErr != nil {
			if authorityRecoveryAvailable(run) {
				return authorityRecoveryNextStep(run, "the durable owner remains bound to this run, but automatic rollback cannot prove authority")
			}
			return runNextStep{
				Action: "inspect and preserve both source and target data, establish writer and traffic authority manually, then create a fresh migration run",
				Reason: "the durable Dokploy host owner no longer proves authority for rollback recovery: " + trafficOwnerErr.Error(),
			}
		}
		if _, err := planAutomaticRollback(run); err != nil {
			if authorityRecoveryAvailable(run) {
				return authorityRecoveryNextStep(run, "automatic stateful rollback is unavailable because Dokploy cannot durably prevent later target deployments")
			}
			return runNextStep{
				Action: "inspect and preserve both source and target data, establish writer and traffic authority manually, then create a fresh migration run",
				Reason: "automatic stateful rollback is unavailable because Dokploy cannot durably prevent later target deployments",
			}
		}
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		return runNextStep{Action: fmt.Sprintf("run `%s` to finish recovery", runScopedCommand(run, "rollback --live")), Reason: "rollback started but has not completed; traffic may already be on the source"}
	}
	if run.Run.CommitStartedAt != nil {
		if hostOwnerErr != nil {
			return runNextStep{Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before continuing source retirement", Reason: "source retirement started and rollback is no longer available: " + hostOwnerErr.Error()}
		}
		if applyActiveErr != nil {
			return runNextStep{Action: fmt.Sprintf("inspect the live-apply lock for run %s before continuing source retirement", shellQuote(run.Run.Name)), Reason: "source retirement started and rollback is no longer available: " + applyActiveErr.Error()}
		}
		if blocked, ok := dokployHostOperationBlocker(hostOperationActive, hostOperationErr); ok {
			blocked.Reason = "source retirement started and rollback is no longer available: " + blocked.Reason
			return blocked
		}
		if trafficOwnerErr != nil {
			return runNextStep{
				Action: "inspect and preserve both source and target state, establish traffic authority manually, then complete source retirement manually",
				Reason: "the durable Dokploy host owner no longer proves target authority for the interrupted commit: " + trafficOwnerErr.Error(),
			}
		}
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		return runNextStep{Action: finishStartedAcceptanceAction(run), Reason: "source retirement started; rollback is no longer available"}
	}
	if applyActive {
		if hostOwnerErr != nil {
			return runNextStep{Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before retrying live apply", Reason: "live apply is recorded as active: " + hostOwnerErr.Error()}
		}
		if blocked, ok := dokployHostOperationBlocker(hostOperationActive, hostOperationErr); ok && !hostOperationActive {
			blocked.Reason = "live apply is recorded as active: " + blocked.Reason
			return blocked
		}
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		return runNextStep{Action: fmt.Sprintf("run `%s` to view the active apply", liveApplyCommand(run)), Reason: "another process is applying this run"}
	}
	if hostOwnerErr != nil {
		return runNextStep{
			Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before further migration work",
			Reason: hostOwnerErr.Error(),
		}
	}
	if applyActiveErr != nil {
		return runNextStep{Action: fmt.Sprintf("inspect the live-apply lock for run %s before retrying", shellQuote(run.Run.Name)), Reason: applyActiveErr.Error()}
	}
	if blocked, ok := dokployHostOperationBlocker(hostOperationActive, hostOperationErr); ok {
		return blocked
	}
	if manualTargetAuthorityRecorded(run) {
		if trafficOwnerErr != nil {
			return runNextStep{Action: "inspect /var/lib/bort/dokploy-traffic-owner.json before retiring the source", Reason: "manual target authority is recorded, but the durable Dokploy host owner no longer proves it: " + trafficOwnerErr.Error()}
		}
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		if coolifySourceRetirementRequired(run) {
			return runNextStep{Action: "verify the target, then " + manualCoolifySourceRetirementAction(run), Reason: "manual target authority is durable; automatic rollback remains unavailable"}
		}
		return runNextStep{Action: fmt.Sprintf("verify the target, then run `%s` to retire the source", runScopedCommand(run, "commit --apply")), Reason: "manual target authority is durable; automatic rollback remains unavailable"}
	}
	if runMayHaveAmbiguousAuthority(run) {
		if authorityRecoveryAvailable(run) {
			return authorityRecoveryNextStep(run, "the stored run cannot prove writer or traffic authority, and Dokploy cannot durably fence later target starts or deployments")
		}
		return runNextStep{
			Action: "inspect and preserve both source and target data, establish writer and traffic authority manually, then create a fresh migration run",
			Reason: "the stored run cannot prove writer or traffic authority, and Dokploy cannot durably fence later target starts or deployments",
		}
	}
	if run.Run.LiveAppliedAt != nil || liveApplySucceeded(run) {
		if trafficOwnerErr != nil {
			if authorityRecoveryAvailable(run) {
				return authorityRecoveryNextStep(run, "the durable owner remains bound to this run, but automatic actions cannot prove target authority")
			}
			return runNextStep{
				Action: "inspect and preserve both source and target data, establish writer and traffic authority manually, then create a fresh migration run",
				Reason: "the durable Dokploy host owner no longer proves target authority: " + trafficOwnerErr.Error(),
			}
		}
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		if _, err := planAutomaticRollback(run); err != nil {
			if authorityRecoveryAvailable(run) {
				return authorityRecoveryNextStep(run, "the live stateful migration completed, but Dokploy cannot durably fence target writers for automatic rollback")
			}
			if coolifySourceRetirementRequired(run) {
				return runNextStep{
					Action: "verify the target; if validation fails, preserve both sides and recover authority manually; otherwise, after the rollback window, " + manualCoolifySourceRetirementAction(run),
					Reason: "the live stateful migration completed, but Dokploy cannot durably fence target writers for automatic rollback",
				}
			}
			return runNextStep{
				Action: fmt.Sprintf("verify the target; if validation fails, preserve both sides and recover authority manually; otherwise run `%s` after the rollback window", runScopedCommand(run, "commit --apply")),
				Reason: "the live stateful migration completed, but Dokploy cannot durably fence target writers for automatic rollback",
			}
		}
		if coolifySourceRetirementRequired(run) {
			return runNextStep{Action: "verify the target through the rollback window, then " + manualCoolifySourceRetirementAction(run) + fmt.Sprintf("; if validation fails, run `%s` to roll back stateless traffic", runScopedCommand(run, "rollback --live")), Reason: "the live apply completed and the source remains available for rollback"}
		}
		return runNextStep{Action: fmt.Sprintf("verify the target, then run `%s` after the rollback window", runScopedCommand(run, "commit --apply")), Reason: "the live apply completed and the source remains available for rollback"}
	}
	if hostOwned {
		return runNextStep{
			Action: fmt.Sprintf("finish %s before applying this run", dokployOwnerRunLabel(owner)),
			Reason: fmt.Sprintf("that run owns Dokploy host mutations with %s authority", owner.Authority),
		}
	}
	if err := validateStatefulLiveApply(run); err != nil {
		if len(interruptedStatefulSourceCleanup(run)) > 0 {
			if blocked, ok := sourceBlocker(); ok {
				return blocked
			}
			return runNextStep{
				Action: fmt.Sprintf("run `%s` to restart the historically paused source without transferring state", liveApplyCommand(run)),
				Reason: "the ledger proves no state transfer or traffic handoff started; automatic state migration remains unavailable after cleanup",
			}
		}
		return runNextStep{
			Action: "do not rerun this blocked run; complete target setup, persistent-state transfer, traffic cutover, and source retirement outside Bort, or create a new run (new runs stage state before the target is deployed)",
			Reason: err.Error(),
		}
	}
	if step, otherMoved, ok := appliedNewRunRefusal(run.Applied); ok {
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		if otherMoved {
			reason := fmt.Sprintf("no retry of this run can pass pause_source for app %s, and an earlier app's source may already be paused or handed off: %s", step.App, step.Error)
			action := "inspect and preserve both source and target data, restore the earlier apps' source writers and traffic manually"
			if authorityRecoveryAvailable(run) {
				action += fmt.Sprintf(", run `%s` to release host ownership", authorityRecoveryCommand(run, dokployTrafficSource))
			}
			return runNextStep{
				Action: action + ", then create a fresh migration run (target authority is unavailable because app " + step.App + " has no target copy)",
				Reason: reason,
			}
		}
		return runNextStep{
			Action: fmt.Sprintf("the source is still running and no state was transferred: delete or reconcile the Dokploy resources this run created, run `%s` to release host ownership, then choose `%s` or change the source compose, and scan a new run", authorityRecoveryCommand(run, dokployTrafficSource), bortCommand("data <app> <store> --recreate|--managed")),
			Reason: step.Error,
		}
	}
	if len(run.Applied.Steps) > 0 {
		if blocked, ok := sourceBlocker(); ok {
			return blocked
		}
		return runNextStep{Action: fmt.Sprintf("run `%s` to resume the interrupted apply", liveApplyCommand(run)), Reason: "the apply ledger contains incomplete work"}
	}
	if err := stagedTransferRefusal(run); err != nil {
		return runNextStep{
			Action: fmt.Sprintf("for a data store volume, choose `%s`, then %s; otherwise change the source compose and scan a new run", bortCommand("data <app> <store> --recreate|--managed"), reviewedMigrationBundleRecovery(run)),
			Reason: err.Error(),
		}
	}
	if decisions == nil {
		decisions = liveApplyBlockingDecisions(run)
	}
	if len(decisions) > 0 {
		decision := decisions[0]
		return runNextStep{
			Action:     decision.Action,
			Reason:     decision.Reason,
			Artifact:   runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Decisions),
			DecisionID: decision.ID,
		}
	}
	if !hasMigratableRunApps(run) {
		return runNextStep{Action: "create a new migration run that selects at least one non-platform application", Reason: "this run contains no migratable applications; platform-role entries are excluded from live apply"}
	}
	if blocked, ok := sourceBlocker(); ok {
		return blocked
	}
	return runNextStep{
		Action:   fmt.Sprintf("run `%s` to apply the planned steps against the target", liveApplyCommand(run)),
		Reason:   "all setup requirements are resolved; interactive target setup runs inline if needed",
		Artifact: runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Commit),
	}
}

func dokployHostOperationBlocker(active bool, err error) (runNextStep, bool) {
	if err != nil {
		if errors.Is(err, errDokployInstallRecoveryRequired) {
			command, found, recoveryErr := dokployInstallationRecoveryCommand()
			if recoveryErr != nil || !found {
				return runNextStep{Action: "inspect /var/lib/bort/dokploy-live.lock.install-recovery-required before further host mutation", Reason: errors.Join(err, recoveryErr).Error()}, true
			}
			return runNextStep{Action: fmt.Sprintf("run `%s` to reconcile the interrupted installation", command), Reason: errDokployInstallRecoveryRequired.Error()}, true
		}
		return runNextStep{Action: "inspect the host-wide Dokploy operation lock before changing this run", Reason: err.Error()}, true
	}
	if active {
		return runNextStep{Action: "wait for the active Dokploy host operation to finish, then check this run again", Reason: "another process holds the host-wide Dokploy operation lock"}, true
	}
	return runNextStep{}, false
}

func unsupportedPlatformNextStep(run loadedMigrationRun) runNextStep {
	phase := migrationRunPhase(run)
	action := "inspect this run here, then continue it on the Linux source host"
	reason := "Dokploy live actions and their host-wide lock are available only on Linux"
	switch phase {
	case "empty":
		action = "create a new migration run that selects at least one non-platform application"
		reason = "this run contains only platform-role entries, which Bort excludes from live apply"
	case "lock-error":
		action = fmt.Sprintf("inspect the live-apply lock for run %s, then continue recovery on the Linux source host", shellQuote(run.Run.Name))
		reason = "the run's apply.lock state could not be verified"
	case "host-lock-error":
		action = "inspect /var/lib/bort/dokploy-live.lock, then continue recovery on the Linux source host"
		reason = "the host-wide Dokploy operation lock could not be verified"
	case "host-owner-error":
		action = "inspect /var/lib/bort/dokploy-traffic-owner.json, then continue recovery on the Linux source host"
		reason = "Dokploy host ownership could not be verified"
	case "authority-finalizing":
		action = fmt.Sprintf("continue the recorded manual %s-authority finalization on the Linux source host", run.Run.ResolvedAuthority)
		reason = "the authority decision is durable, but lifecycle or host-owner finalization is incomplete"
	case "authority-ambiguous", "authority-ambiguous-rollback":
		action = "preserve both sides, then establish writer and traffic authority on the Linux source host"
		reason = "the stored run cannot prove current writer or traffic authority"
	case "applied":
		action = "verify the live target here, then continue acceptance or rollback on the Linux source host"
		reason = "the target is recorded live and the source remains pending acceptance or recovery"
	case "committing":
		action = "finish source retirement on the Linux source host"
		reason = "commit started and rollback is no longer available"
	case "rolling back":
		action = "finish rollback recovery on the Linux source host"
		reason = "rollback started and may already have changed traffic or source state"
	case "source-recovery":
		action = "resume the historically paused source on the Linux source host"
		reason = "the durable ledger records an incomplete source cleanup"
	case "manual-state":
		action = "complete state transfer and authority recovery manually on the Linux source host"
		reason = "automatic stateful migration is unavailable"
	case "plan-blocked":
		action = "choose a data store strategy and re-plan, or change the source compose and scan a new run, on the Linux source host"
		reason = "live apply would refuse the planned state transfer"
	case "new-run-required":
		action = "recover this run's authority on the Linux source host, then choose a data store strategy or change the source compose and create a new run"
		reason = "live apply refused the planned state transfer, and this run cannot be re-planned"
	case "partial", "applying":
		action = "inspect the durable apply ledger, then resume on the Linux source host"
		reason = "live apply has incomplete work"
	case "planning":
		action = "resolve the recorded planning requirements before continuing on the Linux source host"
		reason = "the run is not ready for live apply"
	}
	return runNextStep{Action: action, Reason: reason}
}

// appliedNewRunRefusal reports a pause_source refusal that no retry can
// pass, and whether the ledger leaves another source paused or any state
// or traffic moved: the run can only be released when it did neither.
func appliedNewRunRefusal(applied runApplied) (refused appliedStep, otherMoved bool, found bool) {
	pausedApps := map[string]struct{}{}
	for _, step := range applied.Steps {
		if step.RequiresNewRun && step.Status == string(dokploy.StepStatusError) {
			refused, found = step, true
			continue
		}
		if !appliedStepMayHaveRun(step) {
			continue
		}
		switch dokploy.StepKind(step.Kind) {
		case dokploy.StepPauseSource:
			pausedApps[step.App] = struct{}{}
		case dokploy.StepResumeSource:
			if appliedStepCompleted(step) {
				delete(pausedApps, step.App)
			} else {
				otherMoved = true
			}
		case dokploy.StepDumpDataStore, dokploy.StepRestoreDataStore, dokploy.StepSyncVolume,
			dokploy.StepResumeTarget, dokploy.StepInstallGateway, dokploy.StepActivateRoutes, dokploy.StepStopCoolifyProxy, dokploy.StepStartDokployProxy:
			otherMoved = true
		}
	}
	return refused, otherMoved || len(pausedApps) > 0, found
}

func appliedRequiresNewRun(applied runApplied) bool {
	_, _, ok := appliedNewRunRefusal(applied)
	return ok
}

func authorityRecoveryNextStep(run loadedMigrationRun, reason string) runNextStep {
	return runNextStep{
		Action: authorityRecoveryChoice(run),
		Reason: reason,
	}
}

func liveApplyCommand(run loadedMigrationRun) string {
	return runScopedCommand(run, "migrate --live")
}

func runScopedCommand(run loadedMigrationRun, args string) string {
	runRef := strings.TrimSpace(run.Run.RunDir)
	runName := strings.TrimSpace(run.Run.Name)
	if runName != "" {
		expectedDir := filepath.Join(".bort", "runs", runName)
		runDirAbs, runDirErr := filepath.Abs(runRef)
		expectedDirAbs, expectedDirErr := filepath.Abs(expectedDir)
		if runRef == "" || (runDirErr == nil && expectedDirErr == nil && filepath.Clean(runDirAbs) == filepath.Clean(expectedDirAbs)) {
			runRef = runName
		}
	}
	if runRef == "" {
		return bortCommand(args)
	}
	return bortCommand(args + " --run " + shellQuote(runRef))
}

func hasDokployCredentials(target string) bool {
	state, err := readBortState(defaultStatePath())
	if err != nil {
		return false
	}
	creds, ok := state.Targets[target]
	return ok && creds.URL != "" && creds.Token != ""
}

func openRunDecisions(run loadedMigrationRun) []runDecision {
	var decisions []runDecision
	if run.Decisions.APIVersion == decisionsAPIVersion {
		decisions = run.Decisions.Decisions
	} else {
		decisions = generateRunDecisions(run, run.Run.UpdatedAt).Decisions
	}
	return applyProgressToDecisions(decisions, run.Progress)
}

func openSetupDecisions(run loadedMigrationRun) []runDecision {
	return openFilteredDecisions(run, func(item runDecisionItem) bool {
		return item.Stage == "prepare"
	})
}

func openReviewDecisions(run loadedMigrationRun) []runDecision {
	return openFilteredDecisions(run, func(item runDecisionItem) bool {
		return item.Readiness == preparer.ReadinessNeedsDecision && liveApplyReviewOnlyDecision(item)
	})
}

func openDownstreamBlockingDecisions(run loadedMigrationRun) []runDecision {
	return openFilteredDecisions(run, func(item runDecisionItem) bool {
		if item.Stage == "prepare" {
			return false
		}
		return item.Readiness == preparer.ReadinessBlocked || item.Readiness == preparer.ReadinessNeedsInput || (item.Readiness == preparer.ReadinessNeedsDecision && !liveApplyReviewOnlyDecision(item))
	})
}

func openFilteredDecisions(run loadedMigrationRun, include func(runDecisionItem) bool) []runDecision {
	decisions := []runDecision{}
	for _, decision := range openRunDecisions(run) {
		items := make([]runDecisionItem, 0, len(decision.Items))
		for _, item := range decision.Items {
			if include(item) {
				items = append(items, item)
			}
		}
		if len(items) == 0 {
			continue
		}
		decisions = append(decisions, runDecisionWithItems(decision, items))
	}
	sortRunDecisions(decisions)
	return decisions
}

func runDecisionWithItems(decision runDecision, items []runDecisionItem) runDecision {
	decision.Items = items
	decision.Count = len(items)
	decision.Apps = nil
	decision.Codes = nil
	decision.Readiness = preparer.ReadinessReadyToCreate
	for _, item := range items {
		decision.Apps = uniqueAppend(decision.Apps, item.App)
		decision.Codes = uniqueAppend(decision.Codes, item.Code)
		decision.Readiness = preparer.WorseReadiness(decision.Readiness, item.Readiness)
	}
	decision.Apps = sortedStrings(decision.Apps)
	decision.Codes = sortedStrings(decision.Codes)
	decision.Action = decisionAction(decision)
	decision.Reason = decisionReason(decision)
	return decision
}

func generateRunDecisions(run loadedMigrationRun, generatedAt time.Time) runDecisions {
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	decisions := runDecisions{
		APIVersion:  decisionsAPIVersion,
		RunName:     run.Run.Name,
		RunDir:      run.Run.RunDir,
		GeneratedAt: generatedAt.UTC(),
		BundleDir:   run.Run.BundleDir,
		Target:      run.Run.Target,
		AppName:     run.Run.AppName,
		DryRun:      true,
	}

	builder := newRunDecisionBuilder()
	for _, readiness := range []preparer.Readiness{preparer.ReadinessBlocked, preparer.ReadinessNeedsInput, preparer.ReadinessNeedsDecision} {
		for _, gate := range gatesWithReadiness(run, readiness) {
			builder.addGate(gate)
		}
	}
	decisions.Decisions = builder.decisions()
	return decisions
}

type runDecisionBuilder struct {
	groups map[string]*runDecision
}

func newRunDecisionBuilder() *runDecisionBuilder {
	return &runDecisionBuilder{groups: map[string]*runDecision{}}
}

func (b *runDecisionBuilder) addGate(gate runGateSummary) {
	kind := decisionKindForGate(gate)
	if kind == "" {
		return
	}
	b.addItem(kind, runDecisionItem{
		Stage:       gate.Stage,
		App:         gate.App,
		Code:        gate.Code,
		ResourceRef: gate.ResourceRef,
		Message:     gate.Message,
		Readiness:   gate.Readiness,
		Artifact:    gate.Artifact,
		Evidence:    gate.Evidence,
	})
}

func (b *runDecisionBuilder) addItem(kind string, item runDecisionItem) {
	if kind == "" {
		kind = "review"
	}
	decision := b.groups[kind]
	if decision == nil {
		decision = &runDecision{ID: kind, Kind: kind, Status: "open", Readiness: preparer.ReadinessReadyToCreate}
		b.groups[kind] = decision
	}
	decision.Items = append(decision.Items, item)
	decision.Readiness = preparer.WorseReadiness(decision.Readiness, item.Readiness)
	decision.Count = len(decision.Items)
	decision.Apps = uniqueAppend(decision.Apps, item.App)
	decision.Codes = uniqueAppend(decision.Codes, item.Code)
}

func (b *runDecisionBuilder) decisions() []runDecision {
	decisions := make([]runDecision, 0, len(b.groups))
	for _, decision := range b.groups {
		decision.Apps = sortedStrings(decision.Apps)
		decision.Codes = sortedStrings(decision.Codes)
		decision.Action = decisionAction(*decision)
		decision.Reason = decisionReason(*decision)
		decisions = append(decisions, *decision)
	}
	sortRunDecisions(decisions)
	return decisions
}

func decisionKindForGate(gate runGateSummary) string {
	code := gate.Code
	switch code {
	case "cutover.sync_not_ready", "rollback.cutover_not_ready", "commit.cutover_not_ready":
		return ""
	}
	if gate.Readiness == preparer.ReadinessBlocked {
		switch {
		case strings.HasPrefix(code, "data_store."):
			return "database_review"
		case strings.HasPrefix(code, "app."), strings.HasPrefix(code, "deploy."):
			return "deploy_artifacts"
		default:
			return "blockers"
		}
	}
	switch {
	case strings.HasPrefix(code, "env."):
		return "environment"
	case strings.HasPrefix(code, "external_requirement."):
		return "external_requirements"
	case strings.HasPrefix(code, "linked_resource."):
		return "support_resources"
	case strings.HasPrefix(code, "data_store."):
		return "data_stores"
	case code == "volume.bind_mount_review":
		return "host_files"
	case strings.HasPrefix(code, "domain."), code == "routes.none":
		return "routes"
	case strings.HasPrefix(code, "cutover."):
		return "cutover"
	case strings.HasPrefix(code, "commit."):
		return "commit"
	case strings.HasPrefix(code, "rollback."):
		return "rollback"
	default:
		return gate.Stage
	}
}

func decisionAction(decision runDecision) string {
	apps := len(decision.Apps)
	count := decision.Count
	switch decision.Kind {
	case "deploy_artifacts":
		return fmt.Sprintf("fix deploy artifacts for %d app(s)", apps)
	case "blockers":
		return fmt.Sprintf("fix %d blocking issue(s) across %d app(s)", count, apps)
	case "database_review":
		return fmt.Sprintf("inspect %d unknown database-like service(s)", count)
	case "environment":
		return fmt.Sprintf("fill environment values for %d app(s)", apps)
	case "external_requirements":
		return fmt.Sprintf("resolve external requirements for %d app(s)", apps)
	case "support_resources":
		return fmt.Sprintf("review database/storage settings for %d app(s)", apps)
	case "data_stores":
		return fmt.Sprintf("confirm data-store strategies for %d service(s)", count)
	case "host_files":
		return fmt.Sprintf("review VPS files/folders for %d app(s)", apps)
	case "volume_copy":
		return fmt.Sprintf("confirm %d volume-copy default(s)", count)
	case "routes":
		return fmt.Sprintf("confirm route setup for %d app(s)", apps)
	case "cutover":
		return fmt.Sprintf("confirm cutover readiness for %d app(s)", apps)
	case "commit":
		return fmt.Sprintf("confirm final commit readiness for %d app(s)", apps)
	case "rollback":
		return fmt.Sprintf("confirm rollback readiness for %d app(s)", apps)
	default:
		return fmt.Sprintf("review %d %s item(s) across %d app(s)", count, decision.Kind, apps)
	}
}

func decisionReason(decision runDecision) string {
	switch decision.Kind {
	case "support_resources":
		return fmt.Sprintf("%d item(s), %d app(s): these apps reference database, cache, or storage settings that may point at another Coolify app", decision.Count, len(decision.Apps))
	case "host_files":
		return fmt.Sprintf("%d item(s), %d app(s): these apps mount files or folders from this VPS into their containers", decision.Count, len(decision.Apps))
	}
	return fmt.Sprintf("%d item(s), %d app(s): %s", decision.Count, len(decision.Apps), strings.Join(decision.Codes, ", "))
}

func sortRunDecisions(decisions []runDecision) {
	sort.Slice(decisions, func(i, j int) bool {
		if preparer.ReadinessRank(decisions[i].Readiness) != preparer.ReadinessRank(decisions[j].Readiness) {
			return preparer.ReadinessRank(decisions[i].Readiness) > preparer.ReadinessRank(decisions[j].Readiness)
		}
		if decisionOrder(decisions[i].Kind) != decisionOrder(decisions[j].Kind) {
			return decisionOrder(decisions[i].Kind) < decisionOrder(decisions[j].Kind)
		}
		return decisions[i].Kind < decisions[j].Kind
	})
}

func decisionOrder(kind string) int {
	order := map[string]int{
		"deploy_artifacts":      0,
		"blockers":              1,
		"database_review":       2,
		"environment":           3,
		"external_requirements": 4,
		"support_resources":     5,
		"data_stores":           6,
		"host_files":            7,
		"volume_copy":           8,
		"routes":                9,
		"cutover":               10,
		"rollback":              11,
		"commit":                12,
	}
	if value, ok := order[kind]; ok {
		return value
	}
	return 100
}

func uniqueAppend(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func sortedStrings(values []string) []string {
	sorted := append([]string{}, values...)
	sort.Strings(sorted)
	return sorted
}

func gatesWithReadiness(run loadedMigrationRun, readiness preparer.Readiness) []runGateSummary {
	gates := []runGateSummary{}
	gates = append(gates, prepareGates(run, readiness)...)
	gates = append(gates, syncGates(run, readiness)...)
	gates = append(gates, cutoverGates(run, readiness)...)
	gates = append(gates, commitGates(run, readiness)...)
	gates = append(gates, rollbackGates(run, readiness)...)
	return uniqueRunGates(gates)
}

func uniqueRunGates(gates []runGateSummary) []runGateSummary {
	unique := []runGateSummary{}
	seen := map[string]struct{}{}
	for _, gate := range gates {
		key := strings.Join([]string{gate.App, gate.Code, gate.ResourceRef, string(gate.Readiness), gate.Message}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, gate)
	}
	return unique
}

func prepareGates(run loadedMigrationRun, readiness preparer.Readiness) []runGateSummary {
	gates := []runGateSummary{}
	artifact := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Prepare)
	for _, app := range run.Prepare.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, gate := range app.Gates {
			if gate.Readiness == readiness {
				gates = append(gates, newRunGate("prepare", artifact, app.Name, gate))
			}
		}
	}
	return gates
}

func syncGates(run loadedMigrationRun, readiness preparer.Readiness) []runGateSummary {
	gates := []runGateSummary{}
	artifact := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Sync)
	for _, app := range run.Sync.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, gate := range app.Gates {
			if gate.Readiness == readiness {
				gates = append(gates, newRunGate("sync", artifact, app.Name, gate))
			}
		}
	}
	return gates
}

func cutoverGates(run loadedMigrationRun, readiness preparer.Readiness) []runGateSummary {
	gates := []runGateSummary{}
	artifact := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Cutover)
	for _, app := range run.Cutover.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, gate := range app.Gates {
			if gate.Readiness == readiness {
				gates = append(gates, newRunGate("cutover", artifact, app.Name, gate))
			}
		}
	}
	return gates
}

func rollbackGates(run loadedMigrationRun, readiness preparer.Readiness) []runGateSummary {
	gates := []runGateSummary{}
	artifact := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Rollback)
	for _, app := range run.Rollback.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, gate := range app.Gates {
			if gate.Readiness == readiness {
				gates = append(gates, newRunGate("rollback", artifact, app.Name, gate))
			}
		}
	}
	return gates
}

func commitGates(run loadedMigrationRun, readiness preparer.Readiness) []runGateSummary {
	gates := []runGateSummary{}
	artifact := runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Commit)
	for _, app := range run.Commit.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, gate := range app.Gates {
			if gate.Readiness == readiness {
				gates = append(gates, newRunGate("commit", artifact, app.Name, gate))
			}
		}
	}
	return gates
}

func newRunGate(stage, artifact, app string, gate preparer.Gate) runGateSummary {
	return runGateSummary{Stage: stage, Artifact: artifact, App: app, Code: gate.Code, Message: gate.Message, Readiness: gate.Readiness, ResourceRef: gate.ResourceRef, Evidence: gate.Evidence}
}

func nextAction(gate runGateSummary) string {
	switch gate.Readiness {
	case preparer.ReadinessBlocked:
		return fmt.Sprintf("fix the %s blocker %s, then rerun the local dry-run", gate.Stage, gate.Code)
	case preparer.ReadinessNeedsInput:
		return fmt.Sprintf("fill the %s input %s, then rerun the local dry-run", gate.Stage, gate.Code)
	default:
		if gate.Code == preparer.GateVolumeBindMountReview {
			return "review the listed VPS files/folders only if their source paths should change"
		}
		if strings.HasPrefix(gate.Code, preparer.GateCodePrefixLinkedResource) || strings.HasPrefix(gate.Code, preparer.GateCodePrefixExternalRequirement) {
			return "review the detected database/storage settings only if they should change"
		}
		switch gate.Stage {
		case "prepare":
			return fmt.Sprintf("decide the prepare gate %s before trusting target setup", gate.Code)
		case "sync":
			return fmt.Sprintf("decide the sync gate %s before planning cutover", gate.Code)
		case "cutover":
			return fmt.Sprintf("decide the cutover gate %s before any route change", gate.Code)
		case "commit":
			return fmt.Sprintf("decide the commit gate %s before accepting target ownership", gate.Code)
		case "rollback":
			return fmt.Sprintf("decide the rollback gate %s before relying on rollback routing", gate.Code)
		default:
			return fmt.Sprintf("review %s in the %s artifact", gate.Code, gate.Stage)
		}
	}
}

func gateReason(gate runGateSummary) string {
	parts := []string{fmt.Sprintf("%s/%s", gate.App, gate.Code)}
	if gate.ResourceRef != "" {
		parts = append(parts, gate.ResourceRef)
	}
	if gate.Message != "" {
		parts = append(parts, gate.Message)
	}
	return strings.Join(parts, ": ")
}

func writeMigrationRunText(w io.Writer, heading string, summary migrationRunSummary) {
	writeMigrationRunTextWithFooter(w, heading, summary, "Dry run only: no target resources, sync operations, route changes, ownership commits, or source cleanup were executed.")
}

func writeLiveMigrationRunText(w io.Writer, heading string, summary migrationRunSummary) {
	writeMigrationRunTextWithFooter(w, heading, summary, "Live mode requested: no changes have been made yet; apply will start only after gates and credentials are checked.")
}

func writeMigrationRunTextWithFooter(w io.Writer, heading string, summary migrationRunSummary, footer string) {
	fmt.Fprintf(w, "%s: %s\n", heading, summary.Run.RunDir)
	fmt.Fprintf(w, "Run: %s\n", summary.Run.Name)
	fmt.Fprintf(w, "Bundle: %s -> %s\n", summary.Run.BundleDir, summary.Run.Target)
	if summary.Run.AppName != "" {
		fmt.Fprintf(w, "Scope: app=%s\n", summary.Run.AppName)
	} else {
		fmt.Fprintln(w, "Scope: all apps")
	}
	fmt.Fprintf(w, "Overall: %s (%s)\n", runReadinessLabel(summary.Readiness), summary.Status)
	fmt.Fprintf(w, "Apps: %d total, %d green, %d yellow, %d red\n", summary.Apps, summary.StatusCounts.Green, summary.StatusCounts.Yellow, summary.StatusCounts.Red)
	if summary.PlatformApps > 0 {
		fmt.Fprintf(w, "Platform/internal apps excluded: %d\n", summary.PlatformApps)
	}
	fmt.Fprintf(w, "Routes: %d cutover, %d rollback, %d commit\n", summary.CutoverRoutes, summary.RollbackRoutes, summary.CommitRoutes)
	fmt.Fprintf(w, "State sync: %s, %s\n", pluralize(summary.StateSteps, "resource step", "resource steps"), pluralize(summary.PauseSteps, "pause/decision step", "pause/decision steps"))
	fmt.Fprintf(w, "Gates: %d blocked, %d need input, %d need decision\n", summary.GateCounts.Blocked, summary.GateCounts.NeedsInput, summary.GateCounts.NeedsDecision)
	fmt.Fprintf(w, "Decisions: %d open\n", len(summary.Decisions))
	if summary.Progress != "" {
		fmt.Fprintln(w, summary.Progress)
	}
	fmt.Fprintf(w, "Artifacts: %s, %s, %s, %s, %s, %s\n", summary.Run.Artifacts.Prepare, summary.Run.Artifacts.Sync, summary.Run.Artifacts.Cutover, summary.Run.Artifacts.Rollback, summary.Run.Artifacts.Commit, summary.Run.Artifacts.Decisions)
	if len(summary.Decisions) > 0 {
		fmt.Fprintln(w, "Open decisions:")
		limit := min(len(summary.Decisions), 3)
		for _, decision := range summary.Decisions[:limit] {
			fmt.Fprintf(w, "  %s %s: %s (%d item(s))\n", decision.Kind, decision.Readiness, decision.Action, decision.Count)
		}
	}
	fmt.Fprintf(w, "Next safe step: %s\n", summary.Next.Action)
	if summary.Next.DecisionID != "" {
		fmt.Fprintf(w, "Next decision: %s\n", summary.Next.DecisionID)
	}
	if summary.Next.Artifact != "" {
		fmt.Fprintf(w, "Next artifact: %s\n", summary.Next.Artifact)
	}
	fmt.Fprintln(w, footer)
}

func runSourceLabel(run migrationRun) string {
	switch strings.TrimSpace(run.Source) {
	case "coolify-local":
		return "Coolify on this server"
	case "coolify":
		return "Coolify API"
	case "docker", "local-docker":
		return "Local Docker"
	case "manifest":
		return "Existing manifest"
	}
	if strings.TrimSpace(run.BundleDir) != "" {
		return "local bundle"
	}
	return "source"
}

func writeRunNextText(w io.Writer, summary migrationRunSummary) {
	fmt.Fprintf(w, "Next safe step: %s\n", summary.Next.Action)
	fmt.Fprintf(w, "Reason: %s\n", summary.Next.Reason)
	if summary.Next.Artifact != "" {
		fmt.Fprintf(w, "Artifact: %s\n", summary.Next.Artifact)
	}
	if summary.Next.DecisionID != "" {
		fmt.Fprintf(w, "Decision: %s\n", summary.Next.DecisionID)
	}
	fmt.Fprintf(w, "Run: %s (%s)\n", summary.Run.Name, summary.Run.RunDir)
	fmt.Fprintf(w, "Overall: %s (%s)\n", runReadinessLabel(summary.Readiness), summary.Status)
	fmt.Fprintln(w, "Dry run only: no live migration action is executed by this command.")
}

func runReadinessLabel(readiness preparer.Readiness) string {
	switch readiness {
	case preparer.ReadinessBlocked:
		return "blocked"
	case preparer.ReadinessNeedsInput:
		return "needs_input"
	case preparer.ReadinessNeedsDecision:
		return "needs_decision"
	default:
		return "ready"
	}
}

func isPlatformRunApp(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), "platform")
}

func hasMigratableRunApps(run loadedMigrationRun) bool {
	for _, app := range run.Prepare.Apps {
		if !isPlatformRunApp(app.Role) {
			return true
		}
	}
	return false
}

func hasOnlyPlatformRunApps(run loadedMigrationRun) bool {
	return len(run.Prepare.Apps) > 0 && !hasMigratableRunApps(run)
}

func (counts *runStatusCounts) add(status preparer.Status) {
	switch status {
	case preparer.StatusRed:
		counts.Red++
	case preparer.StatusYellow:
		counts.Yellow++
	default:
		counts.Green++
	}
}

func (counts *runReadinessCounts) add(readiness preparer.Readiness) {
	switch readiness {
	case preparer.ReadinessBlocked:
		counts.Blocked++
	case preparer.ReadinessNeedsInput:
		counts.NeedsInput++
	case preparer.ReadinessNeedsDecision:
		counts.NeedsDecision++
	default:
		counts.Ready++
	}
}

func newRunDir(runRef, bundleDir, appName string, now time.Time) (string, string) {
	if strings.TrimSpace(runRef) == "" {
		name := defaultRunName(bundleDir, appName, now)
		return filepath.Join(".bort", "runs", name), name
	}
	if isPathRef(runRef) {
		runDir := filepath.Clean(runRef)
		return runDir, runNameFromDir(runDir)
	}
	name := planutil.Slug(runRef)
	if name == "" {
		name = "run"
	}
	return filepath.Join(".bort", "runs", name), name
}

func existingRunDir(runRef string) (string, error) {
	if isPathRef(runRef) {
		return filepath.Clean(runRef), nil
	}
	name := planutil.Slug(runRef)
	if name == "" {
		return "", fmt.Errorf("invalid run name %q", runRef)
	}
	return filepath.Join(".bort", "runs", name), nil
}

func isPathRef(value string) bool {
	value = strings.TrimSpace(value)
	return filepath.IsAbs(value) || strings.Contains(value, "/") || strings.Contains(value, "\\") || strings.HasPrefix(value, ".")
}

func defaultRunName(bundleDir, appName string, now time.Time) string {
	scope := planutil.Slug(appName)
	if scope == "" {
		scope = planutil.Slug(filepath.Base(filepath.Clean(bundleDir)))
	}
	if scope == "" || scope == "." {
		return now.Format("2006-01-02-150405")
	}
	return now.Format("2006-01-02-150405") + "-" + scope
}

func runNameFromDir(runDir string) string {
	name := planutil.Slug(filepath.Base(filepath.Clean(runDir)))
	if name == "" {
		return "run"
	}
	return name
}

func ensurePrivateRunDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if filepath.Clean(path) == "." {
		return nil
	}
	return os.Chmod(path, 0o700)
}

func defaultRunArtifacts() runArtifacts {
	return runArtifacts{
		Prepare:   "prepare.json",
		Sync:      "sync.json",
		Cutover:   "cutover.json",
		Rollback:  "rollback.json",
		Commit:    "commit.json",
		Decisions: "decisions.json",
		Progress:  "progress.json",
		Applied:   "applied.json",
	}
}

func (artifacts runArtifacts) withDefaults() runArtifacts {
	defaults := defaultRunArtifacts()
	if artifacts.Prepare == "" {
		artifacts.Prepare = defaults.Prepare
	}
	if artifacts.Sync == "" {
		artifacts.Sync = defaults.Sync
	}
	if artifacts.Cutover == "" {
		artifacts.Cutover = defaults.Cutover
	}
	if artifacts.Rollback == "" {
		artifacts.Rollback = defaults.Rollback
	}
	if artifacts.Commit == "" {
		artifacts.Commit = defaults.Commit
	}
	if artifacts.Decisions == "" {
		artifacts.Decisions = defaults.Decisions
	}
	if artifacts.Progress == "" {
		artifacts.Progress = defaults.Progress
	}
	if artifacts.Applied == "" {
		artifacts.Applied = defaults.Applied
	}
	return artifacts
}

func runArtifactPath(runDir, artifact string) string {
	clean, err := cleanRelativeArtifact(artifact)
	if err != nil {
		return filepath.Join(filepath.FromSlash(runDir), filepath.FromSlash(artifact))
	}
	return filepath.Join(filepath.FromSlash(runDir), clean)
}

func safeRunArtifactPath(runDir, artifact string) (string, error) {
	clean, err := cleanRelativeArtifact(artifact)
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.FromSlash(runDir), clean)
	if err := containedPath(filepath.FromSlash(runDir), path); err != nil {
		return "", err
	}
	return path, nil
}

func readRunMetadata(path string) (migrationRun, error) {
	var run migrationRun
	if err := planfile.Read(path, &run); err != nil {
		return migrationRun{}, err
	}
	if err := planfile.CheckAPIVersion(path, run.APIVersion, runAPIVersion); err != nil {
		return migrationRun{}, err
	}
	if !run.DryRun {
		return migrationRun{}, fmt.Errorf("%s is not a dry-run migration run", path)
	}
	if (run.ResolvedAuthority == "") != (run.AuthorityResolvedAt == nil) {
		return migrationRun{}, fmt.Errorf("%s has an incomplete manual authority resolution", path)
	}
	if run.ResolvedAuthority != "" && run.ResolvedAuthority != dokployTrafficSource && run.ResolvedAuthority != dokployTrafficTarget {
		return migrationRun{}, fmt.Errorf("%s has invalid resolved authority %q", path, run.ResolvedAuthority)
	}
	if run.AuthorityFinalizedAt != nil && (run.ResolvedAuthority != dokployTrafficSource || run.RolledBackAt == nil) {
		return migrationRun{}, fmt.Errorf("%s has invalid manual authority finalization", path)
	}
	if run.HostOwnerReleaseStartedAt != nil && run.CommittedAt == nil && run.RolledBackAt == nil {
		return migrationRun{}, fmt.Errorf("%s records a host-owner release before commit or rollback", path)
	}
	return run, nil
}

func markRunLiveAppliedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.LiveAppliedAt == nil {
			current.LiveAppliedAt = &now
		}
	})
}

func markRunAuthorityResolvedLocked(run migrationRun, authority string) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.ResolvedAuthority == "" {
			current.ResolvedAuthority = authority
			current.AuthorityResolvedAt = &now
		}
	})
}

func markRunAuthorityFinalizedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.AuthorityFinalizedAt == nil {
			current.AuthorityFinalizedAt = &now
		}
	})
}

func markRunHostOwnerReleaseStartedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.HostOwnerReleaseStartedAt == nil {
			current.HostOwnerReleaseStartedAt = &now
		}
	})
}

func markRunCommitStartedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.CommitStartedAt == nil {
			current.CommitStartedAt = &now
		}
	})
}

func markRunCommittedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.CommittedAt == nil {
			current.CommittedAt = &now
		}
	})
}

func markRunRollbackStartedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.RollbackStartedAt == nil {
			current.RollbackStartedAt = &now
		}
	})
}

func markRunRolledBackLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.RolledBackAt == nil {
			current.RolledBackAt = &now
		}
	})
}

func markRunPurgedLocked(run migrationRun) error {
	return updateRunLifecycleLocked(run, func(current *migrationRun, now time.Time) {
		if current.PurgedAt == nil {
			current.PurgedAt = &now
		}
	})
}

func updateRunLifecycleLocked(run migrationRun, update func(*migrationRun, time.Time)) error {
	runDir := filepath.FromSlash(run.RunDir)
	path := filepath.Join(runDir, "run.json")
	current, err := readRunMetadata(path)
	if err != nil {
		return err
	}
	update(&current, time.Now().UTC())
	return writeJSONArtifact(path, current)
}

func readRunDecisions(path string, run migrationRun) (runDecisions, error) {
	var decisions runDecisions
	if err := planfile.Read(path, &decisions); err != nil {
		return runDecisions{}, err
	}
	if err := planfile.CheckAPIVersion(path, decisions.APIVersion, decisionsAPIVersion); err != nil {
		return runDecisions{}, err
	}
	if !decisions.DryRun {
		return runDecisions{}, fmt.Errorf("%s is not a dry-run decisions artifact", path)
	}
	if err := planfile.CheckBundle(path, decisions.BundleDir, run.BundleDir); err != nil {
		return runDecisions{}, err
	}
	if err := planfile.CheckTarget(path, decisions.Target, run.Target); err != nil {
		return runDecisions{}, err
	}
	return decisions, nil
}

func writeJSONArtifact(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return writeFileAtomic(path, contents, 0o600)
}
