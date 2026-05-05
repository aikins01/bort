package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func writePrivateTestBundle(t *testing.T, bundleDir string, m manifest.Manifest) {
	t.Helper()
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir, IncludeEnvValues: true}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleAppDirRejectsEscapingDirectoryHint(t *testing.T) {
	bundleDir := t.TempDir()
	dirs := map[string]string{"api": "../../../etc"}
	if _, err := bundleAppDir(bundleDir, "api", dirs); err == nil {
		t.Fatalf("expected escaping directory hint to be rejected")
	}
}

func TestApplyStateEnvToBundleRejectsMaliciousIndex(t *testing.T) {
	bundleDir := t.TempDir()
	indexJSON := []byte(`{"apps":[{"name":"api","directory":"../../../etc"}]}`)
	if err := os.WriteFile(filepath.Join(bundleDir, "index.json"), indexJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	state := setAppEnv(emptyBortState(), "api", map[string]string{"FIRST_KEY": "v"})
	if _, err := applyStateEnvToBundle(state, bundleDir); err == nil {
		t.Fatalf("expected applyStateEnvToBundle to reject escaping app directory")
	}
}

func TestApplyStateEnvToBundleFillsMissingValues(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(appDir, ".env.api")
	if err := os.WriteFile(envPath, []byte("FIRST_KEY=\nSECOND_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	state := emptyBortState()
	state = setAppEnv(state, "api", map[string]string{"FIRST_KEY": "first-value", "SECOND_KEY": "second-value"})

	touched, err := applyStateEnvToBundle(state, bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if touched != 1 {
		t.Fatalf("expected 1 file touched, got %d", touched)
	}

	contents, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(contents)
	if !strings.Contains(got, "FIRST_KEY=first-value") || !strings.Contains(got, "SECOND_KEY=second-value") {
		t.Fatalf("env file not merged correctly: %q", got)
	}
}

func TestApplyStateOverridesToPrepareDropsResolvedDataStoreGate(t *testing.T) {
	plan := preparer.Result{Apps: []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{
				DataStores: []preparer.DataStoreResource{
					{Service: "postgres", Kind: "postgres", Strategy: "manual_review"},
				},
			},
			Gates: []preparer.Gate{
				{Code: "data_store.prepare_required", ResourceRef: "data-store:postgres", Readiness: preparer.ReadinessNeedsDecision},
				{Code: "env.values_required", ResourceRef: "env:.env.api", Readiness: preparer.ReadinessNeedsInput},
			},
			Readiness: preparer.ReadinessNeedsDecision,
		},
	}}

	state := emptyBortState()
	state = setAppDataStrategy(state, "api", "postgres", dataStrategyMigrate)

	applyStateOverridesToPrepare(state, &plan)

	app := plan.Apps[0]
	if app.Resources.DataStores[0].Strategy != dataStrategyMigrate {
		t.Fatalf("expected strategy migrate, got %q", app.Resources.DataStores[0].Strategy)
	}
	for _, g := range app.Gates {
		if g.Code == "data_store.prepare_required" {
			t.Fatalf("expected data_store.prepare_required gate to be removed, got %#v", app.Gates)
		}
	}
	if len(app.Gates) != 1 || app.Gates[0].Code != "env.values_required" {
		t.Fatalf("expected only the env gate to remain, got %#v", app.Gates)
	}
	if app.Readiness != preparer.ReadinessNeedsInput {
		t.Fatalf("expected readiness recomputed to needs_input, got %q", app.Readiness)
	}
	if app.Status != preparer.StatusYellow {
		t.Fatalf("expected status recomputed to yellow, got %q", app.Status)
	}
}

func TestApplyStateOverridesPromotesReadyAppToGreen(t *testing.T) {
	plan := preparer.Result{Apps: []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{
				DataStores: []preparer.DataStoreResource{
					{Service: "postgres", Kind: "postgres", Strategy: "manual_review"},
				},
			},
			Gates: []preparer.Gate{
				{Code: "data_store.prepare_required", ResourceRef: "data-store:postgres", Readiness: preparer.ReadinessNeedsDecision},
			},
			Readiness: preparer.ReadinessNeedsDecision,
			Status:    preparer.StatusYellow,
		},
	}, Status: preparer.StatusYellow}
	state := setAppDataStrategy(emptyBortState(), "api", "postgres", dataStrategyMigrate)
	applyStateOverridesToPrepare(state, &plan)
	app := plan.Apps[0]
	if app.Readiness != preparer.ReadinessReadyToCreate {
		t.Fatalf("expected ready_to_create, got %q", app.Readiness)
	}
	if app.Status != preparer.StatusGreen {
		t.Fatalf("expected green, got %q", app.Status)
	}
	if plan.Status != preparer.StatusGreen {
		t.Fatalf("expected prepare status green, got %q", plan.Status)
	}
}

func TestApplyStateOverridesMatchesByKindWhenServiceMisses(t *testing.T) {
	plan := preparer.Result{Apps: []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{
				DataStores: []preparer.DataStoreResource{
					{Service: "db", Kind: "postgres", Strategy: "manual_review"},
				},
			},
			Gates: []preparer.Gate{
				{Code: "data_store.prepare_required", ResourceRef: "data-store:db", Readiness: preparer.ReadinessNeedsDecision},
			},
			Readiness: preparer.ReadinessNeedsDecision,
		},
	}}
	state := setAppDataStrategy(emptyBortState(), "api", "postgres", dataStrategyMigrate)
	applyStateOverridesToPrepare(state, &plan)
	if plan.Apps[0].Resources.DataStores[0].Strategy != dataStrategyMigrate {
		t.Fatalf("expected fallback to Kind to apply override, got %q", plan.Apps[0].Resources.DataStores[0].Strategy)
	}
	if len(plan.Apps[0].Gates) != 0 {
		t.Fatalf("expected service-keyed gate to be removed after kind match, got %#v", plan.Apps[0].Gates)
	}
}

func TestApplyStateOverridesDropsManualReviewDataStoreGate(t *testing.T) {
	plan := preparer.Result{Apps: []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{
				DataStores: []preparer.DataStoreResource{
					{Service: "db", Kind: "unknown", Strategy: "manual_review", Readiness: preparer.ReadinessBlocked},
				},
			},
			Gates: []preparer.Gate{
				{Code: "data_store.manual_review", ResourceRef: "data-store:db", Readiness: preparer.ReadinessBlocked},
			},
			Readiness: preparer.ReadinessBlocked,
			Status:    preparer.StatusRed,
		},
	}, Status: preparer.StatusRed}
	state := setAppDataStrategy(emptyBortState(), "api", "db", dataStrategyManaged)
	applyStateOverridesToPrepare(state, &plan)
	if len(plan.Apps[0].Gates) != 0 {
		t.Fatalf("expected manual-review data gate to be removed, got %#v", plan.Apps[0].Gates)
	}
	if plan.Apps[0].Readiness != preparer.ReadinessReadyToCreate || plan.Apps[0].Status != preparer.StatusGreen {
		t.Fatalf("expected app to become ready, got %#v", plan.Apps[0])
	}
	if plan.Status != preparer.StatusGreen {
		t.Fatalf("expected prepare status green, got %q", plan.Status)
	}
}

func TestApplyStateEnvToBundleMaterializesDefaultEnvExamples(t *testing.T) {
	bundleDir := t.TempDir()
	summary, err := exporter.Export(manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "My API",
				Services: []manifest.Service{{
					Name:  "api",
					Image: "example/api:latest",
					Environment: []manifest.EnvVar{
						{Name: "FIRST_KEY", Sensitive: true},
					},
				}},
			},
		},
	}, exporter.Options{OutputDir: bundleDir})
	if err != nil {
		t.Fatal(err)
	}

	state := setAppEnv(emptyBortState(), "My API", map[string]string{"FIRST_KEY": "filled-from-state"})
	touched, err := applyStateEnvToBundle(state, bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if touched != 1 {
		t.Fatalf("expected 1 file touched, got %d", touched)
	}

	appDir := filepath.Join(bundleDir, filepath.FromSlash(summary.Apps[0].Directory))
	contents, err := os.ReadFile(filepath.Join(appDir, ".env.api"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "FIRST_KEY=filled-from-state") {
		t.Fatalf("expected private env file to be filled, got %q", string(contents))
	}
	compose, err := os.ReadFile(filepath.Join(appDir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compose), ".env.api.example") || !strings.Contains(string(compose), ".env.api") {
		t.Fatalf("expected compose to reference private env file, got %q", string(compose))
	}

	prepare, err := preparer.Plan(preparer.Options{BundleDir: bundleDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range prepare.Apps[0].Gates {
		if gate.Code == "env.values_required" {
			t.Fatalf("expected materialized private env file to clear env gate, got %#v", gate)
		}
	}
}

func TestApplyStateEnvToBundlePreservesExistingValues(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(appDir, ".env.api")
	if err := os.WriteFile(envPath, []byte("FIRST_KEY=manual-value\nSECOND_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := setAppEnv(emptyBortState(), "api", map[string]string{
		"FIRST_KEY":  "from-state-should-not-overwrite",
		"SECOND_KEY": "filled-from-state",
	})
	if _, err := applyStateEnvToBundle(state, bundleDir); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(contents)
	if !strings.Contains(got, "FIRST_KEY=manual-value") {
		t.Fatalf("expected manual-value preserved, got: %s", got)
	}
	if strings.Contains(got, "from-state-should-not-overwrite") {
		t.Fatalf("state value clobbered manual edit: %s", got)
	}
	if !strings.Contains(got, "SECOND_KEY=filled-from-state") {
		t.Fatalf("expected empty SECOND_KEY filled from state, got: %s", got)
	}
}

func TestWriteBortStateUsesPrivateFileMode(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, ".bort", "state.json")
	state := setAppEnv(emptyBortState(), "api", map[string]string{"FIRST_KEY": "first-value"})
	if err := writeBortState(statePath, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected state.json mode 0600, got %o", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(statePath))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("expected .bort dir mode 0700, got %o", got)
	}
}

func TestReadBortStateRejectsUnknownAPIVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"apiVersion":"bort.state/v999","apps":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBortState(path); err == nil {
		t.Fatalf("expected error reading unknown apiVersion, got nil")
	}
}

func TestBortEnvThenBortClearsEnvIssue(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writePrivateTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{{
					Name:  "api",
					Image: "example/api:latest",
					Environment: []manifest.EnvVar{
						{Name: "FIRST_KEY", Sensitive: true},
					},
				}},
				Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	})

	// first create a run; the env file will have an empty value.
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"})

	// record the env value via the bort env command.
	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"env", "api", "FIRST_KEY=filled-from-state"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("bort env failed: %v\nstderr:\n%s", err, stderr.String())
	}

	// re-create the run so state env values are merged before preparer scans.
	stdout.Reset()
	stderr.Reset()
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"})

	run, err := loadMigrationRun("demo-app")
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range run.Prepare.Apps {
		for _, ef := range app.Resources.EnvFiles {
			if len(ef.MissingValues) != 0 {
				t.Fatalf("expected env file %s to have no missing values, got %v", ef.Path, ef.MissingValues)
			}
		}
		for _, gate := range app.Gates {
			if gate.Code == "env.values_required" {
				t.Fatalf("expected env.values_required gate to be cleared, got %#v", gate)
			}
		}
	}
}

func TestBortNoArgsRefreshesLatestRunAfterEnvCommand(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writePrivateTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{
				Name: "api",
				Services: []manifest.Service{{
					Name:  "api",
					Image: "example/api:latest",
					Environment: []manifest.EnvVar{
						{Name: "FIRST_KEY", Sensitive: true},
					},
				}},
				Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
			},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "demo-app", "--observation-window", "0", "--rollback-window", "0"})

	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"env", "api", "FIRST_KEY=filled-from-state"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("bort env failed: %v\nstderr:\n%s", err, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := RunWithInput(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("bort failed: %v\nstderr:\n%s", err, stderr.String())
	}
	output := stdout.String()
	if strings.Contains(output, "Fill environment values") {
		t.Fatalf("expected no-arg bort to refresh the latest run after env state, got:\n%s", output)
	}
	if !strings.Contains(output, "All app inputs ready") {
		t.Fatalf("expected refreshed app setup to be ready, got:\n%s", output)
	}
}
