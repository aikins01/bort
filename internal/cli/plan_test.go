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

func TestWritePlanRedactsRepositoryCredentials(t *testing.T) {
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "api",
		Platform: "coolify",
		Git:      &manifest.GitSource{Repository: "https://token:secret@example.com/org/api.git", Branch: "main"},
		Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
	}}}
	var out bytes.Buffer
	if err := writePlan(&out, m, "dokploy"); err != nil {
		t.Fatal(err)
	}
	plan := out.String()
	if strings.Contains(plan, "token") || strings.Contains(plan, "secret@") {
		t.Fatalf("expected repository credentials to be redacted, got:\n%s", plan)
	}
	if !strings.Contains(plan, "https://example.com/org/api.git") {
		t.Fatalf("expected redacted repository host/path, got:\n%s", plan)
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
		"data stores: postgres=postgres volumes[pgdata -> /var/lib/postgresql/data] strategy=pg_dump_restore_or_logical_replication fallback=stopped_volume_copy criticality=critical; redis=redis volumes[redis-data -> /data] strategy=snapshot_aof_or_volume_copy fallback=recreate_if_cache_only criticality=unknown",
		"external requirements: object-storage via MINIO_ENDPOINT",
		"risk reasons:",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected plan to contain %q, got:\n%s", want, plan)
		}
	}
}

func TestWritePlanFiltersByAppAndRole(t *testing.T) {
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify-local", Hostname: "example.com"},
		Apps: []manifest.App{
			{
				ID:       "compose:candidate-1",
				Name:     "new demo app",
				Platform: "coolify",
				Metadata: map[string]string{"migrationRole": "candidate", "coolify.uuid": "candidate-1"},
				Services: []manifest.Service{{Name: "web", Image: "example/web"}},
				Routes:   []manifest.Route{{Host: "candidate.example.com"}},
			},
			{
				ID:       "compose:support-1",
				Name:     "postgresql database",
				Platform: "coolify",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support"},
				Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
			},
		},
	}

	var out bytes.Buffer
	if err := writePlanWithOptions(&out, m, planOptions{Target: "dokploy", AppName: "new-demo-app", Role: "candidate"}); err != nil {
		t.Fatal(err)
	}
	plan := out.String()
	for _, want := range []string{"Apps: 1", "Filters: app=new-demo-app, role=candidate", "[green] new demo app"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected filtered plan to contain %q, got:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "postgresql database") {
		t.Fatalf("expected app filter to exclude support app, got:\n%s", plan)
	}

	out.Reset()
	if err := writePlanWithOptions(&out, m, planOptions{Target: "dokploy", Role: "support"}); err != nil {
		t.Fatal(err)
	}
	plan = out.String()
	if !strings.Contains(plan, "[yellow] postgresql database") || strings.Contains(plan, "new demo app") {
		t.Fatalf("expected role filter to include only support app, got:\n%s", plan)
	}

	if err := writePlanWithOptions(&out, m, planOptions{Target: "dokploy", AppName: "missing"}); err == nil {
		t.Fatal("expected missing app filter to fail")
	}
}
