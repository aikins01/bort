package sync

import (
	"slices"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func TestPlanBuildsDryRunStateSyncStepsFromPrepareResources(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "postgres support",
				Services: []manifest.Service{{
					Name:  "postgres",
					Image: "postgres:16-alpine",
					Mounts: []manifest.Mount{{
						Type:   "volume",
						Name:   "pgdata",
						Target: "/var/lib/postgresql/data",
						RW:     true,
					}},
				}},
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
		t.Fatalf("unexpected sync result metadata: %#v", result)
	}
	if result.Status != preparer.StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected sync result: %#v", result)
	}

	app := result.Apps[0]
	if app.PrepareReadiness != preparer.ReadinessNeedsDecision || app.Readiness != preparer.ReadinessNeedsDecision {
		t.Fatalf("unexpected sync readiness: %#v", app)
	}
	dataStore := findStep(t, app, "data_store")
	if dataStore.Strategy != StrategyPostgresDumpOrLogical || dataStore.TargetAction != "sync_data_store_with_planned_strategy" || dataStore.Pause != PauseCutoverWindow || dataStore.Readiness != preparer.ReadinessReadyToCreate {
		t.Fatalf("unexpected data-store sync step: %#v", dataStore)
	}
	volume := findStep(t, app, "volume")
	if volume.Strategy != StrategyDockerVolumeArchive || volume.TargetAction != "create_volume_and_sync_state" || volume.Pause != PauseStoppedSource {
		t.Fatalf("unexpected volume sync step: %#v", volume)
	}
	verify := findStep(t, app, "app")
	if verify.Phase != PhaseVerify || !slices.Contains(verify.DependsOn, dataStore.ID) || !slices.Contains(verify.DependsOn, volume.ID) {
		t.Fatalf("unexpected verify step: %#v", verify)
	}
	assertSyncAction(t, app, "state-sync", "plan pg_dump_restore_or_logical_replication data-store sync for data-store:postgres")
}

func TestPlanCarriesPrepareBlockersIntoSyncPlan(t *testing.T) {
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
		t.Fatalf("unexpected sync result: %#v", result)
	}
	app := result.Apps[0]
	if app.PrepareReadiness != preparer.ReadinessNeedsInput || app.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected sync to carry prepare input needs, got %#v", app)
	}
	linked := findStep(t, app, "linked_resource")
	if linked.TargetAction != "reuse_detected_support_resource" || linked.Action != "reuse_detected_support_resource" || linked.Pause != PauseNone || linked.Readiness != preparer.ReadinessReadyToCreate {
		t.Fatalf("unexpected linked resource step: %#v", linked)
	}
	volume := findStep(t, app, "volume")
	if volume.Strategy != StrategyNone || volume.Action != "preserve_host_path_mount" || volume.TargetAction != "preserve_vps_file_mount" || volume.Pause != PauseNone || volume.Readiness != preparer.ReadinessReadyToCreate {
		t.Fatalf("unexpected bind volume step: %#v", volume)
	}
}

func findStep(t *testing.T, app AppPlan, resourceType string) Step {
	t.Helper()
	for _, step := range app.Steps {
		if step.ResourceType == resourceType {
			return step
		}
	}
	t.Fatalf("expected %s step in %#v", resourceType, app.Steps)
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

func assertSyncAction(t *testing.T, app AppPlan, kind, message string) {
	t.Helper()
	for _, action := range app.Actions {
		if action.Kind == kind && action.Message == message {
			return
		}
	}
	t.Fatalf("expected action %s %q in %#v", kind, message, app.Actions)
}
