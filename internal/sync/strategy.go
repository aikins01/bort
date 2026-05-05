package sync

import (
	"fmt"
	"strings"

	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
)

const APIVersion = "bort.sync/v1alpha1"

type Strategy string

const (
	StrategyNone                       Strategy = "none"
	StrategyDockerVolumeArchive        Strategy = "docker_volume_archive"
	StrategyRsync                      Strategy = "rsync"
	StrategyPostgresDumpOrLogical      Strategy = "pg_dump_restore_or_logical_replication"
	StrategyMySQLDumpRestore           Strategy = "mysqldump_restore"
	StrategyMongoDumpRestore           Strategy = "mongodump_restore"
	StrategyRedisSnapshotOrVolumeCopy  Strategy = "snapshot_aof_or_volume_copy"
	StrategyObjectStorageMirror        Strategy = "mc_mirror"
	StrategySQLiteStoppedFileCopy      Strategy = "stopped_file_copy"
	StrategyVectorSnapshotOrExport     Strategy = "snapshot_or_collection_export"
	StrategyFilesystemSnapshotOrVolume Strategy = "filesystem_snapshot_or_volume_copy"
	StrategyVolumeSync                 Strategy = "volume_sync"
	StrategyRecreateIfCacheOnly        Strategy = "recreate_if_cache_only"
	StrategyStoppedVolumeCopy          Strategy = "stopped_volume_copy"
	StrategyManual                     Strategy = "manual"
	StrategyManualReview               Strategy = "manual_review"
)

type Phase string

const (
	PhaseTargetPrepare      Phase = "target_prepare"
	PhaseDependencyReuse    Phase = "dependency_reuse"
	PhaseDependencyDecision Phase = "dependency_decision"
	PhaseStateSync          Phase = "state_sync"
	PhaseVerify             Phase = "verify"
)

type PauseRequirement string

const (
	PauseNone          PauseRequirement = "none"
	PauseCutoverWindow PauseRequirement = "cutover_window"
	PauseStoppedSource PauseRequirement = "stopped_source"
	PauseNeedsDecision PauseRequirement = "needs_decision"
)

type Options struct {
	BundleDir string
	AppName   string
	Target    string
}

type Result struct {
	APIVersion string          `json:"apiVersion"`
	BundleDir  string          `json:"bundleDir"`
	Target     string          `json:"target"`
	DryRun     bool            `json:"dryRun"`
	Status     preparer.Status `json:"status"`
	Apps       []AppPlan       `json:"apps"`
}

type AppPlan struct {
	Name             string             `json:"name"`
	Directory        string             `json:"directory"`
	Role             string             `json:"role,omitempty"`
	Status           preparer.Status    `json:"status"`
	Readiness        preparer.Readiness `json:"readiness"`
	PrepareReadiness preparer.Readiness `json:"prepareReadiness"`
	Gates            []preparer.Gate    `json:"gates,omitempty"`
	Steps            []Step             `json:"steps,omitempty"`
	Actions          []Action           `json:"actions"`
}

type Step struct {
	ID           string             `json:"id"`
	Phase        Phase              `json:"phase"`
	ResourceType string             `json:"resourceType"`
	ResourceRef  string             `json:"resourceRef"`
	TargetRef    string             `json:"targetRef,omitempty"`
	Action       string             `json:"action"`
	TargetAction string             `json:"targetAction,omitempty"`
	Strategy     Strategy           `json:"strategy"`
	Pause        PauseRequirement   `json:"pause"`
	Readiness    preparer.Readiness `json:"readiness"`
	DependsOn    []string           `json:"dependsOn,omitempty"`
	Evidence     []string           `json:"evidence,omitempty"`
}

type Action struct {
	Severity preparer.Severity `json:"severity"`
	Kind     string            `json:"kind"`
	Message  string            `json:"message"`
}

func Plan(opts Options) (Result, error) {
	preparePlan, err := preparer.Plan(preparer.Options{BundleDir: opts.BundleDir, AppName: opts.AppName, Target: opts.Target})
	if err != nil {
		return Result{}, err
	}
	return PlanFromPrepare(preparePlan), nil
}

func PlanFromPrepare(preparePlan preparer.Result) Result {
	result := Result{
		APIVersion: APIVersion,
		BundleDir:  preparePlan.BundleDir,
		Target:     preparePlan.Target,
		DryRun:     true,
		Status:     preparer.StatusGreen,
	}
	for _, app := range preparePlan.Apps {
		appPlan := planApp(app)
		result.Apps = append(result.Apps, appPlan)
		result.Status = preparer.WorseStatus(result.Status, appPlan.Status)
	}
	return result
}

func planApp(app preparer.AppPlan) AppPlan {
	plan := AppPlan{
		Name:             app.Name,
		Directory:        app.Directory,
		Role:             app.Role,
		Status:           app.Status,
		Readiness:        app.Readiness,
		PrepareReadiness: app.Readiness,
		Gates:            app.Gates,
	}

	usedIDs := map[string]int{}
	composeStep, hasComposeStep := composeAppStep(app, usedIDs)
	if hasComposeStep {
		plan.addStep(composeStep)
	}

	for _, step := range linkedResourceSteps(app, usedIDs) {
		plan.addStep(step)
	}

	stateStepIDs := []string{}
	for _, step := range dataStoreSteps(app, usedIDs, composeStep.ID) {
		if requiresStateVerification(step) {
			stateStepIDs = append(stateStepIDs, step.ID)
		}
		plan.addStep(step)
	}
	for _, step := range volumeSteps(app, usedIDs, composeStep.ID) {
		if requiresStateVerification(step) {
			stateStepIDs = append(stateStepIDs, step.ID)
		}
		plan.addStep(step)
	}

	if len(stateStepIDs) == 0 {
		plan.add(preparer.SeverityInfo, "state-sync", "no state sync resources detected")
	} else {
		plan.addStep(verifyStep(usedIDs, stateStepIDs))
	}

	for _, step := range plan.Steps {
		plan.Readiness = preparer.WorseReadiness(plan.Readiness, step.Readiness)
	}
	plan.Status = preparer.StatusFromReadiness(plan.Readiness)
	return plan
}

func composeAppStep(app preparer.AppPlan, usedIDs map[string]int) (Step, bool) {
	if strings.TrimSpace(app.Resources.App.Type) == "" {
		return Step{}, false
	}

	targetRef := app.Resources.App.Name
	targetAction := "prepare_compose_app"
	if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
		targetRef = "dokploy.compose_app:" + app.TargetResources.Dokploy.ComposeApp.Name
		targetAction = "create_compose_app_from_prepare_spec"
	}

	return Step{
		ID:           planutil.NextStepID(usedIDs, "target:compose-app"),
		Phase:        PhaseTargetPrepare,
		ResourceType: "compose_app",
		ResourceRef:  "app",
		TargetRef:    targetRef,
		Action:       "prepare_target_app_shell",
		TargetAction: targetAction,
		Strategy:     StrategyNone,
		Pause:        PauseNone,
		Readiness:    app.Resources.App.Readiness,
	}, true
}

func linkedResourceSteps(app preparer.AppPlan, usedIDs map[string]int) []Step {
	targetActions := linkedTargetActions(app)
	steps := make([]Step, 0, len(app.Resources.LinkedResources))
	for _, link := range app.Resources.LinkedResources {
		resourceRef := "linked-resource:" + planutil.Fallback(link.App, link.Kind)
		steps = append(steps, Step{
			ID:           planutil.NextStepID(usedIDs, "dependency:"+link.Kind+":"+link.App),
			Phase:        PhaseDependencyReuse,
			ResourceType: "linked_resource",
			ResourceRef:  resourceRef,
			TargetRef:    linkedTargetRef(app, link),
			Action:       "reuse_detected_support_resource",
			TargetAction: targetActions[newLinkedResourceKey(link.Kind, link.App, link.AppID)],
			Strategy:     StrategyNone,
			Pause:        PauseNone,
			Readiness:    link.Readiness,
			Evidence:     planutil.UniqueStrings(append(append([]string{}, link.Reasons...), link.Networks...)),
		})
	}
	return steps
}

func dataStoreSteps(app preparer.AppPlan, usedIDs map[string]int, composeStepID string) []Step {
	targetActions := dataStoreTargetActions(app)
	steps := make([]Step, 0, len(app.Resources.DataStores))
	for _, store := range app.Resources.DataStores {
		resourceRef := "data-store:" + planutil.Fallback(store.Service, store.Kind)
		steps = append(steps, Step{
			ID:           planutil.NextStepID(usedIDs, "state:data-store:"+store.Service+":"+store.Kind),
			Phase:        PhaseStateSync,
			ResourceType: "data_store",
			ResourceRef:  resourceRef,
			TargetRef:    dataStoreTargetRef(app, store),
			Action:       "plan_data_store_sync",
			TargetAction: targetActions[newDataStoreKey(store.Kind, store.Service)],
			Strategy:     dataStoreStrategy(store),
			Pause:        dataStorePause(store),
			Readiness:    store.Readiness,
			DependsOn:    planutil.OptionalDependency(composeStepID),
			Evidence:     store.Volumes,
		})
	}
	return steps
}

func volumeSteps(app preparer.AppPlan, usedIDs map[string]int, composeStepID string) []Step {
	targetActions := volumeTargetActions(app)
	steps := make([]Step, 0, len(app.Resources.Volumes))
	for _, volume := range app.Resources.Volumes {
		resourceRef := "volume:" + volumeResourceLabel(volume)
		steps = append(steps, Step{
			ID:           planutil.NextStepID(usedIDs, "state:volume:"+volume.Service+":"+volume.Target),
			Phase:        volumePhase(volume),
			ResourceType: "volume",
			ResourceRef:  resourceRef,
			TargetRef:    volumeTargetRef(app, volume),
			Action:       volumeAction(volume),
			TargetAction: targetActions[newVolumeKey(volume.Type, volume.Service, volume.Name, volume.Source, volume.Target)],
			Strategy:     volumeStrategy(volume),
			Pause:        volumePause(volume),
			Readiness:    volume.Readiness,
			DependsOn:    planutil.OptionalDependency(composeStepID),
			Evidence:     planutil.UniqueStrings([]string{planutil.Fallback(volume.Name, volume.Source), volume.Target}),
		})
	}
	return steps
}

func requiresStateVerification(step Step) bool {
	return step.Phase == PhaseStateSync && step.Strategy != StrategyNone
}

func verifyStep(usedIDs map[string]int, dependencies []string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "verify:state"),
		Phase:        PhaseVerify,
		ResourceType: "app",
		ResourceRef:  "app",
		Action:       "verify_restored_state_before_cutover",
		Strategy:     StrategyNone,
		Pause:        PauseNone,
		Readiness:    preparer.ReadinessNeedsDecision,
		DependsOn:    planutil.UniqueStrings(dependencies),
	}
}

func (p *AppPlan) addStep(step Step) {
	step.DependsOn = planutil.UniqueStrings(step.DependsOn)
	step.Evidence = planutil.UniqueStrings(step.Evidence)
	p.Steps = append(p.Steps, step)
	p.add(preparer.SeverityFromReadiness(step.Readiness), actionKind(step), actionMessage(step))
}

func (p *AppPlan) add(severity preparer.Severity, kind, message string) {
	p.Actions = append(p.Actions, Action{Severity: severity, Kind: kind, Message: message})
}

func actionKind(step Step) string {
	switch step.Phase {
	case PhaseTargetPrepare:
		return "target-prepare"
	case PhaseDependencyDecision, PhaseDependencyReuse:
		return "linked-resource"
	case PhaseVerify:
		return "verify"
	default:
		return "state-sync"
	}
}

func actionMessage(step Step) string {
	switch step.ResourceType {
	case "compose_app":
		return fmt.Sprintf("requires prepared target compose app %s before state sync", planutil.Fallback(step.TargetRef, step.ResourceRef))
	case "linked_resource":
		return fmt.Sprintf("reuse existing database/storage settings for %s", step.ResourceRef)
	case "data_store":
		return fmt.Sprintf("plan %s data-store sync for %s", step.Strategy, step.ResourceRef)
	case "volume":
		if step.Action == "preserve_host_path_mount" || step.TargetAction == "preserve_vps_file_mount" {
			return fmt.Sprintf("preserve VPS file/folder for %s", step.ResourceRef)
		}
		return fmt.Sprintf("plan %s volume sync for %s", step.Strategy, step.ResourceRef)
	case "app":
		return "verify restored state before cutover"
	default:
		return step.Action
	}
}

func dataStoreStrategy(store preparer.DataStoreResource) Strategy {
	switch Strategy(store.Strategy) {
	case StrategyPostgresDumpOrLogical,
		StrategyMySQLDumpRestore,
		StrategyMongoDumpRestore,
		StrategyRedisSnapshotOrVolumeCopy,
		StrategyObjectStorageMirror,
		StrategySQLiteStoppedFileCopy,
		StrategyVectorSnapshotOrExport,
		StrategyManualReview:
		return Strategy(store.Strategy)
	case "":
		return StrategyManual
	default:
		return Strategy(store.Strategy)
	}
}

func dataStorePause(store preparer.DataStoreResource) PauseRequirement {
	if store.Readiness == preparer.ReadinessBlocked || store.Strategy == "manual_review" {
		return PauseNeedsDecision
	}
	switch Strategy(store.Strategy) {
	case StrategyPostgresDumpOrLogical,
		StrategyMySQLDumpRestore,
		StrategyMongoDumpRestore,
		StrategyRedisSnapshotOrVolumeCopy,
		StrategyObjectStorageMirror,
		StrategyVectorSnapshotOrExport:
		return PauseCutoverWindow
	case StrategySQLiteStoppedFileCopy:
		return PauseStoppedSource
	default:
		if store.Fallback == string(StrategyStoppedVolumeCopy) || store.Fallback == string(StrategyFilesystemSnapshotOrVolume) || store.Fallback == string(StrategyVolumeSync) {
			return PauseNeedsDecision
		}
		return PauseNeedsDecision
	}
}

func volumePhase(volume preparer.VolumeResource) Phase {
	if volume.Type == "bind" {
		return PhaseTargetPrepare
	}
	return PhaseStateSync
}

func volumeAction(volume preparer.VolumeResource) string {
	if volume.Type == "bind" {
		return "preserve_host_path_mount"
	}
	return "plan_volume_sync"
}

func volumeStrategy(volume preparer.VolumeResource) Strategy {
	switch volume.Type {
	case "volume":
		return StrategyDockerVolumeArchive
	case "bind":
		return StrategyNone
	default:
		return StrategyManual
	}
}

func volumePause(volume preparer.VolumeResource) PauseRequirement {
	switch volume.Type {
	case "volume":
		return PauseStoppedSource
	case "bind":
		return PauseNone
	default:
		return PauseNeedsDecision
	}
}

func linkedTargetActions(app preparer.AppPlan) map[linkedResourceMapKey]string {
	actions := map[linkedResourceMapKey]string{}
	dokploy := dokployTarget(app)
	if dokploy == nil {
		return actions
	}
	for _, link := range dokploy.LinkedResources {
		actions[newLinkedResourceKey(link.Kind, link.CandidateApp, link.CandidateAppID)] = link.Action
	}
	return actions
}

func dataStoreTargetActions(app preparer.AppPlan) map[dataStoreMapKey]string {
	actions := map[dataStoreMapKey]string{}
	dokploy := dokployTarget(app)
	if dokploy == nil {
		return actions
	}
	for _, store := range dokploy.DataStores {
		actions[newDataStoreKey(store.Kind, store.Service)] = store.Action
	}
	return actions
}

func volumeTargetActions(app preparer.AppPlan) map[volumeMapKey]string {
	actions := map[volumeMapKey]string{}
	dokploy := dokployTarget(app)
	if dokploy == nil {
		return actions
	}
	for _, volume := range dokploy.Volumes {
		actions[newVolumeKey(volume.Type, volume.Service, volume.Name, volume.Source, volume.Target)] = volume.Action
	}
	return actions
}

func linkedTargetRef(app preparer.AppPlan, link preparer.LinkedResourceCandidate) string {
	if dokployTarget(app) != nil {
		return "dokploy.linked_resource:" + planutil.Fallback(link.App, link.Kind)
	}
	return planutil.Fallback(link.App, link.Kind)
}

func dataStoreTargetRef(app preparer.AppPlan, store preparer.DataStoreResource) string {
	if dokployTarget(app) != nil {
		return "dokploy.data_store:" + planutil.Fallback(store.Service, store.Kind)
	}
	return planutil.Fallback(store.Service, store.Kind)
}

func volumeTargetRef(app preparer.AppPlan, volume preparer.VolumeResource) string {
	label := volumeResourceLabel(volume)
	if dokployTarget(app) != nil {
		return "dokploy.volume:" + label
	}
	return label
}

func dokployTarget(app preparer.AppPlan) *preparer.DokployResources {
	if app.TargetResources == nil {
		return nil
	}
	return app.TargetResources.Dokploy
}

type dataStoreMapKey struct {
	kind    string
	service string
}

type linkedResourceMapKey struct {
	kind  string
	app   string
	appID string
}

type volumeMapKey struct {
	volumeType string
	service    string
	name       string
	source     string
	target     string
}

func newDataStoreKey(kind, service string) dataStoreMapKey {
	return dataStoreMapKey{kind: kind, service: service}
}

func newLinkedResourceKey(kind, app, appID string) linkedResourceMapKey {
	return linkedResourceMapKey{kind: kind, app: app, appID: appID}
}

func newVolumeKey(volumeType, service, name, source, target string) volumeMapKey {
	return volumeMapKey{volumeType: volumeType, service: service, name: name, source: source, target: target}
}

func volumeResourceLabel(volume preparer.VolumeResource) string {
	label := planutil.Fallback(volume.Service, "app")
	if volume.Target != "" {
		label += " -> " + volume.Target
	}
	return label
}
