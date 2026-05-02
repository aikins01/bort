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
)

type Options struct {
	OutputDir string
	AppName   string
}

const (
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
)

type Summary struct {
	OutputDir   string       `json:"outputDir"`
	GeneratedAt time.Time    `json:"generatedAt"`
	Source      string       `json:"source"`
	Apps        []AppSummary `json:"apps"`
}

type AppSummary struct {
	Name      string   `json:"name"`
	Directory string   `json:"directory"`
	Routes    []string `json:"routes,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
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
	}

	usedDirs := map[string]int{}
	for _, app := range apps {
		dirName := uniqueDirName(planutil.Slug(app.Name), usedDirs)
		appDir := filepath.Join(opts.OutputDir, dirName)
		if err := ensurePrivateDir(appDir); err != nil {
			return Summary{}, err
		}

		topology := analyzer.TopologyForAppInManifest(m, app)
		warnings, err := exportApp(appDir, app, topology)
		if err != nil {
			return Summary{}, err
		}

		summary.Apps = append(summary.Apps, AppSummary{
			Name:      app.Name,
			Directory: filepath.ToSlash(dirName),
			Routes:    routeHosts(app.Routes),
			Warnings:  warnings,
		})
	}

	if err := writeJSON(filepath.Join(opts.OutputDir, "index.json"), summary); err != nil {
		return Summary{}, err
	}

	return summary, nil
}

func exportApp(appDir string, app manifest.App, topology analyzer.Topology) ([]string, error) {
	warnings := []string{}
	compose, composeWarnings, serviceEnvFiles := composeForApp(app)
	warnings = append(warnings, composeWarnings...)

	files := map[string][]byte{
		"compose.yaml":         []byte(compose),
		".env.example":         []byte(envExample(app.Environment)),
		"migration-report.md":  []byte(report(app, warnings)),
		"migration-runbook.md": []byte(runbook(app, topology, warnings)),
	}
	for _, envFile := range serviceEnvFiles {
		files[envFile.Name] = []byte(envExample(envFile.Vars))
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

type envFile struct {
	Name string
	Vars []manifest.EnvVar
}

func composeForApp(app manifest.App) (string, []string, []envFile) {
	warnings := []string{}
	if app.Compose != nil {
		if strings.TrimSpace(app.Compose.Raw) != "" {
			return ensureTrailingNewline(app.Compose.Raw), nil, nil
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
	serviceEnvFileMap := serviceEnvFiles(app.Services)
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
		if len(app.Environment) > 0 {
			envFiles = append(envFiles, ".env.example")
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

func serviceEnvFiles(services []manifest.Service) map[int]envFile {
	usedNames := map[string]int{}
	files := map[int]envFile{}
	for index, service := range services {
		if len(service.Environment) == 0 {
			continue
		}
		base := planutil.Slug(service.Name)
		if base == "" {
			base = "service"
		}
		files[index] = envFile{Name: ".env." + uniqueDirName(base, usedNames) + ".example", Vars: service.Environment}
	}
	return files
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

func envExample(envs []manifest.EnvVar) string {
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
		if env.ValueKnown && !env.Sensitive {
			value = env.Value
		}
		builder.WriteString(name)
		builder.WriteString("=")
		builder.WriteString(escapeEnvValue(value))
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
		builder.WriteString(fmt.Sprintf("repository: `%s`\n\n", planutil.Fallback(app.Git.Repository, "unknown")))
		builder.WriteString(fmt.Sprintf("branch: `%s`\n\n", planutil.Fallback(app.Git.Branch, "unknown")))
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
