package preparer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/exporter"
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
	BundleDir string
	AppName   string
	Target    string
}

type Result struct {
	BundleDir string    `json:"bundleDir"`
	Target    string    `json:"target"`
	Status    Status    `json:"status"`
	Apps      []AppPlan `json:"apps"`
}

type AppPlan struct {
	Name      string   `json:"name"`
	Directory string   `json:"directory"`
	Status    Status   `json:"status"`
	Actions   []Action `json:"actions"`
}

type Action struct {
	Severity Severity `json:"severity"`
	Kind     string   `json:"kind"`
	Message  string   `json:"message"`
}

func Plan(opts Options) (Result, error) {
	if opts.BundleDir == "" {
		opts.BundleDir = "bort-bundle"
	}
	if opts.Target == "" {
		opts.Target = "dokploy"
	}

	index, err := readIndex(opts.BundleDir)
	if err != nil {
		return Result{}, err
	}

	result := Result{BundleDir: opts.BundleDir, Target: opts.Target, Status: StatusGreen}
	for _, app := range index.Apps {
		if opts.AppName != "" && app.Name != opts.AppName && app.Directory != opts.AppName && slug(app.Name) != slug(opts.AppName) {
			continue
		}

		appPlan, err := planApp(opts.BundleDir, opts.Target, app)
		if err != nil {
			return Result{}, err
		}
		result.Apps = append(result.Apps, appPlan)
		result.Status = worseStatus(result.Status, appPlan.Status)
	}

	if len(result.Apps) == 0 {
		if opts.AppName != "" {
			return Result{}, fmt.Errorf("app %q not found in bundle", opts.AppName)
		}
		return Result{}, fmt.Errorf("bundle has no apps")
	}

	return result, nil
}

func planApp(bundleDir, target string, app exporter.AppSummary) (AppPlan, error) {
	appDir := filepath.Join(bundleDir, filepath.FromSlash(app.Directory))
	topology, err := readTopology(filepath.Join(appDir, "topology.json"))
	if err != nil {
		return AppPlan{}, fmt.Errorf("read topology for %s: %w", app.Name, err)
	}

	plan := AppPlan{Name: app.Name, Directory: app.Directory, Status: StatusGreen}
	plan.add(SeverityInfo, "compose", fmt.Sprintf("would create %s compose app from compose.yaml", target))
	addEnvironmentActions(&plan, appDir, topology)
	addRouteActions(&plan, target, topology)
	addDataStoreActions(&plan, topology)
	addLinkedResourceActions(&plan, topology)
	addVolumeActions(&plan, topology)
	plan.Status = statusFromActions(plan.Actions)
	return plan, nil
}

func addEnvironmentActions(plan *AppPlan, appDir string, topology analyzer.Topology) {
	envFiles := envExampleFiles(appDir)
	if len(envFiles) == 0 {
		return
	}

	items := []string{}
	for _, path := range envFiles {
		if count := envKeyCount(path); count > 0 {
			items = append(items, fmt.Sprintf("%s (%d vars)", filepath.Base(path), count))
		}
	}
	if len(items) == 0 {
		return
	}

	severity := SeverityInfo
	if topologyHasRisk(topology, "env.values_redacted") {
		severity = SeverityWarn
	}
	plan.add(severity, "environment", fmt.Sprintf("review and fill exported env examples before deploy: %s", strings.Join(items, ", ")))
}

func addRouteActions(plan *AppPlan, target string, topology analyzer.Topology) {
	if len(topology.Routes) == 0 {
		if topologyHasRisk(topology, "routes.none") {
			plan.add(SeverityWarn, "route", fmt.Sprintf("confirm this app is internal-only or add %s domains manually", target))
		}
		return
	}

	for _, route := range topology.Routes {
		message := fmt.Sprintf("would create %s domain %s", target, fallback(route.Host, "<missing host>"))
		if route.ServiceName != "" {
			message += " for service " + route.ServiceName
		}
		if route.Port != "" {
			message += " on port " + route.Port
		}
		plan.add(SeverityInfo, "route", message)
	}
}

func addDataStoreActions(plan *AppPlan, topology analyzer.Topology) {
	for _, store := range topology.DataStores {
		severity := SeverityWarn
		message := fmt.Sprintf("needs %s data store preparation for service %s with %s", store.Label(), fallback(store.Service, "unknown"), fallback(store.Strategy, "manual_review"))
		if store.Fallback != "" {
			message += "; fallback " + store.Fallback
		}
		if store.Criticality != "" {
			message += "; criticality " + store.Criticality
		}
		plan.add(severity, "data-store", message)
	}
}

func addLinkedResourceActions(plan *AppPlan, topology analyzer.Topology) {
	linksByKind := map[string][]analyzer.ResourceLink{}
	for _, link := range topology.LinkedResources {
		linksByKind[link.Kind] = append(linksByKind[link.Kind], link)
	}

	for _, requirement := range topology.ExternalRequirements {
		evidence := describeRequirementEvidence(requirement)
		if !analyzer.IsLinkableRequirement(requirement.Kind) {
			plan.add(SeverityWarn, "external-requirement", fmt.Sprintf("needs external %s requirement resolved%s", requirement.Kind, evidence))
			continue
		}

		links := linksByKind[requirement.Kind]
		switch {
		case len(links) == 0:
			plan.add(SeverityWarn, "linked-resource", fmt.Sprintf("needs support resource selection for external %s requirement%s", requirement.Kind, evidence))
		case len(links) == 1:
			severity := SeverityInfo
			if links[0].Confidence != "likely" {
				severity = SeverityWarn
			}
			plan.add(severity, "linked-resource", fmt.Sprintf("needs confirmation of %s support resource %s with %s confidence", requirement.Kind, fallback(links[0].App, "unknown"), fallback(links[0].Confidence, "unknown")))
		default:
			plan.add(SeverityWarn, "linked-resource", fmt.Sprintf("needs one %s support resource candidate selected: %s", requirement.Kind, strings.Join(resourceLinkLabels(links), ", ")))
		}
	}
}

func describeRequirementEvidence(requirement analyzer.Requirement) string {
	if len(requirement.Evidence) == 0 {
		return ""
	}
	return " from env names: " + strings.Join(requirement.Evidence, ", ")
}

func addVolumeActions(plan *AppPlan, topology analyzer.Topology) {
	for _, volume := range topology.StatefulVolumes {
		switch volume.Type {
		case "bind":
			plan.add(SeverityWarn, "volume", fmt.Sprintf("review bind mount portability for %s", statefulVolumeLabel(volume)))
		case "volume":
			plan.add(SeverityInfo, "volume", fmt.Sprintf("would create target volume and sync state for %s", statefulVolumeLabel(volume)))
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

func readTopology(path string) (analyzer.Topology, error) {
	var topology analyzer.Topology
	if err := readJSON(path, &topology); err != nil {
		return analyzer.Topology{}, err
	}
	return topology, nil
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
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || !strings.HasSuffix(name, ".example") {
			continue
		}
		paths = append(paths, filepath.Join(appDir, name))
	}
	sort.Strings(paths)
	return paths
}

func envKeyCount(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _ := strings.Cut(line, "=")
		if key != "" {
			count++
		}
	}
	return count
}

func topologyHasRisk(topology analyzer.Topology, code string) bool {
	for _, risk := range topology.RiskReasons {
		if risk.Code == code {
			return true
		}
	}
	return false
}

func resourceLinkLabels(links []analyzer.ResourceLink) []string {
	labels := make([]string, 0, len(links))
	for _, link := range links {
		label := fallback(link.App, "unknown")
		if link.Confidence != "" {
			label += " (" + link.Confidence + ")"
		}
		labels = append(labels, label)
	}
	return uniqueStrings(labels)
}

func statefulVolumeLabel(volume analyzer.StatefulVolume) string {
	label := fallback(volume.Service, "app")
	if volume.Target != "" {
		label += " -> " + volume.Target
	}
	return label
}

func (p *AppPlan) add(severity Severity, kind, message string) {
	p.Actions = append(p.Actions, Action{Severity: severity, Kind: kind, Message: message})
}

func statusFromActions(actions []Action) Status {
	status := StatusGreen
	for _, action := range actions {
		switch action.Severity {
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
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
