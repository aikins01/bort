package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
	syncplan "github.com/aikins01/bort/internal/sync"
	"gopkg.in/yaml.v3"
)

type StepKind string

const (
	StepCreateProject     StepKind = "create_project"
	StepCreateService     StepKind = "create_service"
	StepPushImage         StepKind = "push_image"
	StepUploadEnv         StepKind = "upload_env"
	StepCreateVolume      StepKind = "create_volume"
	StepPauseSource       StepKind = "pause_source"
	StepResumeSource      StepKind = "resume_source"
	StepResumeTarget      StepKind = "resume_target"
	StepSyncVolume        StepKind = "sync_volume"
	StepDumpDataStore     StepKind = "dump_data_store"
	StepRestoreDataStore  StepKind = "restore_data_store"
	StepInstallGateway    StepKind = "install_gateway"
	StepActivateRoutes    StepKind = "activate_routes"
	StepStopCoolifyProxy  StepKind = "stop_coolify_proxy"
	StepStartDokployProxy StepKind = "start_dokploy_proxy"
	StepStopSourceApp     StepKind = "stop_source_app"
)

// proxy container names assumed by the cutover swap. coolify-proxy is
// the canonical name regardless of whether coolify runs in traefik or
// caddy mode (the proxy compose file always names it coolify-proxy).
// dokploy-traefik is created by dokploy's official install.sh.
const (
	coolifyProxyContainer = "coolify-proxy"
	dokployProxyContainer = "dokploy-traefik"
)

type Step struct {
	Kind StepKind
	App  string
	Ref  string
}

type StepStatus string

const (
	StepStatusStarted StepStatus = "started"
	StepStatusOK      StepStatus = "ok"
	StepStatusSkipped StepStatus = "skipped"
	StepStatusError   StepStatus = "error"
)

type StepProgress struct {
	Index   int
	Total   int
	Step    Step
	Status  StepStatus
	Message string
	Err     error
}

type Plan struct {
	Steps      []Step
	Prepare    preparer.Result
	Sync       syncplan.Result
	Cutover    gateway.Result
	RunName    string
	RunDir     string
	ResumeFrom int
	BeforeStep *func(StepProgress) error
	OnProgress *func(StepProgress)
}

func PlanFromArtifacts(prepare preparer.Result, sync syncplan.Result, cutover gateway.Result) Plan {
	plan := Plan{Prepare: prepare, Sync: sync, Cutover: cutover}
	for _, app := range prepare.Apps {
		plan.Steps = append(plan.Steps, Step{Kind: StepCreateProject, App: app.Name, Ref: dokployProjectName(app)})
		if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
			composeRef := app.TargetResources.Dokploy.ComposeApp.Name
			plan.Steps = append(plan.Steps, Step{Kind: StepCreateService, App: app.Name, Ref: composeRef})
			if len(app.TargetResources.Dokploy.EnvFiles) > 0 {
				plan.Steps = append(plan.Steps, Step{Kind: StepUploadEnv, App: app.Name, Ref: composeRef})
			}
			plan.Steps = append(plan.Steps, Step{Kind: StepPushImage, App: app.Name, Ref: composeRef})
			for _, volume := range app.TargetResources.Dokploy.Volumes {
				if volume.Type != "volume" {
					continue
				}
				ref := volume.Name
				if ref == "" {
					ref = volume.Target
				}
				plan.Steps = append(plan.Steps, Step{Kind: StepCreateVolume, App: app.Name, Ref: ref})
			}
		}
	}
	routedAppNames := map[string]struct{}{}
	for _, app := range cutover.Apps {
		for _, route := range app.Routes {
			if strings.TrimSpace(route.Host) == "" {
				continue
			}
			routedAppNames[app.Name] = struct{}{}
		}
	}
	for _, app := range sync.Apps {
		var dataStoreSteps, volumeSteps []Step
		prepApp, hasPrepApp := findPrepareApp(prepare, app.Name)
		for _, step := range app.Steps {
			switch step.ResourceType {
			case "volume":
				if step.Strategy == syncplan.StrategyNone || step.TargetAction == "preserve_vps_file_mount" {
					continue
				}
				// volumes owned by logical-dump or skip-strategy stores must
				// not get a raw sync step: logical owns migration, skip
				// must leave the target empty. only "volume" migration
				// kind allows raw copy of a data store volume.
				if hasPrepApp {
					if volume, ok := findPrepareVolume(prepApp, step.ResourceRef); ok && volumeOwnedByLogicalOrSkippedStore(prepApp, volume) {
						continue
					}
				}
				volumeSteps = append(volumeSteps, Step{Kind: StepSyncVolume, App: app.Name, Ref: step.ResourceRef})
			case "data_store":
				if hasPrepApp {
					if store, ok := findPrepareDataStore(prepApp, step.ResourceRef); ok && dataStoreMigrationKind(store) != dataStoreMigrationLogical {
						continue
					}
				}
				dataStoreSteps = append(dataStoreSteps,
					Step{Kind: StepDumpDataStore, App: app.Name, Ref: step.ResourceRef},
					Step{Kind: StepRestoreDataStore, App: app.Name, Ref: step.ResourceRef},
				)
			}
		}
		// pause runs before any state work so app writers stop before
		// pg_dump captures its snapshot and before raw volume copies
		// see the on-disk format. logical-dump stores stay live (they
		// are excluded from quiesce targets); only the app's writers
		// and volume-strategy stores actually stop here.
		if len(dataStoreSteps) > 0 || len(volumeSteps) > 0 {
			plan.Steps = append(plan.Steps, Step{Kind: StepPauseSource, App: app.Name, Ref: app.Name})
		}
		plan.Steps = append(plan.Steps, dataStoreSteps...)
		plan.Steps = append(plan.Steps, volumeSteps...)
		if len(dataStoreSteps) > 0 || len(volumeSteps) > 0 {
			if _, routed := routedAppNames[app.Name]; !routed {
				plan.Steps = append(plan.Steps, Step{Kind: StepResumeSource, App: app.Name, Ref: app.Name})
			}
			plan.Steps = append(plan.Steps, Step{Kind: StepResumeTarget, App: app.Name, Ref: app.Name})
		}
	}
	hasRoutes := false
	routedApps := []string{}
	routedAppSeen := map[string]struct{}{}
	for _, app := range cutover.Apps {
		for _, route := range app.Routes {
			plan.Steps = append(plan.Steps, Step{Kind: StepInstallGateway, App: app.Name, Ref: route.Host})
			hasRoutes = true
			if _, seen := routedAppSeen[app.Name]; !seen {
				routedAppSeen[app.Name] = struct{}{}
				routedApps = append(routedApps, app.Name)
			}
		}
	}
	for _, appName := range routedApps {
		plan.Steps = append(plan.Steps, Step{Kind: StepActivateRoutes, App: appName, Ref: "routes"})
	}
	// only swap proxies when something actually depends on :80/:443 —
	// keeps no-route migrations from disturbing a healthy coolify host.
	if hasRoutes {
		plan.Steps = append(plan.Steps,
			Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer},
			Step{Kind: StepStartDokployProxy, Ref: dokployProxyContainer},
		)
	}
	return plan
}

func dokployProjectName(app preparer.AppPlan) string {
	if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
		if name := strings.TrimSpace(app.TargetResources.Dokploy.Project.Name); name != "" {
			return name
		}
	}
	if app.ProjectGroup != nil {
		if name := strings.TrimSpace(app.ProjectGroup.Name); name != "" {
			return name
		}
	}
	return app.Name
}

// PlanForCommit produces only the steps that bort commit --apply needs:
// stop every source-app stack, and only reassert that coolify-proxy is
// stopped when the cutover plan actually had routes (otherwise cutover
// never touched the proxy and an app-scoped commit must not affect
// unrelated coolify apps still served by it). it intentionally does NOT
// include any migrate steps so a commit cannot accidentally re-deploy or
// re-route. containers are stopped, never removed: operators on a long
// rollback window can docker start them back up to roll back manually.
func PlanForCommit(prepare preparer.Result, commit gateway.Result) Plan {
	plan := Plan{Prepare: prepare, Cutover: commit}
	for _, app := range prepare.Apps {
		plan.Steps = append(plan.Steps, Step{Kind: StepStopSourceApp, App: app.Name, Ref: app.Name})
	}
	if cutoverPlanHasRoutes(commit) {
		plan.Steps = append(plan.Steps, Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer})
	}
	return plan
}

func cutoverPlanHasRoutes(commit gateway.Result) bool {
	for _, app := range commit.Apps {
		if len(app.Routes) > 0 {
			return true
		}
	}
	return false
}

type appCache struct {
	ProjectID                  string
	EnvironmentID              string
	ComposeID                  string
	ComposeAppName             string
	DiscoveryRedeployAttempted bool
	TargetWritersStopped       []dockerContainer
	MigratedVolumeMounts       map[string]migratedVolumeMount
}

type applyContext struct {
	plan  Plan
	cache map[string]*appCache
}

func (a *applyContext) entry(app string) *appCache {
	if a.cache == nil {
		a.cache = map[string]*appCache{}
	}
	if existing, ok := a.cache[app]; ok {
		return existing
	}
	entry := &appCache{}
	a.cache[app] = entry
	return entry
}

func validatePlanReadyForLiveApply(plan Plan) error {
	for _, app := range plan.Prepare.Apps {
		if isPlatformAppRole(app.Role) {
			continue
		}
		switch app.Readiness {
		case "", preparer.ReadinessReadyToCreate, preparer.ReadinessNeedsDecision:
		default:
			return fmt.Errorf("live apply blocked by prepare readiness for app %s: %s", app.Name, app.Readiness)
		}
		for _, gate := range app.Gates {
			switch gate.Readiness {
			case preparer.ReadinessReadyToCreate, preparer.ReadinessNeedsDecision:
			default:
				return fmt.Errorf("live apply blocked by prepare gate %s/%s: %s", app.Name, gate.Code, gate.Message)
			}
		}
	}
	return nil
}

type activeBortOverridePatch struct {
	PatchID     string `json:"patchId"`
	FilePath    string `json:"filePath"`
	ComposeName string `json:"composeName"`
	SourceType  string `json:"sourceType"`
	Repository  string `json:"repository"`
	Branch      string `json:"branch"`
}

func (c *Client) validateNoActiveBortOverrides(ctx context.Context, composeID string) error {
	composeID = strings.TrimSpace(composeID)
	if composeID == "" {
		return fmt.Errorf("inspect active Dokploy patches: missing composeId")
	}
	patches, err := c.activeBortOverridePatches(ctx, composeID)
	if err != nil {
		return err
	}
	if len(patches) == 0 {
		return nil
	}
	details := make([]string, 0, len(patches))
	for _, patch := range patches {
		repo := strings.Trim(strings.TrimSpace(patch.Repository)+"@"+strings.TrimSpace(patch.Branch), "@")
		if repo == "" {
			repo = strings.TrimSpace(patch.SourceType)
		}
		details = append(details, fmt.Sprintf("%s on %s (%s, %s)", patch.PatchID, patch.ComposeName, patch.FilePath, repo))
	}
	sort.Strings(details)
	return fmt.Errorf("active Bort-owned Dokploy patch(es) override Git-backed compose app(s): %s; merge the patch contents into the repo or disable the patch before live apply so Git remains the source of truth", strings.Join(details, "; "))
}

func (c *Client) activeBortOverridePatches(ctx context.Context, composeID string) ([]activeBortOverridePatch, error) {
	runner := c.dockerRunner()
	pg, err := findDokployPostgresContainer(ctx, runner)
	if err != nil {
		return nil, fmt.Errorf("locate Dokploy postgres for active patch inspection: %w", err)
	}
	var out bytes.Buffer
	if err := runner.Run(ctx, strings.NewReader(activeBortOverridePatchesSQL(composeID)), &out,
		"exec", "-i", pg, "psql", "-U", "dokploy", "-d", "dokploy", "-v", "ON_ERROR_STOP=1", "-At"); err != nil {
		return nil, fmt.Errorf("inspect active Dokploy patches: %w", err)
	}
	return parseActiveBortOverridePatches(out.String())
}

func activeBortOverridePatchesSQL(composeID string) string {
	return fmt.Sprintf(`select json_build_object(
       'patchId', p."patchId",
       'filePath', p."filePath",
       'composeName', c.name,
       'sourceType', coalesce(c."sourceType"::text, ''),
       'repository', concat_ws('/', nullif(c.owner, ''), nullif(c.repository, '')),
       'branch', coalesce(c.branch, '')
       )::text
from patch p
join compose c on c."composeId" = p."composeId"
where p.enabled = true
  and p."patchId" like 'bort-%%'
  and c."composeId" = %s
  and coalesce(c."sourceType"::text, '') not in ('', 'raw')
order by c.name, p."patchId";
`, sqlStringLiteral(composeID))
}

func parseActiveBortOverridePatches(output string) ([]activeBortOverridePatch, error) {
	patches := []activeBortOverridePatch{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var patch activeBortOverridePatch
		if err := json.Unmarshal([]byte(line), &patch); err != nil {
			return nil, fmt.Errorf("parse active Dokploy patch row: %w", err)
		}
		if strings.TrimSpace(patch.PatchID) == "" || strings.TrimSpace(patch.FilePath) == "" || strings.TrimSpace(patch.ComposeName) == "" {
			return nil, fmt.Errorf("parse active Dokploy patch row: missing required identity")
		}
		patches = append(patches, patch)
	}
	return patches, nil
}

func (c *Client) Apply(ctx context.Context, plan Plan) error {
	if err := validatePlanReadyForLiveApply(plan); err != nil {
		return err
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	pausedApps := map[string]struct{}{}
	coolifyProxyStopped := false
	total := len(plan.Steps)
	resumeFrom := plan.ResumeFrom
	if resumeFrom < 0 {
		resumeFrom = 0
	}
	if resumeFrom > total {
		resumeFrom = total
	}
	if resumeFrom > 0 && resumeFrom < total {
		if err := c.primeResumeState(ctx, actx, plan.Steps[:resumeFrom], plan.Steps[resumeFrom], pausedApps, &coolifyProxyStopped); err != nil {
			return err
		}
	}
	for index := resumeFrom; index < len(plan.Steps); index++ {
		step := plan.Steps[index]
		started := StepProgress{Index: index, Total: total, Step: step, Status: StepStatusStarted}
		if plan.BeforeStep != nil {
			if err := (*plan.BeforeStep)(started); err != nil {
				c.bestEffortResume(ctx, actx, plan, total, pausedApps, coolifyProxyStopped, false)
				return fmt.Errorf("before dokploy step %s for %s (%s): %w", step.Kind, step.App, step.Ref, err)
			}
		}
		emitProgress(plan.OnProgress, started)
		if shouldSkipApplyStep(plan, step) {
			emitProgress(plan.OnProgress, StepProgress{Index: index, Total: total, Step: step, Status: StepStatusSkipped})
			continue
		}
		// mark eagerly: a partial pause that stops some containers and
		// then errors must still be cleaned up by bestEffortResume.
		// resume is stateless and only starts what is currently stopped,
		// so marking before the step is safe.
		if step.Kind == StepPauseSource {
			pausedApps[step.App] = struct{}{}
		}
		if step.Kind == StepStopCoolifyProxy {
			coolifyProxyStopped = true
		}
		err := c.applyStep(ctx, actx, step)
		if err != nil {
			emitProgress(plan.OnProgress, StepProgress{Index: index, Total: total, Step: step, Status: StepStatusError, Err: err})
			c.bestEffortResume(ctx, actx, plan, total, pausedApps, coolifyProxyStopped, isUnsafeTargetResumeError(err))
			return fmt.Errorf("dokploy step %s for %s (%s): %w", step.Kind, step.App, step.Ref, err)
		}
		emitProgress(plan.OnProgress, StepProgress{Index: index, Total: total, Step: step, Status: StepStatusOK})
	}
	return nil
}

func shouldSkipApplyStep(plan Plan, step Step) bool {
	if step.App == "" {
		return false
	}
	app, ok := findPrepareApp(plan.Prepare, step.App)
	if !ok {
		return false
	}
	return isPlatformAppRole(app.Role)
}

func isPlatformAppRole(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), "platform")
}

func (c *Client) primeResumeState(ctx context.Context, actx *applyContext, completed []Step, next Step, pausedApps map[string]struct{}, coolifyProxyStopped *bool) error {
	if err := actx.loadMigratedVolumeMounts(); err != nil {
		return err
	}
	pushedApps := map[string]struct{}{}
	resumedTargetApps := map[string]struct{}{}
	for _, step := range completed {
		if shouldSkipApplyStep(actx.plan, step) {
			continue
		}
		replayNextApp := next.App != "" && step.App == next.App
		switch step.Kind {
		case StepCreateProject:
			if err := c.applyCreateProject(ctx, actx, step); err != nil {
				return fmt.Errorf("refresh completed project for resume: %w", err)
			}
		case StepCreateService:
			if err := c.applyCreateService(ctx, actx, step); err != nil {
				return fmt.Errorf("refresh completed service for resume: %w", err)
			}
		case StepUploadEnv:
			if !replayNextApp {
				continue
			}
			if err := c.applyUploadEnv(ctx, actx, step); err != nil {
				return fmt.Errorf("refresh completed env for resume: %w", err)
			}
		case StepPushImage:
			pushedApps[step.App] = struct{}{}
			if !replayNextApp {
				continue
			}
			if err := c.applyPushImage(ctx, actx, step); err != nil {
				return fmt.Errorf("refresh completed deploy for resume: %w", err)
			}
		case StepPauseSource:
			pausedApps[step.App] = struct{}{}
		case StepResumeSource:
			delete(pausedApps, step.App)
		case StepResumeTarget:
			resumedTargetApps[step.App] = struct{}{}
			actx.entry(step.App).TargetWritersStopped = nil
		case StepStopCoolifyProxy:
			*coolifyProxyStopped = true
		case StepStartDokployProxy:
			*coolifyProxyStopped = false
		}
	}
	for app := range pushedApps {
		if _, resumed := resumedTargetApps[app]; resumed {
			continue
		}
		if err := c.primeTargetWritersForResume(ctx, c.dockerRunner(), actx, app); err != nil {
			return fmt.Errorf("refresh target writer pause for resume app %s: %w", app, err)
		}
	}
	return nil
}

// bestEffortResume restarts source containers for every app that the
// failed run had paused. errors are swallowed so they don't mask the
// original failure; operators can still re-run apply or rollback. it
// uses a fresh context so a Ctrl-C that cancelled apply can still
// clean up — without that, source containers would stay stopped.
func (c *Client) bestEffortResume(_ context.Context, actx *applyContext, plan Plan, total int, pausedApps map[string]struct{}, coolifyProxyStopped bool, skipTargetWriters bool) {
	if actx == nil {
		actx = &applyContext{}
	}
	actx.plan = plan
	if len(pausedApps) == 0 && !coolifyProxyStopped && (skipTargetWriters || !actx.hasStoppedTargetWriters()) {
		return
	}
	if !skipTargetWriters {
		c.bestEffortResumeTargetWriters(actx, plan, total)
	}
	for app := range pausedApps {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerStartTimeout)
		resumeStep := Step{Kind: StepResumeSource, App: app, Ref: app}
		emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeStep, Status: StepStatusStarted})
		if err := c.applyResumeSource(cleanupCtx, actx, resumeStep); err != nil {
			cancel()
			emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeStep, Status: StepStatusError, Err: err})
			continue
		}
		cancel()
		emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeStep, Status: StepStatusOK})
	}
	// proxy resume runs last so coolify-proxy comes back online only
	// after source apps are restartable. it gets its own fresh
	// context so a slow source-resume loop can't starve the proxy
	// restart of its budget.
	if coolifyProxyStopped {
		proxyCtx, cancelProxy := context.WithTimeout(context.Background(), dockerStartTimeout)
		defer cancelProxy()
		resumeProxyStep := Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer}
		emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeProxyStep, Status: StepStatusStarted})
		if err := startProxyContainer(proxyCtx, c.dockerRunner(), coolifyProxyContainer); err != nil {
			emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeProxyStep, Status: StepStatusError, Err: err})
			return
		}
		emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeProxyStep, Status: StepStatusOK})
	}
}

func (a *applyContext) hasStoppedTargetWriters() bool {
	if a == nil {
		return false
	}
	for _, entry := range a.cache {
		if len(entry.TargetWritersStopped) > 0 {
			return true
		}
	}
	return false
}

func (c *Client) bestEffortResumeTargetWriters(actx *applyContext, plan Plan, total int) {
	if actx == nil {
		return
	}
	for app, entry := range actx.cache {
		if len(entry.TargetWritersStopped) == 0 {
			continue
		}
		resumeStep := Step{Kind: StepResumeTarget, App: app, Ref: app}
		emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeStep, Status: StepStatusStarted})
		if err := c.applyResumeTarget(context.Background(), actx, resumeStep); err != nil {
			emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeStep, Status: StepStatusError, Err: err})
			continue
		}
		emitProgress(plan.OnProgress, StepProgress{Index: total, Total: total, Step: resumeStep, Status: StepStatusOK})
	}
}

func emitProgress(fn *func(StepProgress), progress StepProgress) {
	if fn == nil || *fn == nil {
		return
	}
	(*fn)(progress)
}

func isUnsafeTargetResumeError(err error) bool {
	var unsafe unsafeTargetResumeError
	return errors.As(err, &unsafe)
}

func (c *Client) applyStep(ctx context.Context, actx *applyContext, step Step) error {
	switch step.Kind {
	case StepCreateProject:
		return c.applyCreateProject(ctx, actx, step)
	case StepCreateService:
		return c.applyCreateService(ctx, actx, step)
	case StepUploadEnv:
		return c.applyUploadEnv(ctx, actx, step)
	case StepPushImage:
		return c.applyPushImage(ctx, actx, step)
	case StepCreateVolume:
		return nil
	case StepInstallGateway:
		return c.applyInstallGateway(ctx, actx, step)
	case StepActivateRoutes:
		return c.applyActivateRoutes(ctx, actx, step)
	case StepPauseSource:
		return c.applyPauseSource(ctx, actx, step)
	case StepResumeSource:
		return c.applyResumeSource(ctx, actx, step)
	case StepResumeTarget:
		return c.applyResumeTarget(ctx, actx, step)
	case StepSyncVolume:
		return c.applySyncVolume(ctx, actx, step)
	case StepDumpDataStore:
		return c.applyDumpDataStore(ctx, actx, step)
	case StepRestoreDataStore:
		return c.applyRestoreDataStore(ctx, actx, step)
	case StepStopCoolifyProxy:
		return c.applyStopCoolifyProxy(ctx, actx, step)
	case StepStartDokployProxy:
		return c.applyStartDokployProxy(ctx, actx, step)
	case StepStopSourceApp:
		return c.applyStopSourceApp(ctx, actx, step)
	default:
		return fmt.Errorf("unknown step kind %q", step.Kind)
	}
}

func (c *Client) applyCreateProject(ctx context.Context, actx *applyContext, step Step) error {
	projectName, environmentName := dokployProjectSelection(actx.plan, step.App, step.Ref)
	project, err := c.CreateProject(ctx, projectName, "managed by bort migration")
	if err != nil {
		return err
	}
	if len(project.Environments) == 0 {
		fresh, err := c.GetProject(ctx, project.ProjectID)
		if err != nil {
			return fmt.Errorf("get project to populate envs: %w", err)
		}
		project = fresh
	}
	env := FindEnvironmentInProject(project, environmentName)
	if env == nil {
		return fmt.Errorf("dokploy project %s has no environments", project.Name)
	}
	entry := actx.entry(step.App)
	entry.ProjectID = project.ProjectID
	entry.EnvironmentID = env.EnvironmentID
	return nil
}

func dokployProjectSelection(plan Plan, appName, fallbackName string) (string, string) {
	name := strings.TrimSpace(fallbackName)
	environment := "production"
	if app, ok := findPrepareApp(plan.Prepare, appName); ok {
		if app.ProjectGroup != nil {
			name = planutilFallback(app.ProjectGroup.Name, name)
			environment = planutilFallback(app.ProjectGroup.Environment, environment)
		}
		if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
			project := app.TargetResources.Dokploy.Project
			name = planutilFallback(project.Name, name)
			environment = planutilFallback(project.Environment, environment)
		}
		name = planutilFallback(name, app.Name)
	}
	return planutilFallback(name, "app"), environment
}

func (c *Client) applyCreateService(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.EnvironmentID == "" {
		return fmt.Errorf("missing environment for app %s; create_project must run first", step.App)
	}
	composeFile, err := c.composeFileForApply(ctx, actx, step.App)
	if err != nil {
		return err
	}
	compose, err := c.CreateCompose(ctx, CreateComposeRequest{
		Name:          step.Ref,
		EnvironmentID: entry.EnvironmentID,
		ComposeFile:   composeFile,
	})
	if err != nil {
		return err
	}
	entry.ComposeID = compose.ComposeID
	entry.ComposeAppName = compose.AppName
	return nil
}

func (c *Client) applyUploadEnv(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.ComposeID == "" {
		return fmt.Errorf("missing composeId for app %s; create_service must run first", step.App)
	}
	composeFile, err := c.composeFileForApply(ctx, actx, step.App)
	if err != nil {
		return err
	}
	envContent, err := readEnvContent(actx.plan, step.App)
	if err != nil {
		return err
	}
	if err := c.validateNoActiveBortOverrides(ctx, entry.ComposeID); err != nil {
		return err
	}
	return c.UpdateCompose(ctx, entry.ComposeID, composeFile, envContent)
}

func (c *Client) applyPushImage(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.ComposeID == "" {
		return fmt.Errorf("missing composeId for app %s; create_service must run first", step.App)
	}
	composeFile, err := c.composeFileForApply(ctx, actx, step.App)
	if err != nil {
		return err
	}
	if err := ensureComposeImagesAvailable(ctx, c.dockerRunner(), actx.plan, step.App, composeFile); err != nil {
		return err
	}
	if err := c.validateNoActiveBortOverrides(ctx, entry.ComposeID); err != nil {
		return err
	}
	if err := c.DeployCompose(ctx, entry.ComposeID, deployComposeTitle(actx.plan)); err != nil {
		if safetyErr := c.validateMigratedVolumeMountsAfterDeploy(ctx, actx, step.App); safetyErr != nil && isUnsafeTargetResumeError(safetyErr) {
			return fmt.Errorf("deploy dokploy compose: %w; post-deploy safety check: %v", err, safetyErr)
		}
		return err
	}
	if err := c.validateMigratedVolumeMountsAfterDeploy(ctx, actx, step.App); err != nil {
		return err
	}
	return c.pauseTargetWritersForState(ctx, c.dockerRunner(), actx, step.App)
}

func deployComposeTitle(plan Plan) string {
	title := "bort-migrate"
	if plan.RunName != "" {
		title = "bort-migrate-" + plan.RunName
	}
	return title
}

func (c *Client) composeFileForApply(ctx context.Context, actx *applyContext, appName string) (string, error) {
	composeFile, err := readComposeFile(actx.plan, appName)
	if err != nil {
		return "", err
	}
	composeFile, err = c.replaceBuildOnlyServicesWithSourceImages(ctx, actx.plan, appName, composeFile)
	if err != nil {
		return "", err
	}
	return composeFile, nil
}

func (c *Client) replaceBuildOnlyServicesWithSourceImages(ctx context.Context, plan Plan, appName, composeFile string) (string, error) {
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok {
		return "", fmt.Errorf("app %s not found in prepare result", appName)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(composeFile), &doc); err != nil {
		return "", err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return composeFile, nil
	}
	changed := false
	for i := 0; i+1 < len(services.Content); i += 2 {
		serviceName := strings.TrimSpace(services.Content[i].Value)
		service := services.Content[i+1]
		if serviceName == "" || service.Kind != yaml.MappingNode || mappingValue(service, "build") == nil || mappingValue(service, "image") != nil {
			continue
		}
		ref, ok := findSourceService(app, serviceName)
		if !ok {
			return "", fmt.Errorf("service %s for app %s uses build but has no source container image to reuse", serviceName, appName)
		}
		container, err := sourceContainer(ctx, c.dockerRunner(), ref.ContainerID, ref.ContainerName)
		if err != nil {
			return "", fmt.Errorf("inspect source service %s for app %s: %w", serviceName, appName, err)
		}
		sourceImage := strings.TrimSpace(container.Config.Image)
		if sourceImage == "" || strings.HasPrefix(sourceImage, "sha256:") {
			return "", fmt.Errorf("source service %s for app %s has no reusable image tag", serviceName, appName)
		}
		setMappingScalar(service, "image", sourceImage)
		removeMappingKey(service, "build")
		changed = true
	}
	if !changed {
		return composeFile, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ensureComposeImagesAvailable(ctx context.Context, runner dockerRunner, plan Plan, appName, composeFile string) error {
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", appName)
	}
	images, err := composeServiceImages(composeFile)
	if err != nil {
		return err
	}
	for service, image := range images {
		if image == "" || strings.Contains(image, "${") {
			continue
		}
		if _, err := runner.Output(ctx, "image", "inspect", image); err == nil {
			continue
		}
		ref, ok := findSourceService(app, service)
		if !ok {
			continue
		}
		container, err := sourceContainer(ctx, runner, ref.ContainerID, ref.ContainerName)
		if err != nil {
			return fmt.Errorf("inspect source service %s for app %s: %w", service, appName, err)
		}
		sourceImage := container.Image
		if sourceImage == "" {
			sourceImage = container.Config.Image
		}
		if sourceImage == "" {
			return fmt.Errorf("source service %s for app %s has no image to reuse", service, appName)
		}
		if _, err := runner.Output(ctx, "tag", sourceImage, image); err != nil {
			return fmt.Errorf("reuse source image for service %s as %s: %w", service, image, err)
		}
	}
	return nil
}

func findSourceService(app preparer.AppPlan, serviceName string) (preparer.SourceServiceRef, bool) {
	for _, ref := range app.Resources.SourceServices {
		if ref.ServiceName == serviceName {
			return ref, true
		}
	}
	return preparer.SourceServiceRef{}, false
}

func composeServiceImages(contents string) (map[string]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &doc); err != nil {
		return nil, err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, nil
	}
	images := map[string]string{}
	for i := 0; i+1 < len(services.Content); i += 2 {
		serviceName := services.Content[i].Value
		service := services.Content[i+1]
		if service.Kind != yaml.MappingNode {
			continue
		}
		image := mappingValue(service, "image")
		if image == nil || image.Kind != yaml.ScalarNode {
			continue
		}
		images[serviceName] = strings.TrimSpace(image.Value)
	}
	return images, nil
}

func (c *Client) applyInstallGateway(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.ComposeID == "" {
		return fmt.Errorf("missing composeId for app %s; create_service must run first", step.App)
	}
	route, ok := findCutoverRoute(actx.plan.Cutover, step.App, step.Ref)
	if !ok {
		return fmt.Errorf("route %s for app %s not found in cutover artifact", step.Ref, step.App)
	}
	composeFile, err := c.composeFileForApply(ctx, actx, step.App)
	if err != nil {
		return err
	}
	route, err = resolveRouteForCompose(route, composeFile)
	if err != nil {
		return err
	}
	return c.ensureRouteDomain(ctx, entry.ComposeID, route)
}

func (c *Client) applyActivateRoutes(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.ComposeID == "" {
		return fmt.Errorf("missing composeId for app %s; create_service must run first", step.App)
	}
	composeFile, err := c.composeFileForApply(ctx, actx, step.App)
	if err != nil {
		return err
	}
	if err := ensureComposeImagesAvailable(ctx, c.dockerRunner(), actx.plan, step.App, composeFile); err != nil {
		return err
	}
	envContent, err := readEnvContent(actx.plan, step.App)
	if err != nil {
		return err
	}
	if err := c.validateNoActiveBortOverrides(ctx, entry.ComposeID); err != nil {
		return err
	}
	if err := c.UpdateCompose(ctx, entry.ComposeID, composeFile, envContent); err != nil {
		return err
	}
	for _, route := range cutoverRoutesForApp(actx.plan.Cutover, step.App) {
		resolved, err := resolveRouteForCompose(route, composeFile)
		if err != nil {
			return err
		}
		if err := c.ensureRouteDomain(ctx, entry.ComposeID, resolved); err != nil {
			return err
		}
	}
	if err := c.DeployCompose(ctx, entry.ComposeID, deployComposeTitle(actx.plan)); err != nil {
		if safetyErr := c.validateMigratedVolumeMountsAfterDeploy(ctx, actx, step.App); safetyErr != nil && isUnsafeTargetResumeError(safetyErr) {
			return fmt.Errorf("deploy dokploy compose: %w; post-deploy safety check: %v", err, safetyErr)
		}
		return err
	}
	return c.validateMigratedVolumeMountsAfterDeploy(ctx, actx, step.App)
}

func (c *Client) ensureRouteDomain(ctx context.Context, composeID string, route gateway.Route) error {
	port := parsePort(route.Port)
	_, err := c.CreateDomain(ctx, CreateDomainRequest{
		Host:            route.Host,
		ComposeID:       composeID,
		ServiceName:     route.ServiceName,
		Port:            port,
		HTTPS:           true,
		CertificateType: "letsencrypt",
	})
	return err
}

func cutoverRoutesForApp(cutover gateway.Result, app string) []gateway.Route {
	for _, a := range cutover.Apps {
		if a.Name == app {
			return append([]gateway.Route{}, a.Routes...)
		}
	}
	return nil
}

func findCutoverRoute(cutover gateway.Result, app, host string) (gateway.Route, bool) {
	for _, a := range cutover.Apps {
		if a.Name != app {
			continue
		}
		for _, route := range a.Routes {
			if strings.EqualFold(route.Host, host) {
				return route, true
			}
		}
	}
	return gateway.Route{}, false
}

func parsePort(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	port := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	return port
}

type composeServiceSummary struct {
	Name  string
	Ports map[string]struct{}
}

func resolveRouteForCompose(route gateway.Route, composeFile string) (gateway.Route, error) {
	services, err := composeServiceSummaries(composeFile)
	if err != nil {
		return gateway.Route{}, err
	}
	if len(services) == 0 {
		return route, nil
	}
	if _, ok := services[route.ServiceName]; ok {
		return route, nil
	}
	if serviceName, ok := inferComposeServiceForRoute(route, services); ok {
		route.ServiceName = serviceName
		return route, nil
	}
	available := make([]string, 0, len(services))
	for name := range services {
		available = append(available, name)
	}
	sort.Strings(available)
	return gateway.Route{}, fmt.Errorf("route %s points at service %q, but the dokploy compose has %s; rescan before retrying so bort can refresh the route mapping",
		planutilFallback(route.Host, "unknown"), route.ServiceName, strings.Join(available, ", "))
}

func inferComposeServiceForRoute(route gateway.Route, services map[string]composeServiceSummary) (string, bool) {
	for _, candidate := range routeServiceNameCandidates(route) {
		if _, ok := services[candidate]; ok {
			return candidate, true
		}
	}
	if route.Port != "" {
		matches := []string{}
		port := normalizeComposePort(route.Port)
		for name, service := range services {
			if _, ok := service.Ports[port]; ok {
				matches = append(matches, name)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
	}
	if strings.TrimSpace(route.ServiceName) == "" && len(services) == 1 {
		for name := range services {
			return name, true
		}
	}
	return "", false
}

func routeServiceNameCandidates(route gateway.Route) []string {
	candidates := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}
	add(route.ServiceName)
	add(stripCoolifyGeneratedServiceSuffix(route.ServiceName))
	if fromSource := serviceNameFromTraefikRouterSource(route.Source); fromSource != "" {
		add(fromSource)
		add(stripCoolifyGeneratedServiceSuffix(fromSource))
	}
	return candidates
}

func stripCoolifyGeneratedServiceSuffix(name string) string {
	parts := strings.Split(strings.TrimSpace(name), "-")
	if len(parts) >= 2 {
		last := parts[len(parts)-1]
		if len(last) >= 8 && !allDigits(last) && allLowerAlphaNum(last) {
			base := strings.Join(parts[:len(parts)-1], "-")
			if strings.TrimSpace(base) != "" {
				return base
			}
		}
	}
	if len(parts) < 3 {
		return ""
	}
	last := parts[len(parts)-1]
	middle := parts[len(parts)-2]
	if !allDigits(last) || len(middle) < 8 || !allLowerAlphaNum(middle) {
		return ""
	}
	base := strings.Join(parts[:len(parts)-2], "-")
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return base
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func allLowerAlphaNum(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func serviceNameFromTraefikRouterSource(source string) string {
	const prefix = "traefik.http.routers."
	const suffix = ".rule"
	if !strings.HasPrefix(source, prefix) || !strings.HasSuffix(source, suffix) {
		return ""
	}
	router := strings.TrimSuffix(strings.TrimPrefix(source, prefix), suffix)
	parts := strings.Split(router, "-")
	if len(parts) < 4 {
		return ""
	}
	return strings.Join(parts[3:], "-")
}

func composeServiceSummaries(contents string) (map[string]composeServiceSummary, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &doc); err != nil {
		return nil, err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, nil
	}
	summaries := map[string]composeServiceSummary{}
	for i := 0; i+1 < len(services.Content); i += 2 {
		name := strings.TrimSpace(services.Content[i].Value)
		if name == "" {
			continue
		}
		service := services.Content[i+1]
		summary := composeServiceSummary{Name: name, Ports: map[string]struct{}{}}
		if service.Kind == yaml.MappingNode {
			for _, port := range composeServicePorts(service) {
				summary.Ports[port] = struct{}{}
			}
		}
		summaries[name] = summary
	}
	return summaries, nil
}

func composeServicePorts(service *yaml.Node) []string {
	ports := []string{}
	for _, key := range []string{"expose", "ports"} {
		node := mappingValue(service, key)
		for _, value := range composePortValues(node) {
			if port := normalizeComposePort(value); port != "" {
				ports = append(ports, port)
			}
		}
	}
	return ports
}

func composePortValues(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return []string{node.Value}
	case yaml.SequenceNode:
		values := []string{}
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode {
				values = append(values, item.Value)
			}
		}
		return values
	default:
		return nil
	}
}

func normalizeComposePort(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if before, _, ok := strings.Cut(value, "/"); ok {
		value = before
	}
	if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		value = parts[len(parts)-1]
	}
	value = strings.TrimSpace(value)
	for _, r := range value {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return value
}

func planutilFallback(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func readComposeFile(plan Plan, appName string) (string, error) {
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok || app.TargetResources == nil || app.TargetResources.Dokploy == nil {
		return "", fmt.Errorf("app %s has no dokploy target resources", appName)
	}
	composePath := app.TargetResources.Dokploy.ComposeApp.ComposePath
	if composePath == "" {
		composePath = "compose.yaml"
	}
	bundleDir := plan.Prepare.BundleDir
	appDir := filepath.Join(bundleDir, filepath.FromSlash(app.Directory))
	if err := safepath.ContainedPath(bundleDir, appDir); err != nil {
		return "", err
	}
	full := filepath.Join(appDir, filepath.FromSlash(composePath))
	if err := safepath.ContainedPath(appDir, full); err != nil {
		return "", err
	}
	contents, err := safepath.ReadFileNoFollow(full)
	if err != nil {
		return "", fmt.Errorf("read compose file %s: %w", full, err)
	}
	compose, err := inlineComposeEnvFilesWithFallbacks(string(contents), appDir, composeEnvFileFallbackPaths(app.TargetResources.Dokploy.EnvFiles))
	if err != nil {
		return "", fmt.Errorf("prepare compose file %s for dokploy: %w", full, err)
	}
	if shouldAttachDokployNetworkAliases(app) {
		compose, err = attachDokployNetworkAliases(compose)
		if err != nil {
			return "", fmt.Errorf("prepare dokploy network aliases for %s: %w", full, err)
		}
	}
	return compose, nil
}

func shouldAttachDokployNetworkAliases(app preparer.AppPlan) bool {
	return strings.EqualFold(strings.TrimSpace(app.Role), "support")
}

func attachDokployNetworkAliases(contents string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &doc); err != nil {
		return "", err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return contents, nil
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return contents, nil
	}
	changed := ensureTopLevelDokployNetwork(root)
	for i := 0; i+1 < len(services.Content); i += 2 {
		serviceName := strings.TrimSpace(services.Content[i].Value)
		service := services.Content[i+1]
		if serviceName == "" || service.Kind != yaml.MappingNode {
			continue
		}
		if ensureServiceDokployNetworkAlias(service, serviceName) {
			changed = true
		}
	}
	if !changed {
		return contents, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func inlineComposeEnvFiles(contents, appDir string) (string, error) {
	return inlineComposeEnvFilesWithFallbacks(contents, appDir, nil)
}

func inlineComposeEnvFilesWithFallbacks(contents, appDir string, fallbackPaths []string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &doc); err != nil {
		return "", err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return contents, nil
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return contents, nil
	}
	serviceNames := make([]string, 0, len(services.Content)/2)
	for i := 0; i+1 < len(services.Content); i += 2 {
		serviceNames = append(serviceNames, services.Content[i].Value)
	}
	fallbacksByService := composeEnvFileFallbacksByService(fallbackPaths, serviceNames)
	sharedEnvFilePath := composeSharedEnvFileFallbackPath(fallbackPaths)
	changed := false
	for i := 1; i < len(services.Content); i += 2 {
		serviceName := services.Content[i-1].Value
		service := services.Content[i]
		if service.Kind != yaml.MappingNode {
			continue
		}
		values := map[string]string{}
		envFileNode := mappingValue(service, "env_file")
		if envFileNode != nil {
			envFilePaths := composeEnvFilePaths(envFileNode)
			preserveSharedEnvFile := hasDefaultComposeEnvFilePath(envFilePaths) && hasDefaultComposeEnvFilePath(fallbackPaths)
			inlinePaths := []string{}
			preservedPaths := []string{}
			for _, path := range envFilePaths {
				if preserveSharedEnvFile && isDefaultComposeEnvFilePath(path) {
					preservedPaths = append(preservedPaths, path)
					continue
				}
				inlinePaths = append(inlinePaths, path)
			}
			envValues, err := readComposeEnvFilePathListValues(appDir, inlinePaths)
			if err != nil {
				return "", err
			}
			for key, value := range envValues {
				values[key] = value
			}
			if len(preservedPaths) > 0 {
				if setComposeEnvFilePaths(envFileNode, preservedPaths) || len(preservedPaths) != len(envFilePaths) {
					changed = true
				}
			} else if removeMappingKey(service, "env_file") {
				changed = true
			}
		}
		if envFileNode == nil && sharedEnvFilePath != "" && !isComposeInfrastructureService(serviceName, service) {
			setMappingScalar(service, "env_file", sharedEnvFilePath)
			changed = true
		}
		for _, path := range fallbacksByService[serviceName] {
			envValues, err := readComposeEnvFilePathValues(appDir, path)
			if err != nil {
				return "", err
			}
			for key, value := range envValues {
				if _, exists := values[key]; !exists {
					values[key] = value
				}
			}
		}
		if len(values) > 0 {
			inlineServiceEnvironment(service, values)
			changed = true
		}
		if sanitizeComposeServiceForDokploy(service) {
			changed = true
		}
	}
	if sanitizeComposeTopLevelResources(root) {
		changed = true
	}
	if !changed {
		return contents, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func composeEnvFileFallbackPaths(envFiles []preparer.DokployEnvFile) []string {
	paths := []string{}
	for _, envFile := range envFiles {
		path := strings.TrimSpace(envFile.Path)
		if path == "" || strings.HasSuffix(path, ".example") {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

func composeSharedEnvFileFallbackPath(paths []string) string {
	for _, path := range paths {
		if isDefaultComposeEnvFilePath(path) {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

func composeEnvFileFallbacksByService(paths, serviceNames []string) map[string][]string {
	out := map[string][]string{}
	sortedServices := append([]string{}, serviceNames...)
	sort.SliceStable(sortedServices, func(i, j int) bool {
		return len(sortedServices[i]) > len(sortedServices[j])
	})
	for _, path := range paths {
		stem := composeEnvFileServiceStem(path)
		if stem == "" {
			continue
		}
		for _, service := range sortedServices {
			if stem == service || strings.HasPrefix(stem, service+"-") || strings.HasPrefix(stem, service+"_") {
				out[service] = append(out[service], path)
				break
			}
		}
	}
	return out
}

func composeEnvFileServiceStem(path string) string {
	base := filepath.Base(filepath.ToSlash(strings.TrimSpace(path)))
	if !strings.HasPrefix(base, ".env.") {
		return ""
	}
	return strings.TrimPrefix(base, ".env.")
}

func sanitizeComposeServiceForDokploy(service *yaml.Node) bool {
	changed := false
	if mappingValue(service, "image") != nil && removeMappingKey(service, "build") {
		changed = true
	}
	if sanitizeComposeServiceLabels(service) {
		changed = true
	}
	if sanitizeComposeServiceEnvironment(service) {
		changed = true
	}
	for _, key := range []string{"container_name", "ports"} {
		if removeMappingKey(service, key) {
			changed = true
		}
	}
	return changed
}

func isComposeInfrastructureService(serviceName string, service *yaml.Node) bool {
	name := strings.ToLower(strings.TrimSpace(serviceName))
	image := ""
	if imageNode := mappingValue(service, "image"); imageNode != nil && imageNode.Kind == yaml.ScalarNode {
		image = strings.ToLower(strings.TrimSpace(imageNode.Value))
	}
	for _, token := range []string{"redis", "postgres", "postgresql", "pgvector", "mysql", "mariadb", "mongo", "dragonfly", "clickhouse"} {
		if name == token || strings.HasPrefix(name, token+"-") || strings.HasSuffix(name, "-"+token) || strings.Contains(image, token) {
			return true
		}
	}
	return false
}

func sanitizeComposeServiceLabels(service *yaml.Node) bool {
	labels := mappingValue(service, "labels")
	if labels == nil {
		return false
	}
	changed := false
	switch labels.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(labels.Content); {
			key := labels.Content[i].Value
			if isSourcePlatformLabel(key) {
				labels.Content = append(labels.Content[:i], labels.Content[i+2:]...)
				changed = true
				continue
			}
			i += 2
		}
	case yaml.SequenceNode:
		kept := labels.Content[:0]
		for _, item := range labels.Content {
			if item.Kind == yaml.ScalarNode && isSourcePlatformLabel(labelKey(item.Value)) {
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		labels.Content = kept
	}
	if changed && len(labels.Content) == 0 {
		removeMappingKey(service, "labels")
	}
	return changed
}

func sanitizeComposeServiceEnvironment(service *yaml.Node) bool {
	environment := mappingValue(service, "environment")
	if environment == nil {
		return false
	}
	changed := false
	switch environment.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(environment.Content); {
			key := environment.Content[i].Value
			if isSourcePlatformEnvName(key) {
				environment.Content = append(environment.Content[:i], environment.Content[i+2:]...)
				changed = true
				continue
			}
			i += 2
		}
	case yaml.SequenceNode:
		kept := environment.Content[:0]
		for _, item := range environment.Content {
			if item.Kind == yaml.ScalarNode && isSourcePlatformEnvName(envAssignmentKey(item.Value)) {
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		environment.Content = kept
	}
	if changed && len(environment.Content) == 0 {
		removeMappingKey(service, "environment")
	}
	return changed
}

func labelKey(value string) string {
	key, _, _ := strings.Cut(strings.TrimSpace(value), "=")
	return strings.TrimSpace(key)
}

func envAssignmentKey(value string) string {
	key, _, _ := strings.Cut(strings.TrimSpace(value), "=")
	return strings.TrimSpace(key)
}

func isSourcePlatformLabel(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(key, "coolify.") ||
		key == "traefik.enable" ||
		strings.HasPrefix(key, "traefik.") ||
		strings.HasPrefix(key, "caddy")
}

func isSourcePlatformEnvName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(name, "COOLIFY_") || name == "SOURCE_COMMIT"
}

func sanitizeComposeTopLevelResources(root *yaml.Node) bool {
	changed := false
	volumes := mappingValue(root, "volumes")
	if volumes != nil && volumes.Kind == yaml.MappingNode {
		for i := 1; i < len(volumes.Content); i += 2 {
			volume := volumes.Content[i]
			if volume.Kind != yaml.MappingNode {
				continue
			}
			if removeMappingKey(volume, "name") {
				changed = true
			}
			if removeMappingKey(volume, "external") {
				changed = true
			}
		}
	}
	networks := mappingValue(root, "networks")
	if networks != nil && networks.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(networks.Content); i += 2 {
			if networks.Content[i].Value == "dokploy-network" {
				continue
			}
			network := networks.Content[i+1]
			if network.Kind != yaml.MappingNode {
				continue
			}
			if removeMappingKey(network, "name") {
				changed = true
			}
			if removeMappingKey(network, "external") {
				changed = true
			}
		}
	}
	return changed
}

func ensureTopLevelDokployNetwork(root *yaml.Node) bool {
	networks := mappingValue(root, "networks")
	changed := false
	if networks == nil || networks.Kind != yaml.MappingNode {
		networks = &yaml.Node{Kind: yaml.MappingNode}
		setMappingNode(root, "networks", networks)
		changed = true
	}
	network := mappingValue(networks, "dokploy-network")
	if network == nil || network.Kind != yaml.MappingNode {
		network = &yaml.Node{Kind: yaml.MappingNode}
		setMappingNode(networks, "dokploy-network", network)
		changed = true
	}
	if ensureMappingBool(network, "external", true) {
		changed = true
	}
	return changed
}

func ensureServiceDokployNetworkAlias(service *yaml.Node, serviceName string) bool {
	networks := mappingValue(service, "networks")
	changed := false
	if networks == nil {
		networks = &yaml.Node{Kind: yaml.MappingNode}
		setMappingNode(service, "networks", networks)
		setMappingNode(networks, "default", &yaml.Node{Kind: yaml.MappingNode})
		changed = true
	} else if networks.Kind == yaml.SequenceNode {
		networks = composeNetworkSequenceToMapping(networks)
		setMappingNode(service, "networks", networks)
		changed = true
	} else if networks.Kind != yaml.MappingNode {
		networks = &yaml.Node{Kind: yaml.MappingNode}
		setMappingNode(service, "networks", networks)
		changed = true
	}
	network := mappingValue(networks, "dokploy-network")
	if network == nil || network.Kind != yaml.MappingNode {
		network = &yaml.Node{Kind: yaml.MappingNode}
		setMappingNode(networks, "dokploy-network", network)
		changed = true
	}
	if ensureServiceNetworkAlias(network, serviceName) {
		changed = true
	}
	return changed
}

func composeNetworkSequenceToMapping(sequence *yaml.Node) *yaml.Node {
	mapping := &yaml.Node{Kind: yaml.MappingNode}
	for _, item := range sequence.Content {
		if item.Kind != yaml.ScalarNode || strings.TrimSpace(item.Value) == "" {
			continue
		}
		setMappingNode(mapping, strings.TrimSpace(item.Value), &yaml.Node{Kind: yaml.MappingNode})
	}
	return mapping
}

func ensureServiceNetworkAlias(network *yaml.Node, alias string) bool {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return false
	}
	aliases := mappingValue(network, "aliases")
	if aliases == nil {
		setMappingNode(network, "aliases", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{stringNode(alias)}})
		return true
	}
	if aliases.Kind != yaml.SequenceNode {
		preserved := strings.TrimSpace(aliases.Value)
		items := []*yaml.Node{}
		if preserved != "" {
			items = append(items, stringNode(preserved))
		}
		if preserved != alias {
			items = append(items, stringNode(alias))
		}
		setMappingNode(network, "aliases", &yaml.Node{Kind: yaml.SequenceNode, Content: items})
		return true
	}
	for _, item := range aliases.Content {
		if item.Kind == yaml.ScalarNode && strings.TrimSpace(item.Value) == alias {
			return false
		}
	}
	aliases.Content = append(aliases.Content, stringNode(alias))
	return true
}

func readComposeEnvFilePathListValues(appDir string, paths []string) (map[string]string, error) {
	values := map[string]string{}
	for _, envPath := range paths {
		envValues, err := readComposeEnvFilePathValues(appDir, envPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for key, value := range envValues {
			values[key] = value
		}
	}
	return values, nil
}

func hasDefaultComposeEnvFilePath(paths []string) bool {
	for _, path := range paths {
		if isDefaultComposeEnvFilePath(path) {
			return true
		}
	}
	return false
}

func isDefaultComposeEnvFilePath(path string) bool {
	return normalizeComposeEnvFilePath(path) == ".env"
}

func composeEnvFilePathInList(path string, paths []string) bool {
	normalized := normalizeComposeEnvFilePath(path)
	for _, candidate := range paths {
		if normalizeComposeEnvFilePath(candidate) == normalized {
			return true
		}
	}
	return false
}

func normalizeComposeEnvFilePath(path string) string {
	path = filepath.ToSlash(strings.TrimSpace(path))
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	return path
}

func readComposeEnvFilePathValues(appDir, envPath string) (map[string]string, error) {
	full := filepath.Join(appDir, filepath.FromSlash(envPath))
	if err := safepath.ContainedPath(appDir, full); err != nil {
		return nil, err
	}
	contents, err := safepath.ReadFileNoFollow(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("read compose env_file %s: %w", full, err)
	}
	return parseComposeEnvFile(string(contents)), nil
}

func composeEnvFilePaths(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if strings.TrimSpace(node.Value) == "" {
			return nil
		}
		return []string{node.Value}
	case yaml.SequenceNode:
		paths := []string{}
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode && strings.TrimSpace(item.Value) != "" {
				paths = append(paths, item.Value)
			}
		}
		return paths
	default:
		return nil
	}
}

func setComposeEnvFilePaths(node *yaml.Node, paths []string) bool {
	old := composeEnvFilePaths(node)
	changed := len(old) != len(paths)
	if !changed {
		for i := range old {
			if old[i] != paths[i] {
				changed = true
				break
			}
		}
	}
	if len(paths) == 1 {
		node.Kind = yaml.ScalarNode
		node.Tag = "!!str"
		node.Value = paths[0]
		node.Content = nil
		return changed
	}
	node.Kind = yaml.SequenceNode
	node.Tag = "!!seq"
	node.Value = ""
	node.Content = node.Content[:0]
	for _, path := range paths {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: path})
	}
	return changed
}

func parseComposeEnvFile(contents string) map[string]string {
	values := map[string]string{}
	for _, raw := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || isSourcePlatformEnvName(key) {
			continue
		}
		values[key] = normalizeEnvUploadValue(value)
	}
	return values
}

func inlineServiceEnvironment(service *yaml.Node, values map[string]string) {
	env := mappingValue(service, "environment")
	if env == nil {
		env = &yaml.Node{Kind: yaml.MappingNode}
		service.Content = append(service.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "environment"},
			env,
		)
	}
	switch env.Kind {
	case yaml.MappingNode:
		existing := mappingKeys(env)
		for key, value := range values {
			if isSourcePlatformEnvName(key) {
				continue
			}
			if existing[key] {
				continue
			}
			env.Content = append(env.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: key},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
			)
		}
	case yaml.SequenceNode:
		existing := sequenceEnvKeys(env)
		for key, value := range values {
			if isSourcePlatformEnvName(key) {
				continue
			}
			if existing[key] {
				continue
			}
			env.Content = append(env.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key + "=" + value})
		}
	}
}

func mappingKeys(node *yaml.Node) map[string]bool {
	keys := map[string]bool{}
	if node == nil || node.Kind != yaml.MappingNode {
		return keys
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys[node.Content[i].Value] = true
	}
	return keys
}

func sequenceEnvKeys(node *yaml.Node) map[string]bool {
	keys := map[string]bool{}
	if node == nil || node.Kind != yaml.SequenceNode {
		return keys
	}
	for _, item := range node.Content {
		key, _, ok := strings.Cut(item.Value, "=")
		if ok && key != "" {
			keys[key] = true
		}
	}
	return keys
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setMappingScalar(node *yaml.Node, key, value string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		node.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		return
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func setMappingNode(node *yaml.Node, key string, value *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		node.Content[i+1] = value
		return
	}
	node.Content = append(node.Content, stringNode(key), value)
}

func ensureMappingBool(node *yaml.Node, key string, value bool) bool {
	want := "false"
	if value {
		want = "true"
	}
	current := mappingValue(node, key)
	if current != nil && current.Kind == yaml.ScalarNode && current.Tag == "!!bool" && current.Value == want {
		return false
	}
	setMappingNode(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: want})
	return true
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func removeMappingKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		node.Content = append(node.Content[:i], node.Content[i+2:]...)
		return true
	}
	return false
}

func readEnvContent(plan Plan, appName string) (string, error) {
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok || app.TargetResources == nil || app.TargetResources.Dokploy == nil {
		return "", nil
	}
	bundleDir := plan.Prepare.BundleDir
	appDir := filepath.Join(bundleDir, filepath.FromSlash(app.Directory))
	if err := safepath.ContainedPath(bundleDir, appDir); err != nil {
		return "", err
	}
	merged := map[string]string{}
	for _, envFile := range app.TargetResources.Dokploy.EnvFiles {
		if !isSharedDokployEnvFile(envFile.Path) {
			continue
		}
		full := filepath.Join(appDir, filepath.FromSlash(envFile.Path))
		if err := safepath.ContainedPath(appDir, full); err != nil {
			return "", err
		}
		contents, err := safepath.ReadFileNoFollow(full)
		if err != nil {
			return "", fmt.Errorf("read env file %s: %w", full, err)
		}
		for _, raw := range strings.Split(string(contents), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" || isSourcePlatformEnvName(key) {
				continue
			}
			merged[key] = normalizeEnvUploadValue(value)
		}
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(formatEnvUploadValue(merged[key]))
		b.WriteString("\n")
	}
	return b.String(), nil
}

func isSharedDokployEnvFile(path string) bool {
	switch normalizeComposeEnvFilePath(path) {
	case ".env", ".env.example":
		return true
	default:
		return false
	}
}

func normalizeEnvUploadValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != value[len(value)-1] || (value[0] != '"' && value[0] != '\'') {
		return value
	}
	if value[0] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return value[1 : len(value)-1]
}

func formatEnvUploadValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, "\r\n") {
		value = strings.ReplaceAll(value, "\r\n", "\\n")
		value = strings.ReplaceAll(value, "\r", "\\n")
		value = strings.ReplaceAll(value, "\n", "\\n")
		return value
	}
	if strings.Contains(value, "\\n") {
		return value
	}
	if strings.ContainsAny(value, " \t#'") {
		return strconv.Quote(value)
	}
	return value
}

func findPrepareApp(prepare preparer.Result, name string) (preparer.AppPlan, bool) {
	for _, app := range prepare.Apps {
		if app.Name == name {
			return app, true
		}
	}
	return preparer.AppPlan{}, false
}
