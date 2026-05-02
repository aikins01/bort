package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/planfile"
	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
	syncplan "github.com/aikins01/bort/internal/sync"
)

const (
	runAPIVersion       = "bort.run/v1alpha1"
	decisionsAPIVersion = "bort.decisions/v1alpha1"
)

type migrationRun struct {
	APIVersion               string       `json:"apiVersion"`
	Name                     string       `json:"name"`
	RunDir                   string       `json:"runDir"`
	CreatedAt                time.Time    `json:"createdAt"`
	UpdatedAt                time.Time    `json:"updatedAt"`
	Source                   string       `json:"source,omitempty"`
	BundleDir                string       `json:"bundleDir"`
	ManifestPath             string       `json:"manifest,omitempty"`
	Target                   string       `json:"target"`
	AppName                  string       `json:"app,omitempty"`
	EnvMode                  string       `json:"envMode,omitempty"`
	DryRun                   bool         `json:"dryRun"`
	ObservationWindowSeconds int          `json:"observationWindowSeconds"`
	RollbackWindowSeconds    int          `json:"rollbackWindowSeconds"`
	Artifacts                runArtifacts `json:"artifacts"`
}

type runArtifacts struct {
	Prepare   string `json:"prepare"`
	Sync      string `json:"sync"`
	Cutover   string `json:"cutover"`
	Rollback  string `json:"rollback"`
	Commit    string `json:"commit"`
	Decisions string `json:"decisions"`
}

type loadedMigrationRun struct {
	Run       migrationRun
	Prepare   preparer.Result
	Sync      syncplan.Result
	Cutover   gateway.Result
	Rollback  rollbackplan.Result
	Commit    commitplan.Result
	Decisions runDecisions
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
}

type runNextStep struct {
	Action     string
	Reason     string
	Artifact   string
	DecisionID string
}

type migrationRunOptions struct {
	BundleDir                string
	Target                   string
	AppName                  string
	RunRef                   string
	Source                   string
	EnvMode                  string
	ManifestPath             string
	ObservationWindowSeconds int
	RollbackWindowSeconds    int
}

func runMigrate(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var runRef string
	observationWindowSeconds := gateway.DefaultObservationWindowSeconds
	rollbackWindowSeconds := gateway.DefaultRollbackWindowSeconds

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to include in the run")
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.IntVar(&observationWindowSeconds, "observation-window", gateway.DefaultObservationWindowSeconds, "observation window in seconds")
	fs.IntVar(&rollbackWindowSeconds, "rollback-window", gateway.DefaultRollbackWindowSeconds, "rollback window in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}

	loadedRun, err := createMigrationRun(migrationRunOptions{
		BundleDir:                bundleDir,
		Target:                   target,
		AppName:                  appName,
		RunRef:                   runRef,
		ObservationWindowSeconds: observationWindowSeconds,
		RollbackWindowSeconds:    rollbackWindowSeconds,
	})
	if err != nil {
		return err
	}

	summary := summarizeMigrationRun(loadedRun)
	writeMigrationRunText(stdout, "Migration run created", summary)
	return nil
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

	createdAt := now
	if existing, err := readRunMetadata(filepath.Join(runDir, "run.json")); err == nil && !existing.CreatedAt.IsZero() {
		createdAt = existing.CreatedAt
	}

	preparePlan, err := preparer.Plan(preparer.Options{BundleDir: opts.BundleDir, Target: opts.Target, AppName: opts.AppName})
	if err != nil {
		return loadedMigrationRun{}, err
	}
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

	run := migrationRun{
		APIVersion:               runAPIVersion,
		Name:                     runName,
		RunDir:                   filepath.ToSlash(filepath.Clean(runDir)),
		CreatedAt:                createdAt,
		UpdatedAt:                now,
		Source:                   opts.Source,
		BundleDir:                opts.BundleDir,
		ManifestPath:             opts.ManifestPath,
		Target:                   opts.Target,
		AppName:                  opts.AppName,
		EnvMode:                  opts.EnvMode,
		DryRun:                   true,
		ObservationWindowSeconds: opts.ObservationWindowSeconds,
		RollbackWindowSeconds:    opts.RollbackWindowSeconds,
		Artifacts:                defaultRunArtifacts(),
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
	if err := writeJSONArtifact(filepath.Join(runDir, "run.json"), run); err != nil {
		return loadedMigrationRun{}, err
	}

	return loadedRun, nil
}

func runStatus(_ context.Context, args []string, stdout, stderr io.Writer) error {
	run, err := loadRunFromArgs("status", args, stderr, false)
	if err != nil {
		return err
	}
	writeMigrationRunText(stdout, "Migration run", summarizeMigrationRun(run))
	return nil
}

func runNext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return runContinue(ctx, args, stdout, stderr)
	}

	run, err := loadRunFromArgs("next", args, stderr, false)
	if err != nil {
		return err
	}
	writeRunNextText(stdout, summarizeMigrationRun(run))
	return nil
}

func runContinue(_ context.Context, args []string, stdout, stderr io.Writer) error {
	run, err := loadRunFromArgs("continue", args, stderr, true)
	if err != nil {
		return err
	}
	writeMigrationCockpitText(stdout, summarizeMigrationRun(run))
	return nil
}

func loadRunFromArgs(command string, args []string, stderr io.Writer, allowLatest bool) (loadedMigrationRun, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var runRef string
	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	if err := fs.Parse(args); err != nil {
		return loadedMigrationRun{}, err
	}
	if runRef == "" && fs.NArg() == 1 {
		runRef = fs.Arg(0)
	}
	if strings.TrimSpace(runRef) == "" {
		if allowLatest {
			if latestRef, ok := latestRunRef(); ok {
				return loadMigrationRun(latestRef)
			}
			return loadedMigrationRun{}, fmt.Errorf("no migration run found; run bort to start one")
		}
		return loadedMigrationRun{}, fmt.Errorf("--run is required")
	}
	return loadMigrationRun(runRef)
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
	if run.RunDir == "" {
		run.RunDir = filepath.ToSlash(filepath.Clean(runDir))
	}
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
	return loadedRun, nil
}

func summarizeMigrationRun(run loadedMigrationRun) migrationRunSummary {
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

	summary.Decisions = openRunDecisions(run)
	summary.FirstGates = firstUnresolvedRunGates(run, 3)
	summary.Next = nextSafeStep(run)
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

func nextSafeStep(run loadedMigrationRun) runNextStep {
	decisions := openRunDecisions(run)
	if len(decisions) > 0 {
		decision := decisions[0]
		return runNextStep{
			Action:     decision.Action,
			Reason:     decision.Reason,
			Artifact:   runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Decisions),
			DecisionID: decision.ID,
		}
	}

	for _, readiness := range []preparer.Readiness{preparer.ReadinessBlocked, preparer.ReadinessNeedsInput, preparer.ReadinessNeedsDecision} {
		gates := gatesWithReadiness(run, readiness)
		if len(gates) == 0 {
			continue
		}
		gate := gates[0]
		return runNextStep{Action: nextAction(gate), Reason: gateReason(gate), Artifact: gate.Artifact}
	}
	return runNextStep{
		Action:   "review the generated run artifacts before adding any live execution step",
		Reason:   "no unresolved dry-run gates were found, but live migration execution is not implemented",
		Artifact: runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Commit),
	}
}

func openRunDecisions(run loadedMigrationRun) []runDecision {
	if run.Decisions.APIVersion == decisionsAPIVersion {
		return run.Decisions.Decisions
	}
	return generateRunDecisions(run, run.Run.UpdatedAt).Decisions
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
	for _, app := range run.Sync.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		for _, step := range app.Steps {
			if step.ResourceType == "volume" && step.Strategy == syncplan.StrategyDockerVolumeArchive {
				builder.addItem("volume_copy", runDecisionItem{
					Stage:       "sync",
					App:         app.Name,
					Code:        "volume.copy_default",
					ResourceRef: step.ResourceRef,
					Message:     fmt.Sprintf("confirm Docker volume copy for %s", step.ResourceRef),
					Readiness:   preparer.ReadinessNeedsDecision,
					Artifact:    runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Sync),
				})
			}
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
		return "bind_mounts"
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
		return fmt.Sprintf("choose or confirm support resources for %d app(s)", apps)
	case "data_stores":
		return fmt.Sprintf("confirm data-store strategies for %d service(s)", count)
	case "bind_mounts":
		return fmt.Sprintf("map or confirm bind mounts for %d app(s)", apps)
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
		"bind_mounts":           7,
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
	return runGateSummary{Stage: stage, Artifact: artifact, App: app, Code: gate.Code, Message: gate.Message, Readiness: gate.Readiness, ResourceRef: gate.ResourceRef}
}

func nextAction(gate runGateSummary) string {
	switch gate.Readiness {
	case preparer.ReadinessBlocked:
		return fmt.Sprintf("fix the %s blocker %s, then rerun the local dry-run", gate.Stage, gate.Code)
	case preparer.ReadinessNeedsInput:
		return fmt.Sprintf("fill the %s input %s, then rerun the local dry-run", gate.Stage, gate.Code)
	default:
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
	fmt.Fprintf(w, "State sync: %d resource step(s), %d pause/decision step(s)\n", summary.StateSteps, summary.PauseSteps)
	fmt.Fprintf(w, "Gates: %d blocked, %d need input, %d need decision\n", summary.GateCounts.Blocked, summary.GateCounts.NeedsInput, summary.GateCounts.NeedsDecision)
	fmt.Fprintf(w, "Decisions: %d open\n", len(summary.Decisions))
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
	fmt.Fprintln(w, "Dry run only: no target resources, sync operations, route changes, ownership commits, or source cleanup were executed.")
}

func writeMigrationCockpitText(w io.Writer, summary migrationRunSummary) {
	fmt.Fprintf(w, "Migration cockpit: %s\n", summary.Run.Name)
	fmt.Fprintf(w, "Migration: %s -> %s\n", runSourceLabel(summary.Run), summary.Run.Target)
	if summary.Run.AppName != "" {
		fmt.Fprintf(w, "Scope: app=%s\n", summary.Run.AppName)
	} else if summary.PlatformApps > 0 {
		fmt.Fprintf(w, "Scope: %d app(s), %d platform/internal hidden\n", summary.Apps, summary.PlatformApps)
	} else {
		fmt.Fprintf(w, "Scope: %d app(s)\n", summary.Apps)
	}
	if summary.Run.EnvMode != "" {
		fmt.Fprintf(w, "Environment: %s\n", envModeLabel(summary.Run.EnvMode))
	}
	fmt.Fprintf(w, "Status: %s (%s)\n", humanReadinessLabel(summary.Readiness), summary.Status)
	fmt.Fprintf(w, "Apps: %d green, %d yellow, %d red\n", summary.StatusCounts.Green, summary.StatusCounts.Yellow, summary.StatusCounts.Red)
	fmt.Fprintf(w, "Routes: %d cutover, %d rollback, %d commit\n", summary.CutoverRoutes, summary.RollbackRoutes, summary.CommitRoutes)
	fmt.Fprintf(w, "State sync: %d resource step(s), %d pause/decision step(s)\n", summary.StateSteps, summary.PauseSteps)
	fmt.Fprintf(w, "Open work: %d decision(s), %d blocked, %d need input, %d need decision\n", len(summary.Decisions), summary.GateCounts.Blocked, summary.GateCounts.NeedsInput, summary.GateCounts.NeedsDecision)
	if len(summary.Decisions) > 0 {
		fmt.Fprintln(w, "Work queue:")
		limit := min(len(summary.Decisions), 3)
		for _, decision := range summary.Decisions[:limit] {
			fmt.Fprintf(w, "  %s: %s (%s, %d item(s))\n", decision.Kind, decision.Action, humanReadinessLabel(decision.Readiness), decision.Count)
		}
	}
	fmt.Fprintf(w, "Next: %s\n", summary.Next.Action)
	if summary.Next.Reason != "" {
		fmt.Fprintf(w, "Why: %s\n", summary.Next.Reason)
	}
	fmt.Fprintln(w, "Continue: bort continue")
	fmt.Fprintln(w, "Safety: dry run only; no target resources, sync operations, route changes, ownership commits, or source cleanup were executed.")
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

func envModeLabel(mode string) string {
	switch mode {
	case "include-values":
		return "known values kept in private local files"
	case "redacted":
		return "redacted; fill private env templates later"
	default:
		return mode
	}
}

func humanReadinessLabel(readiness preparer.Readiness) string {
	switch readiness {
	case preparer.ReadinessBlocked:
		return "blocked"
	case preparer.ReadinessNeedsInput:
		return "needs input"
	case preparer.ReadinessNeedsDecision:
		return "needs decision"
	default:
		return "ready"
	}
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
	return runArtifacts{Prepare: "prepare.json", Sync: "sync.json", Cutover: "cutover.json", Rollback: "rollback.json", Commit: "commit.json", Decisions: "decisions.json"}
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
	return artifacts
}

func runArtifactPath(runDir, artifact string) string {
	if filepath.IsAbs(artifact) {
		return filepath.Clean(artifact)
	}
	return filepath.Join(filepath.FromSlash(runDir), filepath.FromSlash(artifact))
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
	return run, nil
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
	return writeOutput(io.Discard, path, func(out io.Writer) error {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
}
