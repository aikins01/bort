package coolifylocal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/safepath"
	"github.com/aikins01/bort/internal/source"
	"github.com/aikins01/bort/internal/source/coolify"
	"github.com/aikins01/bort/internal/source/localdocker"
)

const defaultDataDir = "/data/coolify"

type Scanner struct {
	Docker  source.Scanner
	DataDir string
}

func NewScanner() *Scanner {
	return &Scanner{Docker: localdocker.NewScanner()}
}

func (s *Scanner) Scan(ctx context.Context, opts source.ScanOptions) (manifest.Manifest, error) {
	docker := s.Docker
	if docker == nil {
		docker = localdocker.NewScanner()
	}

	result, err := docker.Scan(ctx, opts)
	if err != nil {
		return manifest.Manifest{}, err
	}
	result.Source.Platform = "coolify-local"
	dataDir := s.dataDir()
	proxyMode := detectProxyMode(dataDir)
	enrichApps(&result, dataDir)
	if opts.Coolify.BaseURL != "" && opts.Coolify.Token != "" {
		apiScanner, err := coolify.NewScanner(opts.Coolify.BaseURL, opts.Coolify.Token)
		if err != nil {
			result.Warnings = append(result.Warnings, manifest.Warning{Code: "coolify.api_unavailable", Message: err.Error()})
		} else if apiResult, err := apiScanner.Scan(ctx, opts); err == nil {
			mergeAPIMetadata(&result, apiResult)
		} else {
			result.Warnings = append(result.Warnings, manifest.Warning{Code: "coolify.api_unavailable", Message: err.Error()})
		}
	}
	result.ProxyArtifacts = append(result.ProxyArtifacts, loadProxyArtifacts(dataDir)...)
	annotateProxyMode(&result, dataDir, proxyMode)
	return result, nil
}

type proxyMode string

const (
	proxyModeTraefik proxyMode = "traefik"
	proxyModeCaddy   proxyMode = "caddy"
	proxyModeUnknown proxyMode = ""
)

// detectProxyMode picks the proxy implementation by checking which
// dynamic-config directory coolify wrote on the host. coolify v4 stores
// the proxy choice in its own database (servers.proxy->type), not in
// /data/coolify/source/.env, so the filesystem layout is the only
// reliable host-side signal: caddy mode uses proxy/caddy/dynamic, while
// traefik mode uses proxy/dynamic directly.
func detectProxyMode(dataDir string) proxyMode {
	if info, err := os.Stat(filepath.Join(dataDir, "proxy", "caddy", "dynamic")); err == nil && info.IsDir() {
		return proxyModeCaddy
	}
	if info, err := os.Stat(filepath.Join(dataDir, "proxy", "dynamic")); err == nil && info.IsDir() {
		return proxyModeTraefik
	}
	return proxyModeUnknown
}

// annotateProxyMode records the detected proxy on the source platform
// metadata and surfaces a warning for any coolify-managed app that ended
// up with zero routes — operators can then audit whether bort missed a
// caddy/traefik label or whether the app uses an unsupported proxy.
func annotateProxyMode(result *manifest.Manifest, dataDir string, mode proxyMode) {
	if mode != proxyModeUnknown {
		result.Source.Platform = "coolify-local-" + string(mode)
	}
	for i := range result.Apps {
		app := &result.Apps[i]
		if app.Metadata["coolify.managed"] != "true" {
			continue
		}
		if mode != proxyModeUnknown {
			if app.Metadata == nil {
				app.Metadata = map[string]string{}
			}
			putMetadata(app.Metadata, "coolify.proxy", string(mode))
		}
		if len(app.Routes) > 0 {
			continue
		}
		message := fmt.Sprintf("no traefik or caddy routes found for app %q; proxy may be unsupported or labels missing", app.Name)
		if mode == proxyModeUnknown {
			message = fmt.Sprintf("no traefik or caddy routes found for app %q and proxy mode could not be detected from %s", app.Name, filepath.Join(dataDir, "proxy"))
		}
		app.Warnings = append(app.Warnings, manifest.Warning{Code: "proxy.unsupported_or_missing", Message: message})
	}
}

func mergeAPIMetadata(local *manifest.Manifest, api manifest.Manifest) {
	byUUID := map[string]manifest.App{}
	for _, app := range api.Apps {
		uuid := uuidFromAppID(app.ID)
		if uuid != "" {
			byUUID[uuid] = app
		}
	}
	for i := range local.Apps {
		app := &local.Apps[i]
		uuid := app.Metadata["coolify.uuid"]
		match, ok := byUUID[uuid]
		if !ok {
			continue
		}
		if app.Git == nil && match.Git != nil {
			git := *match.Git
			app.Git = &git
		}
		if app.BuildPack == "" {
			app.BuildPack = match.BuildPack
		}
		if app.Compose == nil && match.Compose != nil {
			compose := *match.Compose
			app.Compose = &compose
		} else if app.Compose != nil && match.Compose != nil && app.Compose.Resolved == "" && match.Compose.Resolved != "" {
			app.Compose.Resolved = match.Compose.Resolved
		}
	}
}

func (s *Scanner) dataDir() string {
	if strings.TrimSpace(s.DataDir) != "" {
		return s.DataDir
	}
	if value := strings.TrimSpace(os.Getenv("BORT_COOLIFY_DATA_DIR")); value != "" {
		return value
	}
	return defaultDataDir
}

func enrichApps(result *manifest.Manifest, dataDir string) {
	renamed := map[string]string{}
	for i := range result.Apps {
		oldName := result.Apps[i].Name
		enrichApp(&result.Apps[i], dataDir)
		if result.Apps[i].Name != oldName {
			renamed[oldName] = result.Apps[i].Name
		}
	}

	for i := range result.Volumes {
		for usedByIndex, appName := range result.Volumes[i].UsedBy {
			if next, ok := renamed[appName]; ok {
				result.Volumes[i].UsedBy[usedByIndex] = next
			}
		}
		sort.Strings(result.Volumes[i].UsedBy)
	}

	sort.Slice(result.Apps, func(i, j int) bool { return result.Apps[i].Name < result.Apps[j].Name })
}

func enrichApp(app *manifest.App, dataDir string) {
	labels := appLabels(app)
	if len(labels) == 0 {
		return
	}

	metadata := copyMetadata(app.Metadata)
	putMetadata(metadata, "coolify.uuid", firstNonEmpty(labels["com.docker.compose.project"], uuidFromAppID(app.ID)))
	putMetadata(metadata, "coolify.type", labels["coolify.type"])
	putMetadata(metadata, "coolify.project", labels["coolify.projectName"])
	putMetadata(metadata, "coolify.environment", labels["coolify.environmentName"])
	putMetadata(metadata, "coolify.resourceName", labels["coolify.resourceName"])
	putMetadata(metadata, "coolify.serviceName", labels["coolify.serviceName"])
	putMetadata(metadata, "coolify.composeProject", labels["com.docker.compose.project"])
	putMetadata(metadata, "coolify.composeConfigFiles", labels["com.docker.compose.project.config_files"])
	putMetadata(metadata, "coolify.composeWorkingDir", labels["com.docker.compose.project.working_dir"])
	putMetadata(metadata, "coolify.managed", labels["coolify.managed"])
	putMetadata(metadata, "migrationRole", migrationRole(labels))
	app.Metadata = metadata

	if runtime := labels["coolify.type"]; runtime != "" {
		app.Runtime = runtime
	}
	if name := resourceName(labels); name != "" {
		app.Name = name
	}

	uuid := metadata["coolify.uuid"]
	if raw, path := loadComposeRaw(dataDir, labels["coolify.type"], uuid); raw != "" {
		if app.Compose == nil {
			app.Compose = &manifest.ComposeSource{}
		}
		if app.Compose.Raw == "" {
			app.Compose.Raw = raw
		}
		putMetadata(app.Metadata, "coolify.composeFile", path)
	}
}

func loadComposeRaw(dataDir, coolifyType, uuid string) (string, string) {
	uuid = strings.TrimSpace(uuid)
	if !isCoolifyUUID(uuid) {
		return "", ""
	}
	candidates := []string{}
	switch coolifyType {
	case "service":
		candidates = []string{
			filepath.Join(dataDir, "services", uuid, "docker-compose.yml"),
			filepath.Join(dataDir, "services", uuid, "docker-compose.yaml"),
		}
	default:
		candidates = []string{
			filepath.Join(dataDir, "applications", uuid, "docker-compose.yaml"),
			filepath.Join(dataDir, "applications", uuid, "docker-compose.yml"),
			filepath.Join(dataDir, "services", uuid, "docker-compose.yml"),
		}
	}
	for _, path := range candidates {
		data, err := readContained(dataDir, path)
		if err == nil && len(data) > 0 {
			return string(data), path
		}
	}
	return "", ""
}

func loadProxyArtifacts(dataDir string) []manifest.ProxyArtifact {
	artifacts := []manifest.ProxyArtifact{}
	artifacts = append(artifacts, readProxyDir(dataDir, filepath.Join(dataDir, "proxy", "dynamic"), "traefik-dynamic")...)
	artifacts = append(artifacts, readProxyDir(dataDir, filepath.Join(dataDir, "proxy", "caddy", "dynamic"), "caddyfile")...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts
}

func readProxyDir(dataDir, dir, sourceTag string) []manifest.ProxyArtifact {
	if err := safepath.ContainedPath(dataDir, dir); err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	artifacts := []manifest.ProxyArtifact{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readContained(dataDir, path)
		if err != nil {
			continue
		}
		artifacts = append(artifacts, manifest.ProxyArtifact{Source: sourceTag, Path: path, Content: string(data)})
	}
	return artifacts
}

func isCoolifyUUID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func readContained(root, path string) ([]byte, error) {
	if err := safepath.ContainedPath(root, path); err != nil {
		return nil, err
	}
	return safepath.ReadFileNoFollow(path)
}

func appLabels(app *manifest.App) map[string]string {
	merged := map[string]string{}
	for key, value := range app.Labels {
		merged[key] = value
	}
	for _, service := range app.Services {
		for key, value := range service.Labels {
			if _, ok := merged[key]; !ok {
				merged[key] = value
			}
		}
	}
	return merged
}

func resourceName(labels map[string]string) string {
	uuid := labels["com.docker.compose.project"]
	candidates := []string{
		labels["coolify.service.subName"],
		labels["coolify.serviceName"],
		labels["coolify.resourceName"],
		labels["coolify.name"],
		uuid,
	}
	for _, candidate := range candidates {
		stripped := stripUUIDSuffix(candidate, uuid)
		if stripped != "" && stripped != uuid {
			return stripped
		}
	}
	return firstNonEmpty(candidates...)
}

func stripUUIDSuffix(name, uuid string) string {
	name = strings.TrimSpace(name)
	if name == "" || uuid == "" {
		return name
	}
	suffix := "-" + uuid
	for strings.HasSuffix(name, suffix) {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}

func migrationRole(labels map[string]string) string {
	project := labels["com.docker.compose.project"]
	workingDir := labels["com.docker.compose.project.working_dir"]
	switch {
	case labels["coolify.proxy"] == "true", project == "coolify-proxy", strings.Contains(workingDir, "/data/coolify/proxy"):
		return "platform"
	case project == "source", strings.Contains(workingDir, "/data/coolify/source"):
		return "platform"
	case project == "proxy", strings.Contains(workingDir, "/data/coolify/databases/"):
		return "support"
	default:
		return "candidate"
	}
}

func copyMetadata(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func putMetadata(metadata map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		metadata[key] = value
	}
}

func uuidFromAppID(id string) string {
	parts := strings.Split(id, ":")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
