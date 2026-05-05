package analyzer

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/planutil"
)

type AppAnalysis struct {
	InternalDependencies []Dependency     `json:"internalDependencies,omitempty"`
	ExternalRequirements []Requirement    `json:"externalRequirements,omitempty"`
	DataStores           []DataStore      `json:"dataStores,omitempty"`
	LinkedResources      []ResourceLink   `json:"linkedResources,omitempty"`
	StatefulVolumes      []StatefulVolume `json:"statefulVolumes,omitempty"`
	Networks             []string         `json:"networks,omitempty"`
	RiskReasons          []RiskReason     `json:"riskReasons,omitempty"`
}

type Topology struct {
	Networks             []string         `json:"networks"`
	InternalDependencies []Dependency     `json:"internalDependencies"`
	ExternalRequirements []Requirement    `json:"externalRequirements"`
	DataStores           []DataStore      `json:"dataStores"`
	LinkedResources      []ResourceLink   `json:"linkedResources"`
	StatefulVolumes      []StatefulVolume `json:"statefulVolumes"`
	Routes               []manifest.Route `json:"routes"`
	SourceServices       []SourceService  `json:"sourceServices,omitempty"`
	SourceControl        *SourceControl   `json:"sourceControl,omitempty"`
	RiskReasons          []RiskReason     `json:"riskReasons"`
}

// SourceService describes a single source-side container belonging to an
// app, regardless of whether it owns persistent state. apply uses it to
// quiesce app workers/web containers around volume sync without having to
// re-read the manifest.
type SourceService struct {
	ServiceName   string `json:"serviceName"`
	ContainerID   string `json:"containerId,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	Image         string `json:"image,omitempty"`
}

type SourceControl struct {
	Repository   string   `json:"repository,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	CommitSHA    string   `json:"commitSha,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Auth         string   `json:"auth,omitempty"`
	SourceType   string   `json:"sourceType,omitempty"`
	SourceID     string   `json:"sourceId,omitempty"`
	PrivateKeyID string   `json:"privateKeyId,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
}

type Dependency struct {
	Kind    string   `json:"kind"`
	Service string   `json:"service"`
	Volumes []string `json:"volumes,omitempty"`
}

type Requirement struct {
	Kind     string   `json:"kind"`
	Evidence []string `json:"evidence,omitempty"`
}

type DataStore struct {
	Kind                string   `json:"kind"`
	Engine              string   `json:"engine,omitempty"`
	Service             string   `json:"service"`
	Image               string   `json:"image,omitempty"`
	Volumes             []string `json:"volumes,omitempty"`
	Strategy            string   `json:"strategy"`
	Fallback            string   `json:"fallback,omitempty"`
	Criticality         string   `json:"criticality"`
	SourceContainerID   string   `json:"sourceContainerId,omitempty"`
	SourceContainerName string   `json:"sourceContainerName,omitempty"`
}

func (s DataStore) Label() string {
	label := s.Kind
	if s.Engine != "" && s.Engine != s.Kind {
		label += "/" + s.Engine
	}
	return label
}

type ResourceLink struct {
	Kind       string      `json:"kind"`
	App        string      `json:"app"`
	AppID      string      `json:"appId,omitempty"`
	Role       string      `json:"role,omitempty"`
	Runtime    string      `json:"runtime,omitempty"`
	Confidence string      `json:"confidence"`
	Reasons    []string    `json:"reasons,omitempty"`
	Networks   []string    `json:"networks,omitempty"`
	DataStores []DataStore `json:"dataStores,omitempty"`
}

type StatefulVolume struct {
	Origin              string `json:"origin,omitempty"`
	Service             string `json:"service"`
	Type                string `json:"type"`
	Name                string `json:"name,omitempty"`
	Source              string `json:"source,omitempty"`
	Target              string `json:"target"`
	RW                  bool   `json:"rw"`
	SizeBytes           int64  `json:"sizeBytes,omitempty"`
	FileCount           int64  `json:"fileCount,omitempty"`
	SourceContainerID   string `json:"sourceContainerId,omitempty"`
	SourceContainerName string `json:"sourceContainerName,omitempty"`
}

type RiskSeverity string

const (
	RiskInfo  RiskSeverity = "info"
	RiskWarn  RiskSeverity = "warn"
	RiskError RiskSeverity = "error"
)

type RiskReason struct {
	Severity RiskSeverity `json:"severity"`
	Code     string       `json:"code"`
	Message  string       `json:"message"`
}

type DeployStatus string

const (
	DeployReady        DeployStatus = "ready"
	DeploySourceOnly   DeployStatus = "source-only"
	DeployResolvedOnly DeployStatus = "resolved-only"
	DeployMissing      DeployStatus = "missing"
)

func AnalyzeApp(app manifest.App) AppAnalysis {
	analysis := analyzeApp(app)
	analysis.RiskReasons = riskReasons(app, analysis)
	return analysis
}

func AnalyzeAppInManifest(m manifest.Manifest, app manifest.App) AppAnalysis {
	analysis := analyzeApp(app)
	analysis.LinkedResources = linkedResources(m, app, analysis)
	analysis.StatefulVolumes = enrichVolumeSizes(analysis.StatefulVolumes, m.Volumes)
	analysis.RiskReasons = riskReasons(app, analysis)
	return analysis
}

// joins per-app stateful volumes with measured sizes from the manifest's
// global volume table so plans can show byte/file counts without a second
// scan.
func enrichVolumeSizes(volumes []StatefulVolume, manifestVolumes []manifest.Volume) []StatefulVolume {
	if len(volumes) == 0 || len(manifestVolumes) == 0 {
		return volumes
	}
	byName := make(map[string]manifest.Volume, len(manifestVolumes))
	for _, v := range manifestVolumes {
		byName[v.Name] = v
	}
	for i := range volumes {
		if volumes[i].Name == "" {
			continue
		}
		if v, ok := byName[volumes[i].Name]; ok {
			volumes[i].SizeBytes = v.SizeBytes
			volumes[i].FileCount = v.FileCount
		}
	}
	return volumes
}

func analyzeApp(app manifest.App) AppAnalysis {
	dependencies := internalDependencies(app)
	return AppAnalysis{
		InternalDependencies: dependencies,
		ExternalRequirements: externalRequirements(app, dependencies),
		DataStores:           dataStores(app),
		StatefulVolumes:      statefulVolumes(app),
		Networks:             appNetworks(app),
	}
}

func TopologyForApp(app manifest.App) Topology {
	analysis := AnalyzeApp(app)
	return topologyForApp(app, analysis)
}

func TopologyForAppInManifest(m manifest.Manifest, app manifest.App) Topology {
	analysis := AnalyzeAppInManifest(m, app)
	return topologyForApp(app, analysis)
}

func topologyForApp(app manifest.App, analysis AppAnalysis) Topology {
	return Topology{
		Networks:             analysis.Networks,
		InternalDependencies: analysis.InternalDependencies,
		ExternalRequirements: analysis.ExternalRequirements,
		DataStores:           analysis.DataStores,
		LinkedResources:      analysis.LinkedResources,
		StatefulVolumes:      analysis.StatefulVolumes,
		Routes:               appRoutes(app.Routes),
		SourceServices:       sourceServices(app),
		SourceControl:        sourceControl(app),
		RiskReasons:          analysis.RiskReasons,
	}
}

func sourceServices(app manifest.App) []SourceService {
	services := make([]SourceService, 0, len(app.Services))
	for _, service := range app.Services {
		if service.ID == "" && service.Name == "" {
			continue
		}
		services = append(services, SourceService{
			ServiceName:   serviceName(service),
			ContainerID:   service.ID,
			ContainerName: service.Name,
			Image:         service.Image,
		})
	}
	return services
}

func sourceControl(app manifest.App) *SourceControl {
	if app.Git == nil {
		return nil
	}
	control := &SourceControl{
		Repository:   strings.TrimSpace(app.Git.Repository),
		Branch:       strings.TrimSpace(app.Git.Branch),
		CommitSHA:    strings.TrimSpace(app.Git.CommitSHA),
		Provider:     strings.TrimSpace(app.Git.Provider),
		SourceType:   strings.TrimSpace(app.Git.SourceType),
		SourceID:     strings.TrimSpace(app.Git.SourceID),
		PrivateKeyID: strings.TrimSpace(app.Git.PrivateKeyID),
	}
	if control.Provider == "" {
		control.Provider = providerFromGit(control.Repository, control.SourceType)
	}
	control.Auth = sourceControlAuth(control)
	control.Evidence = sourceControlEvidence(control)
	if control.Repository == "" && control.Branch == "" && control.CommitSHA == "" && control.SourceType == "" && control.SourceID == "" && control.PrivateKeyID == "" {
		return nil
	}
	return control
}

func sourceControlAuth(control *SourceControl) string {
	sourceType := strings.ToLower(control.SourceType)
	switch {
	case control.SourceID != "" && strings.Contains(sourceType, "github"):
		return "coolify_github_app"
	case control.SourceID != "":
		return "coolify_source_connection"
	case control.PrivateKeyID != "":
		return "coolify_deploy_key"
	case strings.HasPrefix(control.Repository, "git@") || strings.HasPrefix(control.Repository, "ssh://"):
		return "ssh"
	case strings.HasPrefix(control.Repository, "http://") || strings.HasPrefix(control.Repository, "https://"):
		return "https"
	default:
		return "git"
	}
}

func sourceControlEvidence(control *SourceControl) []string {
	items := []string{}
	if control.Repository != "" {
		items = append(items, "repository="+control.Repository)
	}
	if control.Branch != "" {
		items = append(items, "branch="+control.Branch)
	}
	if control.Provider != "" {
		items = append(items, "provider="+control.Provider)
	}
	if control.Auth != "" {
		items = append(items, "auth="+control.Auth)
	}
	return planutil.UniqueStrings(items)
}

func providerFromGit(repository, sourceType string) string {
	combined := strings.ToLower(strings.TrimSpace(repository + " " + sourceType))
	switch {
	case strings.Contains(combined, "github"):
		return "github"
	case strings.Contains(combined, "gitlab"):
		return "gitlab"
	case strings.Contains(combined, "bitbucket"):
		return "bitbucket"
	case strings.Contains(combined, "gitea"):
		return "gitea"
	case strings.HasPrefix(strings.TrimSpace(repository), "git@") || strings.HasPrefix(strings.TrimSpace(repository), "ssh://"):
		return "ssh"
	default:
		return "git"
	}
}

func internalDependencies(app manifest.App) []Dependency {
	dependencies := []Dependency{}
	seen := map[string]struct{}{}
	for _, service := range app.Services {
		kind := serviceKind(service)
		if kind == "" {
			continue
		}

		name := serviceName(service)
		key := kind + "\x00" + name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		dependencies = append(dependencies, Dependency{
			Kind:    kind,
			Service: name,
			Volumes: serviceVolumes(service),
		})
	}
	sort.Slice(dependencies, func(i, j int) bool {
		if dependencies[i].Kind == dependencies[j].Kind {
			return dependencies[i].Service < dependencies[j].Service
		}
		return dependencies[i].Kind < dependencies[j].Kind
	})
	return dependencies
}

func externalRequirements(app manifest.App, dependencies []Dependency) []Requirement {
	internalKinds := map[string]struct{}{}
	for _, dependency := range dependencies {
		internalKinds[dependency.Kind] = struct{}{}
	}

	evidence := map[string]map[string]struct{}{}
	for _, env := range app.Environment {
		collectRequirementEvidence(evidence, env.Name)
	}
	for _, service := range app.Services {
		for _, env := range service.Environment {
			collectRequirementEvidence(evidence, env.Name)
		}
	}

	requirements := []Requirement{}
	for kind, values := range evidence {
		if _, ok := internalKinds[kind]; ok {
			continue
		}
		items := make([]string, 0, len(values))
		for value := range values {
			items = append(items, value)
		}
		sort.Strings(items)
		requirements = append(requirements, Requirement{Kind: kind, Evidence: items})
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Kind < requirements[j].Kind })
	return requirements
}

func linkedResources(m manifest.Manifest, app manifest.App, analysis AppAnalysis) []ResourceLink {
	if len(analysis.ExternalRequirements) == 0 {
		return nil
	}

	links := []ResourceLink{}
	for _, other := range m.Apps {
		if sameApp(app, other) || !linkableResourceApp(other) {
			continue
		}

		otherAnalysis := analyzeApp(other)
		if len(otherAnalysis.DataStores) == 0 {
			continue
		}

		for _, requirement := range analysis.ExternalRequirements {
			stores := matchingDataStores(requirement.Kind, otherAnalysis.DataStores)
			if len(stores) == 0 {
				continue
			}

			reasons := resourceLinkReasons(app, other, requirement, analysis.Networks, otherAnalysis.Networks)
			if len(reasons) == 0 {
				continue
			}

			links = append(links, ResourceLink{
				Kind:       requirement.Kind,
				App:        other.Name,
				AppID:      other.ID,
				Role:       migrationRole(other),
				Runtime:    other.Runtime,
				Confidence: resourceLinkConfidence(reasons),
				Reasons:    reasons,
				Networks:   sharedPortableNetworks(analysis.Networks, otherAnalysis.Networks),
				DataStores: stores,
			})
		}
	}

	sort.Slice(links, func(i, j int) bool {
		if links[i].Kind == links[j].Kind {
			return links[i].App < links[j].App
		}
		return links[i].Kind < links[j].Kind
	})
	return links
}

func matchingDataStores(kind string, stores []DataStore) []DataStore {
	matched := []DataStore{}
	for _, store := range stores {
		if requirementMatchesStore(kind, store.Kind) {
			matched = append(matched, store)
		}
	}
	return matched
}

func IsLinkableRequirement(kind string) bool {
	switch kind {
	case "database", "redis", "object-storage", "vector-db", "search":
		return true
	default:
		return false
	}
}

func requirementMatchesStore(requirementKind, storeKind string) bool {
	switch requirementKind {
	case "database":
		return storeKind == "postgres" || storeKind == "mysql" || storeKind == "mongo" || storeKind == "sqlite"
	case "redis", "object-storage", "vector-db", "search":
		return storeKind == requirementKind
	default:
		return false
	}
}

func resourceLinkReasons(app, other manifest.App, requirement Requirement, appNetworks, otherNetworks []string) []string {
	reasons := []string{}
	if len(matchingEnvReferences(app, other, requirement.Kind)) > 0 {
		reasons = append(reasons, "env value points at this resource")
	}
	if len(sharedPortableNetworks(appNetworks, otherNetworks)) > 0 {
		reasons = append(reasons, "shared Docker network")
	}
	if sameProject(app, other) {
		reasons = append(reasons, "same Coolify project")
	}
	if len(reasons) == 0 {
		return nil
	}
	if migrationRole(other) == "support" || other.Runtime == "database" || other.Runtime == "service" {
		reasons = append(reasons, "resource is classified as "+planutil.Fallback(migrationRole(other), other.Runtime))
	}
	if len(requirement.Evidence) > 0 {
		reasons = append(reasons, "env evidence: "+strings.Join(requirement.Evidence, ", "))
	}
	return planutil.UniqueStrings(reasons)
}

func linkableResourceApp(app manifest.App) bool {
	role := migrationRole(app)
	return role == "support" || app.Runtime == "database" || app.Runtime == "service"
}

func resourceLinkConfidence(reasons []string) string {
	for _, reason := range reasons {
		if reason == "env value points at this resource" {
			return "detected"
		}
	}
	for _, reason := range reasons {
		if reason == "shared Docker network" {
			return "likely"
		}
	}
	return "possible"
}

func matchingEnvReferences(app, other manifest.App, kind string) []string {
	refs := envReferenceTokens(app, kind)
	if len(refs) == 0 {
		return nil
	}
	targets := appReferenceTokens(other)
	matches := []string{}
	for ref := range refs {
		if _, ok := targets[ref]; ok {
			matches = append(matches, ref)
			continue
		}
		for target := range targets {
			if strongTokenContains(ref, target) || strongTokenContains(target, ref) {
				matches = append(matches, ref)
				break
			}
		}
	}
	return planutil.UniqueStrings(matches)
}

func envReferenceTokens(app manifest.App, kind string) map[string]struct{} {
	tokens := map[string]struct{}{}
	for _, env := range appEnvironment(app) {
		if !env.ValueKnown || strings.TrimSpace(env.Value) == "" || !envNameMatchesRequirement(env.Name, kind) {
			continue
		}
		for _, token := range valueReferenceTokens(env.Value) {
			tokens[token] = struct{}{}
		}
	}
	return tokens
}

func appReferenceTokens(app manifest.App) map[string]struct{} {
	tokens := map[string]struct{}{}
	addReferenceToken(tokens, app.Name)
	addReferenceToken(tokens, strings.TrimPrefix(app.ID, "compose:"))
	for _, key := range []string{"coolify.uuid", "coolify.resourceName", "coolify.serviceName", "coolify.composeProject"} {
		addReferenceToken(tokens, app.Metadata[key])
	}
	for _, route := range app.Routes {
		addReferenceToken(tokens, route.Host)
		if route.Port != "" {
			addReferenceToken(tokens, route.Host+":"+route.Port)
		}
	}
	for _, service := range app.Services {
		addReferenceToken(tokens, service.Name)
		for _, key := range []string{"com.docker.compose.project", "com.docker.compose.service", "coolify.name", "coolify.resourceName", "coolify.serviceName"} {
			addReferenceToken(tokens, service.Labels[key])
		}
		for _, port := range service.Ports {
			if port.HostPort != "" {
				addReferenceToken(tokens, port.HostPort)
				addReferenceToken(tokens, net.JoinHostPort("127.0.0.1", port.HostPort))
			}
		}
		for _, env := range service.Environment {
			if !env.ValueKnown {
				continue
			}
			switch env.Name {
			case "COOLIFY_FQDN", "COOLIFY_URL", "MINIO_SERVER_URL", "MINIO_BROWSER_REDIRECT_URL":
				for _, token := range valueReferenceTokens(env.Value) {
					tokens[token] = struct{}{}
				}
			}
		}
	}
	return tokens
}

func appEnvironment(app manifest.App) []manifest.EnvVar {
	envs := append([]manifest.EnvVar{}, app.Environment...)
	for _, service := range app.Services {
		envs = append(envs, service.Environment...)
	}
	return envs
}

func envNameMatchesRequirement(name, kind string) bool {
	for _, candidate := range requirementKinds(strings.ToUpper(strings.TrimSpace(name))) {
		if candidate == kind {
			return true
		}
	}
	return false
}

func valueReferenceTokens(value string) []string {
	tokens := map[string]struct{}{}
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' }) {
		part = strings.Trim(strings.TrimSpace(part), "'\"")
		if part == "" || strings.HasPrefix(part, "[REDACTED:") {
			continue
		}
		addReferenceToken(tokens, part)
		if parsed, err := url.Parse(part); err == nil {
			addReferenceToken(tokens, parsed.Host)
			addReferenceToken(tokens, parsed.Hostname())
			if parsed.Port() != "" {
				addReferenceToken(tokens, net.JoinHostPort(parsed.Hostname(), parsed.Port()))
				addReferenceToken(tokens, parsed.Port())
			}
		}
		if host, port, err := net.SplitHostPort(part); err == nil {
			addReferenceToken(tokens, host)
			addReferenceToken(tokens, net.JoinHostPort(host, port))
			addReferenceToken(tokens, port)
		}
	}
	return mapKeys(tokens)
}

func addReferenceToken(tokens map[string]struct{}, value string) {
	value = normalizeReferenceToken(value)
	if value == "" {
		return
	}
	tokens[value] = struct{}{}
}

func normalizeReferenceToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Trim(value, "'\"`[]()")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimSuffix(value, "/")
	if host, port, err := net.SplitHostPort(value); err == nil && host != "" && port != "" {
		return net.JoinHostPort(host, port)
	}
	return value
}

func strongTokenContains(value, token string) bool {
	return len(token) >= 8 && strings.Contains(value, token)
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sharedPortableNetworks(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, network := range a {
		if !isCommonNetwork(network) {
			seen[network] = struct{}{}
		}
	}
	shared := []string{}
	for _, network := range b {
		if _, ok := seen[network]; ok {
			shared = append(shared, network)
		}
	}
	return planutil.UniqueStrings(shared)
}

func isCommonNetwork(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "", "bridge", "host", "none", "coolify", "coolify-proxy", "proxy":
		return true
	default:
		return false
	}
}

func dataStores(app manifest.App) []DataStore {
	if migrationRole(app) == "platform" {
		return nil
	}

	stores := []DataStore{}
	for _, service := range app.Services {
		if store, ok := classifiedDataStore(service); ok {
			stores = append(stores, store)
			continue
		}
		if store, ok := sqliteDataStore(service); ok {
			stores = append(stores, store)
			continue
		}
		if looksLikeDatabaseResource(service) {
			stores = append(stores, unknownDataStore(service))
		}
	}
	sort.Slice(stores, func(i, j int) bool {
		if stores[i].Kind == stores[j].Kind {
			return stores[i].Service < stores[j].Service
		}
		return stores[i].Kind < stores[j].Kind
	})
	return stores
}

func classifiedDataStore(service manifest.Service) (DataStore, bool) {
	if knownApplicationImage(service.Image) {
		return DataStore{}, false
	}

	name := strings.ToLower(serviceName(service))
	image := normalizedImageName(service.Image)
	combined := name + " " + image
	switch {
	case containsAny(combined, "postgres", "postgresql", "pgvector", "postgis", "timescaledb"):
		engine := "postgres"
		switch {
		case strings.Contains(combined, "pgvector"):
			engine = "pgvector"
		case strings.Contains(combined, "postgis"):
			engine = "postgis"
		case strings.Contains(combined, "timescaledb"):
			engine = "timescaledb"
		}
		return newDataStore(service, "postgres", engine, "pg_dump_restore_or_logical_replication", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "mysql") || strings.Contains(combined, "mariadb"):
		return newDataStore(service, "mysql", databaseEngine(combined, "mysql"), "mysqldump_restore", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "mongo"):
		return newDataStore(service, "mongo", "mongo", "mongodump_restore", "stopped_volume_copy", "critical"), true
	case containsAny(combined, "redis", "keydb", "dragonfly"):
		engine := "redis"
		switch {
		case strings.Contains(combined, "dragonfly"):
			engine = "dragonfly"
		case strings.Contains(combined, "keydb"):
			engine = "keydb"
		}
		return newDataStore(service, "redis", engine, "snapshot_aof_or_volume_copy", "recreate_if_cache_only", "unknown"), true
	case strings.Contains(combined, "memcached"):
		return newDataStore(service, "cache", "memcached", "recreate_if_cache_only", "stopped_volume_copy", "cache"), true
	case containsAny(combined, "minio", "garage"):
		return newDataStore(service, "object-storage", databaseEngine(combined, "minio"), "mc_mirror", "volume_sync", "critical_if_uploads_or_files"), true
	case strings.Contains(combined, "qdrant") || strings.Contains(combined, "weaviate"):
		return newDataStore(service, "vector-db", databaseEngine(combined, "vector-db"), "snapshot_or_collection_export", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "clickhouse"):
		return newDataStore(service, "clickhouse", "clickhouse", "native_backup_or_export", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "couchdb"):
		return newDataStore(service, "couchdb", "couchdb", "native_backup_or_export", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "influxdb"):
		return newDataStore(service, "influxdb", "influxdb", "native_backup_or_export", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "neo4j"):
		return newDataStore(service, "graph-db", "neo4j", "native_backup_or_export", "stopped_volume_copy", "critical"), true
	case strings.Contains(combined, "searxng"):
		return newDataStore(service, "search", "searxng", "config_and_volume_copy", "stopped_volume_copy", "unknown"), true
	}
	return DataStore{}, false
}

func sqliteDataStore(service manifest.Service) (DataStore, bool) {
	for _, env := range service.Environment {
		if strings.Contains(strings.ToUpper(env.Name), "SQLITE") {
			return newDataStore(service, "sqlite", "sqlite", "stopped_file_copy", "filesystem_snapshot_or_volume_copy", "critical"), true
		}
	}
	for _, mount := range service.Mounts {
		target := strings.ToLower(mount.Target)
		if strings.Contains(target, "sqlite") || strings.HasSuffix(target, ".sqlite") || strings.HasSuffix(target, ".sqlite3") || strings.HasSuffix(target, ".db") {
			return newDataStore(service, "sqlite", "sqlite", "stopped_file_copy", "filesystem_snapshot_or_volume_copy", "critical"), true
		}
	}
	return DataStore{}, false
}

func unknownDataStore(service manifest.Service) DataStore {
	return newDataStore(service, "unknown", "", "manual_review", "stopped_volume_copy", "unknown")
}

func knownApplicationImage(image string) bool {
	image = normalizedImageName(image)
	for _, pattern := range []string{
		"supertokens/supertokens-mysql",
		"supertokens/supertokens-postgresql",
		"metabase/metabase",
		"superset",
		"nocodb/nocodb",
		"umami-software/umami",
		"infisical/infisical",
		"postgrest/postgrest",
		"supabase/postgres-meta",
		"bluewaveuptime/uptime_redis",
	} {
		if strings.Contains(image, pattern) {
			return true
		}
	}
	return false
}

func looksLikeDatabaseResource(service manifest.Service) bool {
	if len(serviceStatefulVolumes(service)) == 0 {
		return false
	}
	for _, key := range []string{"coolify.database.subType", "coolify.service.subType"} {
		if strings.Contains(strings.ToLower(service.Labels[key]), "database") {
			return true
		}
	}
	if strings.EqualFold(service.Labels["coolify.type"], "database") {
		return true
	}
	name := strings.ToLower(serviceName(service))
	if name == "db" || strings.HasSuffix(name, "-db") || strings.HasSuffix(name, "_db") || strings.Contains(name, "database") {
		return true
	}
	for _, mount := range service.Mounts {
		if databaseMountPath(mount.Target) {
			return true
		}
	}
	return false
}

func databaseMountPath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	for _, marker := range []string{
		"/var/lib/postgresql",
		"/var/lib/mysql",
		"/var/lib/mariadb",
		"/data/db",
		"/bitnami/postgresql",
		"/bitnami/mysql",
		"/bitnami/mariadb",
		"/bitnami/mongodb",
		"/var/lib/clickhouse",
		"/var/lib/redis",
		"/data/redis",
		"/var/lib/neo4j",
		"/var/lib/influxdb",
		"/opt/couchdb/data",
	} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return false
}

func newDataStore(service manifest.Service, kind, engine, strategy, fallback, criticality string) DataStore {
	return DataStore{
		Kind:                kind,
		Engine:              engine,
		Service:             serviceName(service),
		Image:               service.Image,
		Volumes:             serviceVolumes(service),
		Strategy:            strategy,
		Fallback:            fallback,
		Criticality:         criticality,
		SourceContainerID:   service.ID,
		SourceContainerName: service.Name,
	}
}

func databaseEngine(combined, fallback string) string {
	switch {
	case strings.Contains(combined, "mariadb"):
		return "mariadb"
	case strings.Contains(combined, "garage"):
		return "garage"
	case strings.Contains(combined, "mysql"):
		return "mysql"
	case strings.Contains(combined, "qdrant"):
		return "qdrant"
	case strings.Contains(combined, "weaviate"):
		return "weaviate"
	}
	return fallback
}

func statefulVolumes(app manifest.App) []StatefulVolume {
	volumes := []StatefulVolume{}
	for _, storage := range app.Storages {
		storageType := storage.Type
		if storageType == "" {
			storageType = "storage"
		}
		volumes = append(volumes, StatefulVolume{
			Origin:  "storage",
			Service: "app",
			Type:    storageType,
			Name:    storage.Name,
			Source:  storage.Source,
			Target:  storage.Target,
			RW:      true,
		})
	}
	for _, service := range app.Services {
		volumes = append(volumes, serviceStatefulVolumes(service)...)
	}
	sort.Slice(volumes, func(i, j int) bool {
		if volumes[i].Service == volumes[j].Service {
			return volumes[i].Target < volumes[j].Target
		}
		return volumes[i].Service < volumes[j].Service
	})
	return volumes
}

func serviceStatefulVolumes(service manifest.Service) []StatefulVolume {
	volumes := []StatefulVolume{}
	for _, mount := range service.Mounts {
		if mount.Type != "volume" && mount.Type != "bind" {
			continue
		}
		volumes = append(volumes, StatefulVolume{
			Origin:              "mount",
			Service:             serviceName(service),
			Type:                mount.Type,
			Name:                mount.Name,
			Source:              mount.Source,
			Target:              mount.Target,
			RW:                  mount.RW,
			SourceContainerID:   service.ID,
			SourceContainerName: service.Name,
		})
	}
	return volumes
}

func riskReasons(app manifest.App, analysis AppAnalysis) []RiskReason {
	reasons := []RiskReason{}
	switch DeployReadiness(app) {
	case DeployMissing:
		reasons = append(reasons, RiskReason{Severity: RiskError, Code: "deploy.missing_artifact", Message: "missing image or raw compose; server-local scan is required before migration"})
	case DeploySourceOnly:
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: "deploy.source_only", Message: "source build metadata only; run server-local scan or repository export before migration"})
	case DeployResolvedOnly:
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: "deploy.resolved_only", Message: "resolved compose only; raw compose or server-local scan is required before migration"})
	}

	if len(app.Routes) == 0 && migrationRole(app) != "support" && migrationRole(app) != "platform" {
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: "routes.none", Message: "no public routes were detected; verify routing or confirm this resource is internal-only"})
	}

	if control := sourceControl(app); control != nil {
		message := fmt.Sprintf("git source %s detected; Bort deploys a raw compose/image snapshot and does not copy Coolify source credentials", planutil.Fallback(control.Repository, control.Provider))
		reasons = append(reasons, RiskReason{Severity: RiskInfo, Code: "source_control.connect_dokploy", Message: message})
	}

	for _, requirement := range analysis.ExternalRequirements {
		reasons = append(reasons, RiskReason{Severity: RiskInfo, Code: "external." + requirement.Kind, Message: fmt.Sprintf("%s settings inferred from env names: %s", requirement.Kind, strings.Join(requirement.Evidence, ", "))})
	}
	for _, link := range analysis.LinkedResources {
		reasons = append(reasons, RiskReason{Severity: RiskInfo, Code: "linked_resource." + link.Kind, Message: fmt.Sprintf("%s settings may use Coolify app %s", link.Kind, link.App)})
	}

	for _, store := range analysis.DataStores {
		code := "data_store." + store.Kind
		message := fmt.Sprintf("%s service %s needs %s before cutover; criticality=%s", store.Kind, store.Service, store.Strategy, store.Criticality)
		if store.Kind == "unknown" {
			code = "data_store.manual_review"
			message = fmt.Sprintf("service %s has stateful mounts but no known data-store engine; inspect volume contents before migration", store.Service)
		}
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: code, Message: message})
	}

	var namedVolumeMounts int
	var bindMounts int
	for _, volume := range analysis.StatefulVolumes {
		switch volume.Type {
		case "volume":
			namedVolumeMounts++
		case "bind":
			bindMounts++
		}
	}
	if namedVolumeMounts > 0 {
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: "state.named_volumes", Message: fmt.Sprintf("%d named volume mount(s) require target volume creation and data sync", namedVolumeMounts)})
	}
	if bindMounts > 0 {
		reasons = append(reasons, RiskReason{Severity: RiskInfo, Code: "state.bind_mounts", Message: fmt.Sprintf("%d VPS file/folder mount(s) will be preserved on this VPS", bindMounts)})
	}
	if count := redactedEnvCount(app); count > 0 {
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: "env.values_redacted", Message: fmt.Sprintf("%d env value(s) not captured by scan; run `bort` (default flow captures values) or fill via `bort env <app> KEY=value`", count)})
	}
	if names := coolifyServiceMagicEnvNames(app); len(names) > 0 {
		reasons = append(reasons, RiskReason{Severity: RiskInfo, Code: "env.coolify_service_magic", Message: fmt.Sprintf("Coolify service magic env vars detected; review preserved values in Dokploy: %s", strings.Join(names, ", "))})
	}
	for _, warning := range app.Warnings {
		reasons = append(reasons, RiskReason{Severity: RiskWarn, Code: warning.Code, Message: warning.Message})
	}
	return reasons
}

func collectRequirementEvidence(evidence map[string]map[string]struct{}, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	upper := strings.ToUpper(name)
	for _, kind := range requirementKinds(upper) {
		if evidence[kind] == nil {
			evidence[kind] = map[string]struct{}{}
		}
		evidence[kind][upper] = struct{}{}
	}
}

func requirementKinds(name string) []string {
	kinds := []string{}
	switch {
	case strings.Contains(name, "POSTGRES") || strings.Contains(name, "DATABASE_URL") || strings.HasPrefix(name, "DATABASE_") || strings.HasPrefix(name, "DB_") || strings.HasSuffix(name, "_DB"):
		kinds = append(kinds, "database")
	case strings.Contains(name, "MYSQL") || strings.Contains(name, "MARIADB"):
		kinds = append(kinds, "database")
	case strings.Contains(name, "MONGO"):
		kinds = append(kinds, "database")
	}
	if strings.Contains(name, "REDIS") || strings.Contains(name, "CACHE_URL") || strings.Contains(name, "QUEUE_URL") {
		kinds = append(kinds, "redis")
	}
	if strings.Contains(name, "MINIO") || strings.HasPrefix(name, "S3_") || strings.Contains(name, "S3_BUCKET") || strings.Contains(name, "AWS_ACCESS_KEY") || strings.Contains(name, "AWS_SECRET") {
		kinds = append(kinds, "object-storage")
	}
	if strings.HasPrefix(name, "SMTP_") || strings.HasPrefix(name, "EMAIL_HOST") {
		kinds = append(kinds, "email")
	}
	return planutil.UniqueStrings(kinds)
}

func serviceKind(service manifest.Service) string {
	store, ok := classifiedDataStore(service)
	if !ok {
		return ""
	}
	switch store.Kind {
	case "postgres", "mysql", "mongo", "clickhouse", "couchdb", "influxdb", "graph-db":
		return "database"
	case "redis":
		return "redis"
	case "object-storage", "vector-db", "search":
		return store.Kind
	case "cache":
		return "cache"
	default:
		return ""
	}
}

func normalizedImageName(image string) string {
	image = strings.ToLower(strings.TrimSpace(image))
	if before, _, ok := strings.Cut(image, "@"); ok {
		image = before
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		image = image[:lastColon]
	}
	image = strings.TrimPrefix(image, "docker.io/")
	image = strings.TrimPrefix(image, "library/")
	return image
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func DeployReadiness(app manifest.App) DeployStatus {
	if HasRawCompose(app) || HasServiceImage(app) {
		return DeployReady
	}
	if HasSourceBuildMetadata(app) {
		return DeploySourceOnly
	}
	if HasResolvedCompose(app) {
		return DeployResolvedOnly
	}
	return DeployMissing
}

func HasRawCompose(app manifest.App) bool {
	return app.Compose != nil && strings.TrimSpace(app.Compose.Raw) != ""
}

func HasResolvedCompose(app manifest.App) bool {
	return app.Compose != nil && strings.TrimSpace(app.Compose.Resolved) != ""
}

func HasServiceImage(app manifest.App) bool {
	for _, service := range app.Services {
		if strings.TrimSpace(service.Image) != "" {
			return true
		}
	}
	return false
}

func HasSourceBuildMetadata(app manifest.App) bool {
	if app.Git == nil || strings.TrimSpace(app.Git.Repository) == "" || strings.TrimSpace(app.BuildPack) == "" {
		return false
	}
	if app.BuildPack == "dockercompose" {
		return strings.TrimSpace(app.Git.ComposeLocation) != ""
	}
	return true
}

func serviceName(service manifest.Service) string {
	if name := service.Labels["com.docker.compose.service"]; name != "" {
		return name
	}
	return service.Name
}

func serviceVolumes(service manifest.Service) []string {
	volumes := []string{}
	for _, mount := range service.Mounts {
		source := mount.Name
		if source == "" {
			source = mount.Source
		}
		if source == "" {
			continue
		}
		volumes = append(volumes, source+" -> "+mount.Target)
	}
	return planutil.UniqueStrings(volumes)
}

func appNetworks(app manifest.App) []string {
	networks := []string{}
	for _, service := range app.Services {
		for _, network := range service.Networks {
			if network.Name != "" {
				networks = append(networks, network.Name)
			}
		}
	}
	return planutil.UniqueStrings(networks)
}

func appRoutes(routes []manifest.Route) []manifest.Route {
	sorted := append([]manifest.Route{}, routes...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Host == sorted[j].Host {
			if sorted[i].ServiceName == sorted[j].ServiceName {
				return sorted[i].Port < sorted[j].Port
			}
			return sorted[i].ServiceName < sorted[j].ServiceName
		}
		return sorted[i].Host < sorted[j].Host
	})
	return sorted
}

func HasRedactedEnvironment(app manifest.App) bool {
	return redactedEnvCount(app) > 0
}

func CoolifyServiceMagicEnvNames(app manifest.App) []string {
	return coolifyServiceMagicEnvNames(app)
}

func coolifyServiceMagicEnvNames(app manifest.App) []string {
	names := []string{}
	for _, env := range appEnvironment(app) {
		name := strings.ToUpper(strings.TrimSpace(env.Name))
		if isCoolifyServiceMagicEnvName(name) {
			names = append(names, name)
		}
	}
	return planutil.UniqueStrings(names)
}

func isCoolifyServiceMagicEnvName(name string) bool {
	return strings.HasPrefix(name, "SERVICE_FQDN_") ||
		strings.HasPrefix(name, "SERVICE_URL_") ||
		strings.HasPrefix(name, "SERVICE_NAME_")
}

func redactedEnvCount(app manifest.App) int {
	count := 0
	for _, env := range app.Environment {
		if !env.ValueKnown {
			count++
		}
	}
	for _, service := range app.Services {
		for _, env := range service.Environment {
			if !env.ValueKnown {
				count++
			}
		}
	}
	return count
}

func migrationRole(app manifest.App) string {
	if app.Metadata == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(app.Metadata["migrationRole"]))
}

func sameApp(a, b manifest.App) bool {
	if a.ID != "" && b.ID != "" {
		return a.ID == b.ID
	}
	return a.Name == b.Name
}

func sameProject(a, b manifest.App) bool {
	return appProject(a) != "" && appProject(a) == appProject(b)
}

func appProject(app manifest.App) string {
	if app.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(app.Metadata["coolify.project"])
}
