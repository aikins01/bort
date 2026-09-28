package dokploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// applyStopCoolifyProxy frees ports 80/443 on the source host so dokploy's
// traefik can bind them. coolify-proxy is the canonical container name in
// both traefik and caddy modes, so a single stop covers either backend.
// stop is idempotent: a missing or already-stopped proxy is a no-op.
func (c *Client) applyStopCoolifyProxy(ctx context.Context, _ *applyContext, _ Step) error {
	return stopProxyContainer(ctx, c.dockerRunner(), coolifyProxyContainer)
}

// applyStartDokployProxy ensures dokploy-traefik is running so the routes
// installed via dokploy's API begin serving traffic. errors when the
// container is missing because dokploy must already be installed by this
// point — bort init-target --install (workstream b.3) bootstraps it.
func (c *Client) applyStartDokployProxy(ctx context.Context, _ *applyContext, _ Step) error {
	return startProxyContainer(ctx, c.dockerRunner(), dokployProxyContainer)
}

func (c *Client) VerifyTargetTrafficAuthority(ctx context.Context) error {
	verifyCtx, cancel := context.WithTimeout(ctx, dockerStartTimeout)
	defer cancel()
	runner := c.dockerRunner()
	target, err := inspectContainer(verifyCtx, runner, dokployProxyContainer)
	if err != nil {
		return fmt.Errorf("inspect proxy container %s: %w", dokployProxyContainer, err)
	}
	if normalizedRestartPolicyName(target.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		return fmt.Errorf("proxy container %s restart policy is %q, want unless-stopped", dokployProxyContainer, normalizedRestartPolicyName(target.HostConfig.RestartPolicy.Name))
	}
	if !target.State.Running {
		return fmt.Errorf("proxy container %s is not running", dokployProxyContainer)
	}
	source, err := inspectContainer(verifyCtx, runner, coolifyProxyContainer)
	if err != nil {
		if isContainerMissingErr(err) {
			return nil
		}
		return fmt.Errorf("inspect proxy container %s: %w", coolifyProxyContainer, err)
	}
	if normalizedRestartPolicyName(source.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		return fmt.Errorf("proxy container %s restart policy is %q, want unless-stopped", coolifyProxyContainer, normalizedRestartPolicyName(source.HostConfig.RestartPolicy.Name))
	}
	if source.State.Running {
		return fmt.Errorf("proxy container %s is still running", coolifyProxyContainer)
	}
	return nil
}

func (c *Client) AdoptTargetTrafficAuthority(ctx context.Context) error {
	runner := c.dockerRunner()
	if _, _, err := prepareProxyForReconcile(ctx, runner, dokployProxyContainer, false); err != nil {
		return err
	}
	if _, _, err := prepareProxyForReconcile(ctx, runner, coolifyProxyContainer, true); err != nil {
		return err
	}
	return c.VerifyTargetTrafficAuthority(ctx)
}

func (c *Client) ReconcileTargetTrafficAuthority(ctx context.Context) error {
	runner := c.dockerRunner()
	target, _, err := prepareProxyForReconcile(ctx, runner, dokployProxyContainer, false)
	if err != nil {
		return err
	}
	source, found, err := prepareProxyForReconcile(ctx, runner, coolifyProxyContainer, true)
	if err != nil {
		return err
	}
	if found && source.State.Running {
		if err := applyPreparedProxyState(ctx, runner, source, false); err != nil {
			return c.restoreProxyAfterFailedHandoff(err, runner, true, source, target, c.verifySourceTrafficAuthority)
		}
	}
	if !target.State.Running {
		if err := applyPreparedProxyState(ctx, runner, target, true); err != nil {
			return c.restoreProxyAfterFailedHandoff(err, runner, found, source, target, c.verifySourceTrafficAuthority)
		}
	}
	if err := c.VerifyTargetTrafficAuthority(ctx); err != nil {
		return c.restoreProxyAfterFailedHandoff(err, runner, found, source, target, c.verifySourceTrafficAuthority)
	}
	return nil
}

func (c *Client) ReconcileSourceTrafficAuthority(ctx context.Context) error {
	runner := c.dockerRunner()
	source, _, err := prepareProxyForReconcile(ctx, runner, coolifyProxyContainer, false)
	if err != nil {
		return err
	}
	target, found, err := prepareProxyForReconcile(ctx, runner, dokployProxyContainer, true)
	if err != nil {
		return err
	}
	if found && target.State.Running {
		if err := applyPreparedProxyState(ctx, runner, target, false); err != nil {
			return c.restoreProxyAfterFailedHandoff(err, runner, true, target, source, c.VerifyTargetTrafficAuthority)
		}
	}
	if !source.State.Running {
		if err := applyPreparedProxyState(ctx, runner, source, true); err != nil {
			return c.restoreProxyAfterFailedHandoff(err, runner, found, target, source, c.VerifyTargetTrafficAuthority)
		}
	}
	if err := c.verifySourceTrafficAuthority(ctx); err != nil {
		return c.restoreProxyAfterFailedHandoff(err, runner, found, target, source, c.VerifyTargetTrafficAuthority)
	}
	return nil
}

func (c *Client) restoreProxyAfterFailedHandoff(handoffErr error, runner dockerRunner, restore bool, original, destination dockerContainer, verify func(context.Context) error) error {
	if !restore {
		return handoffErr
	}
	restoreCtx, cancel := context.WithTimeout(context.Background(), dockerStartTimeout)
	defer cancel()
	var operationErr error
	if err := applyPreparedProxyState(restoreCtx, runner, destination, false); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	if err := applyPreparedProxyState(restoreCtx, runner, original, true); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	if err := verify(restoreCtx); err != nil {
		return errors.Join(handoffErr, fmt.Errorf("restore proxy traffic authority to %s: %w", original.Name, errors.Join(operationErr, err)))
	}
	return fmt.Errorf("%w; restored proxy traffic authority to %s", handoffErr, original.Name)
}

func prepareProxyForReconcile(ctx context.Context, runner dockerRunner, name string, allowMissing bool) (dockerContainer, bool, error) {
	inspectCtx, cancelInspect := context.WithTimeout(ctx, dockerStopTimeout)
	container, err := inspectContainer(inspectCtx, runner, name)
	cancelInspect()
	if err != nil {
		if allowMissing && isContainerMissingErr(err) {
			return dockerContainer{}, false, nil
		}
		return dockerContainer{}, false, fmt.Errorf("inspect proxy container %s restart policy: %w", name, err)
	}
	if normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		updateCtx, cancelUpdate := context.WithTimeout(ctx, dockerStopTimeout)
		err := updateContainerRestartPolicy(updateCtx, runner, container.ID, "unless-stopped")
		cancelUpdate()
		if err != nil {
			return dockerContainer{}, false, fmt.Errorf("set proxy container %s restart policy: %w", name, err)
		}
	}
	return container, true, nil
}

func applyPreparedProxyState(ctx context.Context, runner dockerRunner, container dockerContainer, running bool) error {
	if running {
		startCtx, cancel := context.WithTimeout(ctx, dockerStartTimeout)
		err := startContainer(startCtx, runner, container.ID)
		cancel()
		if err != nil {
			return fmt.Errorf("start proxy container %s: %w", container.Name, err)
		}
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, dockerStopTimeout)
	err := stopContainer(stopCtx, runner, container.ID)
	cancel()
	if err != nil && !isContainerMissingErr(err) {
		return fmt.Errorf("stop proxy container %s: %w", container.Name, err)
	}
	return nil
}

func updateAndVerifyProxyRestartPolicy(ctx context.Context, runner dockerRunner, name, id string) error {
	updateCtx, cancelUpdate := context.WithTimeout(ctx, dockerStopTimeout)
	err := updateContainerRestartPolicy(updateCtx, runner, id, "unless-stopped")
	cancelUpdate()
	if err != nil {
		return fmt.Errorf("set proxy container %s restart policy: %w", name, err)
	}
	verifyCtx, cancelVerify := context.WithTimeout(ctx, dockerStopTimeout)
	container, err := inspectContainer(verifyCtx, runner, name)
	cancelVerify()
	if err != nil {
		return fmt.Errorf("verify proxy container %s restart policy: %w", name, err)
	}
	if normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		return fmt.Errorf("proxy container %s restart policy is %q after update, want unless-stopped", name, normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name))
	}
	return nil
}

func (c *Client) verifySourceTrafficAuthority(ctx context.Context) error {
	verifyCtx, cancel := context.WithTimeout(ctx, dockerStartTimeout)
	defer cancel()
	target, err := inspectContainer(verifyCtx, c.dockerRunner(), dokployProxyContainer)
	if err != nil {
		if isContainerMissingErr(err) {
			target = dockerContainer{}
		} else {
			return fmt.Errorf("inspect proxy container %s: %w", dokployProxyContainer, err)
		}
	}
	if target.State.Running {
		return fmt.Errorf("proxy container %s is still running", dokployProxyContainer)
	}
	if target.ID != "" && normalizedRestartPolicyName(target.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		return fmt.Errorf("proxy container %s restart policy is %q, want unless-stopped", dokployProxyContainer, normalizedRestartPolicyName(target.HostConfig.RestartPolicy.Name))
	}
	source, err := inspectContainer(verifyCtx, c.dockerRunner(), coolifyProxyContainer)
	if err != nil {
		return fmt.Errorf("inspect proxy container %s: %w", coolifyProxyContainer, err)
	}
	if !source.State.Running {
		return fmt.Errorf("proxy container %s is not running", coolifyProxyContainer)
	}
	if normalizedRestartPolicyName(source.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		return fmt.Errorf("proxy container %s restart policy is %q, want unless-stopped", coolifyProxyContainer, normalizedRestartPolicyName(source.HostConfig.RestartPolicy.Name))
	}
	return nil
}

func stopProxyContainer(ctx context.Context, runner dockerRunner, name string) error {
	container, err := inspectContainer(ctx, runner, name)
	if err != nil {
		// missing container is idempotent: nothing to stop. any other
		// inspect failure (e.g. docker daemon down) bubbles up so the
		// operator sees the underlying problem instead of a silent skip.
		if isContainerMissingErr(err) {
			return nil
		}
		return fmt.Errorf("inspect proxy container %s: %w", name, err)
	}
	policy := normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name)
	if policy != "no" && policy != "unless-stopped" {
		if err := updateAndVerifyProxyRestartPolicy(ctx, runner, name, container.ID); err != nil {
			return err
		}
	}
	if !container.State.Running {
		return nil
	}
	if err := stopContainer(ctx, runner, container.ID); err != nil {
		// stop loses its target between inspect and stop on a busy host;
		// the goal is "container is not running" and that's already true.
		if isContainerMissingErr(err) {
			return nil
		}
		return fmt.Errorf("stop proxy container %s: %w", name, err)
	}
	return nil
}

func startProxyContainer(ctx context.Context, runner dockerRunner, name string) error {
	container, err := inspectContainer(ctx, runner, name)
	if err != nil {
		return fmt.Errorf("inspect proxy container %s: %w", name, err)
	}
	if container.State.Running {
		return nil
	}
	if err := startContainer(ctx, runner, container.ID); err != nil {
		return fmt.Errorf("start proxy container %s: %w", name, err)
	}
	return nil
}

func isContainerMissingErr(err error) bool {
	if err == nil {
		return false
	}
	// docker engines vary: classic CLI prints "No such container", newer
	// builds and some object inspections print "No such object". both
	// mean the target is gone and the caller should treat the op as a
	// no-op. avoid bare "not found" — that would also swallow missing
	// docker binary / daemon errors.
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no such object") ||
		strings.Contains(message, "no results")
}
