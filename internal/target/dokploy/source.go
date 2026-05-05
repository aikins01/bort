package dokploy

import (
	"context"
	"fmt"
	"strings"

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
	refs := sourceQuiesceTargetRefs(app)
	if len(refs) == 0 {
		return nil
	}
	runner := c.dockerRunner()
	for _, ref := range refs {
		container, err := sourceContainerForQuiesce(ctx, runner, ref)
		if err != nil {
			return fmt.Errorf("inspect source container %s: %w", ref.label(), err)
		}
		if !container.State.Running {
			continue
		}
		if err := stopContainer(ctx, runner, container.ID); err != nil {
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
	refs := sourceQuiesceTargetRefs(app)
	if len(refs) == 0 {
		return nil
	}
	runner := c.dockerRunner()
	for _, ref := range refs {
		container, err := sourceContainerForQuiesce(ctx, runner, ref)
		if err != nil {
			return fmt.Errorf("inspect source container %s: %w", ref.label(), err)
		}
		if container.State.Running {
			continue
		}
		if err := startContainer(ctx, runner, container.ID); err != nil {
			return fmt.Errorf("start source container %s: %w", container.ID, err)
		}
	}
	return nil
}

// applyStopSourceApp is the commit-time stop: it shuts down every source
// container belonging to the app, including data stores excluded from the
// cutover quiesce step (logical-dump postgres, skip-strategy stores). the
// stop is idempotent — a missing or already-stopped container is a no-op
// — so commit can run safely even on a partially-cleaned host. containers
// are not removed: operators in a long rollback window can docker start
// them back up to roll back outside bort.
func (c *Client) applyStopSourceApp(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	refs := sourceCommitTargets(app)
	if len(refs) == 0 {
		return nil
	}
	runner := c.dockerRunner()
	for _, ref := range refs {
		container, err := sourceContainer(ctx, runner, ref.id, ref.name)
		if err != nil {
			// recreated source containers can change ID, so we already
			// fell back to the recorded name above. anything still
			// missing here is gone for real and counts as stopped.
			if isContainerMissingErr(err) {
				continue
			}
			return fmt.Errorf("inspect source container %s: %w", ref.label(), err)
		}
		if !container.State.Running {
			continue
		}
		if err := stopContainer(ctx, runner, container.ID); err != nil {
			if isContainerMissingErr(err) {
				continue
			}
			return fmt.Errorf("stop source container %s: %w", container.ID, err)
		}
	}
	return nil
}

type commitTargetRef struct {
	id      string
	name    string
	service string
}

func (r commitTargetRef) label() string {
	if r.id != "" {
		return r.id
	}
	return r.name
}

// sourceCommitTargets returns every unique source container ref for the
// app — workers, web services, and data stores all included. unlike
// sourceQuiesceTargets which leaves logical-dump and skip-strategy stores
// running so pg_dump can read them, commit cleanup must stop everything
// because the migration is over. id and name are kept separately so a
// recreated container (id changed since scan) can still be stopped via
// its stable name.
func sourceCommitTargets(app preparer.AppPlan) []commitTargetRef {
	seenID := map[string]struct{}{}
	seenName := map[string]struct{}{}
	refs := []commitTargetRef{}
	add := func(id, name string) {
		if id == "" && name == "" {
			return
		}
		if id != "" {
			if _, dup := seenID[id]; dup {
				return
			}
			seenID[id] = struct{}{}
		} else {
			if _, dup := seenName[name]; dup {
				return
			}
			seenName[name] = struct{}{}
		}
		refs = append(refs, commitTargetRef{id: id, name: name})
	}
	for _, service := range app.Resources.SourceServices {
		add(service.ContainerID, service.ContainerName)
	}
	for _, volume := range app.Resources.Volumes {
		add(volume.SourceContainerID, volume.SourceContainerName)
	}
	for _, store := range app.Resources.DataStores {
		add(store.SourceContainerID, store.SourceContainerName)
	}
	return refs
}

func sourceContainerForQuiesce(ctx context.Context, runner dockerRunner, ref commitTargetRef) (dockerContainer, error) {
	container, err := sourceContainer(ctx, runner, ref.id, ref.name)
	if err == nil {
		return container, nil
	}
	project := composeProjectFromCoolifyContainerName(ref.name, ref.service)
	if project == "" {
		return dockerContainer{}, err
	}
	container, ok, listErr := findComposeServiceContainerByProject(ctx, runner, project, ref.service)
	if listErr != nil {
		return dockerContainer{}, err
	}
	if ok {
		return container, nil
	}
	return dockerContainer{}, err
}

func findComposeServiceContainerByProject(ctx context.Context, runner dockerRunner, project, service string) (dockerContainer, bool, error) {
	if project == "" || service == "" {
		return dockerContainer{}, false, nil
	}
	out, err := runner.Output(ctx, "ps", "-a",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label="+composeServiceLabel+"="+service,
		"--format", "{{.ID}}",
	)
	if err != nil {
		return dockerContainer{}, false, err
	}
	var fallback dockerContainer
	haveFallback := false
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		container, err := inspectContainer(ctx, runner, id)
		if err != nil {
			if isContainerMissingErr(err) {
				continue
			}
			return dockerContainer{}, false, err
		}
		if container.State.Running {
			return container, true, nil
		}
		if !haveFallback {
			fallback = container
			haveFallback = true
		}
	}
	return fallback, haveFallback, nil
}

func composeProjectFromCoolifyContainerName(name, service string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	service = strings.TrimSpace(service)
	if name == "" || service == "" {
		return ""
	}
	prefix := service + "-"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, prefix)
	lastDash := strings.LastIndex(rest, "-")
	if lastDash <= 0 {
		return ""
	}
	return rest[:lastDash]
}

// sourceQuiesceTargets returns unique source container refs for every
// service that must stop before bort touches state. stateless workers
// always stop because they keep writing to the database and to shared
// volumes. logical-dump stores stay running so pg_dump can read from
// them; volume-strategy stores must be paused so the on-disk format is
// consistent. skip-strategy stores are also excluded because we don't
// touch their data and have no business stopping them.
func sourceQuiesceTargets(app preparer.AppPlan) []string {
	refs := sourceQuiesceTargetRefs(app)
	targets := make([]string, 0, len(refs))
	for _, ref := range refs {
		targets = append(targets, ref.label())
	}
	return targets
}

func sourceQuiesceTargetRefs(app preparer.AppPlan) []commitTargetRef {
	excludedServices := map[string]struct{}{}
	for _, store := range app.Resources.DataStores {
		if store.Service == "" {
			continue
		}
		if dataStoreMigrationKind(store) != dataStoreMigrationVolume {
			excludedServices[store.Service] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	refs := []commitTargetRef{}
	add := func(service, id, name string) {
		if id == "" && name == "" {
			return
		}
		if _, excluded := excludedServices[service]; excluded {
			return
		}
		ref := id
		if ref == "" {
			ref = name
		}
		if _, dup := seen[ref]; dup {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, commitTargetRef{id: id, name: name, service: service})
	}
	for _, service := range app.Resources.SourceServices {
		add(service.ServiceName, service.ContainerID, service.ContainerName)
	}
	// fall back to volume-owner discovery when source services were not
	// captured (older bundles). use container name when id is missing so
	// stale-id bundles still pause cleanly.
	for _, volume := range app.Resources.Volumes {
		add(volume.Service, volume.SourceContainerID, volume.SourceContainerName)
	}
	return refs
}
