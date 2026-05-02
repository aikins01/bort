package gateway

import (
	"fmt"

	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

const (
	APIVersion = "bort.cutover/v1alpha1"

	DefaultObservationWindowSeconds = 300
	DefaultRollbackWindowSeconds    = 3600
)

type Phase string

const (
	PhasePreflight Phase = "preflight"
	PhaseHealth    Phase = "health_check"
	PhaseRoute     Phase = "route_cutover"
	PhaseObserve   Phase = "observe"
	PhaseRollback  Phase = "rollback"
)

type Options struct {
	BundleDir                string
	AppName                  string
	Target                   string
	ObservationWindowSeconds *int
	RollbackWindowSeconds    *int
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
	Name                     string             `json:"name"`
	Directory                string             `json:"directory"`
	Status                   preparer.Status    `json:"status"`
	Readiness                preparer.Readiness `json:"readiness"`
	PrepareReadiness         preparer.Readiness `json:"prepareReadiness"`
	SyncReadiness            preparer.Readiness `json:"syncReadiness"`
	ObservationWindowSeconds int                `json:"observationWindowSeconds"`
	RollbackWindowSeconds    int                `json:"rollbackWindowSeconds"`
	Routes                   []Route            `json:"routes,omitempty"`
	Gates                    []preparer.Gate    `json:"gates,omitempty"`
	Steps                    []Step             `json:"steps,omitempty"`
	Actions                  []Action           `json:"actions"`
}

type Route struct {
	Host        string             `json:"host"`
	ServiceName string             `json:"serviceName,omitempty"`
	Port        string             `json:"port,omitempty"`
	Source      string             `json:"source,omitempty"`
	CurrentRef  string             `json:"currentRef"`
	TargetRef   string             `json:"targetRef"`
	Readiness   preparer.Readiness `json:"readiness"`
}

type Step struct {
	ID           string             `json:"id"`
	Phase        Phase              `json:"phase"`
	ResourceType string             `json:"resourceType"`
	ResourceRef  string             `json:"resourceRef"`
	TargetRef    string             `json:"targetRef,omitempty"`
	Action       string             `json:"action"`
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
	observationWindowSeconds := DefaultObservationWindowSeconds
	if opts.ObservationWindowSeconds != nil {
		observationWindowSeconds = *opts.ObservationWindowSeconds
	}
	rollbackWindowSeconds := DefaultRollbackWindowSeconds
	if opts.RollbackWindowSeconds != nil {
		rollbackWindowSeconds = *opts.RollbackWindowSeconds
	}

	preparePlan, err := preparer.Plan(preparer.Options{BundleDir: opts.BundleDir, AppName: opts.AppName, Target: opts.Target})
	if err != nil {
		return Result{}, err
	}
	syncResult := syncplan.PlanFromPrepare(preparePlan)

	result := Result{APIVersion: APIVersion, BundleDir: preparePlan.BundleDir, Target: preparePlan.Target, DryRun: true, Status: preparer.StatusGreen}
	for i, app := range preparePlan.Apps {
		syncApp := syncResult.Apps[i]
		appPlan := planApp(app, syncApp, observationWindowSeconds, rollbackWindowSeconds)
		result.Apps = append(result.Apps, appPlan)
		result.Status = preparer.WorseStatus(result.Status, appPlan.Status)
	}
	return result, nil
}

func planApp(app preparer.AppPlan, syncApp syncplan.AppPlan, observationWindowSeconds, rollbackWindowSeconds int) AppPlan {
	plan := AppPlan{
		Name:                     app.Name,
		Directory:                app.Directory,
		Status:                   app.Status,
		Readiness:                preparer.WorseReadiness(app.Readiness, syncApp.Readiness),
		PrepareReadiness:         app.Readiness,
		SyncReadiness:            syncApp.Readiness,
		ObservationWindowSeconds: observationWindowSeconds,
		RollbackWindowSeconds:    rollbackWindowSeconds,
		Gates:                    append([]preparer.Gate{}, syncApp.Gates...),
	}

	usedIDs := map[string]int{}
	preflightStep := preflightStep(syncApp, usedIDs)
	plan.addStep(preflightStep, "preflight", "confirm sync plan is verified before cutover")
	if syncApp.Readiness == preparer.ReadinessReadyToCreate {
		plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "cutover.sync_verification_required", "confirm data sync completed and was verified before cutover", "sync", nil)
	} else {
		plan.addGate(syncApp.Readiness, preparer.SeverityFromReadiness(syncApp.Readiness), "cutover.sync_not_ready", "sync plan must be resolved and verified before cutover", "sync", nil)
	}

	for _, route := range routesForApp(app) {
		plan.Routes = append(plan.Routes, route)
		healthStep := healthStep(route, usedIDs, preflightStep.ID)
		plan.addStep(healthStep, "health", fmt.Sprintf("verify target health for %s before route cutover", routeRef(route)))
		if route.Host != "" {
			plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "cutover.health_check_required", fmt.Sprintf("verify target health for %s before route cutover", route.Host), "route:"+route.Host, routeEvidence(route))
		}
		routeStep := routeStep(route, usedIDs, healthStep.ID)
		plan.addStep(routeStep, "route", fmt.Sprintf("plan route cutover for %s to %s", routeRef(route), planutil.Fallback(route.TargetRef, "target")))
		observeStep := observeStep(route, observationWindowSeconds, usedIDs, routeStep.ID)
		plan.addStep(observeStep, "observe", fmt.Sprintf("observe %s after route cutover", routeRef(route)))
		rollbackStep := rollbackStep(route, rollbackWindowSeconds, usedIDs, routeStep.ID)
		plan.addStep(rollbackStep, "rollback", fmt.Sprintf("keep rollback path for %s through %s", routeRef(route), planutil.Fallback(route.CurrentRef, "source")))
	}
	if len(plan.Routes) == 0 {
		plan.add(preparer.SeverityInfo, "route", "no public route cutover resources detected")
	}

	for _, step := range plan.Steps {
		plan.Readiness = preparer.WorseReadiness(plan.Readiness, step.Readiness)
	}
	plan.Readiness = preparer.WorseReadiness(plan.Readiness, preparer.ReadinessFromGates(plan.Gates))
	plan.Status = preparer.StatusFromReadiness(plan.Readiness)
	return plan
}

func preflightStep(syncApp syncplan.AppPlan, usedIDs map[string]int) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "preflight:sync"),
		Phase:        PhasePreflight,
		ResourceType: "sync",
		ResourceRef:  "sync",
		Action:       "confirm_sync_plan_ready_for_cutover",
		Readiness:    cutoverReadiness(syncApp.Readiness),
		Evidence:     syncStepEvidence(syncApp),
	}
}

func healthStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "health:"+route.Host),
		Phase:        PhaseHealth,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.TargetRef,
		Action:       "verify_target_route_health",
		Readiness:    cutoverReadiness(route.Readiness),
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func routeStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "cutover:"+route.Host),
		Phase:        PhaseRoute,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.TargetRef,
		Action:       "plan_route_cutover",
		Readiness:    cutoverReadiness(route.Readiness),
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func observeStep(route Route, seconds int, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "observe:"+route.Host),
		Phase:        PhaseObserve,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.TargetRef,
		Action:       fmt.Sprintf("observe_target_route_for_%d_seconds", seconds),
		Readiness:    cutoverReadiness(route.Readiness),
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func rollbackStep(route Route, seconds int, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "rollback:"+route.Host),
		Phase:        PhaseRollback,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.CurrentRef,
		Action:       fmt.Sprintf("keep_source_route_available_for_%d_seconds", seconds),
		Readiness:    cutoverReadiness(route.Readiness),
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func routesForApp(app preparer.AppPlan) []Route {
	dokployHosts := map[string]struct{}{}
	if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
		for _, domain := range app.TargetResources.Dokploy.Domains {
			dokployHosts[domain.Host] = struct{}{}
		}
	}

	routes := make([]Route, 0, len(app.Resources.Domains))
	for _, domain := range app.Resources.Domains {
		targetRef := "domain:" + planutil.Fallback(domain.Host, "missing-host")
		if _, ok := dokployHosts[domain.Host]; domain.Host != "" && ok {
			targetRef = "dokploy.domain:" + domain.Host
		}
		routes = append(routes, Route{
			Host:        domain.Host,
			ServiceName: domain.ServiceName,
			Port:        domain.Port,
			Source:      domain.Source,
			CurrentRef:  "source.route:" + planutil.Fallback(domain.Host, "missing-host"),
			TargetRef:   targetRef,
			Readiness:   domain.Readiness,
		})
	}
	return routes
}

func (p *AppPlan) addStep(step Step, kind, message string) {
	step.DependsOn = planutil.UniqueStrings(step.DependsOn)
	step.Evidence = planutil.UniqueStrings(step.Evidence)
	p.Steps = append(p.Steps, step)
	p.add(preparer.SeverityFromReadiness(step.Readiness), kind, message)
}

func (p *AppPlan) addGate(readiness preparer.Readiness, severity preparer.Severity, code, message, resourceRef string, evidence []string) {
	p.Gates = append(p.Gates, preparer.Gate{Readiness: readiness, Severity: severity, Code: code, Message: message, ResourceRef: resourceRef, Evidence: planutil.UniqueStrings(evidence)})
}

func (p *AppPlan) add(severity preparer.Severity, kind, message string) {
	p.Actions = append(p.Actions, Action{Severity: severity, Kind: kind, Message: message})
}

func routeEvidence(route Route) []string {
	return planutil.UniqueStrings([]string{route.Host, route.ServiceName, route.Port, route.Source, route.CurrentRef, route.TargetRef})
}

func syncStepEvidence(app syncplan.AppPlan) []string {
	evidence := make([]string, 0, len(app.Steps))
	for _, step := range app.Steps {
		evidence = append(evidence, string(step.Phase)+":"+step.ResourceRef+":"+string(step.Readiness))
	}
	return evidence
}

func routeRef(route Route) string {
	return "route:" + planutil.Fallback(route.Host, "missing-host")
}

func cutoverReadiness(readiness preparer.Readiness) preparer.Readiness {
	switch readiness {
	case preparer.ReadinessBlocked, preparer.ReadinessNeedsInput:
		return readiness
	default:
		return preparer.ReadinessNeedsDecision
	}
}
