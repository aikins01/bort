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
	DockerPath string
	Now        func() time.Time
	runCommand func(context.Context, ...string) ([]byte, error)
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

	if _, err := exec.LookPath(s.DockerPath); err != nil {
		return manifest.Manifest{}, fmt.Errorf("docker CLI not found: %w", err)
	}

	hostname, _ := os.Hostname()
	result := manifest.New(manifest.Source{Platform: "docker", Hostname: hostname}, s.Now())

	containers, err := s.inspectContainers(ctx)
	if err != nil {
		return manifest.Manifest{}, err
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
	Config          configInspect
	State           stateInspect
	Mounts          []mountInspect
	NetworkSettings networkSettingsInspect
}

type configInspect struct {
	Image  string            `json:"Image"`
	Env    []string          `json:"Env"`
	Labels map[string]string `json:"Labels"`
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
		Status:      c.State.Status,
		Environment: envVars(c.Config.Env, includeEnvValues),
		Mounts:      mounts(c.Mounts),
		Ports:       ports(c.NetworkSettings.Ports),
		Networks:    networks(c.NetworkSettings.Networks),
		Labels:      c.Labels(),
	}
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
