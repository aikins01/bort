package dokploy

import (
	"context"
	"fmt"

	"github.com/aikins01/bort/internal/preparer"
)

// applyPauseSource stops the source containers belonging to non-data-store
// services of the app so the upcoming volume/bind sync sees a quiesced
// filesystem. data-store containers stay running here; logical dumps
// already ran in earlier steps, and pausing the database service is left
// to a later step (or to source cleanup on commit).
func (c *Client) applyPauseSource(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	ids := sourceQuiesceTargets(app)
	if len(ids) == 0 {
		return nil
	}
	runner := c.dockerRunner()
	for _, id := range ids {
		container, err := inspectContainer(ctx, runner, id)
		if err != nil {
			return fmt.Errorf("inspect source container %s: %w", id, err)
		}
		if !container.State.Running {
			continue
		}
		if _, err := runner.Output(ctx, "stop", container.ID); err != nil {
			return fmt.Errorf("stop source container %s: %w", container.ID, err)
		}
	}
	return nil
}

// applyResumeSource restarts the source containers that the quiesce step
// owns. it is stateless: rollback may run in a fresh process where the
// in-memory PausedContainers cache is empty, so we re-derive targets from
// the prepare result and only start what is currently stopped.
func (c *Client) applyResumeSource(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	refs := sourceQuiesceTargets(app)
	if len(refs) == 0 {
		return nil
	}
	runner := c.dockerRunner()
	for _, ref := range refs {
		container, err := inspectContainer(ctx, runner, ref)
		if err != nil {
			return fmt.Errorf("inspect source container %s: %w", ref, err)
		}
		if container.State.Running {
			continue
		}
		if _, err := runner.Output(ctx, "start", container.ID); err != nil {
			return fmt.Errorf("start source container %s: %w", container.ID, err)
		}
	}
	return nil
}

// sourceQuiesceTargets returns unique source container refs for every
// non-data-store service of the app. stateless workers must stop too:
// they keep writing to the database and to shared volumes, and either
// would corrupt the migration if left running. data-store containers
// stay up so the logical dump steps can read from them.
func sourceQuiesceTargets(app preparer.AppPlan) []string {
	dataStoreServices := map[string]struct{}{}
	for _, store := range app.Resources.DataStores {
		if store.Service != "" {
			dataStoreServices[store.Service] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	refs := []string{}
	add := func(service, ref string) {
		if ref == "" {
			return
		}
		if _, isStore := dataStoreServices[service]; isStore {
			return
		}
		if _, dup := seen[ref]; dup {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	for _, service := range app.Resources.SourceServices {
		ref := service.ContainerID
		if ref == "" {
			ref = service.ContainerName
		}
		add(service.ServiceName, ref)
	}
	// fall back to volume-owner discovery when source services were not
	// captured (older bundles). use container name when id is missing so
	// stale-id bundles still pause cleanly.
	for _, volume := range app.Resources.Volumes {
		ref := volume.SourceContainerID
		if ref == "" {
			ref = volume.SourceContainerName
		}
		add(volume.Service, ref)
	}
	return refs
}
