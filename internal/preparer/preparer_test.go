package preparer

import (
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
)

func TestPlanBuildsDryRunActionsFromTopology(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "web",
				Metadata: map[string]string{"migrationRole": "candidate", "coolify.project": "vela"},
				Services: []manifest.Service{{
					Name:        "web",
					Image:       "example/web:latest",
					Environment: []manifest.EnvVar{{Name: "DATABASE_URL"}},
					Mounts:      []manifest.Mount{{Type: "bind", Source: "/srv/web/uploads", Target: "/uploads"}},
				}},
				Routes: []manifest.Route{{Host: "web.example.com", ServiceName: "web", Port: "3000"}},
			},
			{
				Name:     "postgres support",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support", "coolify.project": "vela"},
				Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Plan(Options{BundleDir: dir, AppName: "web", Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	app := result.Apps[0]
	for _, want := range []string{
		"compose|would create dokploy compose app from compose.yaml",
		"environment|review and fill exported env examples before deploy: .env.web.example (1 vars)",
		"route|would create dokploy domain web.example.com for service web on port 3000",
		"data-store|needs unknown data store preparation for service web with manual_review; fallback stopped_volume_copy; criticality unknown",
		"linked-resource|needs confirmation of database support resource postgres support with possible confidence",
		"volume|review bind mount portability for web -> /uploads",
	} {
		assertAction(t, app, want)
	}
}

func TestPlanSkipsRouteActionForInternalSupportResource(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "postgres support",
				Runtime:  "database",
				Metadata: map[string]string{"migrationRole": "support"},
				Services: []manifest.Service{{Name: "postgres", Image: "postgres:16-alpine"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Plan(Options{BundleDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Apps) != 1 {
		t.Fatalf("expected one app, got %#v", result.Apps)
	}
	for _, action := range result.Apps[0].Actions {
		if action.Kind == "route" {
			t.Fatalf("did not expect route action for support resource: %#v", result.Apps[0].Actions)
		}
	}
}

func assertAction(t *testing.T, app AppPlan, want string) {
	t.Helper()
	for _, action := range app.Actions {
		got := action.Kind + "|" + action.Message
		if strings.EqualFold(got, want) {
			return
		}
	}
	t.Fatalf("expected action %q in %#v", want, app.Actions)
}
