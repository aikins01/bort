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
		status := classifyApp(app)
		fmt.Fprintf(w, "[%s] %s\n", status, app.Name)
		fmt.Fprintf(w, "  platform: %s\n", fallback(app.Platform, "docker"))
		fmt.Fprintf(w, "  services: %d\n", len(app.Services))
		if len(app.Routes) > 0 {
			fmt.Fprintf(w, "  routes: %s\n", strings.Join(routeHosts(app.Routes), ", "))
		} else {
			fmt.Fprintln(w, "  routes: none detected")
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

func classifyApp(app manifest.App) string {
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

func describeState(app manifest.App) string {
	var volumeMounts int
	var bindMounts int
	var envValuesRedacted bool

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

	parts := []string{}
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
	hosts := make([]string, 0, len(routes))
	for _, route := range routes {
		hosts = append(hosts, route.Host)
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
