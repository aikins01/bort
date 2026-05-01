package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/manifest"
)

func TestWritePlanFlagsMissingDeployableMetadata(t *testing.T) {
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify", Hostname: "example.com"},
		Apps: []manifest.App{
			{
				Name:     "image app",
				Platform: "coolify",
				Services: []manifest.Service{{Name: "web", Image: "example/web:latest"}},
				Routes:   []manifest.Route{{Host: "web.example.com"}},
			},
			{
				Name:      "source app",
				Platform:  "coolify",
				BuildPack: "dockercompose",
				Git:       &manifest.GitSource{Repository: "example/repo", ComposeLocation: "/compose.yaml"},
				Services:  []manifest.Service{{Name: "web"}},
				Routes:    []manifest.Route{{Host: "source.example.com"}},
			},
			{
				Name:     "missing app",
				Platform: "coolify",
				Services: []manifest.Service{{Name: "web"}},
				Routes:   []manifest.Route{{Host: "missing.example.com"}},
			},
		},
	}

	var out bytes.Buffer
	if err := writePlan(&out, m, "dokploy"); err != nil {
		t.Fatal(err)
	}
	plan := out.String()
	for _, want := range []string{
		"[green] image app",
		"deploy: image metadata captured",
		"[yellow] source app",
		"deploy: source build metadata only; run server-local scan or repository export before migration",
		"[red] missing app",
		"deploy: missing image or raw compose; server-local scan is required before migration",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected plan to contain %q, got:\n%s", want, plan)
		}
	}
}

func TestWritePlanShowsTopologyAnalysis(t *testing.T) {
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", Hostname: "example.com"},
		Apps: []manifest.App{
			{
				Name:     "stack",
				Platform: "coolify",
				Services: []manifest.Service{
					{
						Name:        "web",
						Image:       "example/web:latest",
						Environment: []manifest.EnvVar{{Name: "DATABASE_URL"}, {Name: "REDIS_URL"}, {Name: "MINIO_ENDPOINT"}},
						Networks:    []manifest.ServiceNetwork{{Name: "stack-net"}},
					},
					{
						Name:     "postgres-1",
						Image:    "postgres:16-alpine",
						Mounts:   []manifest.Mount{{Type: "volume", Name: "pgdata", Target: "/var/lib/postgresql/data"}},
						Networks: []manifest.ServiceNetwork{{Name: "stack-net"}},
						Labels:   map[string]string{"com.docker.compose.service": "postgres"},
					},
					{
						Name:     "redis-1",
						Image:    "redis:7-alpine",
						Mounts:   []manifest.Mount{{Type: "volume", Name: "redis-data", Target: "/data"}},
						Networks: []manifest.ServiceNetwork{{Name: "stack-net"}},
						Labels:   map[string]string{"com.docker.compose.service": "redis"},
					},
				},
				Routes: []manifest.Route{{Host: "stack.example.com"}},
			},
		},
	}

	var out bytes.Buffer
	if err := writePlan(&out, m, "dokploy"); err != nil {
		t.Fatal(err)
	}
	plan := out.String()
	for _, want := range []string{
		"[yellow] stack",
		"networks: stack-net",
		"internal dependencies: database=postgres volumes[pgdata -> /var/lib/postgresql/data]; redis=redis volumes[redis-data -> /data]",
		"external requirements: object-storage via MINIO_ENDPOINT",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected plan to contain %q, got:\n%s", want, plan)
		}
	}
}
