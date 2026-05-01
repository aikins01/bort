package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
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

	fs.StringVar(&manifestPath, "manifest", "", "migration manifest path")
	fs.StringVar(&target, "target", "dokploy", "target platform")

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

	return writePlan(stdout, m, target)
}

func writePlan(w io.Writer, m manifest.Manifest, target string) error {
	volumeCount := len(m.Volumes)
	networkCount := len(m.Networks)
	routeCount := 0
	for _, app := range m.Apps {
		routeCount += len(app.Routes)
	}

	fmt.Fprintf(w, "Migration plan: %s -> %s\n", m.Source.Platform, target)
	fmt.Fprintf(w, "Host: %s\n", fallback(m.Source.Hostname, "unknown"))
	fmt.Fprintf(w, "Apps: %d, routes: %d, volumes: %d, networks: %d\n\n", len(m.Apps), routeCount, volumeCount, networkCount)

	for _, app := range m.Apps {
		analysis := analyzer.AnalyzeApp(app)
		status := classifyApp(app, analysis)
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
		if len(analysis.ExternalRequirements) > 0 {
			fmt.Fprintf(w, "  external requirements: %s\n", describeRequirements(analysis.ExternalRequirements))
		}
		fmt.Fprintf(w, "  state: %s\n", describeState(app))
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

func classifyApp(app manifest.App, analysis analyzer.AppAnalysis) string {
	deploy := deployReadiness(app)
	if deploy == deployMissing {
		return "red"
	}
	if deploy != deployReady {
		return "yellow"
	}
	if len(analysis.ExternalRequirements) > 0 {
		return "yellow"
	}

	if len(app.Routes) == 0 {
		return "yellow"
	}

	for _, service := range app.Services {
		for _, mount := range service.Mounts {
			if mount.Type == "bind" || mount.Type == "volume" {
				return "yellow"
			}
		}
	}

	return "green"
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

func summarizeList(values []string, limit int) string {
	if len(values) <= limit {
		return strings.Join(values, ", ")
	}
	visible := append([]string{}, values[:limit]...)
	visible = append(visible, fmt.Sprintf("+%d more", len(values)-limit))
	return strings.Join(visible, ", ")
}

type deployStatus int

const (
	deployReady deployStatus = iota
	deploySourceOnly
	deployResolvedOnly
	deployMissing
)

func deployReadiness(app manifest.App) deployStatus {
	if hasRawCompose(app) || hasServiceImage(app) {
		return deployReady
	}
	if hasSourceBuildMetadata(app) {
		return deploySourceOnly
	}
	if hasResolvedCompose(app) {
		return deployResolvedOnly
	}
	return deployMissing
}

func describeDeploy(app manifest.App) string {
	switch deployReadiness(app) {
	case deployReady:
		parts := []string{}
		if hasRawCompose(app) {
			parts = append(parts, "raw compose captured")
		}
		if hasServiceImage(app) {
			parts = append(parts, "image metadata captured")
		}
		return strings.Join(parts, "; ")
	case deploySourceOnly:
		return "source build metadata only; run server-local scan or repository export before migration"
	case deployResolvedOnly:
		return "resolved compose only; raw compose or server-local scan is required before migration"
	default:
		return "missing image or raw compose; server-local scan is required before migration"
	}
}

func hasRawCompose(app manifest.App) bool {
	return app.Compose != nil && strings.TrimSpace(app.Compose.Raw) != ""
}

func hasResolvedCompose(app manifest.App) bool {
	return app.Compose != nil && strings.TrimSpace(app.Compose.Resolved) != ""
}

func hasServiceImage(app manifest.App) bool {
	for _, service := range app.Services {
		if strings.TrimSpace(service.Image) != "" {
			return true
		}
	}
	return false
}

func hasSourceBuildMetadata(app manifest.App) bool {
	if app.Git == nil || strings.TrimSpace(app.Git.Repository) == "" || strings.TrimSpace(app.BuildPack) == "" {
		return false
	}
	if app.BuildPack == "dockercompose" {
		return strings.TrimSpace(app.Git.ComposeLocation) != ""
	}
	return true
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

func fallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
