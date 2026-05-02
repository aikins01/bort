package analyzer

import (
	"testing"

	"github.com/aikins01/bort/internal/manifest"
)

func TestAnalyzeAppFindsInternalDependencies(t *testing.T) {
	analysis := AnalyzeApp(manifest.App{
		Services: []manifest.Service{
			{
				Name:  "web",
				Image: "example/web",
				Environment: []manifest.EnvVar{
					{Name: "DATABASE_URL"},
					{Name: "REDIS_URL"},
					{Name: "SMTP_PASSWORD"},
				},
				Networks: []manifest.ServiceNetwork{{Name: "app-net"}},
			},
			{
				Name:     "postgres-1",
				Image:    "postgres:16-alpine",
				Mounts:   []manifest.Mount{{Type: "volume", Name: "pgdata", Target: "/var/lib/postgresql/data"}},
				Networks: []manifest.ServiceNetwork{{Name: "app-net"}},
				Labels:   map[string]string{"com.docker.compose.service": "postgres"},
			},
			{
				Name:     "redis-1",
				Image:    "redis:7-alpine",
				Mounts:   []manifest.Mount{{Type: "volume", Name: "redis-data", Target: "/data"}},
				Networks: []manifest.ServiceNetwork{{Name: "app-net"}},
				Labels:   map[string]string{"com.docker.compose.service": "redis"},
			},
		},
	})

	if len(analysis.InternalDependencies) != 2 {
		t.Fatalf("expected postgres and redis dependencies, got %#v", analysis.InternalDependencies)
	}
	if analysis.InternalDependencies[0].Kind != "database" || analysis.InternalDependencies[0].Service != "postgres" {
		t.Fatalf("unexpected first dependency: %#v", analysis.InternalDependencies[0])
	}
	if analysis.InternalDependencies[1].Kind != "redis" || analysis.InternalDependencies[1].Service != "redis" {
		t.Fatalf("unexpected second dependency: %#v", analysis.InternalDependencies[1])
	}
	if len(analysis.ExternalRequirements) != 1 || analysis.ExternalRequirements[0].Kind != "email" {
		t.Fatalf("expected only email to remain external, got %#v", analysis.ExternalRequirements)
	}
	if len(analysis.Networks) != 1 || analysis.Networks[0] != "app-net" {
		t.Fatalf("unexpected networks: %#v", analysis.Networks)
	}
}

func TestAnalyzeAppFindsExternalRequirements(t *testing.T) {
	analysis := AnalyzeApp(manifest.App{
		Services: []manifest.Service{
			{
				Name:  "web",
				Image: "example/web",
				Environment: []manifest.EnvVar{
					{Name: "DATABASE_URL"},
					{Name: "MINIO_ENDPOINT"},
					{Name: "REDIS_HOST"},
				},
			},
		},
	})

	if len(analysis.InternalDependencies) != 0 {
		t.Fatalf("expected no internal dependencies, got %#v", analysis.InternalDependencies)
	}
	want := []string{"database", "object-storage", "redis"}
	if len(analysis.ExternalRequirements) != len(want) {
		t.Fatalf("expected %d external requirements, got %#v", len(want), analysis.ExternalRequirements)
	}
	for i, kind := range want {
		if analysis.ExternalRequirements[i].Kind != kind {
			t.Fatalf("expected requirement %d to be %s, got %#v", i, kind, analysis.ExternalRequirements[i])
		}
	}
}

func TestAnalyzeAppClassifiesDataStoresAndRisks(t *testing.T) {
	analysis := AnalyzeApp(manifest.App{
		Services: []manifest.Service{
			{
				Name:  "postgres-1",
				Image: "pgvector/pgvector:pg16",
				Mounts: []manifest.Mount{
					{Type: "volume", Name: "pgdata", Target: "/var/lib/postgresql/data"},
				},
				Labels: map[string]string{"com.docker.compose.service": "postgres"},
			},
			{
				Name:  "redis-1",
				Image: "docker.dragonflydb.io/dragonflydb/dragonfly",
				Mounts: []manifest.Mount{
					{Type: "volume", Name: "redis-data", Target: "/data"},
				},
				Labels: map[string]string{"com.docker.compose.service": "redis"},
			},
			{Name: "uploads", Image: "example/uploads", Mounts: []manifest.Mount{{Type: "bind", Source: "/srv/uploads", Target: "/uploads"}}},
			{Name: "clickhouse", Image: "clickhouse/clickhouse-server:latest", Mounts: []manifest.Mount{{Type: "volume", Name: "clickhouse-data", Target: "/var/lib/clickhouse"}}},
			{Name: "metabase", Image: "metabase/metabase:latest", Mounts: []manifest.Mount{{Type: "volume", Name: "metabase-data", Target: "/metabase-data"}}},
		},
		Routes: []manifest.Route{{Host: "app.example.com"}},
	})

	postgres := findDataStore(t, analysis.DataStores, "postgres", "postgres")
	if postgres.Engine != "pgvector" || postgres.Strategy != "pg_dump_restore_or_logical_replication" || postgres.Criticality != "critical" {
		t.Fatalf("unexpected postgres data store: %#v", postgres)
	}
	redis := findDataStore(t, analysis.DataStores, "redis", "redis")
	if redis.Engine != "dragonfly" || redis.Fallback != "recreate_if_cache_only" || redis.Criticality != "unknown" {
		t.Fatalf("unexpected redis data store: %#v", redis)
	}
	clickhouse := findDataStore(t, analysis.DataStores, "clickhouse", "clickhouse")
	if clickhouse.Strategy != "native_backup_or_export" || clickhouse.Criticality != "critical" {
		t.Fatalf("unexpected clickhouse data store: %#v", clickhouse)
	}
	if len(analysis.DataStores) != 3 {
		t.Fatalf("expected only recognized data stores, got %#v", analysis.DataStores)
	}
	if len(analysis.StatefulVolumes) != 5 {
		t.Fatalf("expected five stateful volumes, got %#v", analysis.StatefulVolumes)
	}
	assertRisk(t, analysis.RiskReasons, "data_store.postgres")
	assertRisk(t, analysis.RiskReasons, "data_store.redis")
	assertRisk(t, analysis.RiskReasons, "data_store.clickhouse")
	assertRisk(t, analysis.RiskReasons, "state.bind_mounts")
}

func TestAnalyzeAppKeepsManualReviewForDatabaseLikeUnknowns(t *testing.T) {
	analysis := AnalyzeApp(manifest.App{
		Services: []manifest.Service{
			{
				Name:   "custom-db",
				Image:  "example/private-database:latest",
				Labels: map[string]string{"coolify.service.subType": "database"},
				Mounts: []manifest.Mount{{Type: "volume", Name: "custom-db-data", Target: "/data"}},
			},
		},
		Routes: []manifest.Route{{Host: "db-admin.example.com"}},
	})

	unknown := findDataStore(t, analysis.DataStores, "unknown", "custom-db")
	if unknown.Strategy != "manual_review" {
		t.Fatalf("unexpected unknown data store: %#v", unknown)
	}
	assertRisk(t, analysis.RiskReasons, "data_store.manual_review")
}

func TestAnalyzeAppInManifestLinksSupportResources(t *testing.T) {
	app := manifest.App{
		ID:       "compose:app",
		Name:     "app",
		Metadata: map[string]string{"migrationRole": "candidate", "coolify.project": "vela"},
		Services: []manifest.Service{{
			Name:        "web",
			Image:       "example/web",
			Environment: []manifest.EnvVar{{Name: "DATABASE_URL"}},
			Networks:    []manifest.ServiceNetwork{{Name: "app-db-net"}},
		}},
	}
	support := manifest.App{
		ID:       "compose:postgres",
		Name:     "postgres support",
		Runtime:  "database",
		Metadata: map[string]string{"migrationRole": "support", "coolify.project": "vela"},
		Services: []manifest.Service{{
			Name:     "postgres",
			Image:    "pgvector/pgvector:pg16",
			Networks: []manifest.ServiceNetwork{{Name: "app-db-net"}},
		}},
	}

	analysis := AnalyzeAppInManifest(manifest.Manifest{Apps: []manifest.App{app, support}}, app)
	if len(analysis.LinkedResources) != 1 {
		t.Fatalf("expected one linked resource, got %#v", analysis.LinkedResources)
	}
	link := analysis.LinkedResources[0]
	if link.Kind != "database" || link.App != "postgres support" || link.Confidence != "likely" {
		t.Fatalf("unexpected link: %#v", link)
	}
	if len(link.DataStores) != 1 || link.DataStores[0].Engine != "pgvector" {
		t.Fatalf("unexpected linked data stores: %#v", link.DataStores)
	}
	assertRisk(t, analysis.RiskReasons, "linked_resource.database")

	topology := TopologyForAppInManifest(manifest.Manifest{Apps: []manifest.App{app, support}}, app)
	if len(topology.LinkedResources) != 1 || topology.LinkedResources[0].App != "postgres support" {
		t.Fatalf("expected topology linked resource, got %#v", topology.LinkedResources)
	}
}

func findDataStore(t *testing.T, stores []DataStore, kind, service string) DataStore {
	t.Helper()
	for _, store := range stores {
		if store.Kind == kind && store.Service == service {
			return store
		}
	}
	t.Fatalf("expected %s data store for %s in %#v", kind, service, stores)
	return DataStore{}
}

func assertRisk(t *testing.T, risks []RiskReason, code string) {
	t.Helper()
	for _, risk := range risks {
		if risk.Code == code {
			return
		}
	}
	t.Fatalf("expected risk %s in %#v", code, risks)
}
