package dokploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
)

const coolifyControlPlaneContainer = "coolify"

func planRequiresCoolifyDeploymentFence(plan Plan) bool {
	source := strings.ToLower(strings.TrimSpace(plan.Prepare.Source))
	coolifySource := source == "coolify-local" || source == "coolify-local-traefik" || source == "coolify-local-caddy"
	for _, step := range plan.Steps {
		if (step.Kind != StepRestoreDataStore && step.Kind != StepSyncVolume) || shouldSkipApplyStep(plan, step) {
			continue
		}
		if coolifySource {
			return true
		}
		if app, ok := findPrepareApp(plan.Prepare, step.App); ok && strings.EqualFold(strings.TrimSpace(app.Platform), "coolify") {
			return true
		}
	}
	return false
}

func requireCoolifyDeploymentFence(ctx context.Context, runner dockerRunner) error {
	container, err := inspectContainer(ctx, runner, coolifyControlPlaneContainer)
	if err != nil && !isContainerMissingErr(err) {
		return fmt.Errorf("stateful live apply requires a durable Coolify deployment fence, but Bort could not inspect the %s control-plane container: %w; record its current restart policy, run `%s`, then retry", coolifyControlPlaneContainer, err, coolifyFenceCommand())
	}
	if err == nil {
		policy := normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name)
		if container.State.Running || policy != "no" {
			return fmt.Errorf("stateful live apply requires the Coolify control-plane container %s to be stopped with restart policy no (running=%t, restart=%s); wait until no Coolify deployment is queued or running (a cancelled deployment can keep running, so confirm it has ended), record its current restart policy, run `%s`, then retry; restart Coolify only after source authority is finalized; after target acceptance, restart it only if other apps need it and immediately delete the migrated apps in Coolify", coolifyControlPlaneContainer, container.State.Running, policy, coolifyFenceCommand())
		}
	}
	helperRepositories := []string{coolifyHelperImageRepository}
	if err == nil {
		if custom := imageRepository(envMap(container.Config.Env)["HELPER_IMAGE"]); custom != "" {
			helperRepositories = append(helperRepositories, custom)
		}
	}
	helpers, err := activeCoolifyDeploymentHelpers(ctx, runner, helperRepositories)
	if err != nil {
		return fmt.Errorf("stateful live apply requires a durable Coolify deployment fence, but Bort could not inspect Coolify deployment helpers: %w", err)
	}
	if len(helpers) > 0 {
		return fmt.Errorf("stateful live apply requires in-flight Coolify deployments to finish, but helper container(s) %s are still running deployment commands that started before the control plane stopped; wait until they finish, then retry", strings.Join(helpers, ", "))
	}
	return nil
}

func coolifyFenceCommand() string {
	docker := "docker"
	if strings.TrimSpace(os.Getenv("SUDO_UID")) != "" {
		docker = "sudo docker"
	}
	return docker + " update --restart=no " + coolifyControlPlaneContainer + " && " + docker + " stop " + coolifyControlPlaneContainer
}

const coolifyHelperImageRepository = "coollabsio/coolify-helper"

func activeCoolifyDeploymentHelpers(ctx context.Context, runner dockerRunner, helperRepositories []string) ([]string, error) {
	var out []byte
	for attempt := 0; ; attempt++ {
		listed, err := runner.Output(ctx, "ps", "-q", "--no-trunc")
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(listed))
		if len(ids) == 0 {
			return nil, nil
		}
		out, err = runner.Output(ctx, append([]string{"inspect", "--type", "container", "--format", "{{.Name}} {{.Config.Image}} {{len .ExecIDs}}"}, ids...)...)
		if err == nil {
			break
		}
		if attempt == 2 || !isContainerMissingErr(err) {
			return nil, err
		}
	}
	active := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] == "0" {
			continue
		}
		repository := imageRepository(fields[1])
		for _, helper := range helperRepositories {
			if repository == helper || strings.HasSuffix(repository, "/"+helper) || strings.HasSuffix(helper, "/"+repository) {
				active = append(active, strings.TrimPrefix(fields[0], "/"))
				break
			}
		}
	}
	return active, nil
}

func imageRepository(image string) string {
	repository, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	return repository
}

func (c *Client) applyPauseSource(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	runner := c.dockerRunner()
	containers, err := inspectSourceQuiesceTargets(ctx, runner, app)
	if err != nil {
		return err
	}
	entry := actx.entry(step.App)
	recorded, err := entry.pausedSourceContainers(step.App, containers)
	if err != nil {
		return err
	}
	toStop := []string{}
	changed := !entry.SourcePauseRecorded
	for _, container := range containers {
		if !container.State.Running {
			continue
		}
		toStop = append(toStop, container.ID)
		for i := range recorded {
			if recorded[i].ID == container.ID && !recorded[i].Stopped {
				recorded[i].Stopped = true
				changed = true
			}
		}
	}
	if changed {
		previous, previouslyRecorded := entry.SourcePausedContainers, entry.SourcePauseRecorded
		entry.SourcePausedContainers = recorded
		entry.SourcePauseRecorded = true
		if err := actx.persistSourcePauseState(); err != nil {
			entry.SourcePausedContainers = previous
			entry.SourcePauseRecorded = previouslyRecorded
			return unsafeSourceResumeError{err: fmt.Errorf("record source container ownership before pause: %w", err)}
		}
	}
	for _, id := range toStop {
		if err := stopContainer(ctx, runner, id); err != nil {
			return fmt.Errorf("stop source container %s: %w", id, err)
		}
	}
	if len(toStop) > 0 {
		containers, err = inspectSourceQuiesceTargets(ctx, runner, app)
		if err != nil {
			return fmt.Errorf("verify paused source containers for app %s: %w", step.App, err)
		}
	}
	byID := make(map[string]dockerContainer, len(containers))
	for _, container := range containers {
		byID[container.ID] = container
	}
	changed = false
	for i, paused := range entry.SourcePausedContainers {
		container, found := byID[paused.ID]
		if !found {
			return unsafeSourceResumeError{err: fmt.Errorf("source container %s for app %s disappeared while pausing; refusing to record its stopped state", paused.ID, step.App)}
		}
		if paused.StartedAt == container.State.StartedAt && paused.FinishedAt == container.State.FinishedAt {
			continue
		}
		entry.SourcePausedContainers[i].StartedAt = container.State.StartedAt
		entry.SourcePausedContainers[i].FinishedAt = container.State.FinishedAt
		changed = true
	}
	if changed {
		if err := actx.persistSourcePauseState(); err != nil {
			return fmt.Errorf("record paused source container state for app %s: %w", step.App, err)
		}
	}
	return nil
}

// pausedSourceContainers returns the durable pause records for the
// inspected quiesce targets: a fresh snapshot of every target on the
// first pause, or the recorded set once it exists. Any mismatch between
// the two means the evidence for a safe retry is gone.
func (e *appCache) pausedSourceContainers(appName string, containers []dockerContainer) ([]sourcePausedContainer, error) {
	if !e.SourcePauseRecorded {
		recorded := make([]sourcePausedContainer, 0, len(containers))
		for _, container := range containers {
			recorded = append(recorded, sourcePausedContainer{ID: container.ID})
		}
		sortSourcePausedContainers(recorded)
		return recorded, nil
	}
	byID := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		byID[container.ID] = struct{}{}
	}
	recorded := make(map[string]struct{}, len(e.SourcePausedContainers))
	for _, paused := range e.SourcePausedContainers {
		recorded[paused.ID] = struct{}{}
		if _, found := byID[paused.ID]; !found {
			return nil, unsafeSourceResumeError{err: fmt.Errorf("durably recorded source container %s for app %s no longer matches the reviewed source; refusing to infer pause ownership", paused.ID, appName)}
		}
	}
	for _, container := range containers {
		if _, found := recorded[container.ID]; !found {
			return nil, unsafeSourceResumeError{err: fmt.Errorf("source container %s for app %s has no durable pause evidence; refusing to infer whether it ran since the pause", container.ID, appName)}
		}
	}
	return append([]sourcePausedContainer(nil), e.SourcePausedContainers...), nil
}

// sourceRanSincePause reports whether any quiesce target of the app ran
// after the recorded pause, which invalidates state transferred while
// the app was paused.
func (c *Client) sourceRanSincePause(ctx context.Context, actx *applyContext, appName string) (bool, error) {
	ran, err := c.firstSourceRanSincePause(ctx, actx, []string{appName})
	return ran != "", err
}

// firstSourceRanSincePause inspects the quiesce targets of all apps in one
// Docker call and returns the first app, in the given order, that ran.
func (c *Client) firstSourceRanSincePause(ctx context.Context, actx *applyContext, appNames []string) (string, error) {
	var refs []commitTargetRef
	counts := make([]int, len(appNames))
	for i, appName := range appNames {
		app, ok := findPrepareApp(actx.plan.Prepare, appName)
		if !ok {
			return "", fmt.Errorf("app %s not found in prepare result", appName)
		}
		appRefs := sourceQuiesceTargetRefs(app)
		counts[i] = len(appRefs)
		refs = append(refs, appRefs...)
	}
	inspected, err := inspectSourceCommitTargets(ctx, c.dockerRunner(), refs)
	if err != nil {
		return "", err
	}
	offset := 0
	for i, appName := range appNames {
		containers := inspected[offset : offset+counts[i]]
		offset += counts[i]
		recorded, err := actx.entry(appName).pausedSourceContainers(appName, containers)
		if err != nil {
			return "", err
		}
		byID := make(map[string]dockerContainer, len(containers))
		for _, container := range containers {
			byID[container.ID] = container
		}
		for _, paused := range recorded {
			container := byID[paused.ID]
			if container.State.Running || paused.StartedAt != container.State.StartedAt || paused.FinishedAt != container.State.FinishedAt {
				return appName, nil
			}
		}
	}
	return "", nil
}

func (c *Client) applyResumeSource(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	entry := actx.entry(step.App)
	if !entry.SourcePauseRecorded {
		return unsafeSourceResumeError{err: fmt.Errorf("source pause ownership for app %s was not durably recorded; refusing to start stopped containers", step.App)}
	}
	runner := c.dockerRunner()
	containers, err := inspectSourceQuiesceTargets(ctx, runner, app)
	if err != nil {
		return err
	}
	byID := make(map[string]dockerContainer, len(containers))
	for _, container := range containers {
		byID[container.ID] = container
	}
	for _, paused := range entry.SourcePausedContainers {
		if !paused.Stopped {
			continue
		}
		container, found := byID[paused.ID]
		if !found {
			return unsafeSourceResumeError{err: fmt.Errorf("durably owned source container %s for app %s no longer matches the reviewed source; refusing to start it", paused.ID, step.App)}
		}
		if container.State.Running {
			continue
		}
		if err := startContainer(ctx, runner, paused.ID); err != nil {
			return fmt.Errorf("start source container %s: %w", paused.ID, err)
		}
	}
	return nil
}

// AdoptHistoricalSourcePause records the reviewed quiesce targets of apps
// whose pause predates durable pause records. Bort cannot tell which of
// them it stopped, so it claims none: a stopped target must be started by
// the operator before the historical pause can be recorded as resumed.
// Apps with a record are left as is.
func (c *Client) AdoptHistoricalSourcePause(ctx context.Context, plan Plan, apps []string) error {
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := actx.loadSourcePauseState(); err != nil {
		return err
	}
	runner := c.dockerRunner()
	changed := false
	for _, name := range apps {
		entry := actx.entry(name)
		if entry.SourcePauseRecorded {
			continue
		}
		app, ok := findPrepareApp(plan.Prepare, name)
		if !ok {
			return fmt.Errorf("app %s not found in prepare result", name)
		}
		containers, err := inspectSourceQuiesceTargets(ctx, runner, app)
		if err != nil {
			return err
		}
		recorded := make([]sourcePausedContainer, 0, len(containers))
		stopped := []string{}
		for _, container := range containers {
			if !container.State.Running {
				stopped = append(stopped, container.ID)
			}
			recorded = append(recorded, sourcePausedContainer{
				ID:         container.ID,
				StartedAt:  container.State.StartedAt,
				FinishedAt: container.State.FinishedAt,
			})
		}
		if len(stopped) > 0 {
			return unsafeSourceResumeError{err: fmt.Errorf("source container(s) %s for app %s are stopped, but this run predates durable pause records so Bort cannot prove it stopped them; verify they should run, start them with `docker start %s`, then retry", strings.Join(stopped, ", "), name, strings.Join(stopped, " "))}
		}
		sortSourcePausedContainers(recorded)
		entry.SourcePausedContainers = recorded
		entry.SourcePauseRecorded = true
		changed = true
	}
	if !changed {
		return nil
	}
	return actx.persistSourcePauseState()
}

func (c *Client) FinalizeSourceResumes(plan Plan) error {
	resumedApps := map[string]struct{}{}
	for _, step := range plan.Steps {
		if step.Kind == StepResumeSource {
			resumedApps[step.App] = struct{}{}
		}
	}
	if len(resumedApps) == 0 {
		return nil
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := actx.loadSourcePauseState(); err != nil {
		return err
	}
	for app := range resumedApps {
		entry := actx.entry(app)
		entry.SourcePausedContainers = nil
		entry.SourcePauseRecorded = false
	}
	return actx.persistSourcePauseState()
}

func inspectSourceQuiesceTargets(ctx context.Context, runner dockerRunner, app preparer.AppPlan) ([]dockerContainer, error) {
	return inspectSourceCommitTargets(ctx, runner, sourceQuiesceTargetRefs(app))
}

const sourcePauseStateArtifact = "source-pause.json"
const sourcePauseStateAPIVersion = "bort.source-pause/v1alpha1"

type sourcePauseState struct {
	APIVersion string                             `json:"apiVersion"`
	Apps       map[string][]sourcePausedContainer `json:"apps"`
}

// sourcePausedContainer records a quiesce target of a paused app: whether
// Bort stopped it (and so may restart it) and the run timestamps observed
// after the pause, so a retry can tell whether it ran again in between.
type sourcePausedContainer struct {
	ID         string `json:"id"`
	Stopped    bool   `json:"stopped,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

func sortSourcePausedContainers(containers []sourcePausedContainer) {
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
}

func (a *applyContext) persistSourcePauseState() error {
	if a == nil || strings.TrimSpace(a.plan.RunDir) == "" {
		return nil
	}
	state := sourcePauseState{APIVersion: sourcePauseStateAPIVersion, Apps: map[string][]sourcePausedContainer{}}
	for app, entry := range a.cache {
		if !entry.SourcePauseRecorded {
			continue
		}
		containers := append([]sourcePausedContainer(nil), entry.SourcePausedContainers...)
		sortSourcePausedContainers(containers)
		state.Apps[app] = containers
	}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode source pause state: %w", err)
	}
	path, err := sourcePauseStatePath(a.plan.RunDir)
	if err != nil {
		return err
	}
	if err := safepath.WriteFileAtomicNoFollow(path, append(contents, '\n'), 0o600); err != nil {
		return fmt.Errorf("write source pause state: %w", err)
	}
	return nil
}

func (a *applyContext) loadSourcePauseState() error {
	if a == nil || strings.TrimSpace(a.plan.RunDir) == "" {
		return nil
	}
	path, err := sourcePauseStatePath(a.plan.RunDir)
	if err != nil {
		return err
	}
	contents, err := safepath.ReadFileNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read source pause state: %w", err)
	}
	var state sourcePauseState
	if err := json.Unmarshal(contents, &state); err != nil {
		return fmt.Errorf("decode source pause state: %w", err)
	}
	if state.APIVersion != sourcePauseStateAPIVersion || state.Apps == nil {
		return fmt.Errorf("%s has invalid source pause state", path)
	}
	for app, containers := range state.Apps {
		seen := map[string]struct{}{}
		for _, container := range containers {
			if strings.TrimSpace(container.ID) == "" {
				return fmt.Errorf("%s contains an empty source container identity for app %s", path, app)
			}
			if _, duplicate := seen[container.ID]; duplicate {
				return fmt.Errorf("%s contains duplicate source container identity %s for app %s", path, container.ID, app)
			}
			seen[container.ID] = struct{}{}
		}
		entry := a.entry(app)
		entry.SourcePausedContainers = append([]sourcePausedContainer(nil), containers...)
		entry.SourcePauseRecorded = true
	}
	return nil
}

func sourcePauseStatePath(runDir string) (string, error) {
	path := filepath.Join(runDir, sourcePauseStateArtifact)
	if err := safepath.ContainedPath(runDir, path); err != nil {
		return "", err
	}
	return path, nil
}

// Reviewed IDs never fall back to names; only legacy name-only refs tolerate absence.
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
	containers, err := inspectSourceCommitTargets(ctx, runner, refs)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name) != "no" {
			if err := updateContainerRestartPolicy(ctx, runner, container.ID, "no"); err != nil {
				return fmt.Errorf("fence retired source container %s restarts: %w", container.ID, err)
			}
		}
		if !container.State.Running {
			continue
		}
		if err := stopContainer(ctx, runner, container.ID); err != nil {
			return fmt.Errorf("stop source container %s: %w", container.ID, err)
		}
	}
	containers, err = inspectSourceCommitTargets(ctx, runner, refs)
	if err != nil {
		return fmt.Errorf("verify retired source containers: %w", err)
	}
	for _, container := range containers {
		if container.State.Running {
			return fmt.Errorf("source container %s is still running after retirement", container.ID)
		}
		if normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name) != "no" {
			return fmt.Errorf("source container %s restart policy is not fenced after retirement", container.ID)
		}
	}
	return nil
}

func inspectSourceCommitTargets(ctx context.Context, runner dockerRunner, refs []commitTargetRef) ([]dockerContainer, error) {
	labels := make([]string, 0, len(refs))
	for _, ref := range refs {
		labels = append(labels, ref.label())
	}
	inspected, err := inspectContainers(ctx, runner, labels)
	if err != nil {
		inspected = nil
		for _, ref := range refs {
			container, inspectErr := sourceContainer(ctx, runner, ref.id, ref.name)
			if inspectErr != nil {
				return nil, fmt.Errorf("inspect source container %s: %w", ref.label(), inspectErr)
			}
			inspected = append(inspected, container)
		}
		return inspected, nil
	}
	ordered := make([]dockerContainer, 0, len(refs))
	for _, ref := range refs {
		found := false
		for _, container := range inspected {
			matches := ref.id != "" && sourceContainerIDMatches(ref.id, container.ID)
			if ref.id == "" {
				matches = strings.TrimPrefix(ref.name, "/") == strings.TrimPrefix(container.Name, "/")
			}
			if !matches {
				continue
			}
			if ref.name != "" && strings.TrimPrefix(ref.name, "/") != strings.TrimPrefix(container.Name, "/") {
				return nil, fmt.Errorf("source container %s has name %q, want reviewed name %q", ref.id, container.Name, ref.name)
			}
			ordered = append(ordered, container)
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("source container %s was absent from batch inspection", ref.label())
		}
	}
	return ordered, nil
}

func normalizedRestartPolicyName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "no"
	}
	return name
}

func updateContainerRestartPolicy(ctx context.Context, runner dockerRunner, containerID, policy string) error {
	name := normalizedRestartPolicyName(policy)
	_, err := runner.Output(ctx, "update", "--restart="+name, containerID)
	return err
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
// because the migration is over.
func sourceCommitTargets(app preparer.AppPlan) []commitTargetRef {
	seenID := map[string]struct{}{}
	seenName := map[string]struct{}{}
	refs := []commitTargetRef{}
	for _, ref := range sourceCommitTargetRefs(app) {
		id, name := ref.id, ref.name
		if id != "" {
			if _, dup := seenID[id]; dup {
				continue
			}
			seenID[id] = struct{}{}
		} else {
			if _, dup := seenName[name]; dup {
				continue
			}
			seenName[name] = struct{}{}
		}
		refs = append(refs, ref)
	}
	return refs
}

func SourceRetirementContainers(plan Plan) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	add := func(ref string) {
		if _, dup := seen[ref]; ref != "" && !dup {
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}
	for _, step := range plan.Steps {
		if shouldSkipApplyStep(plan, step) {
			continue
		}
		switch step.Kind {
		case StepStopSourceApp:
			if app, ok := findPrepareApp(plan.Prepare, step.App); ok {
				for _, ref := range sourceCommitTargets(app) {
					add(ref.label())
				}
			}
		case StepStopCoolifyProxy:
			add(coolifyProxyContainer)
		}
	}
	return refs
}

func sourceCommitTargetRefs(app preparer.AppPlan) []commitTargetRef {
	refs := []commitTargetRef{}
	add := func(id, name string) {
		if id != "" || name != "" {
			refs = append(refs, commitTargetRef{id: id, name: name})
		}
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
	return sourceContainer(ctx, runner, ref.id, ref.name)
}

func VerifySourceContainers(ctx context.Context, apps []preparer.AppPlan) error {
	return verifySourceContainers(ctx, localDockerRunner{}, apps)
}

type sourceContainerAttestation struct {
	app string
	ref commitTargetRef
}

func collectSourceContainerAttestations(apps []preparer.AppPlan) ([]sourceContainerAttestation, error) {
	seen := map[string]sourceContainerAttestation{}
	attestations := []sourceContainerAttestation{}

	for _, app := range apps {
		if isPlatformAppRole(app.Role) {
			continue
		}
		refs := sourceCommitTargetRefs(app)
		if len(refs) == 0 {
			return nil, fmt.Errorf("app %s has no reviewed source container identities; refresh the run from a local Docker scan before live work", app.Name)
		}
		for _, ref := range refs {
			if ref.id == "" {
				return nil, fmt.Errorf("source service %s for app %s has no stable container ID; refresh the run from a local Docker scan before live work", ref.label(), app.Name)
			}
			if ref.name == "" {
				return nil, fmt.Errorf("source container %s for app %s has no reviewed container name; refresh the run from a local Docker scan before live work", ref.id, app.Name)
			}
			attestation := sourceContainerAttestation{app: app.Name, ref: ref}
			if existing, ok := seen[ref.id]; ok {
				if strings.TrimPrefix(existing.ref.name, "/") != strings.TrimPrefix(ref.name, "/") {
					return nil, fmt.Errorf("reviewed source container %s has conflicting names %q and %q", ref.id, existing.ref.name, ref.name)
				}
				continue
			}
			seen[ref.id] = attestation
			attestations = append(attestations, attestation)
		}
	}
	if len(attestations) == 0 {
		return nil, fmt.Errorf("migration run has no stable source container identities; refresh it from a local Docker scan before live work")
	}
	return attestations, nil
}

func ValidateSourceContainerAttestations(apps []preparer.AppPlan) error {
	_, err := collectSourceContainerAttestations(apps)
	return err
}

func verifySourceContainers(ctx context.Context, runner dockerRunner, apps []preparer.AppPlan) error {
	attestations, err := collectSourceContainerAttestations(apps)
	if err != nil {
		return err
	}
	const batchSize = 100
	for start := 0; start < len(attestations); start += batchSize {
		end := min(start+batchSize, len(attestations))
		if err := verifySourceContainerAttestationBatch(ctx, runner, attestations[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func verifySourceContainerAttestationBatch(ctx context.Context, runner dockerRunner, attestations []sourceContainerAttestation) error {
	if len(attestations) == 1 {
		attestation := attestations[0]
		if _, err := sourceContainer(ctx, runner, attestation.ref.id, attestation.ref.name); err != nil {
			return fmt.Errorf("verify reviewed source container %s for app %s: %w", attestation.ref.label(), attestation.app, err)
		}
		return nil
	}
	ids := make([]string, 0, len(attestations))
	for _, attestation := range attestations {
		ids = append(ids, attestation.ref.id)
	}
	containers, err := inspectContainers(ctx, runner, ids)
	if err == nil {
		return validateSourceContainerAttestationBatch(attestations, containers)
	}
	middle := len(attestations) / 2
	if err := verifySourceContainerAttestationBatch(ctx, runner, attestations[:middle]); err != nil {
		return err
	}
	return verifySourceContainerAttestationBatch(ctx, runner, attestations[middle:])
}

func validateSourceContainerAttestationBatch(attestations []sourceContainerAttestation, containers []dockerContainer) error {
	for _, attestation := range attestations {
		matched := false
		for _, container := range containers {
			if !sourceContainerIDMatches(attestation.ref.id, container.ID) {
				continue
			}
			if attestation.ref.name != "" && strings.TrimPrefix(container.Name, "/") != strings.TrimPrefix(attestation.ref.name, "/") {
				return fmt.Errorf("verify reviewed source container %s for app %s: source container has name %q, want reviewed name %q", attestation.ref.label(), attestation.app, container.Name, attestation.ref.name)
			}
			matched = true
			break
		}
		if !matched {
			return fmt.Errorf("verify reviewed source container %s for app %s: reviewed ID was absent from batch inspection", attestation.ref.label(), attestation.app)
		}
	}
	return nil
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
	// captured in older bundles.
	for _, volume := range app.Resources.Volumes {
		add(volume.Service, volume.SourceContainerID, volume.SourceContainerName)
	}
	return refs
}
