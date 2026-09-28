package dokploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

var rollbackObserveSettle = 5 * time.Second

var sourceHealthPollInterval = 2 * time.Second

// PlanForRollback builds the steps that return traffic to the source.
func PlanForRollback(prepare preparer.Result, sync syncplan.Result, cutover gateway.Result) (Plan, error) {
	plan := Plan{Prepare: prepare, Cutover: cutover, stepTimeout: dockerStartTimeout}
	statefulApps := []string{}
	seen := map[string]struct{}{}
	livePlan := PlanFromArtifacts(prepare, sync, cutover)
	for _, step := range livePlan.Steps {
		if step.Kind != StepPauseSource || shouldSkipApplyStep(livePlan, step) {
			continue
		}
		if _, exists := seen[step.App]; exists {
			continue
		}
		seen[step.App] = struct{}{}
		statefulApps = append(statefulApps, step.App)
	}
	if len(statefulApps) > 0 {
		return Plan{}, fmt.Errorf("automatic rollback is unsafe for stateful app(s) %s: Dokploy has no durable application fence that prevents a queued or future deployment from restarting target writers after source writers resume; preserve both sides and establish writer and traffic authority manually", strings.Join(statefulApps, ", "))
	}
	if cutoverPlanHasRoutes(cutover) {
		statelessApps := []preparer.AppPlan{}
		for _, app := range prepare.Apps {
			if len(sourceQuiesceTargetRefs(app)) > 0 {
				statelessApps = append(statelessApps, app)
			}
		}
		for _, app := range statelessApps {
			plan.Steps = append(plan.Steps, Step{Kind: StepVerifySourceHealth, App: app.Name, Ref: app.Name})
		}
		plan.Steps = append(plan.Steps,
			Step{Kind: StepStopDokployProxy, Ref: dokployProxyContainer},
			Step{Kind: StepStartCoolifyProxy, Ref: coolifyProxyContainer},
		)
		for _, app := range statelessApps {
			plan.Steps = append(plan.Steps, Step{Kind: StepObserveRollback, App: app.Name, Ref: app.Name})
		}
	}
	return plan, nil
}

func (c *Client) applyVerifySourceHealth(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	runner := c.dockerRunner()
	for _, ref := range sourceQuiesceTargetRefs(app) {
		if err := verifySourceContainerHealthy(ctx, runner, ref); err != nil {
			return err
		}
	}
	return nil
}

func verifySourceContainerHealthy(ctx context.Context, runner dockerRunner, ref commitTargetRef) error {
	verifyCtx, cancel := context.WithTimeout(ctx, dockerStartTimeout)
	defer cancel()

	container, err := sourceContainerForQuiesce(verifyCtx, runner, ref)
	if err != nil {
		return fmt.Errorf("inspect source container %s: %w", ref.label(), err)
	}
	if !container.State.Running {
		return fmt.Errorf("source container %s is not running; not returning traffic to the source", ref.label())
	}
	if container.State.Health == nil {
		return nil
	}
	for {
		switch container.State.Health.Status {
		case "healthy":
			return nil
		case "unhealthy":
			return fmt.Errorf("source container %s reports unhealthy; not returning traffic to the source", ref.label())
		}
		if err := waitContext(verifyCtx, sourceHealthPollInterval); err != nil {
			return fmt.Errorf("wait for source container %s health: %w", ref.label(), err)
		}
		container, err = sourceContainerForQuiesce(verifyCtx, runner, ref)
		if err != nil {
			return fmt.Errorf("inspect source container %s: %w", ref.label(), err)
		}
		if !container.State.Running {
			return fmt.Errorf("source container %s is not running; not returning traffic to the source", ref.label())
		}
		if container.State.Health == nil {
			return fmt.Errorf("source container %s lost its healthcheck during verification", ref.label())
		}
	}
}

func (c *Client) applyStopDokployProxy(ctx context.Context, _ *applyContext, _ Step) error {
	return stopProxyContainer(ctx, c.dockerRunner(), dokployProxyContainer)
}

func (c *Client) applyStartCoolifyProxy(ctx context.Context, _ *applyContext, _ Step) error {
	runner := c.dockerRunner()
	if err := startProxyContainer(ctx, runner, coolifyProxyContainer); err != nil {
		return err
	}
	container, err := inspectContainer(ctx, runner, coolifyProxyContainer)
	if err != nil {
		return fmt.Errorf("inspect proxy container %s: %w", coolifyProxyContainer, err)
	}
	if !container.State.Running {
		return fmt.Errorf("%s did not stay running; source traffic is not restored", coolifyProxyContainer)
	}
	return nil
}

func (c *Client) applyObserveRollback(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	if err := waitContext(ctx, rollbackObserveSettle); err != nil {
		return fmt.Errorf("observe source rollback for %s: %w", step.App, err)
	}
	runner := c.dockerRunner()
	for _, ref := range sourceQuiesceTargetRefs(app) {
		container, err := sourceContainerForQuiesce(ctx, runner, ref)
		if err != nil {
			if isContainerMissingErr(err) {
				return fmt.Errorf("source container %s disappeared after rollback; check the app before relying on it", ref.label())
			}
			return fmt.Errorf("inspect source container %s: %w", ref.label(), err)
		}
		if !container.State.Running {
			return fmt.Errorf("source container %s stopped after rollback; check the app before relying on it", ref.label())
		}
		if container.State.Health != nil && container.State.Health.Status != "healthy" {
			return fmt.Errorf("source container %s reports %s after rollback; check the app before relying on it", ref.label(), container.State.Health.Status)
		}
	}
	proxy, err := inspectContainer(ctx, runner, coolifyProxyContainer)
	if err != nil {
		return fmt.Errorf("inspect proxy container %s: %w", coolifyProxyContainer, err)
	}
	if !proxy.State.Running {
		return fmt.Errorf("%s stopped after rollback; source traffic is not being served", coolifyProxyContainer)
	}
	return nil
}
