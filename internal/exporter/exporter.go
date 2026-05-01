package exporter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/manifest"
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
		dirName := uniqueDirName(slug(app.Name), usedDirs)
		appDir := filepath.Join(opts.OutputDir, dirName)
		if err := ensurePrivateDir(appDir); err != nil {
			return Summary{}, err
		}

		warnings, err := exportApp(appDir, app)
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

func exportApp(appDir string, app manifest.App) ([]string, error) {
	warnings := []string{}
	compose, composeWarnings, serviceEnvFiles := composeForApp(app)
	warnings = append(warnings, composeWarnings...)

	files := map[string][]byte{
		"compose.yaml":        []byte(compose),
		".env.example":        []byte(envExample(app.Environment)),
		"migration-report.md": []byte(report(app, warnings)),
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
		name := slug(service.Name)
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
		base := slug(service.Name)
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
	builder.WriteString(fmt.Sprintf("platform: `%s`\n\n", fallback(app.Platform, "unknown")))
	builder.WriteString(fmt.Sprintf("runtime: `%s`\n\n", fallback(app.Runtime, "unknown")))
	if app.BuildPack != "" {
		builder.WriteString(fmt.Sprintf("build pack: `%s`\n\n", app.BuildPack))
	}
	if app.Git != nil {
		builder.WriteString("## git\n\n")
		builder.WriteString(fmt.Sprintf("repository: `%s`\n\n", fallback(app.Git.Repository, "unknown")))
		builder.WriteString(fmt.Sprintf("branch: `%s`\n\n", fallback(app.Git.Branch, "unknown")))
	}

	builder.WriteString("## routes\n\n")
	if len(app.Routes) == 0 {
		builder.WriteString("no routes detected.\n\n")
	} else {
		for _, route := range app.Routes {
			builder.WriteString(fmt.Sprintf("- `%s` -> `%s`", route.Host, fallback(route.ServiceName, app.Name)))
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
			builder.WriteString(fmt.Sprintf("- `%s` `%s` -> `%s`\n", fallback(storage.Type, "storage"), fallback(storage.Name, storage.Source), storage.Target))
		}
		for _, service := range app.Services {
			for _, mount := range service.Mounts {
				builder.WriteString(fmt.Sprintf("- `%s` `%s` -> `%s`\n", mount.Type, fallback(mount.Name, mount.Source), mount.Target))
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

func selectedApps(apps []manifest.App, name string) []manifest.App {
	if name == "" {
		return apps
	}

	selected := []manifest.App{}
	for _, app := range apps {
		if app.Name == name || app.ID == name || slug(app.Name) == slug(name) {
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

var slugPattern = regexp.MustCompile(`[^a-z0-9._-]+`)

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "-")
	value = slugPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-._")
	return value
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

func fallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
