package analyzer

import (
	"testing"

	"github.com/aikins01/bort/internal/manifest"
)

func TestAnalyzeAppFindsInternalDependencies(t *testing.T) {
	analysis := AnalyzeApp(manifest.App{
		Services: []manifest.Service{
			{
				Name:  "web",
				Image: "example/web",
				Environment: []manifest.EnvVar{
					{Name: "DATABASE_URL"},
					{Name: "REDIS_URL"},
					{Name: "SMTP_PASSWORD"},
				},
				Networks: []manifest.ServiceNetwork{{Name: "app-net"}},
			},
			{
				Name:     "postgres-1",
				Image:    "postgres:16-alpine",
				Mounts:   []manifest.Mount{{Type: "volume", Name: "pgdata", Target: "/var/lib/postgresql/data"}},
				Networks: []manifest.ServiceNetwork{{Name: "app-net"}},
				Labels:   map[string]string{"com.docker.compose.service": "postgres"},
			},
			{
				Name:     "redis-1",
				Image:    "redis:7-alpine",
				Mounts:   []manifest.Mount{{Type: "volume", Name: "redis-data", Target: "/data"}},
				Networks: []manifest.ServiceNetwork{{Name: "app-net"}},
				Labels:   map[string]string{"com.docker.compose.service": "redis"},
			},
		},
	})

	if len(analysis.InternalDependencies) != 2 {
		t.Fatalf("expected postgres and redis dependencies, got %#v", analysis.InternalDependencies)
	}
	if analysis.InternalDependencies[0].Kind != "database" || analysis.InternalDependencies[0].Service != "postgres" {
		t.Fatalf("unexpected first dependency: %#v", analysis.InternalDependencies[0])
	}
	if analysis.InternalDependencies[1].Kind != "redis" || analysis.InternalDependencies[1].Service != "redis" {
		t.Fatalf("unexpected second dependency: %#v", analysis.InternalDependencies[1])
	}
	if len(analysis.ExternalRequirements) != 1 || analysis.ExternalRequirements[0].Kind != "email" {
		t.Fatalf("expected only email to remain external, got %#v", analysis.ExternalRequirements)
	}
	if len(analysis.Networks) != 1 || analysis.Networks[0] != "app-net" {
		t.Fatalf("unexpected networks: %#v", analysis.Networks)
	}
}

func TestAnalyzeAppFindsExternalRequirements(t *testing.T) {
	analysis := AnalyzeApp(manifest.App{
		Services: []manifest.Service{
			{
				Name:  "web",
				Image: "example/web",
				Environment: []manifest.EnvVar{
					{Name: "DATABASE_URL"},
					{Name: "MINIO_ENDPOINT"},
					{Name: "REDIS_HOST"},
				},
			},
		},
	})

	if len(analysis.InternalDependencies) != 0 {
		t.Fatalf("expected no internal dependencies, got %#v", analysis.InternalDependencies)
	}
	want := []string{"database", "object-storage", "redis"}
	if len(analysis.ExternalRequirements) != len(want) {
		t.Fatalf("expected %d external requirements, got %#v", len(want), analysis.ExternalRequirements)
	}
	for i, kind := range want {
		if analysis.ExternalRequirements[i].Kind != kind {
			t.Fatalf("expected requirement %d to be %s, got %#v", i, kind, analysis.ExternalRequirements[i])
		}
	}
}
