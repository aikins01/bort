package validator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/secrets"
	"gopkg.in/yaml.v3"
)

type Status string

const (
	StatusGreen  Status = "green"
	StatusYellow Status = "yellow"
	StatusRed    Status = "red"
)

type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

type Options struct {
	BundleDir  string
	AppName    string
	DockerPath string
}

type Result struct {
	BundleDir string      `json:"bundleDir"`
	Status    Status      `json:"status"`
	Apps      []AppResult `json:"apps"`
}

type AppResult struct {
	Name      string  `json:"name"`
	Directory string  `json:"directory"`
	Status    Status  `json:"status"`
	Issues    []Issue `json:"issues,omitempty"`
}

type Issue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

func Validate(ctx context.Context, opts Options) (Result, error) {
	if opts.BundleDir == "" {
		opts.BundleDir = "bort-bundle"
	}
	if opts.DockerPath == "" {
		opts.DockerPath = "docker"
	}

	index, err := readIndex(opts.BundleDir)
	if err != nil {
		return Result{}, err
	}

	result := Result{BundleDir: opts.BundleDir, Status: StatusGreen}
	for _, app := range index.Apps {
		if opts.AppName != "" && app.Name != opts.AppName && app.Directory != opts.AppName && slug(app.Name) != slug(opts.AppName) {
			continue
		}

		appResult := validateApp(ctx, opts, app)
		result.Apps = append(result.Apps, appResult)
		result.Status = worseStatus(result.Status, appResult.Status)
	}

	if len(result.Apps) == 0 {
		if opts.AppName != "" {
			return Result{}, fmt.Errorf("app %q not found in bundle", opts.AppName)
		}
		return Result{}, fmt.Errorf("bundle has no apps")
	}

	return result, nil
}

func validateApp(ctx context.Context, opts Options, app exporter.AppSummary) AppResult {
	appDir := filepath.Join(opts.BundleDir, filepath.FromSlash(app.Directory))
	result := AppResult{Name: app.Name, Directory: app.Directory, Status: StatusGreen}

	for _, file := range []string{"compose.yaml", ".env.example", "routes.json", "storages.json", "migration-report.md"} {
		if _, err := os.Stat(filepath.Join(appDir, file)); err != nil {
			result.add(SeverityError, "bundle.missing_file", fmt.Sprintf("%s is missing", file))
		}
	}

	composePath := filepath.Join(appDir, "compose.yaml")
	compose, err := os.ReadFile(composePath)
	if err == nil {
		validateComposeText(&result, string(compose), envKeys(envExampleFiles(appDir)))
		validateDockerCompose(ctx, &result, opts.DockerPath, appDir)
	} else {
		result.add(SeverityError, "compose.read_failed", err.Error())
	}

	validateEnvFiles(&result, envExampleFiles(appDir))
	validateRoutes(&result, filepath.Join(appDir, "routes.json"))
	validateStorages(&result, filepath.Join(appDir, "storages.json"))
	result.Status = statusFromIssues(result.Issues)
	return result
}

func validateDockerCompose(ctx context.Context, result *AppResult, dockerPath, appDir string) {
	if _, err := exec.LookPath(dockerPath); err != nil {
		result.add(SeverityWarn, "compose.docker_unavailable", "docker CLI is not available, skipped docker compose config")
		return
	}

	args := []string{"compose", "--env-file", ".env.example", "-f", "compose.yaml", "config", "--quiet"}
	cmd := exec.CommandContext(ctx, dockerPath, args...)
	cmd.Dir = appDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		result.add(SeverityError, "compose.config_failed", message)
	}
}

var (
	hostPortPattern        = regexp.MustCompile(`(?m)^\s*-\s*["']?(?:[0-9.]+:)?[0-9]+:[0-9]+(?:/(?:tcp|udp))?["']?\s*$`)
	absoluteBindPattern    = regexp.MustCompile(`(?m)^\s*-\s*["']?(/[^:\n]+):(/[^:\n]+)`)
	composeVariablePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::[-?][^}]*)?\}`)
	namedVolumePattern     = regexp.MustCompile(`(?m)^\s*-\s*["']?([A-Za-z0-9_.-]+):/[^\n"']+`)
	namedVolumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	topLevelVolumesPattern = regexp.MustCompile(`(?m)^volumes:\s*$`)
)

func validateComposeText(result *AppResult, compose string, envKeys map[string]struct{}) {
	if strings.Contains(compose, "TODO_REPLACE_IMAGE") {
		result.add(SeverityError, "compose.missing_image", "compose contains TODO_REPLACE_IMAGE")
	}
	if strings.Contains(compose, "TODO_REPLACE_SOURCE") {
		result.add(SeverityError, "compose.missing_volume_source", "compose contains TODO_REPLACE_SOURCE")
	}

	analysis, err := analyzeCompose(compose)
	if err != nil {
		validateComposeTextWithPatterns(result, compose)
	} else {
		addComposeAnalysisIssues(result, analysis)
	}

	missingEnv := []string{}
	for _, match := range composeVariablePattern.FindAllStringSubmatch(compose, -1) {
		if len(match) != 2 {
			continue
		}
		if _, ok := envKeys[match[1]]; !ok {
			missingEnv = append(missingEnv, match[1])
		}
	}
	missingEnv = uniqueStrings(missingEnv)
	if len(missingEnv) > 0 {
		result.add(SeverityWarn, "env.missing_referenced_values", fmt.Sprintf("compose references env vars absent from exported env example files: %s", strings.Join(missingEnv, ", ")))
	}
}

func validateComposeTextWithPatterns(result *AppResult, compose string) {
	if strings.Contains(compose, "container_name:") {
		result.add(SeverityWarn, "compose.container_name", "container_name can break Dokploy logs, metrics, and service recreation")
	}
	if hostPortPattern.MatchString(compose) {
		result.add(SeverityWarn, "compose.host_port", "host port bindings can conflict on Dokploy; prefer internal ports and Dokploy domains")
	}
	if matches := absoluteBindPattern.FindAllStringSubmatch(compose, -1); len(matches) > 0 {
		sources := uniqueMatches(matches, 1)
		result.add(SeverityWarn, "compose.absolute_bind_mount", fmt.Sprintf("absolute bind mounts are not portable: %s", strings.Join(sources, ", ")))
	}
	if namedVolumePattern.MatchString(compose) && !topLevelVolumesPattern.MatchString(compose) {
		result.add(SeverityWarn, "compose.undeclared_named_volume", "named volume mounts were found but no top-level volumes section was detected")
	}
}

func addComposeAnalysisIssues(result *AppResult, analysis composeAnalysis) {
	if analysis.HasContainerName {
		result.add(SeverityWarn, "compose.container_name", "container_name can break Dokploy logs, metrics, and service recreation")
	}
	if analysis.HasHostPortBinding {
		result.add(SeverityWarn, "compose.host_port", "host port bindings can conflict on Dokploy; prefer internal ports and Dokploy domains")
	}
	if len(analysis.AbsoluteBindSources) > 0 {
		result.add(SeverityWarn, "compose.absolute_bind_mount", fmt.Sprintf("absolute bind mounts are not portable: %s", strings.Join(analysis.AbsoluteBindSources, ", ")))
	}
	if len(analysis.UndeclaredNamedVolumes) > 0 {
		result.add(SeverityWarn, "compose.undeclared_named_volume", fmt.Sprintf("named volume mounts are missing top-level volume declarations: %s", strings.Join(analysis.UndeclaredNamedVolumes, ", ")))
	}
}

type composeAnalysis struct {
	HasContainerName       bool
	HasHostPortBinding     bool
	AbsoluteBindSources    []string
	UndeclaredNamedVolumes []string
}

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
}

type composeService struct {
	ContainerName string          `yaml:"container_name"`
	Ports         []composePort   `yaml:"ports"`
	Volumes       []composeVolume `yaml:"volumes"`
}

type composePort struct {
	Short     string
	Published string
}

func (p *composePort) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		p.Short = value.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "published" {
				p.Published = value.Content[i+1].Value
			}
		}
	}
	return nil
}

type composeVolume struct {
	Short  string
	Type   string
	Source string
	Target string
}

func (v *composeVolume) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		v.Short = value.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(value.Content); i += 2 {
			switch value.Content[i].Value {
			case "type":
				v.Type = value.Content[i+1].Value
			case "source":
				v.Source = value.Content[i+1].Value
			case "target":
				v.Target = value.Content[i+1].Value
			}
		}
	}
	return nil
}

func analyzeCompose(compose string) (composeAnalysis, error) {
	var file composeFile
	if err := yaml.Unmarshal([]byte(compose), &file); err != nil {
		return composeAnalysis{}, err
	}

	analysis := composeAnalysis{}
	declaredVolumes := map[string]struct{}{}
	for name := range file.Volumes {
		declaredVolumes[name] = struct{}{}
	}

	absoluteBindSources := []string{}
	undeclaredNamedVolumes := []string{}
	for _, service := range file.Services {
		if service.ContainerName != "" {
			analysis.HasContainerName = true
		}
		for _, port := range service.Ports {
			if port.hasHostBinding() {
				analysis.HasHostPortBinding = true
			}
		}
		for _, volume := range service.Volumes {
			if source := volume.absoluteBindSource(); source != "" {
				absoluteBindSources = append(absoluteBindSources, source)
			}
			if name := volume.namedVolume(); name != "" {
				if _, ok := declaredVolumes[name]; !ok {
					undeclaredNamedVolumes = append(undeclaredNamedVolumes, name)
				}
			}
		}
	}

	analysis.AbsoluteBindSources = uniqueStrings(absoluteBindSources)
	analysis.UndeclaredNamedVolumes = uniqueStrings(undeclaredNamedVolumes)
	return analysis, nil
}

func (p composePort) hasHostBinding() bool {
	if strings.TrimSpace(p.Published) != "" {
		return true
	}
	short := strings.TrimSpace(p.Short)
	if before, _, found := strings.Cut(short, "/"); found {
		short = before
	}
	return strings.Contains(short, ":")
}

func (v composeVolume) absoluteBindSource() string {
	source, _ := v.parts()
	if source == "" || !filepath.IsAbs(source) {
		return ""
	}
	if v.Type == "" || v.Type == "bind" {
		return source
	}
	return ""
}

func (v composeVolume) namedVolume() string {
	if v.Type == "bind" || v.Type == "tmpfs" {
		return ""
	}
	source, _ := v.parts()
	if isNamedVolumeSource(source) {
		return source
	}
	return ""
}

func (v composeVolume) parts() (string, string) {
	if v.Source != "" || v.Target != "" {
		return strings.TrimSpace(v.Source), strings.TrimSpace(v.Target)
	}
	short := strings.TrimSpace(v.Short)
	if short == "" {
		return "", ""
	}
	parts := strings.Split(short, ":")
	if len(parts) < 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func isNamedVolumeSource(source string) bool {
	if source == "" {
		return false
	}
	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~") || strings.HasPrefix(source, "${") {
		return false
	}
	if strings.Contains(source, "/") {
		return false
	}
	return namedVolumeNamePattern.MatchString(source)
}

func validateEnvFiles(result *AppResult, paths []string) {
	for _, path := range paths {
		validateEnvFile(result, path)
	}
}

func validateEnvFile(result *AppResult, path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		if secrets.IsSensitiveName(key) && value == "" {
			result.add(SeverityWarn, "env.sensitive_blank", fmt.Sprintf("%s in %s is sensitive and must be set before deploy", key, filepath.Base(path)))
		}
		if secrets.IsSensitiveName(key) && value != "" {
			result.add(SeverityError, "env.sensitive_value_present", fmt.Sprintf("%s appears sensitive but is populated in %s", key, filepath.Base(path)))
		}
	}
}

func validateRoutes(result *AppResult, path string) {
	var routes []manifest.Route
	if err := readJSON(path, &routes); err != nil {
		return
	}
	if len(routes) == 0 {
		result.add(SeverityWarn, "routes.none", "no routes were exported for this app")
		return
	}
	for i, route := range routes {
		if route.Host == "" {
			result.add(SeverityError, "routes.missing_host", fmt.Sprintf("route %d has no host", i+1))
		}
		if route.ServiceName == "" {
			result.add(SeverityWarn, "routes.missing_service", fmt.Sprintf("route %s has no service mapping", route.Host))
		}
	}
}

func validateStorages(result *AppResult, path string) {
	var storages []manifest.Storage
	if err := readJSON(path, &storages); err != nil {
		return
	}
	for _, storage := range storages {
		if storage.Target == "" {
			result.add(SeverityError, "storage.missing_target", fmt.Sprintf("storage %s has no target", fallback(storage.Name, storage.Source)))
		}
		if storage.Type == "bind" && strings.HasPrefix(storage.Source, "/") {
			result.add(SeverityWarn, "storage.absolute_bind_mount", fmt.Sprintf("storage %s uses absolute host path %s", fallback(storage.Name, storage.Target), storage.Source))
		}
	}
}

func readIndex(bundleDir string) (exporter.Summary, error) {
	var summary exporter.Summary
	if err := readJSON(filepath.Join(bundleDir, "index.json"), &summary); err != nil {
		return exporter.Summary{}, err
	}
	return summary, nil
}

func readJSON(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(out)
}

func envExampleFiles(appDir string) []string {
	paths, err := filepath.Glob(filepath.Join(appDir, ".env*.example"))
	if err != nil {
		return nil
	}
	sort.Strings(paths)
	return paths
}

func envKeys(paths []string) map[string]struct{} {
	keys := map[string]struct{}{}
	for _, path := range paths {
		readEnvKeys(path, keys)
	}
	return keys
}

func readEnvKeys(path string, keys map[string]struct{}) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _ := strings.Cut(line, "=")
		if key != "" {
			keys[key] = struct{}{}
		}
	}
}

func (r *AppResult) add(severity Severity, code, message string) {
	r.Issues = append(r.Issues, Issue{Severity: severity, Code: code, Message: message})
}

func statusFromIssues(issues []Issue) Status {
	status := StatusGreen
	for _, issue := range issues {
		switch issue.Severity {
		case SeverityError:
			return StatusRed
		case SeverityWarn:
			status = StatusYellow
		}
	}
	return status
}

func worseStatus(a, b Status) Status {
	if a == StatusRed || b == StatusRed {
		return StatusRed
	}
	if a == StatusYellow || b == StatusYellow {
		return StatusYellow
	}
	return StatusGreen
}

func uniqueMatches(matches [][]string, index int) []string {
	values := []string{}
	for _, match := range matches {
		if len(match) > index {
			values = append(values, match[index])
		}
	}
	return uniqueStrings(values)
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}

	unique := make([]string, 0, len(seen))
	for value := range seen {
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

var slugPattern = regexp.MustCompile(`[^a-z0-9._-]+`)

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "-")
	value = slugPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-._")
	return value
}

func fallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
