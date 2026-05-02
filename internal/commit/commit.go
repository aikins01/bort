package commit

import (
	"fmt"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/planfile"
	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
)

const (
	APIVersion = "bort.commit/v1alpha1"

	DefaultRollbackWindowSeconds = gateway.DefaultRollbackWindowSeconds
)

type Phase string

const (
	PhasePreflight    Phase = "preflight"
	PhaseVerifyTarget Phase = "verify_target"
	PhaseAcceptTarget Phase = "accept_target"
	PhaseRetireSource Phase = "retire_source"
)

type Options struct {
	BundleDir             string
	AppName               string
	Target                string
	RollbackWindowSeconds *int
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
	Name                  string             `json:"name"`
	Directory             string             `json:"directory"`
	Role                  string             `json:"role,omitempty"`
	Status                preparer.Status    `json:"status"`
	Readiness             preparer.Readiness `json:"readiness"`
	CutoverReadiness      preparer.Readiness `json:"cutoverReadiness"`
	RollbackWindowSeconds int                `json:"rollbackWindowSeconds"`
	Routes                []Route            `json:"routes,omitempty"`
	Gates                 []preparer.Gate    `json:"gates,omitempty"`
	Steps                 []Step             `json:"steps,omitempty"`
	Actions               []Action           `json:"actions"`
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
	cutoverPlan, err := gateway.Plan(gateway.Options{BundleDir: opts.BundleDir, AppName: opts.AppName, Target: opts.Target, RollbackWindowSeconds: opts.RollbackWindowSeconds})
	if err != nil {
		return Result{}, err
	}

	return PlanFromCutover(cutoverPlan)
}

func PlanFromCutover(cutoverPlan gateway.Result) (Result, error) {
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
		appPlan := planApp(app)
		result.Apps = append(result.Apps, appPlan)
		result.Status = preparer.WorseStatus(result.Status, appPlan.Status)
	}
	return result, nil
}

func planApp(cutoverApp gateway.AppPlan) AppPlan {
	plan := AppPlan{
		Name:                  cutoverApp.Name,
		Directory:             cutoverApp.Directory,
		Role:                  cutoverApp.Role,
		Status:                cutoverApp.Status,
		Readiness:             cutoverApp.Readiness,
		CutoverReadiness:      cutoverApp.Readiness,
		RollbackWindowSeconds: cutoverApp.RollbackWindowSeconds,
		Gates:                 append([]preparer.Gate{}, cutoverApp.Gates...),
	}

	usedIDs := map[string]int{}
	preflightStep := preflightStep(cutoverApp, usedIDs)
	plan.addStep(preflightStep, "preflight", "confirm cutover outcome before committing target ownership")
	if cutoverApp.Readiness == preparer.ReadinessBlocked || cutoverApp.Readiness == preparer.ReadinessNeedsInput {
		plan.addGate(cutoverApp.Readiness, preparer.SeverityFromReadiness(cutoverApp.Readiness), "commit.cutover_not_ready", "cutover plan must be resolved before target ownership can be committed", "cutover", nil)
	} else {
		plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "commit.target_acceptance_required", "confirm target is serving production traffic and source rollback is no longer required", "commit", nil)
	}

	for _, cutoverRoute := range cutoverApp.Routes {
		route := routeFromCutover(cutoverRoute)
		plan.Routes = append(plan.Routes, route)
		verifyStep := targetVerificationStep(route, usedIDs, preflightStep.ID)
		plan.addStep(verifyStep, "verify", fmt.Sprintf("verify target route for %s before commit", routeRef(route)))
		if route.Host != "" {
			plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "commit.target_route_acceptance_required", fmt.Sprintf("verify target route %s is serving accepted traffic before commit", route.Host), "route:"+route.Host, routeEvidence(route))
			if cutoverApp.RollbackWindowSeconds > 0 {
				plan.addGate(preparer.ReadinessNeedsDecision, preparer.SeverityWarn, "commit.rollback_window_closed", fmt.Sprintf("confirm rollback window for %s is closed or explicitly waived before retiring source route", route.Host), "route:"+route.Host, routeEvidence(route))
			}
		}
		acceptStep := acceptTargetStep(route, usedIDs, verifyStep.ID)
		plan.addStep(acceptStep, "accept", fmt.Sprintf("accept target ownership for %s", routeRef(route)))
		retireStep := retireSourceStep(route, usedIDs, acceptStep.ID)
		plan.addStep(retireStep, "cleanup", fmt.Sprintf("plan source route retirement for %s", routeRef(route)))
	}
	if len(plan.Routes) == 0 {
		plan.add(preparer.SeverityInfo, "route", "no public route commit resources detected")
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
		ID:           planutil.NextStepID(usedIDs, "preflight:commit"),
		Phase:        PhasePreflight,
		ResourceType: "cutover",
		ResourceRef:  "cutover",
		Action:       "confirm_cutover_outcome",
		Readiness:    preparer.ClampReadinessToDecision(app.Readiness),
		Evidence:     gateway.StepEvidence(app),
	}
}

func targetVerificationStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "verify-target:"+route.Host),
		Phase:        PhaseVerifyTarget,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.TargetRef,
		Action:       "verify_target_route_accepted",
		Readiness:    route.Readiness,
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func acceptTargetStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "accept-target:"+route.Host),
		Phase:        PhaseAcceptTarget,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.TargetRef,
		Action:       "plan_target_route_acceptance",
		Readiness:    route.Readiness,
		DependsOn:    planutil.OptionalDependency(dependency),
		Evidence:     routeEvidence(route),
	}
}

func retireSourceStep(route Route, usedIDs map[string]int, dependency string) Step {
	return Step{
		ID:           planutil.NextStepID(usedIDs, "retire-source:"+route.Host),
		Phase:        PhaseRetireSource,
		ResourceType: "route",
		ResourceRef:  routeRef(route),
		TargetRef:    route.CurrentRef,
		Action:       "plan_source_route_retirement",
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
