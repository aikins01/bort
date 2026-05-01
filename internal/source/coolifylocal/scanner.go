package coolifylocal

import (
	"context"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
	"github.com/aikins01/bort/internal/source/localdocker"
)

type Scanner struct {
	Docker source.Scanner
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
	enrichApps(&result)
	return result, nil
}

func enrichApps(result *manifest.Manifest) {
	renamed := map[string]string{}
	for i := range result.Apps {
		oldName := result.Apps[i].Name
		enrichApp(&result.Apps[i])
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

func enrichApp(app *manifest.App) {
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
	return firstNonEmpty(labels["coolify.resourceName"], labels["coolify.serviceName"], labels["com.docker.compose.project"])
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
