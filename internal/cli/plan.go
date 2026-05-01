package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/manifest"
)

func runPlan(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var manifestPath string
	var target string
	var appName string
	var role string

	fs.StringVar(&manifestPath, "manifest", "", "migration manifest path")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to plan")
	fs.StringVar(&role, "role", "", "optional migration role to plan: candidate, support, platform")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if manifestPath == "" {
		return fmt.Errorf("--manifest is required")
	}

	file, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer file.Close()

	var m manifest.Manifest
	if err := json.NewDecoder(file).Decode(&m); err != nil {
		return err
	}

	return writePlanWithOptions(stdout, m, planOptions{Target: target, AppName: appName, Role: role})
}

func writePlan(w io.Writer, m manifest.Manifest, target string) error {
	return writePlanWithOptions(w, m, planOptions{Target: target})
}

type planOptions struct {
	Target  string
	AppName string
	Role    string
}

func writePlanWithOptions(w io.Writer, m manifest.Manifest, opts planOptions) error {
	if opts.Target == "" {
		opts.Target = "dokploy"
	}
	apps := filteredPlanApps(m.Apps, opts)
	if len(apps) == 0 && (opts.AppName != "" || opts.Role != "") {
		return planFilterError(opts)
	}

	volumeCount := len(m.Volumes)
	networkCount := len(m.Networks)
	routeCount := 0
	for _, app := range apps {
		routeCount += len(app.Routes)
	}

	fmt.Fprintf(w, "Migration plan: %s -> %s\n", m.Source.Platform, opts.Target)
	fmt.Fprintf(w, "Host: %s\n", fallback(m.Source.Hostname, "unknown"))
	fmt.Fprintf(w, "Apps: %d, routes: %d, volumes: %d, networks: %d\n", len(apps), routeCount, volumeCount, networkCount)
	if opts.AppName != "" || opts.Role != "" {
		fmt.Fprintf(w, "Filters: %s\n", describePlanFilters(opts))
	}
	fmt.Fprintln(w)

	for _, app := range apps {
		analysis := analyzer.AnalyzeApp(app)
		status := classifyApp(analysis)
		fmt.Fprintf(w, "[%s] %s\n", status, app.Name)
		fmt.Fprintf(w, "  platform: %s\n", fallback(app.Platform, "docker"))
		if app.Runtime != "" {
			fmt.Fprintf(w, "  runtime: %s\n", app.Runtime)
		}
		if role := app.Metadata["migrationRole"]; role != "" {
			fmt.Fprintf(w, "  role: %s\n", role)
		}
		if project := app.Metadata["coolify.project"]; project != "" {
			fmt.Fprintf(w, "  project: %s\n", project)
		}
		if app.BuildPack != "" {
			fmt.Fprintf(w, "  build pack: %s\n", app.BuildPack)
		}
		fmt.Fprintf(w, "  services: %d\n", len(app.Services))
		if len(app.Routes) > 0 {
			fmt.Fprintf(w, "  routes: %s\n", strings.Join(routeHosts(app.Routes), ", "))
		} else {
			fmt.Fprintln(w, "  routes: none detected")
		}
		fmt.Fprintf(w, "  deploy: %s\n", describeDeploy(app))
		if len(analysis.Networks) > 0 {
			fmt.Fprintf(w, "  networks: %s\n", summarizeList(analysis.Networks, 4))
		}
		if len(analysis.InternalDependencies) > 0 {
			fmt.Fprintf(w, "  %s: %s\n", dependencyLabel(app), describeDependencies(analysis.InternalDependencies))
		}
		if len(analysis.DataStores) > 0 {
			fmt.Fprintf(w, "  data stores: %s\n", describeDataStores(analysis.DataStores))
		}
		if len(analysis.ExternalRequirements) > 0 {
			fmt.Fprintf(w, "  external requirements: %s\n", describeRequirements(analysis.ExternalRequirements))
		}
		fmt.Fprintf(w, "  state: %s\n", describeState(app))
		if len(analysis.RiskReasons) > 0 {
			fmt.Fprintf(w, "  risk reasons: %s\n", describeRiskReasons(analysis.RiskReasons))
		}
		fmt.Fprintln(w)
	}

	if len(m.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range m.Warnings {
			fmt.Fprintf(w, "  - %s: %s\n", warning.Code, warning.Message)
		}
	}

	return nil
}

func classifyApp(analysis analyzer.AppAnalysis) string {
	status := "green"
	for _, reason := range analysis.RiskReasons {
		switch reason.Severity {
		case analyzer.RiskError:
			return "red"
		case analyzer.RiskWarn:
			status = "yellow"
		}
	}
	return status
}

func describeDependencies(dependencies []analyzer.Dependency) string {
	items := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		item := dependency.Kind + "=" + dependency.Service
		if len(dependency.Volumes) > 0 {
			item += " volumes[" + summarizeList(dependency.Volumes, 2) + "]"
		}
		items = append(items, item)
	}
	return strings.Join(items, "; ")
}

func describeDataStores(stores []analyzer.DataStore) string {
	items := make([]string, 0, len(stores))
	for _, store := range stores {
		item := store.Kind + "=" + store.Service
		if store.Engine != "" && store.Engine != store.Kind {
			item += " engine=" + store.Engine
		}
		if len(store.Volumes) > 0 {
			item += " volumes[" + summarizeList(store.Volumes, 2) + "]"
		}
		item += " strategy=" + store.Strategy
		if store.Fallback != "" {
			item += " fallback=" + store.Fallback
		}
		item += " criticality=" + store.Criticality
		items = append(items, item)
	}
	return strings.Join(items, "; ")
}

func dependencyLabel(app manifest.App) string {
	if app.Metadata["migrationRole"] == "support" || app.Runtime == "database" {
		return "detected services"
	}
	return "internal dependencies"
}

func describeRequirements(requirements []analyzer.Requirement) string {
	items := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		item := requirement.Kind
		if len(requirement.Evidence) > 0 {
			item += " via " + summarizeList(requirement.Evidence, 3)
		}
		items = append(items, item)
	}
	return strings.Join(items, "; ")
}

func describeRiskReasons(reasons []analyzer.RiskReason) string {
	items := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		items = append(items, string(reason.Severity)+" "+reason.Code+": "+reason.Message)
	}
	return strings.Join(items, "; ")
}

func summarizeList(values []string, limit int) string {
	if len(values) <= limit {
		return strings.Join(values, ", ")
	}
	visible := append([]string{}, values[:limit]...)
	visible = append(visible, fmt.Sprintf("+%d more", len(values)-limit))
	return strings.Join(visible, ", ")
}

func describeDeploy(app manifest.App) string {
	switch analyzer.DeployReadiness(app) {
	case analyzer.DeployReady:
		parts := []string{}
		if analyzer.HasRawCompose(app) {
			parts = append(parts, "raw compose captured")
		}
		if analyzer.HasServiceImage(app) {
			parts = append(parts, "image metadata captured")
		}
		return strings.Join(parts, "; ")
	case analyzer.DeploySourceOnly:
		return "source build metadata only; run server-local scan or repository export before migration"
	case analyzer.DeployResolvedOnly:
		return "resolved compose only; raw compose or server-local scan is required before migration"
	default:
		return "missing image or raw compose; server-local scan is required before migration"
	}
}

func describeState(app manifest.App) string {
	var volumeMounts int
	var bindMounts int
	var envValuesRedacted bool
	parts := []string{}

	for _, service := range app.Services {
		for _, mount := range service.Mounts {
			switch mount.Type {
			case "volume":
				volumeMounts++
			case "bind":
				bindMounts++
			}
		}

		for _, env := range service.Environment {
			if !env.ValueKnown {
				envValuesRedacted = true
			}
		}
	}

	for _, env := range app.Environment {
		if !env.ValueKnown {
			envValuesRedacted = true
		}
	}
	if len(app.Storages) > 0 {
		parts = append(parts, fmt.Sprintf("%d Coolify storage record(s)", len(app.Storages)))
	}

	if volumeMounts > 0 {
		parts = append(parts, fmt.Sprintf("%d named volume mount(s)", volumeMounts))
	}
	if bindMounts > 0 {
		parts = append(parts, fmt.Sprintf("%d bind mount(s)", bindMounts))
	}
	if envValuesRedacted {
		parts = append(parts, "environment values redacted")
	}
	if len(parts) == 0 {
		return "stateless from discovered Docker metadata"
	}
	return strings.Join(parts, "; ")
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

func filteredPlanApps(apps []manifest.App, opts planOptions) []manifest.App {
	filtered := []manifest.App{}
	for _, app := range apps {
		if opts.AppName != "" && !matchesPlanApp(app, opts.AppName) {
			continue
		}
		if opts.Role != "" && !strings.EqualFold(app.Metadata["migrationRole"], opts.Role) {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func matchesPlanApp(app manifest.App, name string) bool {
	return app.Name == name || app.ID == name || slug(app.Name) == slug(name) || app.Metadata["coolify.uuid"] == name
}

func planFilterError(opts planOptions) error {
	if opts.AppName != "" && opts.Role != "" {
		return fmt.Errorf("no apps matched --app %q and --role %q", opts.AppName, opts.Role)
	}
	if opts.AppName != "" {
		return fmt.Errorf("app %q not found in manifest", opts.AppName)
	}
	return fmt.Errorf("no apps with role %q found in manifest", opts.Role)
}

func describePlanFilters(opts planOptions) string {
	filters := []string{}
	if opts.AppName != "" {
		filters = append(filters, "app="+opts.AppName)
	}
	if opts.Role != "" {
		filters = append(filters, "role="+opts.Role)
	}
	return strings.Join(filters, ", ")
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
