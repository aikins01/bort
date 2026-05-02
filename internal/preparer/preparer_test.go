package preparer

import (
	"slices"
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
	if result.APIVersion != APIVersion {
		t.Fatalf("unexpected api version: %q", result.APIVersion)
	}
	if result.Status != StatusRed || len(result.Apps) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	app := result.Apps[0]
	if app.Readiness != ReadinessBlocked || app.Resources.App.Readiness != ReadinessReadyToCreate {
		t.Fatalf("unexpected readiness: %#v", app)
	}
	if app.Resources.App.Type != "compose" || app.Resources.App.ComposePath != "compose.yaml" {
		t.Fatalf("unexpected app resource: %#v", app.Resources.App)
	}
	if len(app.Resources.Domains) != 1 || app.Resources.Domains[0].Host != "web.example.com" || app.Resources.Domains[0].ServiceName != "web" || app.Resources.Domains[0].Port != "3000" {
		t.Fatalf("unexpected domain resources: %#v", app.Resources.Domains)
	}
	if len(app.Resources.EnvFiles) != 1 || app.Resources.EnvFiles[0].Path != ".env.web.example" || !slices.Contains(app.Resources.EnvFiles[0].Keys, "DATABASE_URL") || !slices.Contains(app.Resources.EnvFiles[0].MissingValues, "DATABASE_URL") {
		t.Fatalf("unexpected env resources: %#v", app.Resources.EnvFiles)
	}
	if len(app.Resources.DataStores) != 1 || app.Resources.DataStores[0].Kind != "unknown" || app.Resources.DataStores[0].Readiness != ReadinessBlocked {
		t.Fatalf("unexpected data-store resources: %#v", app.Resources.DataStores)
	}
	if len(app.Resources.LinkedResources) != 1 || app.Resources.LinkedResources[0].Source != "heuristic" || !app.Resources.LinkedResources[0].RequiresConfirmation || app.Resources.LinkedResources[0].Confidence != "possible" {
		t.Fatalf("unexpected linked resources: %#v", app.Resources.LinkedResources)
	}
	if len(app.Resources.Volumes) != 1 || app.Resources.Volumes[0].Type != "bind" || app.Resources.Volumes[0].Portability != "review_required" {
		t.Fatalf("unexpected volume resources: %#v", app.Resources.Volumes)
	}
	for _, code := range []string{"env.values_required", "env.values_redacted", "data_store.manual_review", "linked_resource.confirm_candidate", "volume.bind_mount_review"} {
		assertGate(t, app, code)
	}
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

func TestPlanMarksSimpleAppReadyToCreate(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
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
	if result.Status != StatusGreen || result.Apps[0].Readiness != ReadinessReadyToCreate {
		t.Fatalf("unexpected ready app result: %#v", result)
	}
	if len(result.Apps[0].Gates) != 0 {
		t.Fatalf("did not expect gates for simple app: %#v", result.Apps[0].Gates)
	}
}

func TestPlanBlocksIncompleteDeployArtifact(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
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
	app := result.Apps[0]
	if result.Status != StatusRed || app.Readiness != ReadinessBlocked || app.Resources.App.Readiness != ReadinessBlocked {
		t.Fatalf("expected blocked deploy artifact, got %#v", result)
	}
	if !slices.Contains(app.Resources.App.MissingInputs, "TODO_REPLACE_IMAGE") {
		t.Fatalf("expected missing image placeholder in app resource: %#v", app.Resources.App)
	}
	assertGate(t, app, "app.compose_incomplete")
	assertGate(t, app, "deploy.missing_artifact")
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

func assertGate(t *testing.T, app AppPlan, code string) {
	t.Helper()
	for _, gate := range app.Gates {
		if gate.Code == code {
			return
		}
	}
	t.Fatalf("expected gate %q in %#v", code, app.Gates)
}
