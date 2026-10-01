package dokploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
	"gopkg.in/yaml.v3"
)

const (
	stagingVolumeRunLabel     = "bort.run"
	stagingVolumeRunIDLabel   = "bort.run-id"
	stagingVolumeAppLabel     = "bort.app"
	stagingVolumeServiceLabel = "bort.service"
	stagingVolumeTargetLabel  = "bort.target"
	stagingVolumePinLabel     = "bort.staging-pin"
)

// stagingOwner identifies the run that owns staging volumes. run names
// repeat across directories and machines, so the CLI supplies a digest
// of the run's durable identity; the name alone is the fallback.
func stagingOwner(plan Plan) string {
	if id := strings.TrimSpace(plan.RunID); id != "" {
		return id
	}
	return plan.RunName
}

type stagedVolume struct {
	Service    string
	Target     string
	VolumeName string
	Source     preparer.VolumeResource
}

// appStateIsStaged reports whether the app's persistent state is
// transferred before its Dokploy deployment exists. in that order the
// target never runs while Bort copies data, so no target writer pause is
// needed and the deployed compose must mount the staging volumes.
func appStateIsStaged(plan Plan, appName string) bool {
	pushIndex, lastStateIndex := -1, -1
	for index, step := range plan.Steps {
		if step.App != appName {
			continue
		}
		switch step.Kind {
		case StepPushImage:
			pushIndex = index
		case StepPauseSource, StepDumpDataStore, StepRestoreDataStore, StepSyncVolume:
			lastStateIndex = index
		}
	}
	return lastStateIndex >= 0 && pushIndex > lastStateIndex
}

func stagedVolumesForApp(plan Plan, appName string) []stagedVolume {
	if !appStateIsStaged(plan, appName) {
		return nil
	}
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok {
		return nil
	}
	seen := map[string]struct{}{}
	staged := []stagedVolume{}
	add := func(volume preparer.VolumeResource) {
		if volume.Type != "volume" || strings.TrimSpace(volume.Service) == "" || strings.TrimSpace(volume.Target) == "" {
			return
		}
		key := migratedMountKey(volume.Service, volume.Target)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		staged = append(staged, stagedVolume{
			Service:    volume.Service,
			Target:     volume.Target,
			VolumeName: stagingVolumeName(plan, appName, volume.Service, volume.Target),
			Source:     volume,
		})
	}
	for _, step := range plan.Steps {
		if step.App != appName {
			continue
		}
		switch step.Kind {
		case StepSyncVolume:
			if volume, ok := findPrepareVolume(app, step.Ref); ok {
				add(volume)
			}
		case StepRestoreDataStore:
			store, ok := findPrepareDataStore(app, step.Ref)
			if !ok {
				continue
			}
			for _, volume := range app.Resources.Volumes {
				if volume.Service == store.Service {
					add(volume)
				}
			}
		}
	}
	return staged
}

func stagedVolumesForService(plan Plan, appName, service string) []stagedVolume {
	volumes := []stagedVolume{}
	for _, volume := range stagedVolumesForApp(plan, appName) {
		if volume.Service == service {
			volumes = append(volumes, volume)
		}
	}
	return volumes
}

func stagedVolumeFor(plan Plan, appName string, volume preparer.VolumeResource) (stagedVolume, bool) {
	for _, staged := range stagedVolumesForApp(plan, appName) {
		if staged.Service == volume.Service && staged.Target == volume.Target {
			return staged, true
		}
	}
	return stagedVolume{}, false
}

func stagingVolumeName(plan Plan, appName, service, target string) string {
	return strings.Join([]string{
		"bort",
		dockerNameSegment(plan.RunName, 24),
		dockerNameSegment(appName, 24),
		dockerNameSegment(service, 24),
		stagingHash(stagingOwner(plan), appName, service, target),
	}, "-")
}

func stagingProjectName(plan Plan, appName, service string) string {
	return "bort-stage-" + dockerNameSegment(appName, 24) + "-" + dockerNameSegment(service, 24) + "-" + stagingHash(stagingOwner(plan), appName, service)
}

func stagingHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:8]
}

func dockerNameSegment(value string, limit int) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
		if b.Len() >= limit {
			break
		}
	}
	segment := strings.Trim(b.String(), "-")
	if segment == "" {
		return "x"
	}
	return segment
}

func stagingVolumeLabels(plan Plan, appName string, volume stagedVolume) []string {
	return []string{
		stagingVolumeRunLabel + "=" + plan.RunName,
		stagingVolumeRunIDLabel + "=" + stagingOwner(plan),
		stagingVolumeAppLabel + "=" + appName,
		stagingVolumeServiceLabel + "=" + volume.Service,
		stagingVolumeTargetLabel + "=" + volume.Target,
	}
}

func stagingVolumeExists(ctx context.Context, runner dockerRunner, plan Plan, appName string, volume stagedVolume) (bool, error) {
	out, err := runner.Output(ctx, "volume", "inspect", volume.VolumeName)
	if err != nil {
		if isDockerResourceMissingErr(err, "volume", volume.VolumeName) {
			return false, nil
		}
		return false, fmt.Errorf("inspect staging volume %s: %w", volume.VolumeName, err)
	}
	return true, validateStagingVolumeOwnership(plan, appName, []stagedVolume{volume}, out)
}

type stagingVolumeState struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

func requireStagingVolumeOwned(ctx context.Context, runner dockerRunner, plan Plan, appName string, volume stagedVolume) error {
	return requireStagingVolumesOwned(ctx, runner, plan, appName, []stagedVolume{volume})
}

func requireStagingVolumesOwned(ctx context.Context, runner dockerRunner, plan Plan, appName string, volumes []stagedVolume) error {
	if len(volumes) == 0 {
		return nil
	}
	args := []string{"volume", "inspect"}
	for _, volume := range volumes {
		args = append(args, volume.VolumeName)
	}
	out, err := runner.Output(ctx, args...)
	if err != nil {
		return fmt.Errorf("inspect staging volume ownership: %w", err)
	}
	return validateStagingVolumeOwnership(plan, appName, volumes, out)
}

func validateStagingVolumeOwnership(plan Plan, appName string, volumes []stagedVolume, out []byte) error {
	expected := make(map[string]stagedVolume, len(volumes))
	for _, volume := range volumes {
		expected[volume.VolumeName] = volume
	}
	var states []stagingVolumeState
	if err := json.Unmarshal(out, &states); err != nil {
		return fmt.Errorf("decode staging volume ownership: %w", err)
	}
	if len(states) != len(expected) {
		return fmt.Errorf("docker volume inspect returned %d resources, want %d", len(states), len(expected))
	}
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		name := strings.TrimSpace(state.Name)
		volume, ok := expected[name]
		if !ok {
			return fmt.Errorf("docker volume inspect returned unexpected resource %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("docker volume inspect returned duplicate resource %q", name)
		}
		seen[name] = struct{}{}
		for _, expectedLabel := range stagingVolumeLabels(plan, appName, volume) {
			key, value, _ := strings.Cut(expectedLabel, "=")
			if state.Labels[key] != value {
				return fmt.Errorf("docker volume %s is not the staged %s:%s volume for app %s in run %q (label %s=%q, want %q)", name, volume.Service, volume.Target, appName, plan.RunName, key, state.Labels[key], value)
			}
		}
	}
	return nil
}

func ensureStagingVolume(ctx context.Context, runner dockerRunner, plan Plan, appName string, volume stagedVolume) error {
	exists, err := stagingVolumeExists(ctx, runner, plan, appName, volume)
	if err != nil || exists {
		return err
	}
	args := []string{"volume", "create"}
	for _, label := range stagingVolumeLabels(plan, appName, volume) {
		args = append(args, "--label", label)
	}
	args = append(args, volume.VolumeName)
	if _, err := runner.Output(ctx, args...); err != nil {
		return fmt.Errorf("create staging volume %s: %w", volume.VolumeName, err)
	}
	return requireStagingVolumeOwned(ctx, runner, plan, appName, volume)
}

type stagingVolumePin struct {
	name        string
	containerID string
}

func ensureAppStagingVolumePin(ctx context.Context, runner dockerRunner, actx *applyContext, appName string, allowCreate bool) ([]stagedVolume, stagingVolumePin, error) {
	volumes := stagedVolumesForApp(actx.plan, appName)
	if len(volumes) == 0 {
		return nil, stagingVolumePin{}, fmt.Errorf("pin staging volumes for app %s: no volumes supplied", appName)
	}
	if pin, ok := actx.stagingVolumePins[appName]; ok {
		if err := requireStagingVolumesOwned(ctx, runner, actx.plan, appName, volumes); err != nil {
			return nil, stagingVolumePin{}, err
		}
		if err := requireStagingVolumePin(ctx, runner, actx.plan, volumes, pin); err != nil {
			return nil, stagingVolumePin{}, err
		}
		return volumes, pin, nil
	}
	if allowCreate {
		for _, volume := range volumes {
			if err := ensureStagingVolume(ctx, runner, actx.plan, appName, volume); err != nil {
				return nil, stagingVolumePin{}, err
			}
		}
	}
	pin, err := acquireStagingVolumePin(ctx, runner, actx.plan, appName, volumes, allowCreate)
	if pin.containerID != "" {
		if actx.stagingVolumePins == nil {
			actx.stagingVolumePins = map[string]stagingVolumePin{}
		}
		actx.stagingVolumePins[appName] = pin
	}
	if err != nil {
		return nil, stagingVolumePin{}, err
	}
	return volumes, pin, nil
}

func acquireStagingVolumePin(ctx context.Context, runner dockerRunner, plan Plan, appName string, volumes []stagedVolume, allowCreate bool) (stagingVolumePin, error) {
	if len(volumes) == 0 {
		return stagingVolumePin{}, fmt.Errorf("pin staging volumes: no volumes supplied")
	}
	if err := requireStagingVolumesOwned(ctx, runner, plan, appName, volumes); err != nil {
		return stagingVolumePin{}, err
	}
	name := stagingVolumePinName(plan, volumes)
	finish := func(pin stagingVolumePin) (stagingVolumePin, error) {
		if err := requireStagingVolumesOwned(ctx, runner, plan, appName, volumes); err != nil {
			return pin, fmt.Errorf("verify staging volume ownership after pinning with %s: %w", pin.name, err)
		}
		if err := removeSupersededStagingVolumePins(ctx, runner, plan, appName, volumes, pin); err != nil {
			return pin, err
		}
		return pin, nil
	}
	existing, err := reconcileStagingVolumePins(ctx, runner, plan, appName, volumes, name)
	if err != nil {
		return stagingVolumePin{}, err
	}
	if existing.containerID != "" {
		return finish(existing)
	}
	if !allowCreate {
		return stagingVolumePin{}, unsafeSourceResumeError{err: authorityRecoveryRequiredError{err: fmt.Errorf("required staging volume pin %s is missing after state transfer; refusing to recreate it because the volume identity was unprotected; paused source applications remain stopped; follow `%s` to recover source or target authority", name, recoveryStatusCommand(plan))}}
	}
	args := []string{
		"run", "-d", "--name", name, "--network", "none", "--read-only", "--restart", "unless-stopped",
		"--label", stagingVolumePinLabel + "=true",
		"--label", stagingVolumeRunIDLabel + "=" + stagingOwner(plan),
		"--label", stagingVolumeAppLabel + "=" + appName,
	}
	for index, volume := range volumes {
		args = append(args, "-v", volume.VolumeName+":/bort-volume/"+strconv.Itoa(index)+":ro")
	}
	args = append(args, volumeCopyImage, "sh", "-c", "while :; do sleep 2147483647; done")
	out, err := runner.Output(ctx, args...)
	if err != nil {
		recovered, reconcileErr := reconcileStagingVolumePinsWithTimeout(runner, plan, appName, volumes, name)
		if reconcileErr == nil && recovered.containerID != "" {
			return finish(recovered)
		}
		return stagingVolumePin{}, errors.Join(fmt.Errorf("pin staging volumes: %w", err), reconcileErr)
	}
	pin := stagingVolumePin{name: name, containerID: strings.TrimSpace(string(out))}
	if pin.containerID == "" {
		recovered, reconcileErr := reconcileStagingVolumePinsWithTimeout(runner, plan, appName, volumes, name)
		if reconcileErr == nil && recovered.containerID != "" {
			return finish(recovered)
		}
		return stagingVolumePin{}, errors.Join(fmt.Errorf("pin staging volumes: docker returned an empty container ID"), reconcileErr)
	}
	if err := requireStagingVolumePin(ctx, runner, plan, volumes, pin); err != nil {
		return stagingVolumePin{}, err
	}
	return finish(pin)
}

func stagingVolumePinName(plan Plan, volumes []stagedVolume) string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.VolumeName)
	}
	slices.Sort(names)
	return "bort-pin-" + dockerNameSegment(plan.RunName, 24) + "-" + stagingHash(stagingOwner(plan), strings.Join(names, "\x00"))
}

func releaseStagingVolumePin(ctx context.Context, runner dockerRunner, plan Plan, volumes []stagedVolume, pin stagingVolumePin) error {
	ctx, cancel := context.WithTimeout(ctx, dockerStopTimeout)
	defer cancel()
	if _, err := runner.Output(ctx, "rm", "-f", pin.containerID); err != nil {
		remaining, inspectErr := findStagingVolumePin(ctx, runner, plan, volumes, pin.name)
		if inspectErr == nil && remaining.containerID == "" {
			return nil
		}
		return errors.Join(fmt.Errorf("release staging volume pin %s: %w", pin.name, err), inspectErr)
	}
	return nil
}

func validateCachedStagingVolumePin(ctx context.Context, runner dockerRunner, actx *applyContext, appName string) error {
	pin, ok := actx.stagingVolumePins[appName]
	if !ok {
		if !stagingTransferStarted(actx.entry(appName)) {
			return nil
		}
		_, reconciled, err := ensureAppStagingVolumePin(ctx, runner, actx, appName, false)
		if err != nil {
			return err
		}
		pin = reconciled
	}
	volumes := stagedVolumesForApp(actx.plan, appName)
	if err := requireStagingVolumesOwned(ctx, runner, actx.plan, appName, volumes); err != nil {
		return err
	}
	if err := requireStagingVolumePin(ctx, runner, actx.plan, volumes, pin); err != nil {
		return err
	}
	return requireStagingVolumeAttachments(ctx, runner, volumes, []string{pin.containerID})
}

type releasableStagingPin struct {
	pin     stagingVolumePin
	volumes []stagedVolume
}

func (c *Client) ValidateStagingVolumePins(ctx context.Context, plan Plan, targetAuthority bool) error {
	ctx, cancel := context.WithTimeout(ctx, targetDiscoveryTimeout)
	defer cancel()
	_, err := c.validatedStagingVolumePins(ctx, plan, targetAuthority, true)
	return err
}

func (c *Client) ReleaseStagingVolumePins(ctx context.Context, plan Plan, targetAuthority bool) error {
	ctx, cancel := context.WithTimeout(ctx, targetDiscoveryTimeout)
	defer cancel()
	pins, err := c.validatedStagingVolumePins(ctx, plan, targetAuthority, false)
	if err != nil {
		return err
	}
	for _, item := range pins {
		if err := releaseStagingVolumePin(ctx, c.dockerRunner(), plan, item.volumes, item.pin); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) validatedStagingVolumePins(ctx context.Context, plan Plan, targetAuthority, requireHandoff bool) ([]releasableStagingPin, error) {
	runner := c.dockerRunner()
	containers, err := listContainersByLabels(ctx, runner, stagingVolumePinLabel+"=true", stagingVolumeRunIDLabel+"="+stagingOwner(plan))
	if err != nil {
		return nil, fmt.Errorf("find staging volume pins for recovery: %w", err)
	}

	type expectedPin struct {
		app     string
		volumes []stagedVolume
	}
	expected := map[string]expectedPin{}
	for _, app := range plan.Prepare.Apps {
		volumes := stagedVolumesForApp(plan, app.Name)
		if len(volumes) == 0 {
			continue
		}
		expected[stagingVolumePinName(plan, volumes)] = expectedPin{app: app.Name, volumes: volumes}
	}

	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := actx.loadMigratedVolumeMounts(); err != nil {
		return nil, err
	}
	for _, app := range plan.StagingTransferApps {
		actx.entry(app).StagingTransferStarted = true
	}
	targetIdentitiesVerified := false

	discovered := map[string]stagingVolumePin{}
	for _, container := range containers {
		want, ok := expected[container.Name]
		if !ok {
			return nil, fmt.Errorf("run %q owns unexpected staging volume pin %s; refusing to remove an unverified container", plan.RunName, container.Name)
		}
		if _, duplicate := discovered[container.Name]; duplicate {
			return nil, fmt.Errorf("run %q has more than one staging volume pin named %s", plan.RunName, container.Name)
		}
		pin := stagingVolumePin{name: container.Name, containerID: container.ID}
		if err := validateStagingVolumePinIdentity(plan, want.volumes, pin, container); err != nil {
			return nil, err
		}
		discovered[container.Name] = pin
	}

	pins := make([]releasableStagingPin, 0, len(discovered))
	for name, want := range expected {
		entry := actx.entry(want.app)
		pin, hasPin := discovered[name]
		if !hasPin && !stagingTransferStarted(entry) {
			continue
		}
		if targetAuthority && !stagedVolumesRecorded(entry, want.volumes) {
			return nil, fmt.Errorf("run %q has no complete durable migrated-volume record for app %s; refusing target-authority finalization", plan.RunName, want.app)
		}
		if targetAuthority && !hasPin && (!requireHandoff || slices.Contains(plan.HandedOffApps, want.app)) {
			continue
		}
		if targetAuthority && !targetIdentitiesVerified {
			if err := c.hydratePersistedTargetIdentities(ctx, actx); err != nil {
				return nil, fmt.Errorf("verify persisted target identities before authority finalization: %w", err)
			}
			targetIdentitiesVerified = true
		}
		volumes := want.volumes
		if !targetAuthority && !hasPin {
			volumes = nil
			for _, volume := range want.volumes {
				exists, err := stagingVolumeExists(ctx, runner, plan, want.app, volume)
				if err != nil {
					return nil, fmt.Errorf("verify staging volume %s for app %s before source-authority finalization: %w", volume.VolumeName, want.app, err)
				}
				if exists {
					volumes = append(volumes, volume)
				}
			}
			if len(volumes) == 0 {
				continue
			}
		}
		if err := requireStagingVolumesOwned(ctx, runner, plan, want.app, volumes); err != nil {
			return nil, fmt.Errorf("verify staging volumes for app %s before authority finalization: %w", want.app, err)
		}
		allowedIDsByVolume := make(map[string][]string, len(volumes))
		for _, volume := range volumes {
			if hasPin {
				allowedIDsByVolume[volume.VolumeName] = []string{pin.containerID}
			} else {
				allowedIDsByVolume[volume.VolumeName] = []string{}
			}
		}
		if targetAuthority {
			targetIDsByVolume, err := c.migratedVolumeAttachmentIDs(ctx, actx, want.app)
			if err != nil {
				return nil, fmt.Errorf("verify target attachments for app %s before authority finalization: %w", want.app, err)
			}
			for _, volume := range want.volumes {
				targetIDs := targetIDsByVolume[volume.VolumeName]
				if len(targetIDs) == 0 {
					return nil, fmt.Errorf("verify target attachments for app %s before authority finalization: migrated volume %s has no target container", want.app, volume.VolumeName)
				}
				allowedIDsByVolume[volume.VolumeName] = append(allowedIDsByVolume[volume.VolumeName], targetIDs...)
			}
		}
		if err := requireStagingVolumeAttachmentSets(ctx, runner, volumes, allowedIDsByVolume); err != nil {
			if !targetAuthority {
				return nil, fmt.Errorf("verify attachments for app %s before source-authority finalization: %w; remove every container attached to these staging volumes other than a bort-pin-* container (stopping is not enough because a stopped container keeps its mounts), then rerun this command", want.app, err)
			}
			return nil, fmt.Errorf("verify attachments for app %s before authority finalization: %w", want.app, err)
		}
		if hasPin {
			pins = append(pins, releasableStagingPin{pin: pin, volumes: want.volumes})
		}
	}

	return pins, nil
}

func PlanStagesVolumes(plan Plan) bool {
	for _, app := range plan.Prepare.Apps {
		if len(stagedVolumesForApp(plan, app.Name)) > 0 {
			return true
		}
	}
	return false
}

func IncompleteStagingTransfers(plan Plan) ([]string, error) {
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := actx.loadMigratedVolumeMounts(); err != nil {
		return nil, err
	}
	for _, app := range plan.StagingTransferApps {
		actx.entry(app).StagingTransferStarted = true
	}
	incomplete := []string{}
	for _, app := range plan.Prepare.Apps {
		volumes := stagedVolumesForApp(plan, app.Name)
		entry := actx.entry(app.Name)
		if len(volumes) > 0 && stagingTransferStarted(entry) && !stagedVolumesRecorded(entry, volumes) {
			incomplete = append(incomplete, app.Name)
		}
	}
	return incomplete, nil
}

func reconcileStagingVolumePinsWithTimeout(runner dockerRunner, plan Plan, appName string, volumes []stagedVolume, expectedName string) (stagingVolumePin, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerStopTimeout)
	defer cancel()
	return reconcileStagingVolumePins(ctx, runner, plan, appName, volumes, expectedName)
}

func reconcileStagingVolumePins(ctx context.Context, runner dockerRunner, plan Plan, appName string, volumes []stagedVolume, expectedName string) (stagingVolumePin, error) {
	containers, err := listContainersByLabels(ctx, runner, stagingVolumePinLabel+"=true", stagingVolumeRunIDLabel+"="+stagingOwner(plan), stagingVolumeAppLabel+"="+appName)
	if err != nil {
		return stagingVolumePin{}, fmt.Errorf("find staging volume pins: %w", err)
	}
	for _, container := range containers {
		if container.Name != expectedName {
			continue
		}
		pin := stagingVolumePin{name: container.Name, containerID: container.ID}
		if err := validateStagingVolumePinIdentity(plan, volumes, pin, container); err != nil {
			return stagingVolumePin{}, err
		}
		if container.State.Running {
			return pin, nil
		}
		_, startErr := runner.Output(ctx, "start", container.ID)
		started, inspectErr := inspectContainer(ctx, runner, container.ID)
		if inspectErr == nil {
			if validateErr := validateStagingVolumePin(plan, volumes, pin, started); validateErr == nil {
				return pin, nil
			} else {
				inspectErr = validateErr
			}
		}
		if startErr != nil {
			startErr = fmt.Errorf("restart stopped staging volume pin %s: %w", container.Name, startErr)
		}
		return stagingVolumePin{}, errors.Join(startErr, inspectErr)
	}
	return stagingVolumePin{}, nil
}

func removeSupersededStagingVolumePins(ctx context.Context, runner dockerRunner, plan Plan, appName string, volumes []stagedVolume, current stagingVolumePin) error {
	containers, err := listContainersByLabels(ctx, runner, stagingVolumePinLabel+"=true", stagingVolumeRunIDLabel+"="+stagingOwner(plan), stagingVolumeAppLabel+"="+appName)
	if err != nil {
		return fmt.Errorf("find superseded staging volume pins: %w", err)
	}
	wanted := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		wanted[volume.VolumeName] = struct{}{}
	}
	for _, container := range containers {
		if stagingContainerIDsMatch(container.ID, current.containerID) || !containerMountsAnyVolume(container, wanted) {
			continue
		}
		if _, err := runner.Output(ctx, "rm", "-f", container.ID); err != nil {
			return fmt.Errorf("remove superseded staging volume pin %s: %w", container.Name, err)
		}
	}
	return nil
}

func findStagingVolumePin(ctx context.Context, runner dockerRunner, plan Plan, volumes []stagedVolume, expectedName string) (stagingVolumePin, error) {
	containers, err := listContainersByLabels(ctx, runner, stagingVolumePinLabel+"=true", stagingVolumeRunIDLabel+"="+stagingOwner(plan))
	if err != nil {
		return stagingVolumePin{}, err
	}
	for _, container := range containers {
		if container.Name != expectedName || strings.TrimSpace(container.Config.Labels[stagingVolumeRunIDLabel]) != stagingOwner(plan) {
			continue
		}
		pin := stagingVolumePin{name: container.Name, containerID: container.ID}
		if err := validateStagingVolumePin(plan, volumes, pin, container); err != nil {
			return stagingVolumePin{}, err
		}
		return pin, nil
	}
	return stagingVolumePin{}, nil
}

func requireStagingVolumePin(ctx context.Context, runner dockerRunner, plan Plan, volumes []stagedVolume, pin stagingVolumePin) error {
	container, err := inspectContainer(ctx, runner, pin.containerID)
	if err != nil {
		return err
	}
	return validateStagingVolumePin(plan, volumes, pin, container)
}

func validateStagingVolumePin(plan Plan, volumes []stagedVolume, pin stagingVolumePin, container dockerContainer) error {
	if err := validateStagingVolumePinIdentity(plan, volumes, pin, container); err != nil {
		return err
	}
	if !container.State.Running {
		return fmt.Errorf("staging volume pin %s is not running", pin.name)
	}
	return nil
}

func validateStagingVolumePinIdentity(plan Plan, volumes []stagedVolume, pin stagingVolumePin, container dockerContainer) error {
	if container.ID == "" || !stagingContainerIDsMatch(pin.containerID, container.ID) || container.Name != pin.name || container.Name != stagingVolumePinName(plan, volumes) {
		return fmt.Errorf("staging volume pin %s resolved to an unexpected container", pin.name)
	}
	if container.Config.Labels[stagingVolumePinLabel] != "true" || strings.TrimSpace(container.Config.Labels[stagingVolumeRunIDLabel]) != stagingOwner(plan) {
		return fmt.Errorf("staging volume pin %s is not owned by run %q", pin.name, plan.RunName)
	}
	if normalizedRestartPolicyName(container.HostConfig.RestartPolicy.Name) != "unless-stopped" {
		return fmt.Errorf("staging volume pin %s does not have restart policy unless-stopped", pin.name)
	}
	wanted := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		wanted[volume.VolumeName] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, mount := range container.Mounts {
		if mount.Type != "volume" {
			continue
		}
		if _, ok := wanted[mount.Name]; !ok || mount.RW {
			return fmt.Errorf("staging volume pin %s has unexpected or writable volume mount %s", pin.name, mount.Name)
		}
		seen[mount.Name] = struct{}{}
	}
	if len(seen) != len(wanted) {
		return fmt.Errorf("staging volume pin %s mounts %d owned volumes, want %d", pin.name, len(seen), len(wanted))
	}
	return nil
}

func containerMountsAnyVolume(container dockerContainer, names map[string]struct{}) bool {
	for _, mount := range container.Mounts {
		if mount.Type == "volume" {
			if _, ok := names[mount.Name]; ok {
				return true
			}
		}
	}
	return false
}

func clearStagingVolume(ctx context.Context, runner dockerRunner, plan Plan, appName string, volume stagedVolume) error {
	if err := requireStagingVolumeOwned(ctx, runner, plan, appName, volume); err != nil {
		return err
	}
	if err := runner.Run(ctx, nil, nil,
		"run", "--rm", "--network", "none",
		"-v", volume.VolumeName+":/volume",
		volumeCopyImage,
		"sh", "-c", "find /volume -mindepth 1 -delete && sync",
	); err != nil {
		return fmt.Errorf("clear staging volume %s: %w", volume.VolumeName, err)
	}
	return nil
}

func stagingVolumeAttachments(ctx context.Context, runner dockerRunner, volume stagedVolume) ([]string, error) {
	args := []string{"ps", "-a", "--filter", "volume=" + volume.VolumeName, "--format", "{{.ID}}"}
	out, err := runner.Output(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list containers using staging volume %s: %w", volume.VolumeName, err)
	}
	return strings.Fields(string(out)), nil
}

func requireStagingVolumeAttachments(ctx context.Context, runner dockerRunner, volumes []stagedVolume, allowedIDs []string) error {
	allowedByVolume := make(map[string][]string, len(volumes))
	for _, volume := range volumes {
		allowedByVolume[volume.VolumeName] = allowedIDs
	}
	return requireStagingVolumeAttachmentSets(ctx, runner, volumes, allowedByVolume)
}

func requireStagingVolumeAttachmentSets(ctx context.Context, runner dockerRunner, volumes []stagedVolume, allowedByVolume map[string][]string) error {
	attachedByVolume, err := stagingVolumeAttachmentSets(ctx, runner, volumes)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		allowedIDs, ok := allowedByVolume[volume.VolumeName]
		if !ok {
			return fmt.Errorf("staging volume %s has no expected attachment set; refusing an incomplete handoff", volume.VolumeName)
		}
		if err := requireExactStagingVolumeAttachments(volume.VolumeName, attachedByVolume[volume.VolumeName], allowedIDs); err != nil {
			return err
		}
	}
	return nil
}

func stagingVolumeAttachmentSets(ctx context.Context, runner dockerRunner, volumes []stagedVolume) (map[string][]string, error) {
	attachedByVolume := make(map[string][]string, len(volumes))
	if len(volumes) == 0 {
		return attachedByVolume, nil
	}
	if len(volumes) == 1 {
		ids, err := stagingVolumeAttachments(ctx, runner, volumes[0])
		if err != nil {
			return nil, err
		}
		attachedByVolume[volumes[0].VolumeName] = ids
		return attachedByVolume, nil
	}
	wanted := make(map[string]struct{}, len(volumes))
	args := []string{"ps", "-a"}
	for _, volume := range volumes {
		wanted[volume.VolumeName] = struct{}{}
		attachedByVolume[volume.VolumeName] = nil
		args = append(args, "--filter", "volume="+volume.VolumeName)
	}
	args = append(args, "--format", "{{.ID}}")
	out, err := runner.Output(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list containers using staging volumes: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return attachedByVolume, nil
	}
	containers, err := inspectContainers(ctx, runner, ids)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]map[string]struct{}, len(volumes))
	for _, container := range containers {
		for _, mount := range container.Mounts {
			if mount.Type != "volume" {
				continue
			}
			if _, ok := wanted[mount.Name]; !ok {
				continue
			}
			if seen[mount.Name] == nil {
				seen[mount.Name] = map[string]struct{}{}
			}
			if _, duplicate := seen[mount.Name][container.ID]; duplicate {
				continue
			}
			seen[mount.Name][container.ID] = struct{}{}
			attachedByVolume[mount.Name] = append(attachedByVolume[mount.Name], container.ID)
		}
	}
	for volumeName := range attachedByVolume {
		slices.Sort(attachedByVolume[volumeName])
	}
	return attachedByVolume, nil
}

func requireExactStagingVolumeAttachments(volumeName string, ids, allowedIDs []string) error {
	for _, attachedID := range ids {
		allowed := false
		for _, id := range allowedIDs {
			if stagingContainerIDsMatch(id, attachedID) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("staging volume %s is attached to unexpected container %s; refusing a handoff with an unverified writer", volumeName, attachedID)
		}
	}
	for _, allowedID := range allowedIDs {
		attached := false
		for _, id := range ids {
			if stagingContainerIDsMatch(allowedID, id) {
				attached = true
				break
			}
		}
		if !attached {
			return fmt.Errorf("expected staging volume %s attachment to container %s is missing; refusing an incomplete handoff", volumeName, allowedID)
		}
	}
	return nil
}

func stagingContainerIDsMatch(first, second string) bool {
	return sourceContainerIDMatches(first, second) || sourceContainerIDMatches(second, first)
}

func recoveryStatusCommand(plan Plan) string {
	if command := strings.TrimSpace(plan.RecoveryCommand); command != "" {
		return command
	}
	return "bort status"
}

const defaultPostgresDataDir = "/var/lib/postgresql/data"

// requireStagedPostgresDataDir refuses a staging restore whose data
// directory is not backed by one of the staged volumes: the restore
// would land in the container layer and vanish with it.
func requireStagedPostgresDataDir(container dockerContainer, staged []stagedVolume) error {
	dataDir := postgresDataDir(container)
	var mount dockerMount
	found := false
	for _, candidate := range container.Mounts {
		if !mountCoversPath(candidate.Destination, dataDir) {
			continue
		}
		if !found || len(candidate.Destination) > len(mount.Destination) {
			mount, found = candidate, true
		}
	}
	if !found {
		return fmt.Errorf("postgres data directory %s is not mounted from a staged volume; the restore would be lost when the staging container stops", dataDir)
	}
	for _, volume := range staged {
		if mount.Type == "volume" && mount.Name == volume.VolumeName {
			return nil
		}
	}
	return fmt.Errorf("postgres data directory %s is mounted from %s %q, not a staged volume; the restore would be lost when the staging container stops", dataDir, mount.Type, firstNonEmpty(mount.Name, mount.Source))
}

// requirePlannedPostgresDataDirStaged refuses at plan time only the
// layouts requireStagedPostgresDataDir is certain to refuse after
// pause_source: a service with nothing to stage, or a literal PGDATA that
// no scanned named volume covers. Interpolated or image-provided PGDATA
// and image VOLUMEs are only known to the created staging container, so
// preflightStagedRestores checks those before the source is paused.
// The mount check reads the source container's mount list, so a mount
// target that compose interpolates could land elsewhere on the target and
// is refused outright.
func requirePlannedPostgresDataDirStaged(composeFile, stagingCompose string, app preparer.AppPlan, service string, staged []stagedVolume) error {
	if len(staged) == 0 {
		return fmt.Errorf("%w: data store service %s for app %s mounts no named volume, so its restore would be lost when the staging container stops; choose a recreate or managed data store strategy or change the source compose before live apply", ErrNotImplemented, service, app.Name)
	}
	if err := requireStagedComposeInputs(composeFile, app.Name, service); err != nil {
		return fmt.Errorf("%w; choose a recreate or managed data store strategy or change the source compose before live apply", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(stagingCompose), &doc); err != nil {
		return err
	}
	root, err := composeRoot(&doc)
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(composeServiceEnvValue(mappingValue(mappingValue(root, "services"), service), "PGDATA"))
	if raw == "" || hasUnescapedComposeDollar(raw) {
		return nil
	}
	if err := requireSourceMountsStageDataDir(app, service, path.Clean(raw), staged); err != nil {
		return fmt.Errorf("%w; choose a recreate or managed data store strategy or change the source compose before live apply", err)
	}
	return nil
}

func requireStagedComposeInputs(composeFile, appName, service string) error {
	entry := resolvedComposeServiceNode(composeFile, service)
	if target, ok := composeServiceInterpolatedMountTarget(entry); ok {
		return fmt.Errorf("%w: data store service %s for app %s mounts %q at an interpolated path, so the postgres data directory cannot be checked", ErrNotImplemented, service, appName, target)
	}
	if input, ok := composeServiceExternalInput(entry); ok {
		return fmt.Errorf("%w: data store service %s for app %s uses Compose %s, which Bort cannot preserve in its isolated staging service", ErrNotImplemented, service, appName, input)
	}
	return nil
}

func resolvedComposeServiceNode(composeFile, service string) *yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(composeFile), &doc); err != nil {
		return nil
	}
	root, err := composeRoot(&doc)
	if err != nil {
		return nil
	}
	entry := mappingValue(mappingValue(root, "services"), service)
	if entry == nil {
		return nil
	}
	if entry, err = selfContainedNode(entry, map[*yaml.Node]bool{}); err != nil {
		return nil
	}
	return entry
}

func composeServiceExternalInput(entry *yaml.Node) (string, bool) {
	for _, key := range []string{"secrets", "configs"} {
		value := mappingValue(entry, key)
		if value == nil {
			continue
		}
		if value.Kind == yaml.SequenceNode || value.Kind == yaml.MappingNode {
			if len(value.Content) > 0 {
				return key, true
			}
			continue
		}
		if strings.TrimSpace(value.Value) != "" && value.Tag != "!!null" {
			return key, true
		}
	}
	return "", false
}

func composeServiceInterpolatedMountTarget(entry *yaml.Node) (string, bool) {
	volumes := mappingValue(entry, "volumes")
	if volumes == nil || volumes.Kind != yaml.SequenceNode {
		return "", false
	}
	for _, item := range volumes.Content {
		var target string
		switch item.Kind {
		case yaml.ScalarNode:
			fields := splitComposeVolumeShortSyntax(item.Value)
			target = fields[0]
			if len(fields) > 1 {
				target = fields[1]
			}
		case yaml.MappingNode:
			if dest := mappingValue(item, "target"); dest != nil {
				target = dest.Value
			}
		}
		if hasUnescapedComposeDollar(target) {
			return strings.TrimSpace(target), true
		}
	}
	return "", false
}

func hasUnescapedComposeDollar(value string) bool {
	return strings.Contains(strings.ReplaceAll(value, "$$", ""), "$")
}

// splitComposeVolumeShortSyntax splits SOURCE:TARGET[:MODE] on colons that
// are not inside a ${...} reference, where ${VAR:-default} keeps its colon.
func splitComposeVolumeShortSyntax(value string) []string {
	fields := []string{}
	depth, start := 0, 0
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] == '$' && i+1 < len(value) && value[i+1] == '{':
			depth++
			i++
		case value[i] == '}' && depth > 0:
			depth--
		case value[i] == ':' && depth == 0:
			fields = append(fields, value[start:i])
			start = i + 1
		}
	}
	return append(fields, value[start:])
}

// requireSourceMountsStageDataDir checks the source service's own mount
// list, which the deployed compose keeps verbatim: a bind at, above, or
// inside the data directory would shadow the restored data once Dokploy
// starts the service, even though the staging container never mounts it.
func requireSourceMountsStageDataDir(app preparer.AppPlan, service, dataDir string, staged []stagedVolume) error {
	var mount preparer.VolumeResource
	found := false
	for _, candidate := range app.Resources.Volumes {
		if candidate.Service != service {
			continue
		}
		if mountCoversPath(candidate.Target, dataDir) {
			if !found || len(path.Clean(candidate.Target)) > len(path.Clean(mount.Target)) {
				mount, found = candidate, true
			}
			continue
		}
		if mountCoversPath(dataDir, candidate.Target) && candidate.ReadWrite && !stagedVolumeMount(candidate, staged) {
			return fmt.Errorf("%w: %s %q at %s for service %s in app %s sits inside postgres data directory %s and would shadow the restored data once Dokploy starts the service", ErrNotImplemented, candidate.Type, firstNonEmpty(candidate.Name, candidate.Source), candidate.Target, service, app.Name, dataDir)
		}
	}
	if !found {
		return fmt.Errorf("%w: postgres data directory %s for service %s in app %s is not mounted from a named volume, so the restore would be lost when the staging container stops", ErrNotImplemented, dataDir, service, app.Name)
	}
	if !stagedVolumeMount(mount, staged) {
		return fmt.Errorf("%w: postgres data directory %s for service %s in app %s is mounted from %s %q at %s, not a named volume, so the restore would be lost when the staging container stops", ErrNotImplemented, dataDir, service, app.Name, mount.Type, firstNonEmpty(mount.Name, mount.Source), mount.Target)
	}
	for _, candidate := range app.Resources.Volumes {
		if candidate.Service != service || candidate.Type != "volume" || path.Clean(candidate.Target) == path.Clean(mount.Target) || mountCoversPath(dataDir, candidate.Target) {
			continue
		}
		return fmt.Errorf("%w: named volume %q at %s for service %s in app %s is outside postgres data directory %s, so a logical restore cannot preserve its contents", ErrNotImplemented, firstNonEmpty(candidate.Name, candidate.Source), candidate.Target, service, app.Name, dataDir)
	}
	return nil
}

func stagedVolumeMount(mount preparer.VolumeResource, staged []stagedVolume) bool {
	if mount.Type != "volume" {
		return false
	}
	for _, volume := range staged {
		if path.Clean(volume.Target) == path.Clean(mount.Target) {
			return true
		}
	}
	return false
}

func postgresDataDir(container dockerContainer) string {
	if dataDir := strings.TrimSpace(envMap(container.Config.Env)["PGDATA"]); dataDir != "" {
		return path.Clean(dataDir)
	}
	return defaultPostgresDataDir
}

func mountCoversPath(destination, target string) bool {
	destination, target = path.Clean(destination), path.Clean(target)
	return destination == target || strings.HasPrefix(target, strings.TrimSuffix(destination, "/")+"/")
}

func composeServiceEnvValue(service *yaml.Node, key string) string {
	environment := mappingValue(service, "environment")
	if environment == nil {
		return ""
	}
	switch environment.Kind {
	case yaml.MappingNode:
		if value := mappingValue(environment, key); value != nil && value.Kind == yaml.ScalarNode && value.Tag != "!!null" {
			return value.Value
		}
	case yaml.SequenceNode:
		for _, item := range environment.Content {
			if name, value, ok := strings.Cut(item.Value, "="); ok && item.Kind == yaml.ScalarNode && strings.TrimSpace(name) == key {
				return value
			}
		}
	}
	return ""
}

func requireSourceQuiescent(containers []dockerContainer) error {
	for _, container := range containers {
		if container.State.Running {
			return fmt.Errorf("source container %s is running while its state is being copied; pause_source must stop it first", container.Name)
		}
	}
	return nil
}

func requireSourceQuiesceUnchanged(before, after []dockerContainer) error {
	if len(before) != len(after) {
		return fmt.Errorf("source container set changed during state copy (%d before, %d after)", len(before), len(after))
	}
	for index := range before {
		b, a := before[index], after[index]
		switch {
		case b.ID != a.ID:
			return fmt.Errorf("source container %s was replaced by %s during state copy", b.Name, a.Name)
		case a.State.Running:
			return fmt.Errorf("source container %s started during state copy; the copied state may be inconsistent", a.Name)
		case b.State.FinishedAt != a.State.FinishedAt || b.State.StartedAt != a.State.StartedAt:
			return fmt.Errorf("source container %s ran during state copy (started %s, finished %s); the copied state may be inconsistent", a.Name, a.State.StartedAt, a.State.FinishedAt)
		}
	}
	return nil
}

func composeServiceVolumeKeys(root *yaml.Node, service string) map[string]string {
	keys := map[string]string{}
	services := mappingValue(root, "services")
	entry := mappingValue(services, service)
	volumes := mappingValue(entry, "volumes")
	if volumes == nil || volumes.Kind != yaml.SequenceNode {
		return keys
	}
	for _, item := range volumes.Content {
		key, target, ok := composeVolumeEntry(item)
		if ok {
			keys[target] = key
		}
	}
	return keys
}

func composeVolumeEntry(item *yaml.Node) (key, target string, ok bool) {
	switch item.Kind {
	case yaml.ScalarNode:
		parts := strings.SplitN(item.Value, ":", 3)
		if len(parts) < 2 {
			return "", "", false
		}
		key, target = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if key == "" || target == "" || strings.ContainsAny(key[:1], "/.~$") {
			return "", "", false
		}
		return key, target, true
	case yaml.MappingNode:
		kind := mappingValue(item, "type")
		if kind == nil || kind.Value != "volume" {
			return "", "", false
		}
		source, dest := mappingValue(item, "source"), mappingValue(item, "target")
		if source == nil || dest == nil || strings.TrimSpace(source.Value) == "" || strings.TrimSpace(dest.Value) == "" {
			return "", "", false
		}
		return strings.TrimSpace(source.Value), strings.TrimSpace(dest.Value), true
	default:
		return "", "", false
	}
}

const composeInitScriptsDir = "/docker-entrypoint-initdb.d"

func composeServiceInitBindMounts(entry *yaml.Node) ([]*yaml.Node, error) {
	volumes := mappingValue(entry, "volumes")
	if volumes == nil || volumes.Kind != yaml.SequenceNode {
		return nil, nil
	}
	mounts := []*yaml.Node{}
	for _, item := range volumes.Content {
		var source, target string
		var options []string
		switch item.Kind {
		case yaml.ScalarNode:
			parts := strings.SplitN(item.Value, ":", 3)
			if len(parts) < 2 {
				continue
			}
			source, target = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			if source == "" || !strings.ContainsAny(source[:1], "/.~$") {
				continue
			}
			if len(parts) == 3 {
				options = strings.Split(parts[2], ",")
			}
		case yaml.MappingNode:
			if kind := mappingValue(item, "type"); kind == nil || kind.Value != "bind" {
				continue
			}
			src, dest := mappingValue(item, "source"), mappingValue(item, "target")
			if src == nil || dest == nil {
				continue
			}
			source, target = strings.TrimSpace(src.Value), strings.TrimSpace(dest.Value)
		default:
			continue
		}
		cleaned := path.Clean(target)
		if cleaned != composeInitScriptsDir && !strings.HasPrefix(cleaned, composeInitScriptsDir+"/") {
			continue
		}
		if !strings.HasPrefix(source, "/") {
			return nil, fmt.Errorf("init script mount %s -> %s must use an absolute host path so the staged restore runs the same scripts as the deploy", source, target)
		}
		if item.Kind == yaml.MappingNode {
			ensureMappingBool(item, "read_only", true)
			mounts = append(mounts, item)
			continue
		}
		mounts = append(mounts, stringNode(source+":"+target+":"+strings.Join(append(withoutAccessMode(options), "ro"), ",")))
	}
	return mounts, nil
}

func withoutAccessMode(options []string) []string {
	kept := options[:0:0]
	for _, option := range options {
		if option := strings.TrimSpace(option); option != "" && option != "ro" && option != "rw" {
			kept = append(kept, option)
		}
	}
	return kept
}

func externalVolumeNode(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		stringNode("name"), stringNode(name),
		stringNode("external"), {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	}}
}

func composeRoot(doc *yaml.Node) (*yaml.Node, error) {
	root := doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("compose file is not a mapping")
	}
	return root, nil
}

// rewriteComposeStagedVolumes points the compose file's top-level volume
// entries at the Bort-owned staging volumes so Dokploy's deploy mounts
// the transferred state instead of creating fresh project volumes.
func rewriteComposeStagedVolumes(composeFile string, staged []stagedVolume) (string, error) {
	if len(staged) == 0 {
		return composeFile, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(composeFile), &doc); err != nil {
		return "", err
	}
	root, err := composeRoot(&doc)
	if err != nil {
		return "", err
	}
	topLevel := mappingValue(root, "volumes")
	if topLevel == nil || topLevel.Kind != yaml.MappingNode {
		topLevel = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingNode(root, "volumes", topLevel)
	}
	assigned := map[string]stagedVolume{}
	for _, volume := range staged {
		key, ok := composeServiceVolumeKeys(root, volume.Service)[volume.Target]
		if !ok {
			return "", fmt.Errorf("compose service %s has no named volume mounted at %s; cannot hand staged volume %s to Dokploy", volume.Service, volume.Target, volume.VolumeName)
		}
		if other, dup := assigned[key]; dup && other.VolumeName != volume.VolumeName {
			return "", fmt.Errorf("compose volume %s is mounted by %s:%s and %s:%s, which would stage as separate volumes %s and %s; a shared named volume cannot be transferred before deploy", key, other.Service, other.Target, volume.Service, volume.Target, other.VolumeName, volume.VolumeName)
		}
		assigned[key] = volume
		setMappingNode(topLevel, key, externalVolumeNode(volume.VolumeName))
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func stagingComposeFile(composeFile, service string, staged []stagedVolume) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(composeFile), &doc); err != nil {
		return "", err
	}
	root, err := composeRoot(&doc)
	if err != nil {
		return "", err
	}
	services := mappingValue(root, "services")
	entry := mappingValue(services, service)
	if entry != nil {
		if entry, err = selfContainedNode(entry, map[*yaml.Node]bool{}); err != nil {
			return "", fmt.Errorf("compose service %s: %w", service, err)
		}
	}
	if entry == nil || entry.Kind != yaml.MappingNode {
		return "", fmt.Errorf("compose service %s not found", service)
	}
	if mappingValue(entry, "extends") != nil {
		return "", fmt.Errorf("compose service %s uses extends, which Bort cannot resolve for staging", service)
	}
	setMappingNode(services, service, entry)
	keys := composeServiceVolumeKeys(root, service)
	mounts := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	topLevel := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, volume := range staged {
		key, ok := keys[volume.Target]
		if !ok {
			return "", fmt.Errorf("compose service %s has no named volume mounted at %s; cannot stage volume %s", service, volume.Target, volume.VolumeName)
		}
		mounts.Content = append(mounts.Content, stringNode(key+":"+volume.Target))
		setMappingNode(topLevel, key, externalVolumeNode(volume.VolumeName))
	}
	initMounts, err := composeServiceInitBindMounts(entry)
	if err != nil {
		return "", fmt.Errorf("compose service %s: %w", service, err)
	}
	mounts.Content = append(mounts.Content, initMounts...)
	for _, key := range []string{"ports", "depends_on", "container_name", "networks", "network_mode", "secrets", "configs", "links", "profiles", "build"} {
		removeMappingKey(entry, key)
	}
	setMappingNode(entry, "volumes", mounts)
	setMappingScalar(entry, "restart", "no")
	root.Content = []*yaml.Node{
		stringNode("services"), {Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{stringNode(service), entry}},
		stringNode("volumes"), topLevel,
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// selfContainedNode copies a subtree with every alias expanded and every
// merge key applied, so it stays valid after the anchors it referenced are
// pruned from the document.
func selfContainedNode(node *yaml.Node, expanding map[*yaml.Node]bool) (*yaml.Node, error) {
	if node.Kind == yaml.AliasNode {
		if node.Alias == nil || expanding[node.Alias] {
			return nil, fmt.Errorf("alias *%s cannot be expanded", node.Value)
		}
		expanding[node.Alias] = true
		defer delete(expanding, node.Alias)
		return selfContainedNode(node.Alias, expanding)
	}
	copied := *node
	copied.Anchor = ""
	copied.Content = nil
	for _, child := range node.Content {
		resolved, err := selfContainedNode(child, expanding)
		if err != nil {
			return nil, err
		}
		copied.Content = append(copied.Content, resolved)
	}
	if copied.Kind == yaml.MappingNode {
		applyMergeKeys(&copied)
	}
	return &copied, nil
}

func applyMergeKeys(mapping *yaml.Node) {
	var content, merged []*yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Tag != "!!merge" {
			content = append(content, key, value)
			continue
		}
		if value.Kind == yaml.SequenceNode {
			merged = append(merged, value.Content...)
		} else {
			merged = append(merged, value)
		}
	}
	mapping.Content = content
	for _, source := range merged {
		for i := 0; i+1 < len(source.Content); i += 2 {
			if mappingValue(mapping, source.Content[i].Value) == nil {
				mapping.Content = append(mapping.Content, source.Content[i], source.Content[i+1])
			}
		}
	}
}

type stagingEnvFormat int

const (
	stagingEnvFormatUnresolved stagingEnvFormat = iota
	stagingEnvFormatRaw
	stagingEnvFormatEscapeEveryDollar
	stagingEnvFormatKeepInterpolation
)

var dokployReleaseVersionPattern = regexp.MustCompile(`^v?(\d{1,6})\.(\d{1,6})\.(\d{1,6})$`)

// stagingEnvFormatForDokployVersion picks the .env writer Dokploy's own
// compose deploy uses at that version: raw KEY=value before v0.30.0, quoted
// with every $ escaped through v0.30.2, quoted with ${VAR} kept from v0.30.3.
func stagingEnvFormatForDokployVersion(version string) (stagingEnvFormat, error) {
	match := dokployReleaseVersionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return stagingEnvFormatUnresolved, fmt.Errorf("Dokploy reported version %q, want vMAJOR.MINOR.PATCH; staged state transfer needs a release version to write the same .env Dokploy will", version)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	switch {
	case major > 0 || minor > 30 || minor == 30 && patch >= 3:
		return stagingEnvFormatKeepInterpolation, nil
	case minor == 30:
		return stagingEnvFormatEscapeEveryDollar, nil
	default:
		return stagingEnvFormatRaw, nil
	}
}

func planStagesDataStoreRestore(plan Plan) bool {
	for _, step := range plan.Steps {
		if step.Kind == StepRestoreDataStore && appStateIsStaged(plan, step.App) {
			return true
		}
	}
	return false
}

// stagingEnvFileContent mirrors the .env Dokploy writes next to a compose
// deployment so interpolation in the staged service resolves identically.
func stagingEnvFileContent(composeAppName, envContent string, format stagingEnvFormat) string {
	content := "APP_NAME=" + composeAppName + "\nCOMPOSE_PROJECT_NAME=" + composeAppName + "\n" + envContent
	if !strings.Contains(content, "DOCKER_CONFIG") {
		content += "\nDOCKER_CONFIG=/root/.docker"
	}
	lines := []string{}
	for _, raw := range strings.Split(content, "\n") {
		key, value, ok := parseDotenvLine(raw)
		if !ok {
			continue
		}
		switch format {
		case stagingEnvFormatRaw:
			lines = append(lines, key+"="+value)
		case stagingEnvFormatEscapeEveryDollar:
			lines = append(lines, key+"=\""+escapeDotenvValue(value, false)+"\"")
		default:
			lines = append(lines, key+"=\""+escapeDotenvValue(value, true)+"\"")
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func parseDotenvLine(raw string) (string, string, bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, value, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !ok || !dotenvKey(key) {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if inner, ok := dotenvQuotedValue(value); ok {
		return key, inner, true
	}
	if comment := strings.IndexByte(value, '#'); comment >= 0 {
		value = value[:comment]
	}
	return key, strings.TrimSpace(value), true
}

func dotenvKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r == '_', r == '.', r == '-', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func dotenvQuotedValue(value string) (string, bool) {
	if len(value) < 2 {
		return "", false
	}
	quote := value[0]
	if quote != '"' && quote != '\'' && quote != '`' {
		return "", false
	}
	end := -1
	for index := 1; index < len(value); index++ {
		if value[index] == '\\' && index+1 < len(value) && value[index+1] == quote {
			index++
			continue
		}
		if value[index] == quote {
			end = index
			break
		}
	}
	if end < 0 {
		return "", false
	}
	if rest := strings.TrimSpace(value[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", false
	}
	inner := value[1:end]
	if quote == '"' {
		inner = strings.ReplaceAll(inner, `\n`, "\n")
		inner = strings.ReplaceAll(inner, `\r`, "\r")
	}
	return inner, true
}

func escapeDotenvValue(value string, keepInterpolation bool) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	var b strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '$' && !(keepInterpolation && dotenvInterpolationAt(value[index:])) {
			b.WriteString(`\$`)
			continue
		}
		b.WriteByte(value[index])
	}
	return b.String()
}

func dotenvInterpolationAt(value string) bool {
	if len(value) < 3 || value[1] != '{' || !dotenvNameStart(value[2]) {
		return false
	}
	index := 3
	for index < len(value) && (dotenvNameStart(value[index]) || value[index] >= '0' && value[index] <= '9') {
		index++
	}
	if index < len(value) && value[index] == '}' {
		return true
	}
	if index < len(value) && value[index] == ':' {
		index++
	}
	if index >= len(value) || !strings.ContainsRune("-+?", rune(value[index])) {
		return false
	}
	for index++; index < len(value); index++ {
		switch value[index] {
		case '}':
			return true
		case '{':
			return false
		}
	}
	return false
}

func dotenvNameStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

type stagingProject struct {
	name        string
	composePath string
	envPath     string
}

func (p stagingProject) args(extra ...string) []string {
	return append([]string{"compose", "-p", p.name, "--env-file", p.envPath, "-f", p.composePath}, extra...)
}

func (p stagingProject) down(runner dockerRunner) error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerStopTimeout+dockerStartTimeout)
	defer cancel()
	if err := runner.Run(ctx, nil, nil, p.args("down", "--remove-orphans")...); err != nil {
		return fmt.Errorf("stop staging compose project %s: %w", p.name, err)
	}
	return nil
}

func writeStagingProject(plan Plan, appName, service, composeFile, envContent string) (stagingProject, error) {
	if strings.TrimSpace(plan.RunDir) == "" {
		return stagingProject{}, fmt.Errorf("plan.RunDir is empty; cannot stage compose project for %s/%s", appName, service)
	}
	dir := filepath.Join(plan.RunDir, "stage", safeDataPathSegment(appName), safeDataPathSegment(service))
	if err := safepath.ContainedPath(plan.RunDir, dir); err != nil {
		return stagingProject{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return stagingProject{}, fmt.Errorf("prepare staging dir: %w", err)
	}
	project := stagingProject{
		name:        stagingProjectName(plan, appName, service),
		composePath: filepath.Join(dir, "compose.yaml"),
		envPath:     filepath.Join(dir, ".env"),
	}
	if err := os.WriteFile(project.composePath, []byte(composeFile), 0o600); err != nil {
		return stagingProject{}, fmt.Errorf("write staging compose: %w", err)
	}
	if err := os.WriteFile(project.envPath, []byte(envContent), 0o600); err != nil {
		return stagingProject{}, fmt.Errorf("write staging env: %w", err)
	}
	return project, nil
}

func (p stagingProject) serviceContainer(ctx context.Context, runner dockerRunner, service string) (dockerContainer, error) {
	deadline := time.Now().Add(targetDiscoveryTimeout)
	for {
		out, err := runner.Output(ctx, p.args("ps", "-a", "-q", service)...)
		if err != nil {
			return dockerContainer{}, fmt.Errorf("find staging container for %s: %w", service, err)
		}
		if ids := strings.Fields(string(out)); len(ids) == 1 {
			return inspectContainer(ctx, runner, ids[0])
		} else if len(ids) > 1 {
			return dockerContainer{}, fmt.Errorf("staging compose project %s has %d containers for service %s", p.name, len(ids), service)
		}
		if time.Now().After(deadline) {
			return dockerContainer{}, fmt.Errorf("staging compose project %s has no container for service %s after %s", p.name, service, targetDiscoveryTimeout)
		}
		select {
		case <-ctx.Done():
			return dockerContainer{}, ctx.Err()
		case <-time.After(targetDiscoveryDelay):
		}
	}
}
