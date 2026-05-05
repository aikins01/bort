package commit

import (
	"slices"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func TestPlanBuildsDryRunCommitStepsFromCutover(t *testing.T) {
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
		t.Fatalf("unexpected commit result metadata: %#v", result)
	}
	if result.Status != preparer.StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected commit result: %#v", result)
	}

	app := result.Apps[0]
	if app.CutoverReadiness != preparer.ReadinessNeedsDecision || app.Readiness != preparer.ReadinessNeedsDecision {
		t.Fatalf("unexpected commit readiness: %#v", app)
	}
	if app.RollbackWindowSeconds != DefaultRollbackWindowSeconds {
		t.Fatalf("unexpected rollback window: %#v", app)
	}
	if len(app.Routes) != 1 || app.Routes[0].Readiness != preparer.ReadinessNeedsDecision || app.Routes[0].TargetRef != "dokploy.domain:api.example.com" || app.Routes[0].CurrentRef != "source.route:api.example.com" {
		t.Fatalf("unexpected commit route: %#v", app.Routes)
	}
	assertGate(t, app, "commit.target_acceptance_required")
	assertGate(t, app, "commit.target_route_acceptance_required")
	assertGate(t, app, "commit.rollback_window_closed")
	verify := findStep(t, app, PhaseVerifyTarget)
	accept := findStep(t, app, PhaseAcceptTarget)
	retire := findStep(t, app, PhaseRetireSource)
	if accept.Readiness != preparer.ReadinessNeedsDecision || accept.TargetRef != "dokploy.domain:api.example.com" || !slices.Contains(accept.DependsOn, verify.ID) {
		t.Fatalf("unexpected accept step: %#v", accept)
	}
	if retire.TargetRef != "source.route:api.example.com" || !slices.Contains(retire.DependsOn, accept.ID) {
		t.Fatalf("unexpected retire step: %#v", retire)
	}
	assertCommitAction(t, app, "cleanup", "plan source route retirement for route:api.example.com")
}

func TestPlanCarriesCutoverBlockersIntoCommitPlan(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name:     "web",
				Metadata: map[string]string{"migrationRole": "candidate", "coolify.project": "demo-project"},
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
				Metadata: map[string]string{"migrationRole": "support", "coolify.project": "demo-project"},
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
		t.Fatalf("unexpected commit result: %#v", result)
	}
	app := result.Apps[0]
	if app.CutoverReadiness != preparer.ReadinessNeedsInput || app.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected commit to carry cutover input needs, got %#v", app)
	}
	assertGate(t, app, "commit.cutover_not_ready")
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
		t.Fatalf("expected needs_input commit readiness, got %#v", app)
	}
	assertGate(t, app, "domain.host_missing")
	if route := findStep(t, app, PhaseAcceptTarget); route.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected accept step to preserve needs_input, got %#v", route)
	}
}

func TestPlanSkipsRollbackWindowGateForZeroRollbackWindow(t *testing.T) {
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
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy", RollbackWindowSeconds: &zero})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if app.RollbackWindowSeconds != 0 {
		t.Fatalf("expected explicit zero rollback window, got %#v", app)
	}
	assertGate(t, app, "commit.target_route_acceptance_required")
	assertNoGate(t, app, "commit.rollback_window_closed")
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

func assertNoGate(t *testing.T, app AppPlan, code string) {
	t.Helper()
	for _, gate := range app.Gates {
		if gate.Code == code {
			t.Fatalf("did not expect gate %q in %#v", code, app.Gates)
		}
	}
}

func assertCommitAction(t *testing.T, app AppPlan, kind, message string) {
	t.Helper()
	for _, action := range app.Actions {
		if action.Kind == kind && action.Message == message {
			return
		}
	}
	t.Fatalf("expected action %s %q in %#v", kind, message, app.Actions)
}
