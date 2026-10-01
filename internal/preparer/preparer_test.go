package preparer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/analyzer"
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
				Metadata: map[string]string{"migrationRole": "candidate", "coolify.project": "demo-project", "coolify.environment": "production"},
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
				Metadata: map[string]string{"migrationRole": "support", "coolify.project": "demo-project", "coolify.environment": "production"},
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
	if result.Status != StatusYellow || len(result.Apps) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	app := result.Apps[0]
	if app.Readiness != ReadinessNeedsInput || app.Resources.App.Readiness != ReadinessReadyToCreate {
		t.Fatalf("unexpected readiness: %#v", app)
	}
	if app.ProjectGroup == nil || app.ProjectGroup.Name != "web" || app.ProjectGroup.Environment != "production" {
		t.Fatalf("unexpected project group: %#v", app.ProjectGroup)
	}
	if app.Resources.App.Type != "compose" || app.Resources.App.ComposePath != "compose.yaml" || app.Resources.App.ComposeSource != ComposeSourceGenerated {
		t.Fatalf("unexpected app resource: %#v", app.Resources.App)
	}
	if len(app.Resources.Domains) != 1 || app.Resources.Domains[0].Host != "web.example.com" || app.Resources.Domains[0].ServiceName != "web" || app.Resources.Domains[0].Port != "3000" {
		t.Fatalf("unexpected domain resources: %#v", app.Resources.Domains)
	}
	if len(app.Resources.EnvFiles) != 1 || app.Resources.EnvFiles[0].Path != ".env.web.example" || !slices.Contains(app.Resources.EnvFiles[0].Keys, "DATABASE_URL") || !slices.Contains(app.Resources.EnvFiles[0].MissingValues, "DATABASE_URL") {
		t.Fatalf("unexpected env resources: %#v", app.Resources.EnvFiles)
	}
	if len(app.Resources.DataStores) != 0 {
		t.Fatalf("unexpected data-store resources: %#v", app.Resources.DataStores)
	}
	if len(app.Resources.LinkedResources) != 1 || app.Resources.LinkedResources[0].Source != "heuristic" || app.Resources.LinkedResources[0].RequiresConfirmation || app.Resources.LinkedResources[0].Confidence != "possible" {
		t.Fatalf("unexpected linked resources: %#v", app.Resources.LinkedResources)
	}
	if len(app.Resources.Volumes) != 1 || app.Resources.Volumes[0].Type != "bind" || app.Resources.Volumes[0].Portability != "host_path_preserved" || app.Resources.Volumes[0].Readiness != ReadinessReadyToCreate {
		t.Fatalf("unexpected volume resources: %#v", app.Resources.Volumes)
	}
	if app.TargetResources == nil || app.TargetResources.Platform != "dokploy" || !app.TargetResources.DryRun || app.TargetResources.Dokploy == nil {
		t.Fatalf("unexpected target resources: %#v", app.TargetResources)
	}
	dokploy := app.TargetResources.Dokploy
	if dokploy.Project.Name != "web" || dokploy.Project.Environment != "production" || dokploy.Project.Source != "app" {
		t.Fatalf("unexpected dokploy project group: %#v", dokploy.Project)
	}
	if dokploy.ComposeApp.Name != "web" || dokploy.ComposeApp.Readiness != ReadinessReadyToCreate {
		t.Fatalf("unexpected dokploy compose app: %#v", dokploy.ComposeApp)
	}
	if len(dokploy.Domains) != 1 || dokploy.Domains[0].AttachTo != "web" || dokploy.Domains[0].Host != "web.example.com" {
		t.Fatalf("unexpected dokploy domains: %#v", dokploy.Domains)
	}
	if len(dokploy.EnvFiles) != 1 || !dokploy.EnvFiles[0].NeedsValues {
		t.Fatalf("unexpected dokploy env files: %#v", dokploy.EnvFiles)
	}
	if len(dokploy.Volumes) != 1 || dokploy.Volumes[0].Action != "preserve_vps_file_mount" {
		t.Fatalf("unexpected dokploy volumes: %#v", dokploy.Volumes)
	}
	if len(dokploy.DataStores) != 0 {
		t.Fatalf("unexpected dokploy data stores: %#v", dokploy.DataStores)
	}
	if len(dokploy.LinkedResources) != 1 || dokploy.LinkedResources[0].RequiresConfirmation || dokploy.LinkedResources[0].Source != "heuristic" || dokploy.LinkedResources[0].Action != "reuse_detected_support_resource" {
		t.Fatalf("unexpected dokploy linked resources: %#v", dokploy.LinkedResources)
	}
	for _, code := range []string{"env.values_required", "env.values_redacted"} {
		assertGate(t, app, code)
	}
	for _, code := range []string{"linked_resource.confirm_candidate", "volume.bind_mount_review"} {
		assertNoGate(t, app, code)
	}
	for _, want := range []string{
		"compose|would create dokploy compose app from compose.yaml",
		"environment|review and fill exported env examples before deploy: .env.web.example (1 vars)",
		"route|would create dokploy domain web.example.com for service web on port 3000",
		"linked-resource|detected database uses postgres support (possible match) in Dokploy",
		"volume|will preserve VPS file/folder /srv/web/uploads -> /uploads",
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
	if result.Apps[0].TargetResources == nil || result.Apps[0].TargetResources.Dokploy == nil || result.Apps[0].TargetResources.Dokploy.ComposeApp.Readiness != ReadinessReadyToCreate {
		t.Fatalf("expected ready dokploy target render: %#v", result.Apps[0].TargetResources)
	}
}

func TestPlanSurfacesSourceControlAsNonBlockingAction(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify"},
		Apps: []manifest.App{{
			Name:      "api",
			BuildPack: "dockercompose",
			Git: &manifest.GitSource{
				Repository:      "https://github.com/example/api",
				Branch:          "main",
				Provider:        "github",
				SourceType:      "App\\Models\\GithubApp",
				SourceID:        "42",
				ComposeLocation: "/docker-compose.yml",
			},
			Compose:  &manifest.ComposeSource{Raw: "services:\n  api:\n    image: example/api\n"},
			Services: []manifest.Service{{Name: "api", Image: "example/api"}},
			Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api"}},
		}},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}

	result, err := Plan(Options{BundleDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if app.Readiness != ReadinessReadyToCreate || app.Resources.SourceControl == nil || app.Resources.SourceControl.Auth != "coolify_github_app" {
		t.Fatalf("expected source control to be non-blocking, got %#v", app)
	}
	if app.Resources.App.ComposeSource != ComposeSourceRaw {
		t.Fatalf("expected raw compose provenance, got %#v", app.Resources.App)
	}
	if app.TargetResources == nil || app.TargetResources.Dokploy == nil || app.TargetResources.Dokploy.SourceControl == nil || app.TargetResources.Dokploy.SourceControl.Action != "connect_dokploy_source_after_cutover_if_needed" {
		t.Fatalf("expected dokploy source-control action, got %#v", app.TargetResources)
	}
	assertAction(t, app, "source-control|will not copy Coolify source credentials for https://github.com/example/api; connect a Dokploy source after cutover for future Git deploys")
	for _, gate := range app.Gates {
		if strings.HasPrefix(gate.Code, "source_control.") {
			t.Fatalf("did not expect source-control gate: %#v", app.Gates)
		}
	}
}

func TestPlanPersistsReviewedRawComposeRouteService(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify"},
		Apps: []manifest.App{{
			Name:    "api",
			Compose: &manifest.ComposeSource{Raw: "services:\n  apiworker:\n    image: example/api\n"},
			Services: []manifest.Service{{
				Name:   "project-apiworker-1",
				Image:  "example/api",
				Labels: map[string]string{"com.docker.compose.service": "apiworker"},
			}},
			Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "project-apiworker-1", Port: "8080"}},
		}},
	}

	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if len(app.Resources.Domains) != 1 || app.Resources.Domains[0].ServiceName != "apiworker" {
		t.Fatalf("expected reviewed route service in prepare output, got %#v", app.Resources.Domains)
	}
	if app.TargetResources == nil || app.TargetResources.Dokploy == nil || len(app.TargetResources.Dokploy.Domains) != 1 || app.TargetResources.Dokploy.Domains[0].ServiceName != "apiworker" {
		t.Fatalf("expected reviewed route service in target output, got %#v", app.TargetResources)
	}
	assertAction(t, app, "route|would create dokploy domain api.example.com for service apiworker on port 8080")
}

func TestPlanPersistsUniqueComposeServiceForEmptyRouteMapping(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "api",
		Compose:  &manifest.ComposeSource{Raw: "services:\n  api:\n    image: example/api\n"},
		Services: []manifest.Service{{Name: "api", Image: "example/api"}},
		Routes:   []manifest.Route{{Host: "api.example.com", Port: "8080"}},
	}}}
	exportWithLegacyRoutes(t, dir, m)
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if result.Status != StatusGreen || app.Readiness != ReadinessReadyToCreate || len(app.Resources.Domains) != 1 || app.Resources.Domains[0].ServiceName != "api" {
		t.Fatalf("expected unique Compose service to be persisted, got %#v", app)
	}
	assertNoGate(t, app, GateDomainServiceMissing)
}

func TestPlanMapsLegacyFQDNRouteToUniqueRawComposeService(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:    "api",
		Compose: &manifest.ComposeSource{Raw: "services:\n  api:\n    image: example/api\n"},
		Routes:  []manifest.Route{{Host: "api.example.com", ServiceName: "coolify-fqdn-target", Port: "8080", Source: "fqdn"}},
	}}}
	exportWithLegacyRoutes(t, dir, m)

	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if result.Status != StatusGreen || app.Readiness != ReadinessReadyToCreate || len(app.Resources.Domains) != 1 || app.Resources.Domains[0].ServiceName != "api" {
		t.Fatalf("legacy FQDN route was not mapped to the unique raw Compose service: %#v", app)
	}
	assertNoGate(t, app, GateDomainServiceNotInCompose)
}

func TestPlanPersistsSluggedGeneratedComposeService(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "blog",
		Services: []manifest.Service{{Name: "Ghost Blog", Image: "example/blog"}},
		Routes:   []manifest.Route{{Host: "blog.example.com", ServiceName: "Ghost Blog", Port: "3000"}},
	}}}
	exportWithLegacyRoutes(t, dir, m)
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if result.Status != StatusGreen || app.Readiness != ReadinessReadyToCreate || len(app.Resources.Domains) != 1 || app.Resources.Domains[0].ServiceName != "ghost-blog" {
		t.Fatalf("expected generated Compose service name to be persisted, got %#v", app)
	}
	assertNoGate(t, app, GateDomainServiceNotInCompose)
}

func exportWithLegacyRoutes(t *testing.T, dir string, m manifest.Manifest) {
	t.Helper()
	summary, err := exporter.Export(m, exporter.Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(dir, summary.Apps[0].Directory)
	topologyPath := filepath.Join(appDir, "topology.json")
	contents, err := os.ReadFile(topologyPath)
	if err != nil {
		t.Fatal(err)
	}
	var topology analyzer.Topology
	if err := json.Unmarshal(contents, &topology); err != nil {
		t.Fatal(err)
	}
	topology.Routes = append([]manifest.Route(nil), m.Apps[0].Routes...)
	contents, err = json.MarshalIndent(topology, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topologyPath, append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	routes, err := json.MarshalIndent(m.Apps[0].Routes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "routes.json"), append(routes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPlanBlocksMismatchedGeneratedComposeService(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "blog",
		Services: []manifest.Service{{Name: "Ghost Blog", Image: "example/blog"}},
		Routes:   []manifest.Route{{Host: "blog.example.com", ServiceName: "Wrong Blog", Port: "3000"}},
	}}}
	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if result.Status != StatusRed || app.Readiness != ReadinessBlocked || len(app.Resources.Domains) != 1 || app.Resources.Domains[0].ServiceName != "Wrong Blog" {
		t.Fatalf("expected mismatched generated Compose service to block preparation, got %#v", app)
	}
	assertGate(t, app, GateDomainServiceNotInCompose)
}

func TestPlanReportsMalformedCompose(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "api",
		Compose:  &manifest.ComposeSource{Raw: "services:\n  api:\n    image: example/api\n"},
		Services: []manifest.Service{{Name: "api", Image: "example/api"}},
		Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api", Port: "8080"}},
	}}}
	summary, err := exporter.Export(m, exporter.Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, summary.Apps[0].Directory, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  api: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(Options{BundleDir: dir, Target: "dokploy"}); err == nil || !strings.Contains(err.Error(), "read compose services for api") {
		t.Fatalf("expected malformed Compose error, got %v", err)
	}
}

func TestPlanBlocksEmptyRouteMappingWithMultipleComposeServices(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "api",
		Compose:  &manifest.ComposeSource{Raw: "services:\n  web:\n    image: example/web\n  api:\n    image: example/api\n"},
		Services: []manifest.Service{{Name: "web", Image: "example/web"}, {Name: "api", Image: "example/api"}},
		Routes:   []manifest.Route{{Host: "api.example.com", Port: "8080"}},
	}}}
	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if result.Status != StatusRed || app.Readiness != ReadinessBlocked || len(app.Resources.Domains) != 1 || app.Resources.Domains[0].Readiness != ReadinessBlocked {
		t.Fatalf("expected ambiguous empty route mapping to block preparation, got %#v", app)
	}
	assertGate(t, app, GateDomainServiceMissing)
}

func TestPlanBlocksRouteServiceMissingFromCompose(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "api",
		Compose:  &manifest.ComposeSource{Raw: "services:\n  api:\n    image: example/api\n"},
		Services: []manifest.Service{{Name: "api", Image: "example/api"}},
		Routes:   []manifest.Route{{Host: "api.example.com", ServiceName: "api-stale", Port: "8080"}},
	}}}
	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}
	result, err := Plan(Options{BundleDir: dir, Target: "dokploy"})
	if err != nil {
		t.Fatal(err)
	}
	app := result.Apps[0]
	if result.Status != StatusRed || app.Readiness != ReadinessBlocked || len(app.Resources.Domains) != 1 || app.Resources.Domains[0].Readiness != ReadinessBlocked {
		t.Fatalf("expected missing Compose service to block preparation, got %#v", app)
	}
	assertGate(t, app, GateDomainServiceNotInCompose)
	if app.TargetResources == nil || app.TargetResources.Dokploy == nil || len(app.TargetResources.Dokploy.Domains) != 1 || app.TargetResources.Dokploy.Domains[0].Readiness != ReadinessBlocked {
		t.Fatalf("expected blocked route in target output, got %#v", app.TargetResources)
	}
}

func TestPlanRejectsUnknownComposeSource(t *testing.T) {
	dir := t.TempDir()
	m := manifest.Manifest{Apps: []manifest.App{{
		Name:     "api",
		Services: []manifest.Service{{Name: "api", Image: "example/api"}},
	}}}
	if _, err := exporter.Export(m, exporter.Options{OutputDir: dir}); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "index.json")
	contents, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var summary exporter.Summary
	if err := json.Unmarshal(contents, &summary); err != nil {
		t.Fatal(err)
	}
	summary.Apps[0].ComposeSource = "genrated"
	contents, err = json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(Options{BundleDir: dir}); err == nil || !strings.Contains(err.Error(), `unsupported compose source "genrated"`) {
		t.Fatalf("expected unknown compose source rejection, got %v", err)
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
	if app.TargetResources == nil || app.TargetResources.Dokploy == nil || app.TargetResources.Dokploy.ComposeApp.Readiness != ReadinessBlocked {
		t.Fatalf("expected blocked dokploy compose app: %#v", app.TargetResources)
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

func TestWorseReadinessPrioritizesMissingInputOverDecision(t *testing.T) {
	if got := WorseReadiness(ReadinessNeedsInput, ReadinessNeedsDecision); got != ReadinessNeedsInput {
		t.Fatalf("expected needs_input to outrank needs_decision, got %s", got)
	}
	if got := WorseReadiness(ReadinessNeedsDecision, ReadinessNeedsInput); got != ReadinessNeedsInput {
		t.Fatalf("expected needs_input to outrank needs_decision, got %s", got)
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

func assertNoGate(t *testing.T, app AppPlan, code string) {
	t.Helper()
	for _, gate := range app.Gates {
		if gate.Code == code {
			t.Fatalf("did not expect gate %q in %#v", code, app.Gates)
		}
	}
}
