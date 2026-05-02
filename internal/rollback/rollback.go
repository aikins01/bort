package rollback

import (
	"fmt"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/planfile"
	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
)

const (
	APIVersion = "bort.rollback/v1alpha1"

	DefaultObservationWindowSeconds = gateway.DefaultObservationWindowSeconds
)

type Phase string

const (
	PhasePreflight    Phase = "preflight"
	PhaseSourceHealth Phase = "source_health_check"
	PhaseRoute        Phase = "route_rollback"
	PhaseObserve      Phase = "observe"
)

type Options struct {
	BundleDir                string
	AppName                  string
	Target                   string
	ObservationWindowSeconds *int
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
	Role                     string             `json:"role,omitempty"`
	Status                   preparer.Status    `json:"status"`
	Readiness                preparer.Readiness `json:"readiness"`
	CutoverReadiness         preparer.Readiness `json:"cutoverReadiness"`
	ObservationWindowSeconds int                `json:"observationWindowSeconds"`
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

	cutoverPlan, err := gateway.Plan(gateway.Options{BundleDir: opts.BundleDir, AppName: opts.AppName, Target: opts.Target})
	if err != nil {
		return Result{}, err
	}

	return PlanFromCutover(cutoverPlan, observationWindowSeconds)
}

func PlanFromCutover(cutoverPlan gateway.Result, observationWindowSeconds int) (Result, error) {
	if err := planfile.CheckAPIVersion("cutover plan", cutoverPlan.APIVersion, gateway.APIVersion); err != nil {
		return Result{}, err
	}
	if err := planfile.CheckDryRun("cutover plan", cutoverPlan.DryRun); err != nil {
		return Result{}, err
	}
	if len(cutoverPlan.Apps) == 0 {
		return Result{}, fmt.Errorf("cutover plan has no apps")
	}

	result := Result{APIVersion: APIVersion, BundleDir: cutoverPlan.BundleDir, Target: cutoverPlan.Target, DryRun: true, Status: preparer.StatusGreen}
	for _, app := range cutoverPlan.Apps {
		appPlan := planApp(app, observationWindowSeconds)
		result.Apps = append(result.Apps, appPlan)
		result.Status = preparer.WorseStatus(result.Status, appPlan.Status)
	}
	return result, nil
}

func planApp(cutoverApp gateway.AppPlan, observationWindowSeconds int) AppPlan {
	plan := AppPlan{
		Name:                     cutoverApp.Name,
		Directory:                cutoverApp.Directory,
		Role:                     cutoverApp.Role,
		Status:                   cutoverApp.Status,
		Readiness:                cutoverApp.Readiness,
		CutoverReadiness:         cutoverApp.Readiness,
		ObservationWindowSeconds: observationWindowSeconds,
		Gates:                    append([]preparer.Gate{}, cutoverApp.Gates...),
	}

	usedIDs := map[string]int{}
	preflightStep := preflightStep(cutoverApp, usedIDs)
	plan.addStep(preflightStep, "preflight", "confirm rollback trigger and current traffic state before route rollback")
	if cutoverApp.Readiness == preparer.ReadinessBlocked || cutoverApp.Readiness == preparer.ReadinessNeedsInput {
		plan.addGate(cutoverApp.Readiness, preparer.SeverityFromReadiness(cutoverApp.Readiness), "rollback.cutover_not_ready", "cutover plan must be resolved before rollback route actions can be trusted", "cutover", nil)
	} else {
		plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "rollback.trigger_required", "confirm rollback trigger and current traffic location before changing routes", "rollback", nil)
	}

	for _, cutoverRoute := range cutoverApp.Routes {
		route := routeFromCutover(cutoverRoute)
		plan.Routes = append(plan.Routes, route)
		healthStep := sourceHealthStep(route, usedIDs, preflightStep.ID)
		plan.addStep(healthStep, "health", fmt.Sprintf("verify source health for %s before route rollback", routeRef(route)))
		if route.Host != "" {
			plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "rollback.source_health_required", fmt.Sprintf("verify source health for %s before route rollback", route.Host), "route:"+route.Host, routeEvidence(route))
		}
		routeStep := routeStep(route, usedIDs, healthStep.ID)
		plan.addStep(routeStep, "route", fmt.Sprintf("plan route rollback for %s to %s", routeRef(route), planutil.Fallback(route.CurrentRef, "source")))
		if observationWindowSeconds > 0 {
			observeStep := observeStep(route, observationWindowSeconds, usedIDs, routeStep.ID)
			plan.addStep(observeStep, "observe", fmt.Sprintf("observe %s after route rollback", routeRef(route)))
		}
	}
	if len(plan.Routes) == 0 {
		plan.add(preparer.SeverityInfo, "route", "no public route rollback resources detected")
	}

	for _, step := range plan.Steps {
		plan.Readiness = preparer.WorseReadiness(plan.Readiness, step.Readiness)
	}
	plan.Readiness = preparer.WorseReadiness(plan.Readiness, preparer.ReadinessFromGates(plan.Gates))
	plan.Status = preparer.StatusFromReadiness(plan.Readiness)
	return plan
}

func preflightStep(app gateway.AppPlan, usedIDs map[string]int) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "preflight:rollback"),
		Phase:        PhasePreflight,
		ResourceType: "cutover",
		ResourceRef:  "cutover",
		Action:       "confirm_rollback_context",
		Readiness:    preparer.ClampReadinessToDecision(app.Readiness),
		Evidence:     gateway.StepEvidence(app),
	}
}

func sourceHealthStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "source-health:"+route.Host),
		Phase:        PhaseSourceHealth,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.CurrentRef,
		Action:       "verify_source_route_health",
		Readiness:    route.Readiness,
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func routeStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "rollback:"+route.Host),
		Phase:        PhaseRoute,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.CurrentRef,
		Action:       "rollback_route",
		Readiness:    route.Readiness,
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
		TargetRef:    route.CurrentRef,
		Action:       fmt.Sprintf("observe_source_route_for_%d_seconds", seconds),
		Readiness:    route.Readiness,
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func routeFromCutover(route gateway.Route) Route {
	return Route{
		Host:        route.Host,
		ServiceName: route.ServiceName,
		Port:        route.Port,
		Source:      route.Source,
		CurrentRef:  route.CurrentRef,
		TargetRef:   route.TargetRef,
		Readiness:   preparer.ClampReadinessToDecision(route.Readiness),
	}
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

func routeRef(route Route) string {
	return "route:" + planutil.Fallback(route.Host, "missing-host")
}
