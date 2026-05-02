package rollback

import (
	"slices"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func TestPlanBuildsDryRunRollbackStepsFromCutover(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "3000", Source: "coolify"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	if result.APIVersion != APIVersion || !result.DryRun || result.Target != "dokploy" {
		t.Fatalf("unexpected rollback result metadata: %#v", result)
	}
	if result.Status != preparer.StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected rollback result: %#v", result)
	}

	app := result.Apps[0]
	if app.CutoverReadiness != preparer.ReadinessNeedsDecision || app.Readiness != preparer.ReadinessNeedsDecision {
		t.Fatalf("unexpected rollback readiness: %#v", app)
	}
	if app.ObservationWindowSeconds != DefaultObservationWindowSeconds {
		t.Fatalf("unexpected rollback observation window: %#v", app)
	}
	if len(app.Routes) != 1 || app.Routes[0].TargetRef != "dokploy.domain:api.example.com" || app.Routes[0].CurrentRef != "source.route:api.example.com" {
		t.Fatalf("unexpected rollback route: %#v", app.Routes)
	}
	assertGate(t, app, "rollback.trigger_required")
	assertGate(t, app, "rollback.source_health_required")
	sourceHealth := findStep(t, app, PhaseSourceHealth)
	route := findStep(t, app, PhaseRoute)
	if route.Readiness != preparer.ReadinessNeedsDecision || route.TargetRef != "source.route:api.example.com" || !slices.Contains(route.DependsOn, sourceHealth.ID) {
		t.Fatalf("unexpected route rollback step: %#v", route)
	}
	if route.Action != "rollback_route" {
		t.Fatalf("unexpected route rollback action: %#v", route)
	}
	observe := findStep(t, app, PhaseObserve)
	if !slices.Contains(observe.DependsOn, route.ID) {
		t.Fatalf("unexpected observe dependency: %#v", observe)
	}
	assertRollbackAction(t, app, "route", "plan route rollback for route:api.example.com to source.route:api.example.com")
}

func TestPlanCarriesCutoverBlockersIntoRollbackPlan(t *testing.T) {
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
					Mounts:      []manifest.Mount{{Type: "bind", Source: "/srv/web/uploads", Target: "/uploads", RW: true}},
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
	if result.Status != preparer.StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected rollback result: %#v", result)
	}
	app := result.Apps[0]
	if app.CutoverReadiness != preparer.ReadinessNeedsInput || app.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected rollback to carry cutover input needs, got %#v", app)
	}
	assertGate(t, app, "rollback.cutover_not_ready")
	preflight := findStep(t, app, PhasePreflight)
	if preflight.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("unexpected preflight step: %#v", preflight)
	}
}

func TestPlanPreservesRouteInputReadiness(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "api",
				Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}},
				Routes:   []manifest.Route{{ServiceName: "api", Port: "3000"}},
			},
		},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if app.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected needs_input rollback readiness, got %#v", app)
	}
	assertGate(t, app, "domain.host_missing")
	if route := findStep(t, app, PhaseRoute); route.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected route step to preserve needs_input, got %#v", route)
	}
}

func TestPlanSkipsObserveStepForZeroObservationWindow(t *testing.T) {
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

	zero := 0
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy", ObservationWindowSeconds: &zero})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if app.ObservationWindowSeconds != 0 {
		t.Fatalf("expected explicit zero observation window, got %#v", app)
	}
	for _, step := range app.Steps {
		if step.Phase == PhaseObserve {
			t.Fatalf("did not expect observe step for zero observation window: %#v", app.Steps)
		}
	}
}

func findStep(t *testing.T, app AppPlan, phase Phase) Step {
	t.Helper()
	for _, step := range app.Steps {
		if step.Phase == phase {
			return step
		}
	}
	t.Fatalf("expected %s step in %#v", phase, app.Steps)
	return Step{}
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

func assertRollbackAction(t *testing.T, app AppPlan, kind, message string) {
	t.Helper()
	for _, action := range app.Actions {
		if action.Kind == kind && action.Message == message {
			return
		}
	}
	t.Fatalf("expected action %s %q in %#v", kind, message, app.Actions)
}
