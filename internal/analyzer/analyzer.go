package analyzer

import (
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/manifest"
)

type AppAnalysis struct {
	InternalDependencies []Dependency
	ExternalRequirements []Requirement
	Networks             []string
}

type Dependency struct {
	Kind    string
	Service string
	Volumes []string
}

type Requirement struct {
	Kind     string
	Evidence []string
}

func AnalyzeApp(app manifest.App) AppAnalysis {
	dependencies := internalDependencies(app)
	return AppAnalysis{
		InternalDependencies: dependencies,
		ExternalRequirements: externalRequirements(app, dependencies),
		Networks:             appNetworks(app),
	}
}

func internalDependencies(app manifest.App) []Dependency {
	dependencies := []Dependency{}
	seen := map[string]struct{}{}
	for _, service := range app.Services {
		kind := serviceKind(service)
		if kind == "" {
			continue
		}

		name := serviceName(service)
		key := kind + "\x00" + name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		dependencies = append(dependencies, Dependency{
			Kind:    kind,
			Service: name,
			Volumes: serviceVolumes(service),
		})
	}
	sort.Slice(dependencies, func(i, j int) bool {
		if dependencies[i].Kind == dependencies[j].Kind {
			return dependencies[i].Service < dependencies[j].Service
		}
		return dependencies[i].Kind < dependencies[j].Kind
	})
	return dependencies
}

func externalRequirements(app manifest.App, dependencies []Dependency) []Requirement {
	internalKinds := map[string]struct{}{}
	for _, dependency := range dependencies {
		internalKinds[dependency.Kind] = struct{}{}
	}

	evidence := map[string]map[string]struct{}{}
	for _, env := range app.Environment {
		collectRequirementEvidence(evidence, env.Name)
	}
	for _, service := range app.Services {
		for _, env := range service.Environment {
			collectRequirementEvidence(evidence, env.Name)
		}
	}

	requirements := []Requirement{}
	for kind, values := range evidence {
		if _, ok := internalKinds[kind]; ok {
			continue
		}
		items := make([]string, 0, len(values))
		for value := range values {
			items = append(items, value)
		}
		sort.Strings(items)
		requirements = append(requirements, Requirement{Kind: kind, Evidence: items})
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Kind < requirements[j].Kind })
	return requirements
}

func collectRequirementEvidence(evidence map[string]map[string]struct{}, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	upper := strings.ToUpper(name)
	for _, kind := range requirementKinds(upper) {
		if evidence[kind] == nil {
			evidence[kind] = map[string]struct{}{}
		}
		evidence[kind][upper] = struct{}{}
	}
}

func requirementKinds(name string) []string {
	kinds := []string{}
	switch {
	case strings.Contains(name, "POSTGRES") || strings.Contains(name, "DATABASE_URL") || strings.HasPrefix(name, "DATABASE_") || strings.HasPrefix(name, "DB_") || strings.HasSuffix(name, "_DB"):
		kinds = append(kinds, "database")
	case strings.Contains(name, "MYSQL") || strings.Contains(name, "MARIADB"):
		kinds = append(kinds, "database")
	case strings.Contains(name, "MONGO"):
		kinds = append(kinds, "database")
	}
	if strings.Contains(name, "REDIS") || strings.Contains(name, "CACHE_URL") || strings.Contains(name, "QUEUE_URL") {
		kinds = append(kinds, "redis")
	}
	if strings.Contains(name, "MINIO") || strings.HasPrefix(name, "S3_") || strings.Contains(name, "S3_BUCKET") || strings.Contains(name, "AWS_ACCESS_KEY") || strings.Contains(name, "AWS_SECRET") {
		kinds = append(kinds, "object-storage")
	}
	if strings.HasPrefix(name, "SMTP_") || strings.HasPrefix(name, "EMAIL_HOST") {
		kinds = append(kinds, "email")
	}
	return uniqueStrings(kinds)
}

func serviceKind(service manifest.Service) string {
	name := strings.ToLower(serviceName(service))
	image := strings.ToLower(service.Image)
	combined := name + " " + image
	switch {
	case strings.Contains(combined, "postgres") || strings.Contains(combined, "pgvector"):
		return "database"
	case strings.Contains(combined, "mysql") || strings.Contains(combined, "mariadb") || strings.Contains(combined, "mongo"):
		return "database"
	case strings.Contains(combined, "redis") || strings.Contains(combined, "dragonfly"):
		return "redis"
	case strings.Contains(combined, "minio"):
		return "object-storage"
	case strings.Contains(combined, "qdrant") || strings.Contains(combined, "weaviate"):
		return "vector-db"
	case strings.Contains(combined, "searxng"):
		return "search"
	}
	return ""
}

func serviceName(service manifest.Service) string {
	if name := service.Labels["com.docker.compose.service"]; name != "" {
		return name
	}
	return service.Name
}

func serviceVolumes(service manifest.Service) []string {
	volumes := []string{}
	for _, mount := range service.Mounts {
		source := mount.Name
		if source == "" {
			source = mount.Source
		}
		if source == "" {
			continue
		}
		volumes = append(volumes, source+" -> "+mount.Target)
	}
	return uniqueStrings(volumes)
}

func appNetworks(app manifest.App) []string {
	networks := []string{}
	for _, service := range app.Services {
		for _, network := range service.Networks {
			if network.Name != "" {
				networks = append(networks, network.Name)
			}
		}
	}
	return uniqueStrings(networks)
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
