package preparer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/planutil"
)

type Status string

const (
	APIVersion = "bort.prepare/v1alpha1"

	StatusGreen  Status = "green"
	StatusYellow Status = "yellow"
	StatusRed    Status = "red"
)

type Readiness string

const (
	ReadinessReadyToCreate Readiness = "ready_to_create"
	ReadinessNeedsInput    Readiness = "needs_input"
	ReadinessNeedsDecision Readiness = "needs_decision"
	ReadinessBlocked       Readiness = "blocked"
)

type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Gate code identifiers emitted by the planner. Consumers (CLI views,
// state merging) should reference these constants instead of literal
// strings so renames stay compile-checked.
const (
	GateAppComposeMissing              = "app.compose_missing"
	GateAppComposeIncomplete           = "app.compose_incomplete"
	GateDeployMissingArtifact          = "deploy.missing_artifact"
	GateEnvValuesRequired              = "env.values_required"
	GateEnvValuesRedacted              = "env.values_redacted"
	GateDomainHostMissing              = "domain.host_missing"
	GateDomainServiceMissing           = "domain.service_missing"
	GateRoutesNone                     = "routes.none"
	GateDataStoreManualReview          = "data_store.manual_review"
	GateDataStorePrepareRequired       = "data_store.prepare_required"
	GateVolumeBindMountReview          = "volume.bind_mount_review"
	GateExternalRequirementResolve     = "external_requirement.resolve"
	GateLinkedResourceMissingCandidate = "linked_resource.missing_candidate"
	GateLinkedResourceConfirmCandidate = "linked_resource.confirm_candidate"
	GateLinkedResourceSelectCandidate  = "linked_resource.select_candidate"
)

// Code prefixes used by consumers to bucket gates by category.
const (
	GateCodePrefixEnv                 = "env."
	GateCodePrefixDataStore           = "data_store."
	GateCodePrefixDomain              = "domain."
	GateCodePrefixLinkedResource      = "linked_resource."
	GateCodePrefixExternalRequirement = "external_requirement."
)

type Options struct {
	BundleDir string
	AppName   string
	Target    string
}

type Result struct {
	APIVersion string    `json:"apiVersion"`
	BundleDir  string    `json:"bundleDir"`
	Target     string    `json:"target"`
	Status     Status    `json:"status"`
	Apps       []AppPlan `json:"apps"`
}

type AppPlan struct {
	Name            string           `json:"name"`
	Directory       string           `json:"directory"`
	Role            string           `json:"role,omitempty"`
	ProjectGroup    *ProjectGroup    `json:"projectGroup,omitempty"`
	Status          Status           `json:"status"`
	Readiness       Readiness        `json:"readiness"`
	Resources       ResourceSpecs    `json:"resources"`
	TargetResources *TargetResources `json:"targetResources,omitempty"`
	Gates           []Gate           `json:"gates,omitempty"`
	Actions         []Action         `json:"actions"`
}

type ProjectGroup struct {
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	Source      string `json:"source,omitempty"`
}

type Action struct {
	Severity Severity `json:"severity"`
	Kind     string   `json:"kind"`
	Message  string   `json:"message"`
}

type Gate struct {
	Readiness   Readiness `json:"readiness"`
	Severity    Severity  `json:"severity"`
	Code        string    `json:"code"`
	Message     string    `json:"message"`
	ResourceRef string    `json:"resourceRef,omitempty"`
	Evidence    []string  `json:"evidence,omitempty"`
}

type ResourceSpecs struct {
	App                  AppResource                   `json:"app"`
	SourceControl        *SourceControlResource        `json:"sourceControl,omitempty"`
	Domains              []DomainResource              `json:"domains,omitempty"`
	EnvFiles             []EnvFileResource             `json:"envFiles,omitempty"`
	Volumes              []VolumeResource              `json:"volumes,omitempty"`
	DataStores           []DataStoreResource           `json:"dataStores,omitempty"`
	ExternalRequirements []ExternalRequirementResource `json:"externalRequirements,omitempty"`
	LinkedResources      []LinkedResourceCandidate     `json:"linkedResources,omitempty"`
	SourceServices       []SourceServiceRef            `json:"sourceServices,omitempty"`
}

// SourceServiceRef carries the source-side container coordinates that
// apply needs to quiesce app workers/web services around a data move.
type SourceServiceRef struct {
	ServiceName   string `json:"serviceName"`
	ContainerID   string `json:"containerId,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
}

type AppResource struct {
	Type           string    `json:"type"`
	Name           string    `json:"name"`
	ComposePath    string    `json:"composePath"`
	ComposeMissing bool      `json:"composeMissing,omitempty"`
	MissingInputs  []string  `json:"missingInputs,omitempty"`
	Readiness      Readiness `json:"readiness"`
}

type SourceControlResource struct {
	Repository string    `json:"repository,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	CommitSHA  string    `json:"commitSha,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Auth       string    `json:"auth,omitempty"`
	Evidence   []string  `json:"evidence,omitempty"`
	Readiness  Readiness `json:"readiness"`
}

type DomainResource struct {
	Host        string    `json:"host"`
	ServiceName string    `json:"serviceName,omitempty"`
	Port        string    `json:"port,omitempty"`
	Source      string    `json:"source,omitempty"`
	Readiness   Readiness `json:"readiness"`
}

type EnvFileResource struct {
	Path          string   `json:"path"`
	Keys          []string `json:"keys,omitempty"`
	MissingValues []string `json:"missingValues,omitempty"`
}

type VolumeResource struct {
	Service             string    `json:"service,omitempty"`
	Type                string    `json:"type"`
	Name                string    `json:"name,omitempty"`
	Source              string    `json:"source,omitempty"`
	Target              string    `json:"target"`
	ReadWrite           bool      `json:"readWrite"`
	Portability         string    `json:"portability,omitempty"`
	Readiness           Readiness `json:"readiness"`
	SizeBytes           int64     `json:"sizeBytes,omitempty"`
	FileCount           int64     `json:"fileCount,omitempty"`
	SourceContainerID   string    `json:"sourceContainerId,omitempty"`
	SourceContainerName string    `json:"sourceContainerName,omitempty"`
}

type DataStoreResource struct {
	Kind                string    `json:"kind"`
	Engine              string    `json:"engine,omitempty"`
	Service             string    `json:"service"`
	Image               string    `json:"image,omitempty"`
	Volumes             []string  `json:"volumes,omitempty"`
	Strategy            string    `json:"strategy"`
	Fallback            string    `json:"fallback,omitempty"`
	Criticality         string    `json:"criticality"`
	Readiness           Readiness `json:"readiness"`
	SourceContainerID   string    `json:"sourceContainerId,omitempty"`
	SourceContainerName string    `json:"sourceContainerName,omitempty"`
}

type ExternalRequirementResource struct {
	Kind      string    `json:"kind"`
	Evidence  []string  `json:"evidence,omitempty"`
	Linkable  bool      `json:"linkable"`
	Readiness Readiness `json:"readiness"`
}

type LinkedResourceCandidate struct {
	Kind                 string              `json:"kind"`
	App                  string              `json:"app"`
	AppID                string              `json:"appId,omitempty"`
	Role                 string              `json:"role,omitempty"`
	Runtime              string              `json:"runtime,omitempty"`
	Confidence           string              `json:"confidence"`
	Reasons              []string            `json:"reasons,omitempty"`
	Networks             []string            `json:"networks,omitempty"`
	DataStores           []DataStoreResource `json:"dataStores,omitempty"`
	Source               string              `json:"source"`
	RequiresConfirmation bool                `json:"requiresConfirmation"`
	Readiness            Readiness           `json:"readiness"`
}

func Plan(opts Options) (Result, error) {
	if opts.BundleDir == "" {
		opts.BundleDir = "bort-bundle"
	}
	if opts.Target == "" {
		opts.Target = "dokploy"
	}

	index, err := readIndex(opts.BundleDir)
	if err != nil {
		return Result{}, err
	}

	result := Result{APIVersion: APIVersion, BundleDir: opts.BundleDir, Target: opts.Target, Status: StatusGreen}
	for _, app := range index.Apps {
		if opts.AppName != "" && app.Name != opts.AppName && app.Directory != opts.AppName && planutil.Slug(app.Name) != planutil.Slug(opts.AppName) {
			continue
		}

		appPlan, err := planApp(opts.BundleDir, opts.Target, app)
		if err != nil {
			return Result{}, err
		}
		result.Apps = append(result.Apps, appPlan)
		result.Status = WorseStatus(result.Status, appPlan.Status)
	}

	if len(result.Apps) == 0 {
		if opts.AppName != "" {
			return Result{}, fmt.Errorf("app %q not found in bundle", opts.AppName)
		}
		return Result{}, fmt.Errorf("bundle has no apps")
	}

	return result, nil
}

func planApp(bundleDir, target string, app exporter.AppSummary) (AppPlan, error) {
	appDir := filepath.Join(bundleDir, filepath.FromSlash(app.Directory))
	topology, err := readTopology(filepath.Join(appDir, "topology.json"))
	if err != nil {
		return AppPlan{}, fmt.Errorf("read topology for %s: %w", app.Name, err)
	}

	plan := AppPlan{Name: app.Name, Directory: app.Directory, Role: app.Role, ProjectGroup: projectGroup(app.ProjectGroup), Status: StatusGreen, Readiness: ReadinessReadyToCreate}
	plan.Resources = resourceSpecs(app, appDir, topology)
	addReadinessGates(&plan, topology)
	plan.add(SeverityInfo, "compose", fmt.Sprintf("would create %s compose app from compose.yaml", target))
	addSourceControlActions(&plan)
	addEnvironmentActions(&plan, topology)
	addRouteActions(&plan, target)
	addDataStoreActions(&plan)
	addLinkedResourceActions(&plan)
	addVolumeActions(&plan)
	plan.Readiness = ReadinessFromGates(plan.Gates)
	plan.Status = StatusFromReadiness(plan.Readiness)
	plan.TargetResources = targetResources(target, plan)
	return plan, nil
}

func targetResources(target string, plan AppPlan) *TargetResources {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "dokploy":
		return &TargetResources{Platform: "dokploy", DryRun: true, Dokploy: dokployResources(plan)}
	default:
		return nil
	}
}

func projectGroup(group *exporter.ProjectGroup) *ProjectGroup {
	if group == nil {
		return nil
	}
	return &ProjectGroup{Name: group.Name, Environment: group.Environment, Source: group.Source}
}

func resourceSpecs(app exporter.AppSummary, appDir string, topology analyzer.Topology) ResourceSpecs {
	resources := ResourceSpecs{
		App:      appResource(app.Name, appDir),
		EnvFiles: envFileResources(appDir, app.PrivateEnvValues),
	}
	resources.SourceControl = sourceControlResource(topology.SourceControl)

	for _, route := range topology.Routes {
		readiness := ReadinessReadyToCreate
		if strings.TrimSpace(route.Host) == "" {
			readiness = ReadinessNeedsInput
		} else if strings.TrimSpace(route.ServiceName) == "" {
			readiness = ReadinessNeedsDecision
		}
		resources.Domains = append(resources.Domains, DomainResource{
			Host:        route.Host,
			ServiceName: route.ServiceName,
			Port:        route.Port,
			Source:      route.Source,
			Readiness:   readiness,
		})
	}

	for _, volume := range topology.StatefulVolumes {
		resources.Volumes = append(resources.Volumes, volumeResource(volume))
	}
	for _, store := range topology.DataStores {
		resources.DataStores = append(resources.DataStores, dataStoreResource(store))
	}
	for _, requirement := range topology.ExternalRequirements {
		linkable := analyzer.IsLinkableRequirement(requirement.Kind)
		resources.ExternalRequirements = append(resources.ExternalRequirements, ExternalRequirementResource{
			Kind:      requirement.Kind,
			Evidence:  planutil.UniqueStrings(requirement.Evidence),
			Linkable:  linkable,
			Readiness: ReadinessReadyToCreate,
		})
	}
	for _, link := range topology.LinkedResources {
		resources.LinkedResources = append(resources.LinkedResources, linkedResourceCandidate(link))
	}
	for _, service := range topology.SourceServices {
		if service.ContainerID == "" && service.ContainerName == "" {
			continue
		}
		resources.SourceServices = append(resources.SourceServices, SourceServiceRef{
			ServiceName:   service.ServiceName,
			ContainerID:   service.ContainerID,
			ContainerName: service.ContainerName,
		})
	}

	return resources
}

func appResource(name, appDir string) AppResource {
	resource := AppResource{
		Type:        "compose",
		Name:        name,
		ComposePath: "compose.yaml",
		Readiness:   ReadinessReadyToCreate,
	}

	contents, err := os.ReadFile(filepath.Join(appDir, resource.ComposePath))
	if err != nil {
		resource.Readiness = ReadinessBlocked
		resource.ComposeMissing = true
		return resource
	}
	for _, placeholder := range []string{"TODO_REPLACE_IMAGE", "TODO_REPLACE_SOURCE"} {
		if strings.Contains(string(contents), placeholder) {
			resource.MissingInputs = append(resource.MissingInputs, placeholder)
		}
	}
	resource.MissingInputs = planutil.UniqueStrings(resource.MissingInputs)
	if len(resource.MissingInputs) > 0 {
		resource.Readiness = ReadinessBlocked
	}
	return resource
}

func sourceControlResource(source *analyzer.SourceControl) *SourceControlResource {
	if source == nil {
		return nil
	}
	return &SourceControlResource{
		Repository: source.Repository,
		Branch:     source.Branch,
		CommitSHA:  source.CommitSHA,
		Provider:   source.Provider,
		Auth:       source.Auth,
		Evidence:   planutil.UniqueStrings(source.Evidence),
		Readiness:  ReadinessReadyToCreate,
	}
}

func envFileResources(appDir string, privateEnvValues bool) []EnvFileResource {
	paths := envFiles(appDir, privateEnvValues)
	resources := make([]EnvFileResource, 0, len(paths))
	for _, path := range paths {
		resource := envFileResource(path)
		if len(resource.Keys) > 0 {
			resources = append(resources, resource)
		}
	}
	return resources
}

func envFileResource(path string) EnvFileResource {
	resource := EnvFileResource{Path: filepath.Base(path)}

	file, err := os.Open(path)
	if err != nil {
		return resource
	}
	defer file.Close()

	keys := []string{}
	missing := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		keys = append(keys, key)
		if !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, key)
		}
	}

	resource.Keys = planutil.UniqueStrings(keys)
	resource.MissingValues = planutil.UniqueStrings(missing)
	return resource
}

func volumeResource(volume analyzer.StatefulVolume) VolumeResource {
	readiness := ReadinessReadyToCreate
	portability := "portable"
	if volume.Type == "bind" {
		portability = "host_path_preserved"
	}
	return VolumeResource{
		Service:             volume.Service,
		Type:                volume.Type,
		Name:                volume.Name,
		Source:              volume.Source,
		Target:              volume.Target,
		ReadWrite:           volume.RW,
		Portability:         portability,
		Readiness:           readiness,
		SizeBytes:           volume.SizeBytes,
		FileCount:           volume.FileCount,
		SourceContainerID:   volume.SourceContainerID,
		SourceContainerName: volume.SourceContainerName,
	}
}

func dataStoreResource(store analyzer.DataStore) DataStoreResource {
	readiness := ReadinessReadyToCreate
	if store.Kind == "unknown" || store.Strategy == "manual_review" {
		readiness = ReadinessBlocked
	} else if strings.TrimSpace(store.Strategy) == "" {
		readiness = ReadinessNeedsDecision
	}
	return DataStoreResource{
		Kind:                store.Kind,
		Engine:              store.Engine,
		Service:             store.Service,
		Image:               store.Image,
		Volumes:             planutil.UniqueStrings(store.Volumes),
		Strategy:            store.Strategy,
		Fallback:            store.Fallback,
		Criticality:         store.Criticality,
		Readiness:           readiness,
		SourceContainerID:   store.SourceContainerID,
		SourceContainerName: store.SourceContainerName,
	}
}

func linkedResourceCandidate(link analyzer.ResourceLink) LinkedResourceCandidate {
	stores := make([]DataStoreResource, 0, len(link.DataStores))
	for _, store := range link.DataStores {
		stores = append(stores, dataStoreResource(store))
	}
	confidence := planutil.Fallback(link.Confidence, "unknown")
	return LinkedResourceCandidate{
		Kind:                 link.Kind,
		App:                  link.App,
		AppID:                link.AppID,
		Role:                 link.Role,
		Runtime:              link.Runtime,
		Confidence:           confidence,
		Reasons:              planutil.UniqueStrings(link.Reasons),
		Networks:             planutil.UniqueStrings(link.Networks),
		DataStores:           stores,
		Source:               "heuristic",
		RequiresConfirmation: false,
		Readiness:            ReadinessReadyToCreate,
	}
}

func addReadinessGates(plan *AppPlan, topology analyzer.Topology) {
	if plan.Resources.App.ComposeMissing {
		plan.addGate(ReadinessBlocked, SeverityError, GateAppComposeMissing, "compose.yaml is missing; target app shell cannot be prepared", "app", nil)
	}
	if len(plan.Resources.App.MissingInputs) > 0 {
		missing := plan.Resources.App.MissingInputs
		message := fmt.Sprintf("compose.yaml contains placeholders that must be replaced before target preparation: %s", strings.Join(missing, ", "))
		plan.addGate(ReadinessBlocked, SeverityError, GateAppComposeIncomplete, message, "app", missing)
	}
	if risk, ok := topologyRisk(topology, GateDeployMissingArtifact); ok {
		plan.Resources.App.Readiness = ReadinessBlocked
		plan.addGate(ReadinessBlocked, SeverityError, GateDeployMissingArtifact, risk.Message, "app", nil)
	}
	for _, code := range []string{"deploy.source_only", "deploy.resolved_only"} {
		if risk, ok := topologyRisk(topology, code); ok {
			plan.Resources.App.Readiness = WorseReadiness(plan.Resources.App.Readiness, ReadinessNeedsDecision)
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, code, risk.Message, "app", nil)
		}
	}

	for _, envFile := range plan.Resources.EnvFiles {
		if len(envFile.MissingValues) > 0 {
			message := fmt.Sprintf("fill %d value(s) in %s before deploy", len(envFile.MissingValues), envFile.Path)
			plan.addGate(ReadinessNeedsInput, SeverityWarn, GateEnvValuesRequired, message, "env:"+envFile.Path, envFile.MissingValues)
		}
	}
	if risk, ok := topologyRisk(topology, GateEnvValuesRedacted); ok {
		plan.addGate(ReadinessNeedsInput, SeverityWarn, GateEnvValuesRedacted, risk.Message, "env", nil)
	}

	for _, domain := range plan.Resources.Domains {
		resourceRef := "domain:" + planutil.Fallback(domain.Host, "missing-host")
		if strings.TrimSpace(domain.Host) == "" {
			plan.addGate(ReadinessNeedsInput, SeverityWarn, GateDomainHostMissing, "domain route has no host and must be filled before deploy", resourceRef, nil)
		} else if strings.TrimSpace(domain.ServiceName) == "" {
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, GateDomainServiceMissing, fmt.Sprintf("domain %s has no service mapping; confirm target service manually", domain.Host), resourceRef, nil)
		}
	}
	if len(plan.Resources.Domains) == 0 {
		if risk, ok := topologyRisk(topology, GateRoutesNone); ok {
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, GateRoutesNone, risk.Message, "domains", nil)
		}
	}

	for _, store := range plan.Resources.DataStores {
		resourceRef := "data-store:" + planutil.Fallback(store.Service, store.Kind)
		if store.Readiness == ReadinessBlocked {
			message := fmt.Sprintf("manual data-store review required for service %s before target preparation can be trusted", planutil.Fallback(store.Service, "unknown"))
			plan.addGate(ReadinessBlocked, SeverityError, GateDataStoreManualReview, message, resourceRef, store.Volumes)
			continue
		}
		if store.Readiness == ReadinessNeedsDecision {
			message := fmt.Sprintf("confirm %s data-store preparation strategy %s for service %s", dataStoreLabel(store), planutil.Fallback(store.Strategy, "manual_review"), planutil.Fallback(store.Service, "unknown"))
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, GateDataStorePrepareRequired, message, resourceRef, store.Volumes)
		}
	}
}

func addEnvironmentActions(plan *AppPlan, topology analyzer.Topology) {
	_, hasMagicEnv := topologyRisk(topology, "env.coolify_service_magic")
	if len(plan.Resources.EnvFiles) == 0 {
		if hasMagicEnv {
			plan.add(SeverityInfo, "environment", "review preserved Coolify SERVICE_URL/SERVICE_FQDN/SERVICE_NAME values after Dokploy routes are in place")
		}
		return
	}

	items := []string{}
	for _, envFile := range plan.Resources.EnvFiles {
		if count := len(envFile.Keys); count > 0 {
			items = append(items, fmt.Sprintf("%s (%d vars)", envFile.Path, count))
		}
	}
	if len(items) == 0 {
		if hasMagicEnv {
			plan.add(SeverityInfo, "environment", "review preserved Coolify SERVICE_URL/SERVICE_FQDN/SERVICE_NAME values after Dokploy routes are in place")
		}
		return
	}

	severity := SeverityInfo
	if plan.hasGate(GateEnvValuesRedacted) {
		severity = SeverityWarn
	}
	plan.add(severity, "environment", fmt.Sprintf("review and fill exported env examples before deploy: %s", strings.Join(items, ", ")))
	if hasMagicEnv {
		plan.add(SeverityInfo, "environment", "review preserved Coolify SERVICE_URL/SERVICE_FQDN/SERVICE_NAME values after Dokploy routes are in place")
	}
}

func addSourceControlActions(plan *AppPlan) {
	if plan.Resources.SourceControl == nil {
		return
	}
	control := plan.Resources.SourceControl
	repo := planutil.Fallback(control.Repository, "the source repository")
	message := fmt.Sprintf("will not copy Coolify source credentials for %s; connect a Dokploy source after cutover for future Git deploys", repo)
	if control.Auth == "https" || control.Auth == "git" {
		message = fmt.Sprintf("will deploy %s from raw compose/image snapshot; connect it in Dokploy later only if future Git deploys are needed", repo)
	}
	plan.add(SeverityInfo, "source-control", message)
}

func addRouteActions(plan *AppPlan, target string) {
	if len(plan.Resources.Domains) == 0 {
		if plan.hasGate(GateRoutesNone) {
			plan.add(SeverityWarn, "route", fmt.Sprintf("confirm this app is internal-only or add %s domains manually", target))
		}
		return
	}

	for _, domain := range plan.Resources.Domains {
		message := fmt.Sprintf("would create %s domain %s", target, planutil.Fallback(domain.Host, "<missing host>"))
		if domain.ServiceName != "" {
			message += " for service " + domain.ServiceName
		}
		if domain.Port != "" {
			message += " on port " + domain.Port
		}
		plan.add(SeverityInfo, "route", message)
	}
}

func addDataStoreActions(plan *AppPlan) {
	for _, store := range plan.Resources.DataStores {
		severity := SeverityInfo
		message := fmt.Sprintf("will prepare %s data store for service %s with %s", dataStoreLabel(store), planutil.Fallback(store.Service, "unknown"), planutil.Fallback(store.Strategy, "manual_review"))
		if store.Readiness != ReadinessReadyToCreate {
			severity = SeverityWarn
			message = fmt.Sprintf("needs %s data store preparation for service %s with %s", dataStoreLabel(store), planutil.Fallback(store.Service, "unknown"), planutil.Fallback(store.Strategy, "manual_review"))
		}
		if store.Fallback != "" {
			message += "; fallback " + store.Fallback
		}
		if store.Criticality != "" {
			message += "; criticality " + store.Criticality
		}
		plan.add(severity, "data-store", message)
	}
}

func addLinkedResourceActions(plan *AppPlan) {
	linksByKind := map[string][]LinkedResourceCandidate{}
	for _, link := range plan.Resources.LinkedResources {
		linksByKind[link.Kind] = append(linksByKind[link.Kind], link)
	}

	for _, requirement := range plan.Resources.ExternalRequirements {
		evidence := describeRequirementEvidence(requirement)
		if !requirement.Linkable {
			plan.add(SeverityInfo, "external-requirement", fmt.Sprintf("uses external %s settings from source%s", requirement.Kind, evidence))
			continue
		}

		links := linksByKind[requirement.Kind]
		switch {
		case len(links) == 0:
			plan.add(SeverityInfo, "linked-resource", fmt.Sprintf("will keep existing %s settings%s", supportResourceKindLabel(requirement.Kind), evidence))
		case len(links) == 1:
			plan.add(SeverityInfo, "linked-resource", fmt.Sprintf("detected %s uses %s in Dokploy", supportResourceKindLabel(requirement.Kind), supportResourceCandidateLabel(links[0])))
		default:
			plan.add(SeverityInfo, "linked-resource", fmt.Sprintf("detected %s can use these Dokploy apps: %s", supportResourceKindLabel(requirement.Kind), summarizeSupportResourceLabels(linkedResourceCandidateLabels(links), 4)))
		}
	}
}

func describeRequirementEvidence(requirement ExternalRequirementResource) string {
	if len(requirement.Evidence) == 0 {
		return ""
	}
	return " from env names: " + strings.Join(requirement.Evidence, ", ")
}

func addVolumeActions(plan *AppPlan) {
	for _, volume := range plan.Resources.Volumes {
		switch volume.Type {
		case "bind":
			plan.add(SeverityInfo, "volume", hostFileMountAction(volume))
		case "volume":
			plan.add(SeverityInfo, "volume", fmt.Sprintf("would create target volume and sync state for %s", volumeResourceLabel(volume)))
		}
	}
}

func readIndex(bundleDir string) (exporter.Summary, error) {
	var summary exporter.Summary
	if err := readJSON(filepath.Join(bundleDir, "index.json"), &summary); err != nil {
		return exporter.Summary{}, err
	}
	return summary, nil
}

func readTopology(path string) (analyzer.Topology, error) {
	var topology analyzer.Topology
	if err := readJSON(path, &topology); err != nil {
		return analyzer.Topology{}, err
	}
	return topology, nil
}

func readJSON(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(out)
}

func envExampleFiles(appDir string) []string {
	return envFiles(appDir, false)
}

func envFiles(appDir string, privateEnvValues bool) []string {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || (privateEnvValues && strings.HasSuffix(name, ".example")) || (!privateEnvValues && !strings.HasSuffix(name, ".example")) {
			continue
		}
		paths = append(paths, filepath.Join(appDir, name))
	}
	sort.Strings(paths)
	return paths
}

func topologyRisk(topology analyzer.Topology, code string) (analyzer.RiskReason, bool) {
	for _, risk := range topology.RiskReasons {
		if risk.Code == code {
			return risk, true
		}
	}
	return analyzer.RiskReason{}, false
}

func linkedResourceCandidateLabels(links []LinkedResourceCandidate) []string {
	labels := make([]string, 0, len(links))
	for _, link := range links {
		labels = append(labels, supportResourceCandidateLabel(link))
	}
	return planutil.UniqueStrings(labels)
}

func supportResourceCandidateLabel(link LinkedResourceCandidate) string {
	label := planutil.Fallback(link.App, "unknown")
	confidence := strings.TrimSpace(link.Confidence)
	if confidence != "" && confidence != "likely" && confidence != "detected" {
		label += " (possible match)"
	}
	return label
}

func supportResourceKindLabel(kind string) string {
	switch kind {
	case "database":
		return "database"
	case "redis":
		return "redis/cache"
	case "object-storage":
		return "object storage"
	case "email":
		return "email"
	default:
		return strings.ReplaceAll(kind, "-", " ")
	}
}

func summarizeSupportResourceLabels(labels []string, limit int) string {
	if limit <= 0 || len(labels) <= limit {
		return strings.Join(labels, ", ")
	}
	shown := append([]string{}, labels[:limit]...)
	shown = append(shown, fmt.Sprintf("+%d more", len(labels)-limit))
	return strings.Join(shown, ", ")
}

func volumeResourceLabel(volume VolumeResource) string {
	return volumeTargetLabel(volume.Service, volume.Target)
}

func hostFileMountAction(volume VolumeResource) string {
	return "will preserve VPS file/folder " + hostFileMountLabel(volume)
}

func hostFileMountLabel(volume VolumeResource) string {
	source := strings.TrimSpace(volume.Source)
	target := strings.TrimSpace(volume.Target)
	switch {
	case source != "" && target != "":
		return source + " -> " + target
	case source != "":
		return source
	case target != "":
		return "mounted at " + target
	default:
		return "mounted into the container"
	}
}

func volumeTargetLabel(service, target string) string {
	label := planutil.Fallback(service, "app")
	if target != "" {
		label += " -> " + target
	}
	return label
}

func dataStoreLabel(store DataStoreResource) string {
	label := store.Kind
	if store.Engine != "" && store.Engine != store.Kind {
		label += "/" + store.Engine
	}
	return label
}

func (p *AppPlan) add(severity Severity, kind, message string) {
	p.Actions = append(p.Actions, Action{Severity: severity, Kind: kind, Message: message})
}

func (p AppPlan) hasGate(code string) bool {
	for _, gate := range p.Gates {
		if gate.Code == code {
			return true
		}
	}
	return false
}

func (p *AppPlan) addGate(readiness Readiness, severity Severity, code, message, resourceRef string, evidence []string) {
	p.Gates = append(p.Gates, Gate{
		Readiness:   readiness,
		Severity:    severity,
		Code:        code,
		Message:     message,
		ResourceRef: resourceRef,
		Evidence:    planutil.UniqueStrings(evidence),
	})
}

func ReadinessFromGates(gates []Gate) Readiness {
	readiness := ReadinessReadyToCreate
	for _, gate := range gates {
		readiness = WorseReadiness(readiness, gate.Readiness)
	}
	return readiness
}

func WorseReadiness(a, b Readiness) Readiness {
	if ReadinessRank(b) > ReadinessRank(a) {
		return b
	}
	return a
}

func ReadinessRank(readiness Readiness) int {
	switch readiness {
	case ReadinessBlocked:
		return 3
	case ReadinessNeedsInput:
		return 2
	case ReadinessNeedsDecision:
		return 1
	default:
		return 0
	}
}

func StatusFromReadiness(readiness Readiness) Status {
	switch readiness {
	case ReadinessBlocked:
		return StatusRed
	case ReadinessNeedsInput, ReadinessNeedsDecision:
		return StatusYellow
	default:
		return StatusGreen
	}
}

func SeverityFromReadiness(readiness Readiness) Severity {
	switch readiness {
	case ReadinessBlocked:
		return SeverityError
	case ReadinessNeedsInput, ReadinessNeedsDecision:
		return SeverityWarn
	default:
		return SeverityInfo
	}
}

func ClampReadinessToDecision(readiness Readiness) Readiness {
	switch readiness {
	case ReadinessBlocked, ReadinessNeedsInput:
		return readiness
	default:
		return ReadinessNeedsDecision
	}
}

func WorseStatus(a, b Status) Status {
	if a == StatusRed || b == StatusRed {
		return StatusRed
	}
	if a == StatusYellow || b == StatusYellow {
		return StatusYellow
	}
	return StatusGreen
}
