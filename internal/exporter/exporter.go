package exporter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/planutil"
	"gopkg.in/yaml.v3"
)

type Options struct {
	OutputDir        string
	AppName          string
	IncludeEnvValues bool
}

const (
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
)

type Summary struct {
	OutputDir   string       `json:"outputDir"`
	GeneratedAt time.Time    `json:"generatedAt"`
	Source      string       `json:"source"`
	EnvMode     string       `json:"envMode,omitempty"`
	Apps        []AppSummary `json:"apps"`
}

type AppSummary struct {
	Name             string        `json:"name"`
	Directory        string        `json:"directory"`
	Role             string        `json:"role,omitempty"`
	ProjectGroup     *ProjectGroup `json:"projectGroup,omitempty"`
	PrivateEnvValues bool          `json:"privateEnvValues,omitempty"`
	Routes           []string      `json:"routes,omitempty"`
	Warnings         []string      `json:"warnings,omitempty"`
}

type ProjectGroup struct {
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	Source      string `json:"source,omitempty"`
}

func Export(m manifest.Manifest, opts Options) (Summary, error) {
	if opts.OutputDir == "" {
		opts.OutputDir = "bort-bundle"
	}

	apps := selectedApps(m.Apps, opts.AppName)
	if len(apps) == 0 {
		if opts.AppName != "" {
			return Summary{}, fmt.Errorf("app %q not found in manifest", opts.AppName)
		}
		return Summary{}, fmt.Errorf("manifest has no apps to export")
	}

	if err := ensurePrivateDir(opts.OutputDir); err != nil {
		return Summary{}, err
	}

	summary := Summary{
		OutputDir:   opts.OutputDir,
		GeneratedAt: time.Now().UTC(),
		Source:      m.Source.Platform,
		EnvMode:     exportEnvMode(opts),
	}
	projectGroups := projectGroupsForApps(m, apps)

	usedDirs := map[string]int{}
	for _, app := range apps {
		dirName := uniqueDirName(planutil.Slug(app.Name), usedDirs)
		appDir := filepath.Join(opts.OutputDir, dirName)
		if err := ensurePrivateDir(appDir); err != nil {
			return Summary{}, err
		}

		topology := analyzer.TopologyForAppInManifest(m, app)
		warnings, err := exportApp(appDir, app, topology, opts)
		if err != nil {
			return Summary{}, err
		}

		summary.Apps = append(summary.Apps, AppSummary{
			Name:             app.Name,
			Directory:        filepath.ToSlash(dirName),
			Role:             migrationRole(app),
			ProjectGroup:     projectGroups[appKey(app)],
			PrivateEnvValues: opts.IncludeEnvValues,
			Routes:           routeHosts(app.Routes),
			Warnings:         warnings,
		})
	}

	if err := writeJSON(filepath.Join(opts.OutputDir, "index.json"), summary); err != nil {
		return Summary{}, err
	}

	return summary, nil
}

func exportApp(appDir string, app manifest.App, topology analyzer.Topology, opts Options) ([]string, error) {
	warnings := []string{}
	compose, composeWarnings, serviceEnvFiles := composeForApp(app, opts.IncludeEnvValues)
	warnings = append(warnings, composeWarnings...)
	if names := analyzer.CoolifyServiceMagicEnvNames(app); len(names) > 0 {
		warnings = append(warnings, "preserved Coolify service magic env vars for review: "+strings.Join(names, ", "))
	}
	appEnvironment := exportableEnvVars(app.Environment)

	files := map[string][]byte{
		"compose.yaml":         []byte(compose),
		".env.example":         []byte(envExample(appEnvironment, false)),
		"migration-report.md":  []byte(report(app, warnings)),
		"migration-runbook.md": []byte(runbook(app, topology, warnings)),
	}
	for _, envFile := range serviceEnvFilesForMode(app.Services, false) {
		files[envFile.Name] = []byte(envExample(envFile.Vars, false))
	}
	if opts.IncludeEnvValues {
		if len(appEnvironment) > 0 {
			files[".env"] = []byte(envExample(appEnvironment, true))
		}
		for _, envFile := range serviceEnvFiles {
			files[envFile.Name] = []byte(envExample(envFile.Vars, true))
		}
	}

	for name, contents := range files {
		if err := writePrivateFile(filepath.Join(appDir, name), contents); err != nil {
			return nil, err
		}
	}

	if err := writeJSON(filepath.Join(appDir, "routes.json"), app.Routes); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(appDir, "storages.json"), app.Storages); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(appDir, "topology.json"), topology); err != nil {
		return nil, err
	}

	return warnings, nil
}

func exportEnvMode(opts Options) string {
	if opts.IncludeEnvValues {
		return "include-values"
	}
	return "redacted"
}

type envFile struct {
	Name string
	Vars []manifest.EnvVar
}

func composeForApp(app manifest.App, includePrivateValues bool) (string, []string, []envFile) {
	warnings := []string{}
	if app.Compose != nil {
		if strings.TrimSpace(app.Compose.Raw) != "" {
			compose, sanitized := sanitizeRawComposeForDokploy(app.Compose.Raw)
			if sanitized {
				warnings = append(warnings, "removed source platform labels/env from compose before writing Dokploy bundle")
			}
			if names := composeCoolifyServiceMagicEnvNames(compose); len(names) > 0 {
				warnings = append(warnings, "preserved Coolify service magic env vars in raw compose for review: "+strings.Join(names, ", "))
			}
			return ensureTrailingNewline(compose), warnings, serviceEnvFilesForMode(app.Services, includePrivateValues)
		}
		if strings.TrimSpace(app.Compose.Resolved) != "" {
			warnings = append(warnings, "skipped resolved compose because it may contain interpolated secret values")
		}
	}

	if len(app.Services) == 0 {
		warnings = append(warnings, "no services were present in the manifest")
		return "services: {}\n", warnings, nil
	}

	var builder strings.Builder
	volumes := map[string]struct{}{}
	appEnvironment := exportableEnvVars(app.Environment)
	serviceEnvFileMap := serviceEnvFileMapForMode(app.Services, includePrivateValues)
	builder.WriteString("services:\n")
	for index, service := range app.Services {
		name := planutil.Slug(service.Name)
		if name == "" {
			name = "app"
		}
		builder.WriteString(fmt.Sprintf("  %s:\n", name))
		if service.Image != "" {
			builder.WriteString(fmt.Sprintf("    image: %s\n", quoteYAML(service.Image)))
		} else {
			builder.WriteString("    image: TODO_REPLACE_IMAGE\n")
		}
		builder.WriteString("    restart: unless-stopped\n")
		envFiles := []string{}
		if len(appEnvironment) > 0 {
			envFiles = append(envFiles, appEnvFileName(includePrivateValues))
		}
		if envFile, ok := serviceEnvFileMap[index]; ok {
			envFiles = append(envFiles, envFile.Name)
		}
		if len(envFiles) > 0 {
			builder.WriteString("    env_file:\n")
			for _, envFile := range envFiles {
				builder.WriteString(fmt.Sprintf("      - %s\n", quoteYAML(envFile)))
			}
		}
		if len(service.Ports) > 0 {
			builder.WriteString("    expose:\n")
			for _, port := range service.Ports {
				containerPort := strings.Split(port.ContainerPort, "/")[0]
				if containerPort != "" {
					builder.WriteString(fmt.Sprintf("      - %s\n", quoteYAML(containerPort)))
				}
			}
		}
		if len(service.Mounts) > 0 {
			builder.WriteString("    volumes:\n")
			for _, mount := range service.Mounts {
				source := mount.Source
				if mount.Type == "volume" && mount.Name != "" {
					source = mount.Name
					volumes[mount.Name] = struct{}{}
				}
				if source == "" {
					source = "TODO_REPLACE_SOURCE"
				}
				builder.WriteString(fmt.Sprintf("      - %s\n", quoteYAML(source+":"+mount.Target)))
			}
		}
	}

	if len(volumes) > 0 {
		builder.WriteString("volumes:\n")
		for _, volume := range sortedKeys(volumes) {
			builder.WriteString(fmt.Sprintf("  %s: {}\n", quoteYAML(volume)))
		}
	}

	warnings = append(warnings, "generated compose from discovered container metadata")
	return builder.String(), warnings, sortedEnvFiles(serviceEnvFileMap)
}

func serviceEnvFilesForMode(services []manifest.Service, includePrivateValues bool) []envFile {
	return sortedEnvFiles(serviceEnvFileMapForMode(services, includePrivateValues))
}

func serviceEnvFileMapForMode(services []manifest.Service, includePrivateValues bool) map[int]envFile {
	usedNames := map[string]int{}
	files := map[int]envFile{}
	for index, service := range services {
		environment := exportableEnvVars(service.Environment)
		if len(environment) == 0 {
			continue
		}
		base := planutil.Slug(service.Name)
		if base == "" {
			base = "service"
		}
		files[index] = envFile{Name: serviceEnvFileName(uniqueDirName(base, usedNames), includePrivateValues), Vars: environment}
	}
	return files
}

func exportableEnvVars(envs []manifest.EnvVar) []manifest.EnvVar {
	filtered := make([]manifest.EnvVar, 0, len(envs))
	for _, env := range envs {
		if isSourcePlatformEnvName(env.Name) {
			continue
		}
		filtered = append(filtered, env)
	}
	return filtered
}

func sanitizeRawComposeForDokploy(raw string) (string, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return raw, false
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return raw, false
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return raw, false
	}
	changed := false
	for i := 1; i < len(services.Content); i += 2 {
		service := services.Content[i]
		if service.Kind != yaml.MappingNode {
			continue
		}
		if sanitizeComposeServiceLabels(service) {
			changed = true
		}
		if sanitizeComposeServiceEnvironment(service) {
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return raw, false
	}
	return string(out), true
}

func sanitizeComposeServiceLabels(service *yaml.Node) bool {
	labels := mappingValue(service, "labels")
	if labels == nil {
		return false
	}
	changed := false
	switch labels.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(labels.Content); {
			key := labels.Content[i].Value
			if isSourcePlatformLabel(key) {
				labels.Content = append(labels.Content[:i], labels.Content[i+2:]...)
				changed = true
				continue
			}
			i += 2
		}
	case yaml.SequenceNode:
		kept := labels.Content[:0]
		for _, item := range labels.Content {
			if item.Kind == yaml.ScalarNode && isSourcePlatformLabel(labelKey(item.Value)) {
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		labels.Content = kept
	}
	if changed && len(labels.Content) == 0 {
		removeMappingKey(service, "labels")
	}
	return changed
}

func sanitizeComposeServiceEnvironment(service *yaml.Node) bool {
	environment := mappingValue(service, "environment")
	if environment == nil {
		return false
	}
	changed := false
	switch environment.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(environment.Content); {
			key := environment.Content[i].Value
			if isSourcePlatformEnvName(key) {
				environment.Content = append(environment.Content[:i], environment.Content[i+2:]...)
				changed = true
				continue
			}
			i += 2
		}
	case yaml.SequenceNode:
		kept := environment.Content[:0]
		for _, item := range environment.Content {
			if item.Kind == yaml.ScalarNode && isSourcePlatformEnvName(envAssignmentKey(item.Value)) {
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		environment.Content = kept
	}
	if changed && len(environment.Content) == 0 {
		removeMappingKey(service, "environment")
	}
	return changed
}

func labelKey(value string) string {
	key, _, _ := strings.Cut(strings.TrimSpace(value), "=")
	return strings.TrimSpace(key)
}

func envAssignmentKey(value string) string {
	key, _, _ := strings.Cut(strings.TrimSpace(value), "=")
	return strings.TrimSpace(key)
}

func isSourcePlatformLabel(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(key, "coolify.") ||
		key == "traefik.enable" ||
		strings.HasPrefix(key, "traefik.") ||
		strings.HasPrefix(key, "caddy")
}

func isSourcePlatformEnvName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(name, "COOLIFY_") || name == "SOURCE_COMMIT"
}

func composeCoolifyServiceMagicEnvNames(raw string) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil
	}
	names := []string{}
	for i := 1; i < len(services.Content); i += 2 {
		service := services.Content[i]
		if service.Kind != yaml.MappingNode {
			continue
		}
		names = append(names, composeServiceMagicEnvNames(service)...)
	}
	return planutil.UniqueStrings(names)
}

func composeServiceMagicEnvNames(service *yaml.Node) []string {
	environment := mappingValue(service, "environment")
	if environment == nil {
		return nil
	}
	names := []string{}
	switch environment.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(environment.Content); i += 2 {
			name := strings.ToUpper(strings.TrimSpace(environment.Content[i].Value))
			if isCoolifyServiceMagicEnvName(name) {
				names = append(names, name)
			}
		}
	case yaml.SequenceNode:
		for _, item := range environment.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			name := strings.ToUpper(strings.TrimSpace(envAssignmentKey(item.Value)))
			if isCoolifyServiceMagicEnvName(name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func isCoolifyServiceMagicEnvName(name string) bool {
	return strings.HasPrefix(name, "SERVICE_FQDN_") || strings.HasPrefix(name, "SERVICE_URL_") || strings.HasPrefix(name, "SERVICE_NAME_")
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func removeMappingKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		node.Content = append(node.Content[:i], node.Content[i+2:]...)
		return true
	}
	return false
}

func appEnvFileName(includePrivateValues bool) string {
	if includePrivateValues {
		return ".env"
	}
	return ".env.example"
}

func serviceEnvFileName(base string, includePrivateValues bool) string {
	name := ".env." + base
	if !includePrivateValues {
		name += ".example"
	}
	return name
}

func sortedEnvFiles(files map[int]envFile) []envFile {
	indexes := make([]int, 0, len(files))
	for index := range files {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	sorted := make([]envFile, 0, len(indexes))
	for _, index := range indexes {
		sorted = append(sorted, files[index])
	}
	return sorted
}

func envExample(envs []manifest.EnvVar, includePrivateValues bool) string {
	vars := map[string]manifest.EnvVar{}
	for _, env := range envs {
		vars[env.Name] = env
	}

	names := make([]string, 0, len(vars))
	for name := range vars {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var builder strings.Builder
	for _, name := range names {
		env := vars[name]
		value := ""
		if env.ValueKnown && (includePrivateValues || !env.Sensitive) {
			value = env.Value
		}
		builder.WriteString(name)
		builder.WriteString("=")
		if includePrivateValues && env.ValueKnown && value == "" {
			builder.WriteString("\"\"")
		} else {
			builder.WriteString(escapeEnvValue(value))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

func report(app manifest.App, warnings []string) string {
	var builder strings.Builder
	builder.WriteString("# migration report\n\n")
	builder.WriteString(fmt.Sprintf("app: `%s`\n\n", app.Name))
	builder.WriteString(fmt.Sprintf("platform: `%s`\n\n", planutil.Fallback(app.Platform, "unknown")))
	builder.WriteString(fmt.Sprintf("runtime: `%s`\n\n", planutil.Fallback(app.Runtime, "unknown")))
	if app.BuildPack != "" {
		builder.WriteString(fmt.Sprintf("build pack: `%s`\n\n", app.BuildPack))
	}
	if app.Git != nil {
		builder.WriteString("## git\n\n")
		builder.WriteString(fmt.Sprintf("repository: `%s`\n\n", planutil.Fallback(planutil.RedactRepositoryCredentials(app.Git.Repository), "unknown")))
		builder.WriteString(fmt.Sprintf("branch: `%s`\n\n", planutil.Fallback(app.Git.Branch, "unknown")))
		if app.Git.Provider != "" {
			builder.WriteString(fmt.Sprintf("provider: `%s`\n\n", app.Git.Provider))
		}
		builder.WriteString("Bort does not copy Coolify GitHub App, deploy-key, or webhook credentials into Dokploy. The migration bundle deploys the current raw compose/image snapshot; connect the repository in Dokploy after cutover if you want future Git deploys.\n\n")
	}

	builder.WriteString("## routes\n\n")
	if len(app.Routes) == 0 {
		builder.WriteString("no routes detected.\n\n")
	} else {
		for _, route := range app.Routes {
			builder.WriteString(fmt.Sprintf("- `%s` -> `%s`", route.Host, planutil.Fallback(route.ServiceName, app.Name)))
			if route.Port != "" {
				builder.WriteString(fmt.Sprintf(" port `%s`", route.Port))
			}
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## state\n\n")
	if len(app.Storages) == 0 && mountCount(app) == 0 {
		builder.WriteString("no storage detected in the manifest.\n\n")
	} else {
		for _, storage := range app.Storages {
			builder.WriteString(fmt.Sprintf("- `%s` `%s` -> `%s`\n", planutil.Fallback(storage.Type, "storage"), planutil.Fallback(storage.Name, storage.Source), storage.Target))
		}
		for _, service := range app.Services {
			for _, mount := range service.Mounts {
				builder.WriteString(fmt.Sprintf("- `%s` `%s` -> `%s`\n", mount.Type, planutil.Fallback(mount.Name, mount.Source), mount.Target))
			}
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## warnings\n\n")
	if len(warnings) == 0 && len(app.Warnings) == 0 {
		builder.WriteString("none.\n")
	} else {
		for _, warning := range warnings {
			builder.WriteString(fmt.Sprintf("- %s\n", warning))
		}
		for _, warning := range app.Warnings {
			builder.WriteString(fmt.Sprintf("- %s: %s\n", warning.Code, warning.Message))
		}
	}

	return builder.String()
}

func runbook(app manifest.App, topology analyzer.Topology, warnings []string) string {
	var builder strings.Builder
	builder.WriteString("# migration runbook\n\n")
	builder.WriteString(fmt.Sprintf("app: `%s`\n\n", app.Name))
	builder.WriteString(fmt.Sprintf("role: `%s`\n\n", planutil.Fallback(app.Metadata["migrationRole"], "unknown")))
	builder.WriteString(fmt.Sprintf("runtime: `%s`\n\n", planutil.Fallback(app.Runtime, "unknown")))

	builder.WriteString("## preflight\n\n")
	if len(topology.RiskReasons) == 0 && len(warnings) == 0 {
		builder.WriteString("- no plan risks were detected from the exported manifest.\n")
	} else {
		for _, risk := range topology.RiskReasons {
			builder.WriteString(fmt.Sprintf("- `%s` `%s`: %s\n", risk.Severity, risk.Code, risk.Message))
		}
		for _, warning := range warnings {
			builder.WriteString(fmt.Sprintf("- `warn` `export`: %s\n", warning))
		}
	}
	builder.WriteString("\n")

	builder.WriteString("## deploy artifact\n\n")
	if app.Compose != nil && strings.TrimSpace(app.Compose.Raw) != "" {
		builder.WriteString("- use `compose.yaml`; raw compose was captured from the source manifest.\n")
	} else {
		builder.WriteString("- review generated `compose.yaml` before creating target resources.\n")
	}
	if analyzer.HasServiceImage(app) {
		builder.WriteString("- source image metadata was captured for at least one service.\n")
	}
	builder.WriteString("- fill `.env.example` and any service-specific `.env.*.example` files before deploy.\n\n")

	if topology.SourceControl != nil {
		builder.WriteString("## source control\n\n")
		if topology.SourceControl.Repository != "" {
			builder.WriteString(fmt.Sprintf("- repository `%s`", topology.SourceControl.Repository))
			if topology.SourceControl.Branch != "" {
				builder.WriteString(fmt.Sprintf(" branch `%s`", topology.SourceControl.Branch))
			}
			builder.WriteString(".\n")
		}
		builder.WriteString("- do not reuse Coolify GitHub App/deploy-key/webhook secrets directly in Dokploy; add or select a Dokploy source connection after cutover if this app should keep deploying from Git.\n")
		builder.WriteString("- this migration run does not require those credentials because Dokploy receives a raw compose/image snapshot.\n\n")
	}

	builder.WriteString("## routes\n\n")
	if len(topology.Routes) == 0 {
		builder.WriteString("- no routes were detected; confirm this app is internal-only or add Dokploy domains manually.\n\n")
	} else {
		for _, route := range topology.Routes {
			builder.WriteString(fmt.Sprintf("- create route `%s`", route.Host))
			if route.ServiceName != "" {
				builder.WriteString(fmt.Sprintf(" for service `%s`", route.ServiceName))
			}
			if route.Port != "" {
				builder.WriteString(fmt.Sprintf(" on port `%s`", route.Port))
			}
			builder.WriteString(".\n")
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## data stores\n\n")
	if len(topology.DataStores) == 0 && len(topology.LinkedResources) == 0 {
		builder.WriteString("- no data stores were inferred from this app bundle.\n\n")
	} else {
		for _, store := range topology.DataStores {
			builder.WriteString(fmt.Sprintf("- `%s` service `%s`: use `%s`", store.Label(), store.Service, store.Strategy))
			if store.Fallback != "" {
				builder.WriteString(fmt.Sprintf(", fallback `%s`", store.Fallback))
			}
			builder.WriteString(fmt.Sprintf(", criticality `%s`", store.Criticality))
			if len(store.Volumes) > 0 {
				builder.WriteString(fmt.Sprintf(", volumes `%s`", strings.Join(store.Volumes, "`, `")))
			}
			builder.WriteString(".\n")
		}
		for _, link := range topology.LinkedResources {
			builder.WriteString(fmt.Sprintf("- possible `%s` resource `%s` with `%s` confidence", link.Kind, link.App, link.Confidence))
			if len(link.DataStores) > 0 {
				builder.WriteString(fmt.Sprintf(": %s", strings.Join(runbookDataStoreLabels(link.DataStores), ", ")))
			}
			if len(link.Reasons) > 0 {
				builder.WriteString(fmt.Sprintf("; reasons: %s", strings.Join(link.Reasons, ", ")))
			}
			builder.WriteString(".\n")
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## stateful volumes\n\n")
	if len(topology.StatefulVolumes) == 0 {
		builder.WriteString("- no stateful volumes or storage records were inferred.\n\n")
	} else {
		for _, volume := range topology.StatefulVolumes {
			builder.WriteString(fmt.Sprintf("- `%s` `%s`", volume.Type, planutil.Fallback(volume.Name, volume.Source)))
			if volume.Service != "" {
				builder.WriteString(fmt.Sprintf(" from `%s`", volume.Service))
			}
			builder.WriteString(fmt.Sprintf(" -> `%s`.\n", volume.Target))
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## cutover checklist\n\n")
	builder.WriteString("- create target services privately and keep source traffic unchanged.\n")
	builder.WriteString("- restore or sync every data store before enabling public routes.\n")
	builder.WriteString("- run `bort validate --bundle <bundle>` after edits.\n")
	builder.WriteString("- verify health checks and application logs before DNS or proxy cutover.\n")
	builder.WriteString("- keep rollback route and source state untouched until the target is accepted.\n")

	return builder.String()
}

func runbookDataStoreLabels(stores []analyzer.DataStore) []string {
	labels := make([]string, 0, len(stores))
	for _, store := range stores {
		labels = append(labels, store.Label()+" on "+store.Service)
	}
	return labels
}

func selectedApps(apps []manifest.App, name string) []manifest.App {
	if name == "" {
		return apps
	}

	selected := []manifest.App{}
	for _, app := range apps {
		if app.Name == name || app.ID == name || planutil.Slug(app.Name) == planutil.Slug(name) {
			selected = append(selected, app)
		}
	}
	return selected
}

func writeJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return writePrivateFile(path, contents)
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirMode); err != nil {
		return err
	}
	return os.Chmod(path, privateDirMode)
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, privateFileMode)
	if err != nil {
		return err
	}
	if err := file.Chmod(privateFileMode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func uniqueDirName(base string, used map[string]int) string {
	if base == "" {
		base = "app"
	}
	used[base]++
	if used[base] == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, used[base])
}

func routeHosts(routes []manifest.Route) []string {
	seen := map[string]struct{}{}
	for _, route := range routes {
		if route.Host != "" {
			seen[route.Host] = struct{}{}
		}
	}

	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func migrationRole(app manifest.App) string {
	if app.Metadata == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(app.Metadata["migrationRole"]))
}

func projectGroupsForApps(m manifest.Manifest, apps []manifest.App) map[string]*ProjectGroup {
	owners := detectedSupportOwners(m)
	groups := map[string]*ProjectGroup{}
	for _, app := range apps {
		name := app.Name
		source := "app"
		if owner := owners[appKey(app)]; owner != "" {
			name = owner
			source = "detected-link"
		}
		environment, _ := metadataValue(app.Metadata, "coolify.environment", "coolify.environmentName", "environment_name", "environment")
		if name == app.Name && environment == "" {
			continue
		}
		groups[appKey(app)] = &ProjectGroup{Name: name, Environment: environment, Source: source}
	}
	return groups
}

func detectedSupportOwners(m manifest.Manifest) map[string]string {
	ownerByApp := map[string]string{}
	ownerCounts := map[string]int{}
	for _, app := range m.Apps {
		if migrationRole(app) == "support" || migrationRole(app) == "platform" {
			continue
		}
		topology := analyzer.TopologyForAppInManifest(m, app)
		for _, link := range topology.LinkedResources {
			if link.Confidence != "detected" {
				continue
			}
			resource, ok := appByLink(m.Apps, link)
			if !ok || migrationRole(resource) != "support" {
				continue
			}
			key := appKey(resource)
			ownerByApp[key] = app.Name
			ownerCounts[key]++
		}
	}
	for key, count := range ownerCounts {
		if count != 1 {
			delete(ownerByApp, key)
		}
	}
	return ownerByApp
}

func appByLink(apps []manifest.App, link analyzer.ResourceLink) (manifest.App, bool) {
	for _, app := range apps {
		if link.AppID != "" && app.ID == link.AppID {
			return app, true
		}
		if app.Name == link.App {
			return app, true
		}
	}
	return manifest.App{}, false
}

func appKey(app manifest.App) string {
	if app.ID != "" {
		return app.ID
	}
	return app.Name
}

func metadataValue(metadata map[string]string, keys ...string) (string, string) {
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return value, key
		}
	}
	return "", ""
}

func mountCount(app manifest.App) int {
	count := 0
	for _, service := range app.Services {
		count += len(service.Mounts)
	}
	return count
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func quoteYAML(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func escapeEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\n\r#'") {
		return quoteYAML(value)
	}
	return value
}

func ensureTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		return value
	}
	return value + "\n"
}
