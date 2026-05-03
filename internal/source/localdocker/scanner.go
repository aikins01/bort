package localdocker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/secrets"
	"github.com/aikins01/bort/internal/source"
)

type Scanner struct {
	DockerPath     string
	Now            func() time.Time
	runCommand     func(context.Context, ...string) ([]byte, error)
	measureCommand func(context.Context, string) (int64, int64, error)
}

const dockerInspectChunkSize = 100

func NewScanner() *Scanner {
	return &Scanner{
		DockerPath: "docker",
		Now:        time.Now,
	}
}

func (s *Scanner) Scan(ctx context.Context, opts source.ScanOptions) (manifest.Manifest, error) {
	if s.DockerPath == "" {
		s.DockerPath = "docker"
	}
	if s.Now == nil {
		s.Now = time.Now
	}

	if s.runCommand == nil {
		if _, err := exec.LookPath(s.DockerPath); err != nil {
			return manifest.Manifest{}, fmt.Errorf("docker CLI not found: %w", err)
		}
	}

	hostname, _ := os.Hostname()
	result := manifest.New(manifest.Source{Platform: "docker", Hostname: hostname}, s.Now())

	containers, err := s.inspectContainers(ctx)
	if err != nil {
		return manifest.Manifest{}, err
	}

	imageDigests, err := s.inspectImageDigests(ctx, containers)
	if err != nil {
		result.Warnings = append(result.Warnings, manifest.Warning{Code: "docker.image_inspect_failed", Message: err.Error()})
	}

	volumeUsers := map[string]map[string]struct{}{}
	appsByKey := map[string]*manifest.App{}
	for _, container := range containers {
		key, appName, platform := classifyContainer(container)
		app := appsByKey[key]
		if app == nil {
			app = &manifest.App{
				ID:       key,
				Name:     appName,
				Platform: platform,
				Runtime:  "docker",
				Labels:   appLabels(container.Labels()),
			}
			appsByKey[key] = app
		}

		service := container.toService(opts.IncludeEnvValues)
		if digest := imageDigests[container.Image]; digest != "" {
			service.ImageDigest = digest
		}
		app.Services = append(app.Services, service)
		app.Routes = mergeRoutes(app.Routes, routesFromLabels(service.Name, container.Labels()))

		for _, mount := range service.Mounts {
			if mount.Type != "volume" || mount.Name == "" {
				continue
			}
			if volumeUsers[mount.Name] == nil {
				volumeUsers[mount.Name] = map[string]struct{}{}
			}
			volumeUsers[mount.Name][app.Name] = struct{}{}
		}
	}

	for _, app := range appsByKey {
		sort.Slice(app.Services, func(i, j int) bool { return app.Services[i].Name < app.Services[j].Name })
		sort.Slice(app.Routes, func(i, j int) bool { return app.Routes[i].Host < app.Routes[j].Host })
		result.Apps = append(result.Apps, *app)
	}
	sort.Slice(result.Apps, func(i, j int) bool { return result.Apps[i].Name < result.Apps[j].Name })

	volumes, err := s.inspectVolumes(ctx)
	if err != nil {
		result.Warnings = append(result.Warnings, manifest.Warning{Code: "docker.volume_inspect_failed", Message: err.Error()})
	} else {
		for i := range volumes {
			for appName := range volumeUsers[volumes[i].Name] {
				volumes[i].UsedBy = append(volumes[i].UsedBy, appName)
			}
			sort.Strings(volumes[i].UsedBy)
			if len(volumes[i].UsedBy) > 0 && volumes[i].Mountpoint != "" {
				if size, files, err := s.measureVolume(ctx, volumes[i].Mountpoint); err == nil {
					volumes[i].SizeBytes = size
					volumes[i].FileCount = files
				}
			}
		}
		result.Volumes = volumes
	}

	networks, err := s.inspectNetworks(ctx)
	if err != nil {
		result.Warnings = append(result.Warnings, manifest.Warning{Code: "docker.network_inspect_failed", Message: err.Error()})
	} else {
		result.Networks = networks
	}

	return result, nil
}

func (s *Scanner) inspectContainers(ctx context.Context) ([]containerInspect, error) {
	idsRaw, err := s.run(ctx, "ps", "-aq")
	if err != nil {
		return nil, fmt.Errorf("list docker containers: %w", err)
	}

	ids := strings.Fields(string(idsRaw))
	if len(ids) == 0 {
		return nil, nil
	}

	containers := []containerInspect{}
	for _, chunk := range chunks(ids, dockerInspectChunkSize) {
		args := append([]string{"inspect"}, chunk...)
		raw, err := s.run(ctx, args...)
		if err != nil {
			return nil, fmt.Errorf("inspect docker containers: %w", err)
		}

		var inspected []containerInspect
		if err := json.Unmarshal(raw, &inspected); err != nil {
			return nil, err
		}
		containers = append(containers, inspected...)
	}
	return containers, nil
}

func (s *Scanner) inspectVolumes(ctx context.Context) ([]manifest.Volume, error) {
	namesRaw, err := s.run(ctx, "volume", "ls", "-q")
	if err != nil {
		return nil, fmt.Errorf("list docker volumes: %w", err)
	}

	names := strings.Fields(string(namesRaw))
	if len(names) == 0 {
		return nil, nil
	}

	inspected := []volumeInspect{}
	for _, chunk := range chunks(names, dockerInspectChunkSize) {
		args := append([]string{"volume", "inspect"}, chunk...)
		raw, err := s.run(ctx, args...)
		if err != nil {
			return nil, fmt.Errorf("inspect docker volumes: %w", err)
		}

		var chunkInspected []volumeInspect
		if err := json.Unmarshal(raw, &chunkInspected); err != nil {
			return nil, err
		}
		inspected = append(inspected, chunkInspected...)
	}

	volumes := make([]manifest.Volume, 0, len(inspected))
	for _, volume := range inspected {
		volumes = append(volumes, manifest.Volume{
			Name:       volume.Name,
			Driver:     volume.Driver,
			Mountpoint: volume.Mountpoint,
			Scope:      volume.Scope,
			Labels:     cleanMap(volume.Labels),
			Options:    cleanMap(volume.Options),
		})
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, nil
}

func (s *Scanner) inspectNetworks(ctx context.Context) ([]manifest.Network, error) {
	idsRaw, err := s.run(ctx, "network", "ls", "-q")
	if err != nil {
		return nil, fmt.Errorf("list docker networks: %w", err)
	}

	ids := strings.Fields(string(idsRaw))
	if len(ids) == 0 {
		return nil, nil
	}

	inspected := []networkInspect{}
	for _, chunk := range chunks(ids, dockerInspectChunkSize) {
		args := append([]string{"network", "inspect"}, chunk...)
		raw, err := s.run(ctx, args...)
		if err != nil {
			return nil, fmt.Errorf("inspect docker networks: %w", err)
		}

		var chunkInspected []networkInspect
		if err := json.Unmarshal(raw, &chunkInspected); err != nil {
			return nil, err
		}
		inspected = append(inspected, chunkInspected...)
	}

	networks := make([]manifest.Network, 0, len(inspected))
	for _, network := range inspected {
		networks = append(networks, manifest.Network{
			ID:       shortID(network.ID),
			Name:     network.Name,
			Driver:   network.Driver,
			Scope:    network.Scope,
			Internal: network.Internal,
			Labels:   cleanMap(network.Labels),
		})
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	return networks, nil
}

// inspectImageDigests resolves repo digests (sha256 from RepoDigests)
// for every unique image id referenced by the running containers, so
// the manifest captures what should be pulled on the target — not just
// the floating tag the source ran.
func (s *Scanner) inspectImageDigests(ctx context.Context, containers []containerInspect) (map[string]string, error) {
	uniqueIDs := map[string]struct{}{}
	for _, c := range containers {
		if c.Image != "" {
			uniqueIDs[c.Image] = struct{}{}
		}
	}
	if len(uniqueIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(uniqueIDs))
	for id := range uniqueIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	digests := map[string]string{}
	for _, chunk := range chunks(ids, dockerInspectChunkSize) {
		args := append([]string{"image", "inspect"}, chunk...)
		raw, err := s.run(ctx, args...)
		if err != nil {
			return digests, fmt.Errorf("inspect docker images: %w", err)
		}
		var inspected []imageInspect
		if err := json.Unmarshal(raw, &inspected); err != nil {
			return digests, err
		}
		for _, image := range inspected {
			if len(image.RepoDigests) > 0 {
				digests[image.ID] = image.RepoDigests[0]
			}
		}
	}
	return digests, nil
}

// measureVolume reports apparent size in bytes and file count for the
// given mountpoint. The fields feed sync window planning. We use du -sb
// (apparent bytes) and find -printf so we don't depend on docker
// system df, which only reports for named volumes and skips bind mounts.
// Best-effort: any failure means the volume's size stays 0.
func (s *Scanner) measureVolume(ctx context.Context, mountpoint string) (int64, int64, error) {
	if s.measureCommand != nil {
		return s.measureCommand(ctx, mountpoint)
	}
	sizeOut, err := runHost(ctx, "du", "-sb", "--", mountpoint)
	if err != nil {
		return 0, 0, err
	}
	size, err := parseDuBytes(sizeOut)
	if err != nil {
		return 0, 0, err
	}
	countOut, err := runHost(ctx, "find", mountpoint, "-mindepth", "1", "-print0")
	if err != nil {
		return size, 0, nil
	}
	return size, int64(bytes.Count(countOut, []byte{0})), nil
}

func runHost(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func parseDuBytes(out []byte) (int64, error) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty du output")
	}
	return parseInt64(fields[0])
}

func parseInt64(value string) (int64, error) {
	var n int64
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("non-numeric: %q", value)
		}
		n = n*10 + int64(ch-'0')
	}
	return n, nil
}

func (s *Scanner) run(ctx context.Context, args ...string) ([]byte, error) {
	if s.runCommand != nil {
		return s.runCommand(ctx, args...)
	}

	cmd := exec.CommandContext(ctx, s.DockerPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("docker %s: %s", strings.Join(args, " "), message)
	}
	return out, nil
}

type containerInspect struct {
	ID              string `json:"Id"`
	Name            string `json:"Name"`
	Image           string `json:"Image"`
	Config          configInspect
	State           stateInspect
	Mounts          []mountInspect
	NetworkSettings networkSettingsInspect
}

type configInspect struct {
	Image       string             `json:"Image"`
	Env         []string           `json:"Env"`
	Labels      map[string]string  `json:"Labels"`
	Healthcheck *healthcheckConfig `json:"Healthcheck,omitempty"`
}

type healthcheckConfig struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	Retries     int      `json:"Retries"`
	StartPeriod int64    `json:"StartPeriod"`
}

type stateInspect struct {
	Status string `json:"Status"`
}

type mountInspect struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type networkSettingsInspect struct {
	Ports    map[string][]portBinding        `json:"Ports"`
	Networks map[string]networkAttachInspect `json:"Networks"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type networkAttachInspect struct {
	NetworkID string `json:"NetworkID"`
	IPAddress string `json:"IPAddress"`
}

type imageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

type volumeInspect struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	Scope      string            `json:"Scope"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
}

type networkInspect struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Scope    string            `json:"Scope"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
}

func (c containerInspect) Labels() map[string]string {
	return cleanMap(c.Config.Labels)
}

func (c containerInspect) toService(includeEnvValues bool) manifest.Service {
	return manifest.Service{
		ID:          shortID(c.ID),
		Name:        strings.TrimPrefix(c.Name, "/"),
		Image:       c.Config.Image,
		ImageID:     c.Image,
		Status:      c.State.Status,
		Healthcheck: healthcheckFromConfig(c.Config.Healthcheck),
		Environment: envVars(c.Config.Env, includeEnvValues),
		Mounts:      mounts(c.Mounts),
		Ports:       ports(c.NetworkSettings.Ports),
		Networks:    networks(c.NetworkSettings.Networks),
		Labels:      c.Labels(),
	}
}

// docker reports healthcheck durations in nanoseconds; we expose them as
// human-readable strings so manifests stay readable and stable across
// docker versions.
func healthcheckFromConfig(hc *healthcheckConfig) *manifest.Healthcheck {
	if hc == nil || len(hc.Test) == 0 {
		return nil
	}
	return &manifest.Healthcheck{
		Test:        hc.Test,
		Interval:    durationString(hc.Interval),
		Timeout:     durationString(hc.Timeout),
		Retries:     hc.Retries,
		StartPeriod: durationString(hc.StartPeriod),
	}
}

func durationString(nanos int64) string {
	if nanos <= 0 {
		return ""
	}
	return time.Duration(nanos).String()
}

func classifyContainer(container containerInspect) (key, name, platform string) {
	labels := container.Labels()
	platform = detectPlatform(labels)

	if project := labels["com.docker.compose.project"]; project != "" {
		return "compose:" + project, project, platform
	}

	if appID := labels["coolify.applicationId"]; appID != "" {
		return "coolify:application:" + appID, "coolify-application-" + appID, "coolify"
	}

	serviceID := firstNonEmpty(labels["coolify.serviceId"], labels["coolify.serviceUuid"], labels["coolify.service_uuid"])
	if serviceID != "" {
		return "coolify:service:" + serviceID, "coolify-service-" + serviceID, "coolify"
	}

	containerName := strings.TrimPrefix(container.Name, "/")
	if containerName == "" {
		containerName = shortID(container.ID)
	}

	return "container:" + shortID(container.ID), containerName, platform
}

func detectPlatform(labels map[string]string) string {
	for key, value := range labels {
		combined := strings.ToLower(key + "=" + value)
		switch {
		case strings.Contains(combined, "coolify"):
			return "coolify"
		case strings.Contains(combined, "dokploy"):
			return "dokploy"
		}
	}

	workingDir := labels["com.docker.compose.project.working_dir"]
	if strings.Contains(workingDir, "/etc/dokploy/") {
		return "dokploy"
	}

	return "docker"
}

func appLabels(labels map[string]string) map[string]string {
	selected := map[string]string{}
	for key, value := range labels {
		if strings.HasPrefix(key, "com.docker.compose.") || strings.HasPrefix(key, "coolify.") || strings.Contains(strings.ToLower(key), "dokploy") {
			selected[key] = value
		}
	}
	return cleanMap(selected)
}

func envVars(raw []string, includeValues bool) []manifest.EnvVar {
	vars := make([]manifest.EnvVar, 0, len(raw))
	for _, item := range raw {
		name, value, hasValue := strings.Cut(item, "=")
		if name == "" {
			continue
		}

		env := manifest.EnvVar{Name: name, Sensitive: secrets.IsSensitiveName(name)}
		if includeValues && hasValue {
			env.Value = value
			env.ValueKnown = true
		}
		vars = append(vars, env)
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars
}

func mounts(raw []mountInspect) []manifest.Mount {
	mounts := make([]manifest.Mount, 0, len(raw))
	for _, mount := range raw {
		mounts = append(mounts, manifest.Mount{
			Type:   mount.Type,
			Name:   mount.Name,
			Source: mount.Source,
			Target: mount.Destination,
			RW:     mount.RW,
		})
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Target < mounts[j].Target })
	return mounts
}

func ports(raw map[string][]portBinding) []manifest.Port {
	ports := []manifest.Port{}
	for containerPort, bindings := range raw {
		if len(bindings) == 0 {
			ports = append(ports, manifest.Port{ContainerPort: containerPort})
			continue
		}
		for _, binding := range bindings {
			ports = append(ports, manifest.Port{ContainerPort: containerPort, HostIP: binding.HostIP, HostPort: binding.HostPort})
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].ContainerPort == ports[j].ContainerPort {
			return ports[i].HostPort < ports[j].HostPort
		}
		return ports[i].ContainerPort < ports[j].ContainerPort
	})
	return ports
}

func networks(raw map[string]networkAttachInspect) []manifest.ServiceNetwork {
	networks := make([]manifest.ServiceNetwork, 0, len(raw))
	for name, network := range raw {
		networks = append(networks, manifest.ServiceNetwork{Name: name, NetworkID: shortID(network.NetworkID), IPAddress: network.IPAddress})
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	return networks
}

var (
	hostCallPattern     = regexp.MustCompile(`Host\(([^)]*)\)`)
	hostBacktickPattern = regexp.MustCompile("`([^`]+)`")
	hostQuotePattern    = regexp.MustCompile(`"([^"]+)"`)
)

func routesFromLabels(serviceName string, labels map[string]string) []manifest.Route {
	servicePorts := map[string]string{}
	routerServices := map[string]string{}
	for key, value := range labels {
		if strings.HasPrefix(key, "traefik.http.services.") && strings.HasSuffix(key, ".loadbalancer.server.port") {
			name := strings.TrimSuffix(strings.TrimPrefix(key, "traefik.http.services."), ".loadbalancer.server.port")
			servicePorts[name] = value
		}

		if strings.HasPrefix(key, "traefik.http.routers.") && strings.HasSuffix(key, ".service") {
			name := strings.TrimSuffix(strings.TrimPrefix(key, "traefik.http.routers."), ".service")
			routerServices[name] = value
		}
	}

	routes := []manifest.Route{}
	for key, value := range labels {
		if !strings.HasPrefix(key, "traefik.http.routers.") || !strings.HasSuffix(key, ".rule") {
			continue
		}

		routerName := strings.TrimSuffix(strings.TrimPrefix(key, "traefik.http.routers."), ".rule")
		port := servicePorts[routerServices[routerName]]
		if port == "" {
			port = servicePorts[routerName]
		}

		for _, host := range hostsFromRule(value) {
			if net.ParseIP(host) != nil {
				continue
			}
			routes = append(routes, manifest.Route{Host: host, ServiceName: serviceName, Port: port, Source: key})
		}
	}
	return routes
}

func hostsFromRule(rule string) []string {
	hosts := []string{}
	for _, call := range hostCallPattern.FindAllStringSubmatch(rule, -1) {
		if len(call) != 2 {
			continue
		}

		for _, match := range hostBacktickPattern.FindAllStringSubmatch(call[1], -1) {
			if len(match) == 2 {
				hosts = append(hosts, match[1])
			}
		}
		for _, match := range hostQuotePattern.FindAllStringSubmatch(call[1], -1) {
			if len(match) == 2 {
				hosts = append(hosts, match[1])
			}
		}
	}
	return hosts
}

func mergeRoutes(existing, next []manifest.Route) []manifest.Route {
	seen := map[string]manifest.Route{}
	for _, route := range existing {
		seen[route.Host+"|"+route.ServiceName+"|"+route.Port] = route
	}
	for _, route := range next {
		seen[route.Host+"|"+route.ServiceName+"|"+route.Port] = route
	}

	merged := make([]manifest.Route, 0, len(seen))
	for _, route := range seen {
		merged = append(merged, route)
	}
	return merged
}

func cleanMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	cleaned := make(map[string]string, len(values))
	for key, value := range values {
		if key == "" {
			continue
		}
		cleaned[key] = value
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func chunks(values []string, size int) [][]string {
	if size <= 0 {
		size = len(values)
	}

	chunked := [][]string{}
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		chunked = append(chunked, values[start:end])
	}
	return chunked
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
