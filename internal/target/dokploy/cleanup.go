package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/safepath"
)

type StalePlatformProject struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
}

type StalePlatformCleanupOptions struct {
	ProjectNames []string
	ProjectIDs   map[string]string
	BackupDir    string
	BackupPrefix string
}

type StalePlatformCleanupResult struct {
	BackupPath string                 `json:"backupPath"`
	Deleted    []StalePlatformProject `json:"deleted"`
}

type SourcePurgeContainer struct {
	App           string `json:"app,omitempty"`
	Service       string `json:"service,omitempty"`
	ContainerID   string `json:"containerId,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
}

type SourcePurgeVolume struct {
	App            string `json:"app,omitempty"`
	Service        string `json:"service,omitempty"`
	Name           string `json:"name"`
	ExpectedAbsent bool   `json:"expectedAbsent,omitempty"`
}

type SourcePurgeNetwork struct {
	App                string `json:"app,omitempty"`
	Name               string `json:"name"`
	DiscoveredIdentity string `json:"discoveredIdentity,omitempty"`
	ExpectedIdentity   string `json:"expectedIdentity,omitempty"`
	ExpectedAbsent     bool   `json:"expectedAbsent,omitempty"`
}

type SourcePurgePath struct {
	App            string `json:"app,omitempty"`
	Source         string `json:"source,omitempty"`
	Path           string `json:"path"`
	AllowPlatform  bool   `json:"allowPlatform,omitempty"`
	ExpectedAbsent bool   `json:"expectedAbsent,omitempty"`
}

type SourcePurgeOptions struct {
	Containers []SourcePurgeContainer
	Volumes    []SourcePurgeVolume
	Networks   []SourcePurgeNetwork
	Paths      []SourcePurgePath
	OnProgress func(SourcePurgeResult) error
}

type SourcePurgeResult struct {
	Containers []SourcePurgeResourceResult `json:"containers,omitempty"`
	Volumes    []SourcePurgeResourceResult `json:"volumes,omitempty"`
	Networks   []SourcePurgeResourceResult `json:"networks,omitempty"`
	Paths      []SourcePurgeResourceResult `json:"paths,omitempty"`
}

type SourcePurgeResourceResult struct {
	App      string `json:"app,omitempty"`
	Ref      string `json:"ref"`
	Identity string `json:"identity,omitempty"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}

func (c *Client) CleanupStalePlatformProjects(ctx context.Context, opts StalePlatformCleanupOptions) (StalePlatformCleanupResult, error) {
	names := cleanupProjectNames(opts.ProjectNames)
	if len(names) == 0 {
		return StalePlatformCleanupResult{}, fmt.Errorf("at least one project name is required")
	}
	runner := c.dockerRunner()
	pg, err := findDokployPostgresContainer(ctx, runner)
	if err != nil {
		return StalePlatformCleanupResult{}, err
	}
	backupPath, err := backupDokployDatabase(ctx, runner, pg, opts.BackupDir, opts.BackupPrefix)
	if err != nil {
		return StalePlatformCleanupResult{}, err
	}
	deleted, err := deleteStalePlatformProjects(ctx, runner, pg, names, opts.ProjectIDs)
	if err != nil {
		return StalePlatformCleanupResult{}, fmt.Errorf("delete stale Dokploy platform metadata after backup %s: %w", backupPath, err)
	}
	return StalePlatformCleanupResult{BackupPath: backupPath, Deleted: deleted}, nil
}

func (c *Client) PurgeSourceResources(ctx context.Context, opts SourcePurgeOptions) (SourcePurgeResult, error) {
	runner := c.dockerRunner()
	result := SourcePurgeResult{}

	containers, err := cleanupSourcePurgeContainers(opts.Containers)
	if err != nil {
		return result, err
	}
	volumes := cleanupSourcePurgeVolumes(opts.Volumes)
	paths := cleanupSourcePurgePaths(opts.Paths)
	networks, err := cleanupSourcePurgeNetworks(opts.Networks)
	if err != nil {
		return result, err
	}
	if err := validateSourcePurgeNetworksForExecution(networks); err != nil {
		return result, err
	}
	if sourcePurgeRequiresManualCompletion(volumes, paths, networks) {
		for _, volume := range volumes {
			if err := runSourcePurgeResource(&result, &result.Volumes, opts.OnProgress, SourcePurgeResourceResult{App: volume.App, Ref: volume.Name}, func() (SourcePurgeResourceResult, error) {
				return purgeSourceVolume(ctx, runner, volume)
			}); err != nil {
				return result, err
			}
		}
		for _, path := range paths {
			if err := runSourcePurgeResource(&result, &result.Paths, opts.OnProgress, SourcePurgeResourceResult{App: path.App, Ref: path.Path}, func() (SourcePurgeResourceResult, error) {
				return purgeSourcePath(path)
			}); err != nil {
				return result, err
			}
		}
		for _, container := range containers {
			if err := runSourcePurgeResource(&result, &result.Containers, opts.OnProgress, SourcePurgeResourceResult{App: container.App, Ref: container.ContainerID}, func() (SourcePurgeResourceResult, error) {
				return verifySourcePurgeContainerAbsent(ctx, runner, container)
			}); err != nil {
				return result, err
			}
		}
		for _, network := range networks {
			if err := runSourcePurgeResource(&result, &result.Networks, opts.OnProgress, SourcePurgeResourceResult{App: network.App, Ref: network.Name, Identity: network.ExpectedIdentity}, func() (SourcePurgeResourceResult, error) {
				return verifySourcePurgeNetworkAbsent(ctx, runner, network)
			}); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	for _, volume := range volumes {
		if err := runSourcePurgeResource(&result, &result.Volumes, opts.OnProgress, SourcePurgeResourceResult{App: volume.App, Ref: volume.Name}, func() (SourcePurgeResourceResult, error) {
			return purgeSourceVolume(ctx, runner, volume)
		}); err != nil {
			return result, err
		}
	}
	for _, path := range paths {
		if err := runSourcePurgeResource(&result, &result.Paths, opts.OnProgress, SourcePurgeResourceResult{App: path.App, Ref: path.Path}, func() (SourcePurgeResourceResult, error) {
			return purgeSourcePath(path)
		}); err != nil {
			return result, err
		}
	}
	for _, network := range networks {
		if !network.ExpectedAbsent {
			continue
		}
		if err := runSourcePurgeResource(&result, &result.Networks, opts.OnProgress, SourcePurgeResourceResult{App: network.App, Ref: network.Name, Identity: network.ExpectedIdentity}, func() (SourcePurgeResourceResult, error) {
			return purgeSourceNetwork(ctx, runner, network)
		}); err != nil {
			return result, err
		}
	}
	for _, container := range containers {
		if err := runSourcePurgeResource(&result, &result.Containers, opts.OnProgress, SourcePurgeResourceResult{App: container.App, Ref: container.ContainerID}, func() (SourcePurgeResourceResult, error) {
			return purgeSourceContainer(ctx, runner, container)
		}); err != nil {
			return result, err
		}
	}
	for _, network := range networks {
		if network.ExpectedAbsent {
			continue
		}
		if err := runSourcePurgeResource(&result, &result.Networks, opts.OnProgress, SourcePurgeResourceResult{App: network.App, Ref: network.Name, Identity: network.ExpectedIdentity}, func() (SourcePurgeResourceResult, error) {
			return purgeSourceNetwork(ctx, runner, network)
		}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func sourcePurgeRequiresManualCompletion(volumes []SourcePurgeVolume, paths []SourcePurgePath, networks []SourcePurgeNetwork) bool {
	if len(volumes) > 0 || len(paths) > 0 {
		return true
	}
	for _, network := range networks {
		if network.ExpectedAbsent {
			return true
		}
	}
	return false
}

func runSourcePurgeResource(result *SourcePurgeResult, resources *[]SourcePurgeResourceResult, onProgress func(SourcePurgeResult) error, started SourcePurgeResourceResult, operation func() (SourcePurgeResourceResult, error)) error {
	started.Status = "started"
	*resources = append(*resources, started)
	if err := publishSourcePurgeProgress(*result, onProgress); err != nil {
		return err
	}
	outcome, operationErr := operation()
	if operationErr != nil && strings.TrimSpace(outcome.Message) == "" {
		outcome.Message = operationErr.Error()
	}
	(*resources)[len(*resources)-1] = outcome
	if err := publishSourcePurgeProgress(*result, onProgress); err != nil {
		if operationErr != nil {
			return fmt.Errorf("%v; %w", operationErr, err)
		}
		return err
	}
	return operationErr
}

func publishSourcePurgeProgress(result SourcePurgeResult, onProgress func(SourcePurgeResult) error) error {
	if onProgress == nil {
		return nil
	}
	snapshot := SourcePurgeResult{
		Containers: append([]SourcePurgeResourceResult{}, result.Containers...),
		Volumes:    append([]SourcePurgeResourceResult{}, result.Volumes...),
		Networks:   append([]SourcePurgeResourceResult{}, result.Networks...),
		Paths:      append([]SourcePurgeResourceResult{}, result.Paths...),
	}
	if err := onProgress(snapshot); err != nil {
		return fmt.Errorf("persist source purge progress: %w", err)
	}
	return nil
}

func (c *Client) IdentifySourcePurgeResources(ctx context.Context, opts SourcePurgeOptions) (SourcePurgeOptions, error) {
	runner := c.dockerRunner()
	identified := opts
	containers, err := cleanupSourcePurgeContainers(opts.Containers)
	if err != nil {
		return SourcePurgeOptions{}, err
	}
	for _, container := range containers {
		if strings.TrimSpace(container.ContainerID) == "" {
			return SourcePurgeOptions{}, fmt.Errorf("refusing source container %q without a stable container ID", container.ContainerName)
		}
	}
	identified.Containers = containers
	identified.Volumes = nil
	for _, volume := range cleanupSourcePurgeVolumes(opts.Volumes) {
		absent, err := inspectSourcePurgeVolumeAbsent(ctx, runner, volume.Name)
		if err != nil {
			return SourcePurgeOptions{}, err
		}
		if !absent {
			return SourcePurgeOptions{}, fmt.Errorf("named volume %q still exists; remove it manually before cleanup purge --apply", volume.Name)
		}
		volume.ExpectedAbsent = true
		identified.Volumes = append(identified.Volumes, volume)
	}
	identified.Networks = nil
	networks, err := cleanupSourcePurgeNetworks(opts.Networks)
	if err != nil {
		return SourcePurgeOptions{}, err
	}
	for _, network := range networks {
		identity, absent, err := inspectSourcePurgeNetworkIdentity(ctx, runner, network.Name)
		if err != nil {
			return SourcePurgeOptions{}, err
		}
		discoveredIdentity := strings.TrimSpace(network.DiscoveredIdentity)
		if !absent && discoveredIdentity == "" {
			return SourcePurgeOptions{}, fmt.Errorf("source network %q has no stable ID from discovery; remove it manually before cleanup purge --apply", network.Name)
		}
		if !absent && !sourcePurgeCanonicalNetworkID(discoveredIdentity) {
			return SourcePurgeOptions{}, fmt.Errorf("source network %q has non-canonical discovered ID %q; remove it manually before cleanup purge --apply", network.Name, discoveredIdentity)
		}
		if !absent && !sourcePurgeCanonicalNetworkID(identity) {
			return SourcePurgeOptions{}, fmt.Errorf("docker returned non-canonical ID %q for source network %q", identity, network.Name)
		}
		if !absent && !SourcePurgeNetworkIDsEquivalent(identity, discoveredIdentity) {
			return SourcePurgeOptions{}, fmt.Errorf("refusing source network %q because current ID %s does not match discovered ID %s", network.Name, identity, discoveredIdentity)
		}
		network.ExpectedIdentity = identity
		network.ExpectedAbsent = absent
		identified.Networks = append(identified.Networks, network)
	}
	identified.Paths = nil
	for _, path := range cleanupSourcePurgePaths(opts.Paths) {
		absent, err := sourcePurgePathAbsentNoFollow(path.Path, path.AllowPlatform)
		if err != nil {
			return SourcePurgeOptions{}, err
		}
		if !absent {
			return SourcePurgeOptions{}, fmt.Errorf("source path %q still exists; remove it manually before cleanup purge --apply", path.Path)
		}
		path.ExpectedAbsent = true
		identified.Paths = append(identified.Paths, path)
	}
	if sourcePurgeRequiresManualCompletion(identified.Volumes, identified.Paths, identified.Networks) {
		for _, container := range identified.Containers {
			if _, err := verifySourcePurgeContainerAbsent(ctx, runner, container); err != nil {
				return SourcePurgeOptions{}, fmt.Errorf("source purge has absence-only prerequisites and cannot remove other resources safely; remove all listed containers and networks manually first: %w", err)
			}
		}
		for _, network := range identified.Networks {
			if !network.ExpectedAbsent {
				return SourcePurgeOptions{}, fmt.Errorf("source purge has absence-only prerequisites and cannot remove other resources safely; remove source network %q manually before cleanup purge --apply", network.Name)
			}
		}
	} else {
		for i, container := range identified.Containers {
			canonical, err := identifySourcePurgeContainer(ctx, runner, container)
			if err != nil {
				return SourcePurgeOptions{}, err
			}
			identified.Containers[i] = canonical
		}
	}
	return identified, nil
}

func identifySourcePurgeContainer(ctx context.Context, runner dockerRunner, item SourcePurgeContainer) (SourcePurgeContainer, error) {
	containerID := strings.TrimSpace(item.ContainerID)
	container, err := inspectContainer(ctx, runner, containerID)
	if err != nil {
		if isContainerMissingErr(err) {
			return item, nil
		}
		return SourcePurgeContainer{}, err
	}
	canonicalID := strings.TrimSpace(container.ID)
	if !sourcePurgeContainerIDMatches(containerID, canonicalID) {
		return SourcePurgeContainer{}, fmt.Errorf("refusing source container %s: docker returned container ID %q, not the reviewed ID %q", firstNonEmpty(item.ContainerName, containerID), canonicalID, containerID)
	}
	if item.ContainerName != "" && container.Name != "" && item.ContainerName != container.Name {
		return SourcePurgeContainer{}, fmt.Errorf("refusing source container %s: expected name %q, found %q", containerID, item.ContainerName, container.Name)
	}
	item.ContainerID = canonicalID
	if item.ContainerName == "" {
		item.ContainerName = container.Name
	}
	return item, nil
}

func sourcePurgeContainerIDMatches(reviewed, inspected string) bool {
	reviewed = strings.TrimSpace(reviewed)
	inspected = strings.TrimSpace(inspected)
	return inspected == reviewed || len(reviewed) >= 12 && strings.HasPrefix(inspected, reviewed)
}

func sourcePurgeContainerIDsEquivalent(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return a != ""
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 12 && strings.HasPrefix(b, a)
}

func ValidSourcePurgeNetworkID(identity string) bool {
	identity = strings.TrimSpace(identity)
	if len(identity) < 12 || len(identity) > 64 {
		return false
	}
	for _, r := range identity {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func sourcePurgeCanonicalNetworkID(identity string) bool {
	return len(strings.TrimSpace(identity)) == 64 && ValidSourcePurgeNetworkID(identity)
}

func SourcePurgeNetworkIDsEquivalent(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	return sourcePurgeCanonicalNetworkID(a) && sourcePurgeCanonicalNetworkID(b) && a == b
}

func cleanupSourcePurgeContainers(containers []SourcePurgeContainer) ([]SourcePurgeContainer, error) {
	idsByName := map[string]string{}
	for _, container := range containers {
		container.ContainerID = strings.TrimSpace(container.ContainerID)
		container.ContainerName = trimDockerName(container.ContainerName)
		if container.ContainerID == "" || container.ContainerName == "" {
			continue
		}
		if id, ok := idsByName[container.ContainerName]; ok {
			if !sourcePurgeContainerIDsEquivalent(id, container.ContainerID) {
				return nil, fmt.Errorf("source container name %q refers to conflicting container IDs", container.ContainerName)
			}
			if len(id) > len(container.ContainerID) {
				container.ContainerID = id
			}
		}
		idsByName[container.ContainerName] = container.ContainerID
	}
	seen := map[string]int{}
	cleaned := []SourcePurgeContainer{}
	for _, container := range containers {
		container.ContainerID = strings.TrimSpace(container.ContainerID)
		container.ContainerName = trimDockerName(container.ContainerName)
		ref := firstNonEmpty(container.ContainerID, container.ContainerName)
		if ref == "" {
			continue
		}
		key := "name:" + container.ContainerName
		if container.ContainerID != "" {
			key = "id:" + container.ContainerID
		}
		index, ok := seen[key]
		matchedKey := key
		if !ok && container.ContainerID != "" {
			for seenKey, seenIndex := range seen {
				if strings.HasPrefix(seenKey, "id:") && sourcePurgeContainerIDsEquivalent(strings.TrimPrefix(seenKey, "id:"), container.ContainerID) {
					index, ok, matchedKey = seenIndex, true, seenKey
					break
				}
			}
		}
		if ok {
			if cleaned[index].ContainerName != "" && container.ContainerName != "" && cleaned[index].ContainerName != container.ContainerName {
				return nil, fmt.Errorf("source container ID %q refers to conflicting container names", container.ContainerID)
			}
			if cleaned[index].Service != "" && container.Service != "" && cleaned[index].Service != container.Service {
				return nil, fmt.Errorf("source container ID %q refers to conflicting services", container.ContainerID)
			}
			if cleaned[index].ContainerName == "" {
				cleaned[index].ContainerName = container.ContainerName
			}
			if cleaned[index].Service == "" {
				cleaned[index].Service = container.Service
			}
			if len(container.ContainerID) > len(cleaned[index].ContainerID) {
				cleaned[index].ContainerID = container.ContainerID
				delete(seen, matchedKey)
				seen["id:"+container.ContainerID] = index
			}
			continue
		}
		seen[key] = len(cleaned)
		cleaned = append(cleaned, container)
	}
	return cleaned, nil
}

func cleanupSourcePurgeVolumes(volumes []SourcePurgeVolume) []SourcePurgeVolume {
	seen := map[string]struct{}{}
	cleaned := []SourcePurgeVolume{}
	for _, volume := range volumes {
		volume.Name = strings.TrimSpace(volume.Name)
		if volume.Name == "" {
			continue
		}
		if _, ok := seen[volume.Name]; ok {
			continue
		}
		seen[volume.Name] = struct{}{}
		cleaned = append(cleaned, volume)
	}
	sort.Slice(cleaned, func(i, j int) bool { return cleaned[i].Name < cleaned[j].Name })
	return cleaned
}

func cleanupSourcePurgeNetworks(networks []SourcePurgeNetwork) ([]SourcePurgeNetwork, error) {
	seen := map[string]int{}
	cleaned := []SourcePurgeNetwork{}
	for _, network := range networks {
		network.Name = strings.TrimSpace(network.Name)
		network.DiscoveredIdentity = strings.TrimSpace(network.DiscoveredIdentity)
		network.ExpectedIdentity = strings.TrimSpace(network.ExpectedIdentity)
		if network.Name == "" {
			continue
		}
		if network.ExpectedAbsent && network.ExpectedIdentity != "" {
			return nil, fmt.Errorf("source network %q has conflicting expected identity and absence state", network.Name)
		}
		if index, ok := seen[network.Name]; ok {
			current := &cleaned[index]
			if current.DiscoveredIdentity != "" && network.DiscoveredIdentity != "" && current.DiscoveredIdentity != network.DiscoveredIdentity {
				return nil, fmt.Errorf("source network %q has conflicting discovered identities", network.Name)
			}
			if current.ExpectedIdentity != "" && network.ExpectedIdentity != "" && current.ExpectedIdentity != network.ExpectedIdentity {
				return nil, fmt.Errorf("source network %q has conflicting expected identities", network.Name)
			}
			if current.ExpectedAbsent != network.ExpectedAbsent {
				return nil, fmt.Errorf("source network %q has conflicting expected absence state", network.Name)
			}
			if current.DiscoveredIdentity == "" {
				current.DiscoveredIdentity = network.DiscoveredIdentity
			}
			if current.ExpectedIdentity == "" {
				current.ExpectedIdentity = network.ExpectedIdentity
			}
			continue
		}
		seen[network.Name] = len(cleaned)
		cleaned = append(cleaned, network)
	}
	for _, network := range cleaned {
		if network.ExpectedIdentity != "" && !sourcePurgeCanonicalNetworkID(network.ExpectedIdentity) {
			return nil, fmt.Errorf("source network %q has non-canonical confirmed ID %q", network.Name, network.ExpectedIdentity)
		}
	}
	sort.Slice(cleaned, func(i, j int) bool { return cleaned[i].Name < cleaned[j].Name })
	return cleaned, nil
}

func validateSourcePurgeNetworksForExecution(networks []SourcePurgeNetwork) error {
	for _, network := range networks {
		if network.ExpectedAbsent {
			continue
		}
		if !sourcePurgeCanonicalNetworkID(network.ExpectedIdentity) {
			return fmt.Errorf("source network %q has no canonical confirmed ID", network.Name)
		}
		if !sourcePurgeCanonicalNetworkID(network.DiscoveredIdentity) {
			return fmt.Errorf("source network %q has non-canonical discovered ID %q", network.Name, network.DiscoveredIdentity)
		}
		if !SourcePurgeNetworkIDsEquivalent(network.ExpectedIdentity, network.DiscoveredIdentity) {
			return fmt.Errorf("source network %q has conflicting discovered and confirmed IDs", network.Name)
		}
	}
	return nil
}

func cleanupSourcePurgePaths(paths []SourcePurgePath) []SourcePurgePath {
	seen := map[string]struct{}{}
	cleaned := []SourcePurgePath{}
	for _, path := range paths {
		path.Path = pathpkg.Clean(strings.TrimSpace(path.Path))
		if path.Path == "." || path.Path == "" {
			continue
		}
		if _, ok := seen[path.Path]; ok {
			continue
		}
		seen[path.Path] = struct{}{}
		cleaned = append(cleaned, path)
	}
	sort.Slice(cleaned, func(i, j int) bool { return cleaned[i].Path < cleaned[j].Path })
	return cleaned
}

func purgeSourceContainer(ctx context.Context, runner dockerRunner, item SourcePurgeContainer) (SourcePurgeResourceResult, error) {
	ref := firstNonEmpty(item.ContainerID, item.ContainerName)
	result := SourcePurgeResourceResult{App: item.App, Ref: ref}
	containerID := strings.TrimSpace(item.ContainerID)
	if containerID == "" {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove source container %q without a stable container ID", item.ContainerName)
	}
	container, err := inspectContainer(ctx, runner, containerID)
	if err != nil {
		if isContainerMissingErr(err) {
			containerName := strings.TrimSpace(item.ContainerName)
			if containerName != "" {
				replacement, nameErr := inspectContainer(ctx, runner, containerName)
				if nameErr == nil {
					result.Status = "blocked"
					return result, fmt.Errorf("refusing source container name %q because it now refers to container ID %q", containerName, replacement.ID)
				}
				if !isContainerMissingErr(nameErr) {
					result.Status = "error"
					return result, nameErr
				}
			}
			result.Status = "skipped"
			result.Message = "container is already absent"
			return result, nil
		}
		result.Status = "error"
		return result, err
	}
	if strings.TrimSpace(container.ID) != containerID {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove container %s: docker returned container ID %q, not the reviewed ID %q", ref, container.ID, containerID)
	}
	if item.ContainerName != "" && container.Name != "" && container.Name != item.ContainerName {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove container %s: expected name %q, found %q", ref, item.ContainerName, container.Name)
	}
	if _, err := runner.Output(ctx, "rm", "-f", containerID); err != nil {
		if isContainerMissingErr(err) {
			result.Status = "skipped"
			result.Message = "container is already absent"
			return result, nil
		}
		result.Status = "error"
		return result, err
	}
	result.Ref = firstNonEmpty(container.Name, containerID)
	result.Status = "removed"
	return result, nil
}

func verifySourcePurgeContainerAbsent(ctx context.Context, runner dockerRunner, item SourcePurgeContainer) (SourcePurgeResourceResult, error) {
	containerID := strings.TrimSpace(item.ContainerID)
	result := SourcePurgeResourceResult{App: item.App, Ref: firstNonEmpty(containerID, item.ContainerName)}
	if containerID == "" {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing source container %q without a stable container ID", item.ContainerName)
	}
	container, err := inspectContainer(ctx, runner, containerID)
	if err != nil {
		if isContainerMissingErr(err) {
			containerName := strings.TrimSpace(item.ContainerName)
			if containerName != "" {
				replacement, nameErr := inspectContainer(ctx, runner, containerName)
				if nameErr == nil {
					result.Status = "blocked"
					return result, fmt.Errorf("source container name %q now refers to container ID %q; remove it manually before cleanup purge --apply", containerName, replacement.ID)
				}
				if !isContainerMissingErr(nameErr) {
					result.Status = "error"
					return result, nameErr
				}
			}
			result.Status = "skipped"
			result.Message = "container remains absent after confirmation"
			return result, nil
		}
		result.Status = "error"
		return result, err
	}
	if !sourcePurgeContainerIDMatches(containerID, container.ID) {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing source container %s: docker returned container ID %q, not the reviewed ID %q", result.Ref, container.ID, containerID)
	}
	result.Status = "blocked"
	return result, fmt.Errorf("source container %q still exists; remove it manually before cleanup purge --apply", firstNonEmpty(container.Name, containerID))
}

func purgeSourceVolume(ctx context.Context, runner dockerRunner, item SourcePurgeVolume) (SourcePurgeResourceResult, error) {
	name := strings.TrimSpace(item.Name)
	result := SourcePurgeResourceResult{App: item.App, Ref: name}
	if err := validateDockerPurgeName("volume", name); err != nil {
		result.Status = "blocked"
		return result, err
	}
	if !item.ExpectedAbsent {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing source volume %q without pre-confirmation absence validation", name)
	}
	absent, err := inspectSourcePurgeVolumeAbsent(ctx, runner, name)
	if err != nil {
		result.Status = "error"
		return result, err
	}
	if !absent {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing source volume %q because it appeared after confirmation", name)
	}
	result.Status = "skipped"
	result.Message = "volume remains absent after confirmation"
	return result, nil
}

func purgeSourceNetwork(ctx context.Context, runner dockerRunner, item SourcePurgeNetwork) (SourcePurgeResourceResult, error) {
	name := strings.TrimSpace(item.Name)
	result := SourcePurgeResourceResult{App: item.App, Ref: name}
	if err := validateDockerPurgeName("network", name); err != nil {
		result.Status = "blocked"
		return result, err
	}
	if IsProtectedSourcePurgeNetwork(name) {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove protected docker network %q", name)
	}
	if item.ExpectedAbsent {
		_, absent, err := inspectSourcePurgeNetworkIdentity(ctx, runner, name)
		if err != nil {
			result.Status = "error"
			return result, err
		}
		if !absent {
			result.Status = "blocked"
			return result, fmt.Errorf("refusing source network %q because it appeared after confirmation", name)
		}
		result.Status = "skipped"
		result.Message = "network remains absent after confirmation"
		return result, nil
	}
	identity := strings.TrimSpace(item.ExpectedIdentity)
	if identity == "" {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove docker network %q without a pre-confirmation identity", name)
	}
	if !sourcePurgeCanonicalNetworkID(identity) {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove docker network %q with non-canonical confirmed ID %q", name, identity)
	}
	discoveredIdentity := strings.TrimSpace(item.DiscoveredIdentity)
	if discoveredIdentity == "" {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove docker network %q without a stable ID from discovery", name)
	}
	if !sourcePurgeCanonicalNetworkID(discoveredIdentity) {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove docker network %q with non-canonical discovered ID %q", name, discoveredIdentity)
	}
	if !SourcePurgeNetworkIDsEquivalent(identity, discoveredIdentity) {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing to remove docker network %q because confirmed ID %s does not match discovered ID %s", name, identity, discoveredIdentity)
	}
	result.Identity = identity
	if _, err := runner.Output(ctx, "network", "rm", identity); err != nil {
		if isDockerVolumeOrNetworkMissingErr(err) {
			_, absent, inspectErr := inspectSourcePurgeNetworkIdentity(ctx, runner, name)
			if inspectErr != nil {
				result.Status = "error"
				return result, fmt.Errorf("recheck source network %q after reviewed network %s disappeared: %w", name, identity, inspectErr)
			}
			if !absent {
				result.Status = "blocked"
				return result, fmt.Errorf("refusing source network %q because a network with that name appeared after confirmation", name)
			}
			result.Status = "skipped"
			result.Message = "network is already absent"
			return result, nil
		}
		result.Status = "error"
		return result, err
	}
	result.Status = "removed"
	return result, nil
}

func verifySourcePurgeNetworkAbsent(ctx context.Context, runner dockerRunner, item SourcePurgeNetwork) (SourcePurgeResourceResult, error) {
	name := strings.TrimSpace(item.Name)
	result := SourcePurgeResourceResult{App: item.App, Ref: name, Identity: strings.TrimSpace(item.ExpectedIdentity)}
	if err := validateDockerPurgeName("network", name); err != nil {
		result.Status = "blocked"
		return result, err
	}
	if IsProtectedSourcePurgeNetwork(name) {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing protected docker network %q", name)
	}
	_, absent, err := inspectSourcePurgeNetworkIdentity(ctx, runner, name)
	if err != nil {
		result.Status = "error"
		return result, err
	}
	if !absent {
		result.Status = "blocked"
		return result, fmt.Errorf("source network %q still exists; remove it manually before cleanup purge --apply", name)
	}
	result.Status = "skipped"
	result.Message = "network remains absent after confirmation"
	return result, nil
}

func purgeSourcePath(item SourcePurgePath) (SourcePurgeResourceResult, error) {
	path := pathpkg.Clean(strings.TrimSpace(item.Path))
	result := SourcePurgeResourceResult{App: item.App, Ref: path}
	if err := ValidateSourcePurgePath(path, item.AllowPlatform); err != nil {
		result.Status = "blocked"
		return result, err
	}
	if !item.ExpectedAbsent {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing source path %q without pre-confirmation absence validation", path)
	}
	absent, err := sourcePurgePathAbsentNoFollow(path, item.AllowPlatform)
	if err != nil {
		result.Status = "error"
		return result, err
	}
	if !absent {
		result.Status = "blocked"
		return result, fmt.Errorf("refusing source path %q because it appeared after confirmation", path)
	}
	result.Status = "skipped"
	result.Message = "path remains absent after confirmation"
	return result, nil
}

type sourcePurgeVolumeState struct {
	Name string `json:"Name"`
}

type sourcePurgeNetworkIdentity struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

func inspectSourcePurgeVolumeAbsent(ctx context.Context, runner dockerRunner, name string) (bool, error) {
	out, err := runner.Output(ctx, "volume", "inspect", name)
	if err != nil {
		if isDockerVolumeOrNetworkMissingErr(err) {
			return true, nil
		}
		return false, err
	}
	var volumes []sourcePurgeVolumeState
	if err := json.Unmarshal(out, &volumes); err != nil {
		return false, fmt.Errorf("decode docker volume inspect %s: %w", name, err)
	}
	if len(volumes) != 1 || strings.TrimSpace(volumes[0].Name) != name {
		return false, fmt.Errorf("docker volume inspect %s returned an unexpected resource", name)
	}
	return false, nil
}

func inspectSourcePurgeNetworkIdentity(ctx context.Context, runner dockerRunner, name string) (string, bool, error) {
	out, err := runner.Output(ctx, "network", "inspect", name)
	if err != nil {
		if isDockerVolumeOrNetworkMissingErr(err) {
			return "", true, nil
		}
		return "", false, err
	}
	var networks []sourcePurgeNetworkIdentity
	if err := json.Unmarshal(out, &networks); err != nil {
		return "", false, fmt.Errorf("decode docker network inspect %s: %w", name, err)
	}
	if len(networks) != 1 || strings.TrimSpace(networks[0].Name) != name || strings.TrimSpace(networks[0].ID) == "" {
		return "", false, fmt.Errorf("docker network inspect %s returned an unexpected resource", name)
	}
	return strings.TrimSpace(networks[0].ID), false, nil
}

func validateDockerPurgeName(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("docker %s name is empty", kind)
	}
	if name == "/" || strings.Contains(name, "\x00") {
		return fmt.Errorf("refusing to remove invalid docker %s %q", kind, name)
	}
	return nil
}

func IsProtectedSourcePurgeNetwork(name string) bool {
	switch strings.TrimSpace(name) {
	case "bridge", "host", "none", "ingress":
		return true
	default:
		return false
	}
}

func ValidateSourcePurgePath(path string, allowPlatform bool) error {
	if err := validateSourcePurgePathLexical(path, allowPlatform); err != nil {
		return err
	}
	cleaned := pathpkg.Clean(path)
	localPath := filepath.FromSlash(cleaned)
	resolved, ok, err := sourcePurgeResolveExistingPath(localPath)
	if err != nil {
		return err
	}
	if ok && resolved != filepath.Clean(localPath) {
		return fmt.Errorf("source purge path %q contains or resolves through a symbolic link (%s)", cleaned, resolved)
	}
	return nil
}

func validateSourcePurgePathLexical(path string, allowPlatform bool) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("source purge path is empty")
	}
	if !pathpkg.IsAbs(path) {
		return fmt.Errorf("source purge path must be absolute (got %q)", path)
	}
	cleaned := pathpkg.Clean(path)
	if strings.Contains(cleaned, "\x00") {
		return fmt.Errorf("source purge path contains invalid null byte")
	}
	if cleaned == "/" {
		return fmt.Errorf("refusing to remove host root")
	}
	for _, protected := range []string{"/bin", "/boot", "/data", "/dev", "/etc", "/home", "/lib", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var"} {
		if cleaned == protected {
			return fmt.Errorf("refusing to remove protected host path %q", cleaned)
		}
	}
	roots := []string{pathpkg.Clean("/data/coolify")}
	if !sourcePurgeCoolifyPathAllowed(cleaned, roots, allowPlatform) {
		return fmt.Errorf("source purge path %q must be inside /data/coolify applications/<id>, services/<id>, or databases/<id>", cleaned)
	}
	return nil
}

func sourcePurgeCoolifyPathAllowed(path string, roots []string, allowPlatform bool) bool {
	for _, root := range roots {
		if sourcePurgeCoolifyPathAllowedAtRoot(path, root, allowPlatform) {
			return true
		}
	}
	return false
}

func sourcePurgeCoolifyPathAllowedAtRoot(path, root string, allowPlatform bool) bool {
	cleanedRoot := pathpkg.Clean(root)
	cleanedPath := pathpkg.Clean(path)
	prefix := strings.TrimSuffix(cleanedRoot, "/") + "/"
	if cleanedPath == cleanedRoot || !strings.HasPrefix(cleanedPath, prefix) {
		return false
	}
	rel := strings.TrimPrefix(cleanedPath, prefix)
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[1] != "" {
		switch parts[0] {
		case "applications", "services", "databases":
			return true
		}
	}
	if allowPlatform && len(parts) >= 1 {
		switch parts[0] {
		case "source", "proxy":
			return true
		}
	}
	return false
}

func sourcePurgeResolveExistingPath(path string) (string, bool, error) {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved), true, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}

	missing := []string{}
	parent := cleaned
	for {
		if _, err := os.Lstat(parent); err != nil {
			if os.IsNotExist(err) {
				next := filepath.Dir(parent)
				if next == parent || next == "." || next == "" {
					return "", false, nil
				}
				missing = append([]string{filepath.Base(parent)}, missing...)
				parent = next
				continue
			}
			return "", false, err
		}
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			if os.IsNotExist(err) {
				return "", false, nil
			}
			return "", false, err
		}
		parts := append([]string{filepath.Clean(resolvedParent)}, missing...)
		return filepath.Join(parts...), true, nil
	}
}

func trimDockerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isDockerVolumeOrNetworkMissingErr(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such volume") ||
		strings.Contains(message, "no such network") ||
		(strings.Contains(message, "error response from daemon: network ") && strings.Contains(message, " not found"))
}

func cleanupProjectNames(names []string) []string {
	seen := map[string]struct{}{}
	cleaned := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		cleaned = append(cleaned, name)
	}
	sort.Strings(cleaned)
	return cleaned
}

func findDokployPostgresContainer(ctx context.Context, runner dockerRunner) (string, error) {
	out, err := runner.Output(ctx, "ps", "--format", "{{.Names}}")
	if err != nil {
		return "", fmt.Errorf("list docker containers for Dokploy postgres: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "dokploy-postgres" || strings.HasPrefix(name, "dokploy-postgres.") || strings.HasPrefix(name, "dokploy-postgres-") {
			return name, nil
		}
	}
	return "", fmt.Errorf("dokploy postgres container was not found; cleanup must run on the Dokploy host")
}

func backupDokployDatabase(ctx context.Context, runner dockerRunner, pg, backupDir, backupPrefix string) (_ string, resultErr error) {
	backupDir = strings.TrimSpace(backupDir)
	if backupDir == "" {
		backupDir = filepath.Join(".bort", "backups")
	}
	if backupPrefix = strings.TrimSpace(backupPrefix); backupPrefix == "" {
		backupPrefix = "dokploy-cleanup"
	}
	dir, err := safepath.OpenPrivateDirNoFollow(backupDir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	name := fmt.Sprintf("%s-%s.sql", backupPrefix, time.Now().UTC().Format("20060102-150405.000000000"))
	path := filepath.Join(backupDir, name)
	file, err := dir.CreateFile(name, 0o600)
	if err != nil {
		return "", err
	}
	removeBackup := true
	defer func() {
		if removeBackup {
			_ = file.Close()
			if err := dir.Remove(name); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete Dokploy database backup %s: %w", path, err))
			}
		}
	}()
	if err := runner.Run(ctx, nil, file, "exec", pg, "pg_dump", "-U", "dokploy", "-d", "dokploy"); err != nil {
		return "", fmt.Errorf("backup dokploy database to %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close dokploy database backup %s: %w", path, err)
	}
	if err := dir.Sync(); err != nil {
		return "", fmt.Errorf("sync dokploy database backup directory for %s: %w", path, err)
	}
	if err := dir.ValidatePath(); err != nil {
		return "", fmt.Errorf("verify dokploy database backup location %s: %w", path, err)
	}
	removeBackup = false
	return path, nil
}

func deleteStalePlatformProjects(ctx context.Context, runner dockerRunner, pg string, names []string, projectIDs map[string]string) ([]StalePlatformProject, error) {
	var out bytes.Buffer
	if err := runner.Run(ctx, strings.NewReader(stalePlatformCleanupSQL(names, projectIDs)), &out,
		"exec", "-i", pg, "psql", "-U", "dokploy", "-d", "dokploy", "-v", "ON_ERROR_STOP=1", "-At", "-F", "|"); err != nil {
		return nil, err
	}
	return parseDeletedStalePlatformProjects(out.String()), nil
}

func stalePlatformCleanupSQL(names []string, projectIDs map[string]string) string {
	values := make([]string, 0, len(names))
	for _, name := range names {
		id := "null"
		if projectID := strings.TrimSpace(projectIDs[name]); projectID != "" {
			id = sqlStringLiteral(projectID)
		}
		values = append(values, "("+sqlStringLiteral(name)+", "+id+")")
	}
	return fmt.Sprintf(`begin;
create temporary table bort_stale_platform_project(project_name text primary key, project_id text) on commit drop;
insert into bort_stale_platform_project(project_name, project_id) values
  %s;

do $$
declare
  bad text;
begin
  select string_agg(p.name || ' compose=' || coalesce(c.compose_count, 0) || ' domains=' || coalesce(d.domain_count, 0), ', ' order by p.name)
    into bad
  from project p
  join bort_stale_platform_project x on x.project_name = p.name
   and (x.project_id is null or x.project_id = p."projectId")
  left join lateral (
    select count(*) as compose_count
    from environment e
    join compose cmp on cmp."environmentId" = e."environmentId"
    where e."projectId" = p."projectId"
  ) c on true
  left join lateral (
    select count(*) as domain_count
    from environment e
    join compose cmp on cmp."environmentId" = e."environmentId"
    join domain dom on dom."composeId" = cmp."composeId"
    where e."projectId" = p."projectId"
  ) d on true
  where coalesce(c.compose_count, 0) <> 0 or coalesce(d.domain_count, 0) <> 0;
  if bad is not null then
    raise exception 'refusing to delete stale platform projects with attached Dokploy resources: %%', bad;
  end if;
end $$;

with deleted as (
  delete from project p
  using bort_stale_platform_project x
  where p.name = x.project_name
    and (x.project_id is null or x.project_id = p."projectId")
  returning p."projectId", p.name
)
select 'deleted', name, "projectId" from deleted order by name;
commit;
`, strings.Join(values, ",\n  "))
}

func sqlStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func parseDeletedStalePlatformProjects(output string) []StalePlatformProject {
	deleted := []StalePlatformProject{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 || parts[0] != "deleted" {
			continue
		}
		deleted = append(deleted, StalePlatformProject{Name: parts[1], ProjectID: parts[2]})
	}
	return deleted
}
