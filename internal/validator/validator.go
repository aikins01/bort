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

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/planutil"
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
		if opts.AppName != "" && app.Name != opts.AppName && app.Directory != opts.AppName && planutil.Slug(app.Name) != planutil.Slug(opts.AppName) {
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

	for _, file := range []string{"compose.yaml", ".env.example", "routes.json", "storages.json", "topology.json", "migration-report.md", "migration-runbook.md"} {
		if _, err := os.Stat(filepath.Join(appDir, file)); err != nil {
			result.add(SeverityError, "bundle.missing_file", fmt.Sprintf("%s is missing", file))
		}
	}

	topology, hasTopology := validateTopologyFile(&result, filepath.Join(appDir, "topology.json"))
	validateRunbook(&result, filepath.Join(appDir, "migration-runbook.md"))

	composePath := filepath.Join(appDir, "compose.yaml")
	compose, err := os.ReadFile(composePath)
	if err == nil {
		validateComposeText(&result, string(compose), envKeys(envFiles(appDir, app.PrivateEnvValues)))
		validateDockerCompose(ctx, &result, opts.DockerPath, appDir)
	} else {
		result.add(SeverityError, "compose.read_failed", err.Error())
	}

	validateEnvFiles(&result, envFiles(appDir, app.PrivateEnvValues), app.PrivateEnvValues)
	routes := validateRoutes(&result, filepath.Join(appDir, "routes.json"), !hasTopology || topologyHasRisk(topology, "routes.none"))
	validateStorages(&result, filepath.Join(appDir, "storages.json"))
	if hasTopology {
		validateTopology(&result, topology, routes)
	}
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
	missingEnv = planutil.UniqueStrings(missingEnv)
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

	analysis.AbsoluteBindSources = planutil.UniqueStrings(absoluteBindSources)
	analysis.UndeclaredNamedVolumes = planutil.UniqueStrings(undeclaredNamedVolumes)
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

func validateTopologyFile(result *AppResult, path string) (analyzer.Topology, bool) {
	var topology analyzer.Topology
	if err := readJSON(path, &topology); err != nil {
		if !os.IsNotExist(err) {
			result.add(SeverityError, "topology.read_failed", err.Error())
		}
		return analyzer.Topology{}, false
	}
	return topology, true
}

func validateRunbook(result *AppResult, path string) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			result.add(SeverityError, "runbook.read_failed", err.Error())
		}
		return
	}
	if strings.TrimSpace(string(contents)) == "" {
		result.add(SeverityError, "runbook.empty", "migration-runbook.md is empty")
	}
}

func validateTopology(result *AppResult, topology analyzer.Topology, routes []manifest.Route) {
	validateTopologyExternalRequirements(result, topology)
	validateTopologyLinkedResources(result, topology)
	validateTopologyDataStores(result, topology.DataStores)
	validateTopologyStatefulVolumes(result, topology.StatefulVolumes)
	validateTopologyEnvValues(result, topology)
	validateTopologyRoutes(result, topology, routes)
}

func validateTopologyExternalRequirements(result *AppResult, topology analyzer.Topology) {
	if len(topology.ExternalRequirements) == 0 {
		return
	}
	result.add(SeverityWarn, "topology.external_requirements", fmt.Sprintf("external requirements inferred from env names must be resolved before deploy: %s", describeRequirements(topology.ExternalRequirements)))
}

func validateTopologyLinkedResources(result *AppResult, topology analyzer.Topology) {
	linksByKind := map[string][]analyzer.ResourceLink{}
	for _, link := range topology.LinkedResources {
		linksByKind[link.Kind] = append(linksByKind[link.Kind], link)
	}

	for _, requirement := range topology.ExternalRequirements {
		if !analyzer.IsLinkableRequirement(requirement.Kind) {
			continue
		}
		links := linksByKind[requirement.Kind]
		switch {
		case len(links) == 0:
			result.add(SeverityWarn, "topology.linked_resource_ambiguous", fmt.Sprintf("external %s requirement has no linked support resource candidate", requirement.Kind))
		case len(links) > 1:
			result.add(SeverityWarn, "topology.linked_resource_ambiguous", fmt.Sprintf("external %s requirement has multiple possible support resource candidates: %s", requirement.Kind, strings.Join(resourceLinkLabels(links), ", ")))
		case strings.ToLower(strings.TrimSpace(links[0].Confidence)) != "likely":
			result.add(SeverityWarn, "topology.linked_resource_ambiguous", fmt.Sprintf("external %s requirement is linked to %s with %s confidence", requirement.Kind, planutil.Fallback(links[0].App, "unknown resource"), planutil.Fallback(links[0].Confidence, "unknown")))
		}
	}
}

func validateTopologyDataStores(result *AppResult, stores []analyzer.DataStore) {
	manualReview := []string{}
	for _, store := range stores {
		if store.Kind == "unknown" || store.Strategy == "manual_review" {
			manualReview = append(manualReview, planutil.Fallback(store.Service, store.Label()))
		}
	}
	manualReview = planutil.UniqueStrings(manualReview)
	if len(manualReview) > 0 {
		result.add(SeverityWarn, "topology.data_store_manual_review", fmt.Sprintf("manual data-store review required for: %s", strings.Join(manualReview, ", ")))
	}
}

func validateTopologyStatefulVolumes(result *AppResult, volumes []analyzer.StatefulVolume) {
	bindMounts := []string{}
	for _, volume := range volumes {
		if volume.Type == "bind" {
			bindMounts = append(bindMounts, statefulVolumeLabel(volume))
		}
	}
	bindMounts = planutil.UniqueStrings(bindMounts)
	if len(bindMounts) > 0 {
		result.add(SeverityWarn, "topology.bind_mounts", fmt.Sprintf("bind mount state is host-specific and needs portability review: %s", strings.Join(bindMounts, ", ")))
	}
}

func validateTopologyEnvValues(result *AppResult, topology analyzer.Topology) {
	if risk, ok := topologyRisk(topology, "env.values_redacted"); ok {
		result.add(SeverityWarn, "topology.env_values_redacted", risk.Message)
	}
}

func validateTopologyRoutes(result *AppResult, topology analyzer.Topology, routes []manifest.Route) {
	if len(topology.Routes) == 0 {
		return
	}
	missing := missingRouteLabels(topology.Routes, routes)
	if len(missing) > 0 {
		result.add(SeverityWarn, "topology.routes_mismatch", fmt.Sprintf("topology routes are absent from routes.json: %s", strings.Join(missing, ", ")))
	}
}

func topologyHasRisk(topology analyzer.Topology, code string) bool {
	_, ok := topologyRisk(topology, code)
	return ok
}

func topologyRisk(topology analyzer.Topology, code string) (analyzer.RiskReason, bool) {
	for _, risk := range topology.RiskReasons {
		if risk.Code == code {
			return risk, true
		}
	}
	return analyzer.RiskReason{}, false
}

func describeRequirements(requirements []analyzer.Requirement) string {
	items := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		item := requirement.Kind
		if len(requirement.Evidence) > 0 {
			item += " via " + strings.Join(requirement.Evidence, ", ")
		}
		items = append(items, item)
	}
	return strings.Join(items, "; ")
}

func resourceLinkLabels(links []analyzer.ResourceLink) []string {
	labels := make([]string, 0, len(links))
	for _, link := range links {
		label := planutil.Fallback(link.App, "unknown resource")
		if link.Confidence != "" {
			label += " (" + link.Confidence + ")"
		}
		labels = append(labels, label)
	}
	return planutil.UniqueStrings(labels)
}

func statefulVolumeLabel(volume analyzer.StatefulVolume) string {
	label := planutil.Fallback(volume.Service, "app")
	if volume.Target != "" {
		label += " -> " + volume.Target
	}
	return label
}

func missingRouteLabels(want, got []manifest.Route) []string {
	gotKeys := map[string]struct{}{}
	for _, route := range got {
		gotKeys[routeKey(route)] = struct{}{}
	}

	missing := []string{}
	for _, route := range want {
		if _, ok := gotKeys[routeKey(route)]; !ok {
			missing = append(missing, routeLabel(route))
		}
	}
	return planutil.UniqueStrings(missing)
}

func routeKey(route manifest.Route) string {
	return route.Host + "\x00" + route.ServiceName + "\x00" + route.Port
}

func routeLabel(route manifest.Route) string {
	label := planutil.Fallback(route.Host, "missing host")
	if route.ServiceName != "" {
		label += " -> " + route.ServiceName
	}
	if route.Port != "" {
		label += ":" + route.Port
	}
	return label
}

func validateEnvFiles(result *AppResult, paths []string, privateEnvValues bool) {
	for _, path := range paths {
		validateEnvFile(result, path, privateEnvValues)
	}
}

func validateEnvFile(result *AppResult, path string, privateEnvValues bool) {
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
			if privateEnvValues {
				result.add(SeverityInfo, "env.private_value_present", fmt.Sprintf("%s has a private value in %s", key, filepath.Base(path)))
				continue
			}
			result.add(SeverityError, "env.sensitive_value_present", fmt.Sprintf("%s appears sensitive but is populated in %s", key, filepath.Base(path)))
		}
	}
}

func validateRoutes(result *AppResult, path string, warnOnEmpty bool) []manifest.Route {
	var routes []manifest.Route
	if err := readJSON(path, &routes); err != nil {
		return nil
	}
	if len(routes) == 0 {
		if warnOnEmpty {
			result.add(SeverityWarn, "routes.none", "no public routes were detected; verify routing or confirm this resource is internal-only")
		}
		return routes
	}
	for i, route := range routes {
		if route.Host == "" {
			result.add(SeverityError, "routes.missing_host", fmt.Sprintf("route %d has no host", i+1))
		}
		if route.ServiceName == "" {
			result.add(SeverityWarn, "routes.missing_service", fmt.Sprintf("route %s has no service mapping", route.Host))
		}
	}
	return routes
}

func validateStorages(result *AppResult, path string) {
	var storages []manifest.Storage
	if err := readJSON(path, &storages); err != nil {
		return
	}
	for _, storage := range storages {
		if storage.Target == "" {
			result.add(SeverityError, "storage.missing_target", fmt.Sprintf("storage %s has no target", planutil.Fallback(storage.Name, storage.Source)))
		}
		if storage.Type == "bind" && strings.HasPrefix(storage.Source, "/") {
			result.add(SeverityWarn, "storage.absolute_bind_mount", fmt.Sprintf("storage %s uses absolute host path %s", planutil.Fallback(storage.Name, storage.Target), storage.Source))
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
	return envFiles(appDir, false)
}

func envFiles(appDir string, privateEnvValues bool) []string {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || (privateEnvValues && strings.HasSuffix(name, ".example")) || (!privateEnvValues && !strings.HasSuffix(name, ".example")) {
			continue
		}
		paths = append(paths, filepath.Join(appDir, name))
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
	return planutil.UniqueStrings(values)
}
