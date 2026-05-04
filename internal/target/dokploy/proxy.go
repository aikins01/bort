package dokploy

import (
	"context"
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
	if !container.State.Running {
		return nil
	}
	if _, err := runner.Output(ctx, "stop", container.ID); err != nil {
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
	if _, err := runner.Output(ctx, "start", container.ID); err != nil {
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
