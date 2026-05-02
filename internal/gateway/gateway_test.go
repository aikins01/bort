package gateway

import (
	"slices"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func TestPlanBuildsDryRunCutoverStepsFromPrepareAndSync(t *testing.T) {
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
		t.Fatalf("unexpected cutover result metadata: %#v", result)
	}
	if result.Status != preparer.StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected cutover result: %#v", result)
	}

	app := result.Apps[0]
	if app.PrepareReadiness != preparer.ReadinessReadyToCreate || app.SyncReadiness != preparer.ReadinessReadyToCreate || app.Readiness != preparer.ReadinessNeedsDecision {
		t.Fatalf("unexpected cutover readiness: %#v", app)
	}
	if app.ObservationWindowSeconds != DefaultObservationWindowSeconds || app.RollbackWindowSeconds != DefaultRollbackWindowSeconds {
		t.Fatalf("unexpected cutover windows: %#v", app)
	}
	if len(app.Routes) != 1 || app.Routes[0].Host != "api.example.com" || app.Routes[0].TargetRef != "dokploy.domain:api.example.com" {
		t.Fatalf("unexpected route plan: %#v", app.Routes)
	}
	assertGate(t, app, "cutover.sync_verification_required")
	assertGate(t, app, "cutover.health_check_required")
	health := findStep(t, app, PhaseHealth)
	route := findStep(t, app, PhaseRoute)
	observe := findStep(t, app, PhaseObserve)
	rollback := findStep(t, app, PhaseRollback)
	if route.Readiness != preparer.ReadinessNeedsDecision || !slices.Contains(route.DependsOn, health.ID) {
		t.Fatalf("unexpected route step: %#v", route)
	}
	if !slices.Contains(observe.DependsOn, route.ID) || !slices.Contains(rollback.DependsOn, route.ID) {
		t.Fatalf("unexpected observe/rollback dependencies: observe=%#v rollback=%#v", observe, rollback)
	}
	assertCutoverAction(t, app, "route", "plan route cutover for route:api.example.com to dokploy.domain:api.example.com")
}

func TestPlanCarriesSyncBlockersIntoCutoverPlan(t *testing.T) {
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
	if result.Status != preparer.StatusRed || len(result.Apps) != 1 {
		t.Fatalf("unexpected cutover result: %#v", result)
	}
	app := result.Apps[0]
	if app.PrepareReadiness != preparer.ReadinessBlocked || app.SyncReadiness != preparer.ReadinessBlocked || app.Readiness != preparer.ReadinessBlocked {
		t.Fatalf("expected cutover to carry sync blocker, got %#v", app)
	}
	assertGate(t, app, "data_store.manual_review")
	assertGate(t, app, "cutover.sync_not_ready")
	preflight := findStep(t, app, PhasePreflight)
	if preflight.Readiness != preparer.ReadinessBlocked {
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
		t.Fatalf("expected needs_input cutover readiness, got %#v", app)
	}
	assertGate(t, app, "domain.host_missing")
	if route := findStep(t, app, PhaseRoute); route.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected route step to preserve needs_input, got %#v", route)
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

func assertCutoverAction(t *testing.T, app AppPlan, kind, message string) {
	t.Helper()
	for _, action := range app.Actions {
		if action.Kind == kind && action.Message == message {
			return
		}
	}
	t.Fatalf("expected action %s %q in %#v", kind, message, app.Actions)
}
