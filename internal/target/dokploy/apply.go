package dokploy

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
	syncplan "github.com/aikins01/bort/internal/sync"
)

type StepKind string

const (
	StepCreateProject    StepKind = "create_project"
	StepCreateService    StepKind = "create_service"
	StepPushImage        StepKind = "push_image"
	StepUploadEnv        StepKind = "upload_env"
	StepCreateVolume     StepKind = "create_volume"
	StepSyncVolume       StepKind = "sync_volume"
	StepDumpDataStore    StepKind = "dump_data_store"
	StepRestoreDataStore StepKind = "restore_data_store"
	StepInstallGateway   StepKind = "install_gateway"
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
	OnProgress *func(StepProgress)
}

func PlanFromArtifacts(prepare preparer.Result, sync syncplan.Result, cutover gateway.Result) Plan {
	plan := Plan{Prepare: prepare, Sync: sync, Cutover: cutover}
	for _, app := range prepare.Apps {
		plan.Steps = append(plan.Steps, Step{Kind: StepCreateProject, App: app.Name, Ref: app.Name})
		if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
			composeRef := app.TargetResources.Dokploy.ComposeApp.Name
			plan.Steps = append(plan.Steps, Step{Kind: StepCreateService, App: app.Name, Ref: composeRef})
			if len(app.TargetResources.Dokploy.EnvFiles) > 0 {
				plan.Steps = append(plan.Steps, Step{Kind: StepUploadEnv, App: app.Name, Ref: composeRef})
			}
			plan.Steps = append(plan.Steps, Step{Kind: StepPushImage, App: app.Name, Ref: composeRef})
			for _, volume := range app.TargetResources.Dokploy.Volumes {
				ref := volume.Name
				if ref == "" {
					ref = volume.Target
				}
				plan.Steps = append(plan.Steps, Step{Kind: StepCreateVolume, App: app.Name, Ref: ref})
			}
		}
	}
	for _, app := range sync.Apps {
		for _, step := range app.Steps {
			switch step.ResourceType {
			case "volume":
				plan.Steps = append(plan.Steps, Step{Kind: StepSyncVolume, App: app.Name, Ref: step.ResourceRef})
			case "data_store":
				plan.Steps = append(plan.Steps, Step{Kind: StepDumpDataStore, App: app.Name, Ref: step.ResourceRef})
				plan.Steps = append(plan.Steps, Step{Kind: StepRestoreDataStore, App: app.Name, Ref: step.ResourceRef})
			}
		}
	}
	for _, app := range cutover.Apps {
		for _, route := range app.Routes {
			plan.Steps = append(plan.Steps, Step{Kind: StepInstallGateway, App: app.Name, Ref: route.Host})
		}
	}
	return plan
}

type appCache struct {
	ProjectID      string
	EnvironmentID  string
	ComposeID      string
	ComposeAppName string
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

func (c *Client) Apply(ctx context.Context, plan Plan) error {
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	total := len(plan.Steps)
	for index, step := range plan.Steps {
		emitProgress(plan.OnProgress, StepProgress{Index: index, Total: total, Step: step, Status: StepStatusStarted})
		err := c.applyStep(ctx, actx, step)
		if err != nil {
			emitProgress(plan.OnProgress, StepProgress{Index: index, Total: total, Step: step, Status: StepStatusError, Err: err})
			return fmt.Errorf("dokploy step %s for %s (%s): %w", step.Kind, step.App, step.Ref, err)
		}
		emitProgress(plan.OnProgress, StepProgress{Index: index, Total: total, Step: step, Status: StepStatusOK})
	}
	return nil
}

func emitProgress(fn *func(StepProgress), progress StepProgress) {
	if fn == nil || *fn == nil {
		return
	}
	(*fn)(progress)
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
	case StepSyncVolume:
		return c.applySyncVolume(ctx, actx, step)
	case StepDumpDataStore:
		return c.applyDumpDataStore(ctx, actx, step)
	case StepRestoreDataStore:
		return c.applyRestoreDataStore(ctx, actx, step)
	default:
		return fmt.Errorf("unknown step kind %q", step.Kind)
	}
}

func (c *Client) applyCreateProject(ctx context.Context, actx *applyContext, step Step) error {
	project, err := c.CreateProject(ctx, step.Ref, "managed by bort migration")
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
	env := FindEnvironmentInProject(project, "production")
	if env == nil {
		return fmt.Errorf("dokploy project %s has no environments", project.Name)
	}
	entry := actx.entry(step.App)
	entry.ProjectID = project.ProjectID
	entry.EnvironmentID = env.EnvironmentID
	return nil
}

func (c *Client) applyCreateService(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.EnvironmentID == "" {
		return fmt.Errorf("missing environment for app %s; create_project must run first", step.App)
	}
	composeFile, err := readComposeFile(actx.plan, step.App)
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
	composeFile, err := readComposeFile(actx.plan, step.App)
	if err != nil {
		return err
	}
	envContent, err := readEnvContent(actx.plan, step.App)
	if err != nil {
		return err
	}
	return c.UpdateCompose(ctx, entry.ComposeID, composeFile, envContent)
}

func (c *Client) applyPushImage(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if entry.ComposeID == "" {
		return fmt.Errorf("missing composeId for app %s; create_service must run first", step.App)
	}
	title := "bort-migrate"
	if actx.plan.RunName != "" {
		title = "bort-migrate-" + actx.plan.RunName
	}
	return c.DeployCompose(ctx, entry.ComposeID, title)
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
	port := parsePort(route.Port)
	_, err := c.CreateDomain(ctx, CreateDomainRequest{
		Host:        route.Host,
		ComposeID:   entry.ComposeID,
		ServiceName: route.ServiceName,
		Port:        port,
	})
	return err
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
	return string(contents), nil
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
			if !ok || key == "" {
				continue
			}
			merged[key] = strings.TrimSpace(value)
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
		b.WriteString(merged[key])
		b.WriteString("\n")
	}
	return b.String(), nil
}

func findPrepareApp(prepare preparer.Result, name string) (preparer.AppPlan, bool) {
	for _, app := range prepare.Apps {
		if app.Name == name {
			return app, true
		}
	}
	return preparer.AppPlan{}, false
}
