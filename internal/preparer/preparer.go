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
	Status          Status           `json:"status"`
	Readiness       Readiness        `json:"readiness"`
	Resources       ResourceSpecs    `json:"resources"`
	TargetResources *TargetResources `json:"targetResources,omitempty"`
	Gates           []Gate           `json:"gates,omitempty"`
	Actions         []Action         `json:"actions"`
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
	Domains              []DomainResource              `json:"domains,omitempty"`
	EnvFiles             []EnvFileResource             `json:"envFiles,omitempty"`
	Volumes              []VolumeResource              `json:"volumes,omitempty"`
	DataStores           []DataStoreResource           `json:"dataStores,omitempty"`
	ExternalRequirements []ExternalRequirementResource `json:"externalRequirements,omitempty"`
	LinkedResources      []LinkedResourceCandidate     `json:"linkedResources,omitempty"`
}

type AppResource struct {
	Type           string    `json:"type"`
	Name           string    `json:"name"`
	ComposePath    string    `json:"composePath"`
	ComposeMissing bool      `json:"composeMissing,omitempty"`
	MissingInputs  []string  `json:"missingInputs,omitempty"`
	Readiness      Readiness `json:"readiness"`
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
	Service     string    `json:"service,omitempty"`
	Type        string    `json:"type"`
	Name        string    `json:"name,omitempty"`
	Source      string    `json:"source,omitempty"`
	Target      string    `json:"target"`
	ReadWrite   bool      `json:"readWrite"`
	Portability string    `json:"portability,omitempty"`
	Readiness   Readiness `json:"readiness"`
}

type DataStoreResource struct {
	Kind        string    `json:"kind"`
	Engine      string    `json:"engine,omitempty"`
	Service     string    `json:"service"`
	Image       string    `json:"image,omitempty"`
	Volumes     []string  `json:"volumes,omitempty"`
	Strategy    string    `json:"strategy"`
	Fallback    string    `json:"fallback,omitempty"`
	Criticality string    `json:"criticality"`
	Readiness   Readiness `json:"readiness"`
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

	plan := AppPlan{Name: app.Name, Directory: app.Directory, Status: StatusGreen, Readiness: ReadinessReadyToCreate}
	plan.Resources = resourceSpecs(app, appDir, topology)
	addReadinessGates(&plan, topology)
	plan.add(SeverityInfo, "compose", fmt.Sprintf("would create %s compose app from compose.yaml", target))
	addEnvironmentActions(&plan)
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

func resourceSpecs(app exporter.AppSummary, appDir string, topology analyzer.Topology) ResourceSpecs {
	resources := ResourceSpecs{
		App:      appResource(app.Name, appDir),
		EnvFiles: envFileResources(appDir),
	}

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
		readiness := ReadinessNeedsDecision
		linkable := analyzer.IsLinkableRequirement(requirement.Kind)
		if !linkable {
			readiness = ReadinessNeedsInput
		}
		resources.ExternalRequirements = append(resources.ExternalRequirements, ExternalRequirementResource{
			Kind:      requirement.Kind,
			Evidence:  planutil.UniqueStrings(requirement.Evidence),
			Linkable:  linkable,
			Readiness: readiness,
		})
	}
	for _, link := range topology.LinkedResources {
		resources.LinkedResources = append(resources.LinkedResources, linkedResourceCandidate(link))
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

func envFileResources(appDir string) []EnvFileResource {
	paths := envExampleFiles(appDir)
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
		readiness = ReadinessNeedsDecision
		portability = "review_required"
	}
	return VolumeResource{
		Service:     volume.Service,
		Type:        volume.Type,
		Name:        volume.Name,
		Source:      volume.Source,
		Target:      volume.Target,
		ReadWrite:   volume.RW,
		Portability: portability,
		Readiness:   readiness,
	}
}

func dataStoreResource(store analyzer.DataStore) DataStoreResource {
	readiness := ReadinessNeedsDecision
	if store.Kind == "unknown" || store.Strategy == "manual_review" {
		readiness = ReadinessBlocked
	}
	return DataStoreResource{
		Kind:        store.Kind,
		Engine:      store.Engine,
		Service:     store.Service,
		Image:       store.Image,
		Volumes:     planutil.UniqueStrings(store.Volumes),
		Strategy:    store.Strategy,
		Fallback:    store.Fallback,
		Criticality: store.Criticality,
		Readiness:   readiness,
	}
}

func linkedResourceCandidate(link analyzer.ResourceLink) LinkedResourceCandidate {
	stores := make([]DataStoreResource, 0, len(link.DataStores))
	for _, store := range link.DataStores {
		stores = append(stores, dataStoreResource(store))
	}
	return LinkedResourceCandidate{
		Kind:                 link.Kind,
		App:                  link.App,
		AppID:                link.AppID,
		Role:                 link.Role,
		Runtime:              link.Runtime,
		Confidence:           planutil.Fallback(link.Confidence, "unknown"),
		Reasons:              planutil.UniqueStrings(link.Reasons),
		Networks:             planutil.UniqueStrings(link.Networks),
		DataStores:           stores,
		Source:               "heuristic",
		RequiresConfirmation: true,
		Readiness:            ReadinessNeedsDecision,
	}
}

func addReadinessGates(plan *AppPlan, topology analyzer.Topology) {
	if plan.Resources.App.ComposeMissing {
		plan.addGate(ReadinessBlocked, SeverityError, "app.compose_missing", "compose.yaml is missing; target app shell cannot be prepared", "app", nil)
	}
	if len(plan.Resources.App.MissingInputs) > 0 {
		missing := plan.Resources.App.MissingInputs
		message := fmt.Sprintf("compose.yaml contains placeholders that must be replaced before target preparation: %s", strings.Join(missing, ", "))
		plan.addGate(ReadinessBlocked, SeverityError, "app.compose_incomplete", message, "app", missing)
	}
	if risk, ok := topologyRisk(topology, "deploy.missing_artifact"); ok {
		plan.Resources.App.Readiness = ReadinessBlocked
		plan.addGate(ReadinessBlocked, SeverityError, "deploy.missing_artifact", risk.Message, "app", nil)
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
			plan.addGate(ReadinessNeedsInput, SeverityWarn, "env.values_required", message, "env:"+envFile.Path, envFile.MissingValues)
		}
	}
	if risk, ok := topologyRisk(topology, "env.values_redacted"); ok {
		plan.addGate(ReadinessNeedsInput, SeverityWarn, "env.values_redacted", risk.Message, "env", nil)
	}

	for _, domain := range plan.Resources.Domains {
		resourceRef := "domain:" + planutil.Fallback(domain.Host, "missing-host")
		if strings.TrimSpace(domain.Host) == "" {
			plan.addGate(ReadinessNeedsInput, SeverityWarn, "domain.host_missing", "domain route has no host and must be filled before deploy", resourceRef, nil)
		} else if strings.TrimSpace(domain.ServiceName) == "" {
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, "domain.service_missing", fmt.Sprintf("domain %s has no service mapping; confirm target service manually", domain.Host), resourceRef, nil)
		}
	}
	if len(plan.Resources.Domains) == 0 {
		if risk, ok := topologyRisk(topology, "routes.none"); ok {
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, "routes.none", risk.Message, "domains", nil)
		}
	}

	for _, store := range plan.Resources.DataStores {
		resourceRef := "data-store:" + planutil.Fallback(store.Service, store.Kind)
		if store.Readiness == ReadinessBlocked {
			message := fmt.Sprintf("manual data-store review required for service %s before target preparation can be trusted", planutil.Fallback(store.Service, "unknown"))
			plan.addGate(ReadinessBlocked, SeverityError, "data_store.manual_review", message, resourceRef, store.Volumes)
			continue
		}
		message := fmt.Sprintf("confirm %s data-store preparation strategy %s for service %s", dataStoreLabel(store), planutil.Fallback(store.Strategy, "manual_review"), planutil.Fallback(store.Service, "unknown"))
		plan.addGate(ReadinessNeedsDecision, SeverityWarn, "data_store.prepare_required", message, resourceRef, store.Volumes)
	}

	addLinkedResourceGates(plan)

	for _, volume := range plan.Resources.Volumes {
		if volume.Type == "bind" {
			message := fmt.Sprintf("review bind mount portability for %s", volumeResourceLabel(volume))
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, "volume.bind_mount_review", message, "volume:"+volumeResourceLabel(volume), nil)
		}
	}
}

func addLinkedResourceGates(plan *AppPlan) {
	linksByKind := map[string][]LinkedResourceCandidate{}
	for _, link := range plan.Resources.LinkedResources {
		linksByKind[link.Kind] = append(linksByKind[link.Kind], link)
	}

	for _, requirement := range plan.Resources.ExternalRequirements {
		resourceRef := "external:" + requirement.Kind
		if !requirement.Linkable {
			message := fmt.Sprintf("resolve external %s requirement before deploy", requirement.Kind)
			plan.addGate(ReadinessNeedsInput, SeverityWarn, "external_requirement.resolve", message, resourceRef, requirement.Evidence)
			continue
		}

		links := linksByKind[requirement.Kind]
		switch {
		case len(links) == 0:
			message := fmt.Sprintf("select or create a support resource for external %s requirement", requirement.Kind)
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, "linked_resource.missing_candidate", message, resourceRef, requirement.Evidence)
		case len(links) == 1:
			message := fmt.Sprintf("confirm heuristic %s support resource candidate %s with %s confidence", requirement.Kind, planutil.Fallback(links[0].App, "unknown"), planutil.Fallback(links[0].Confidence, "unknown"))
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, "linked_resource.confirm_candidate", message, "linked-resource:"+planutil.Fallback(links[0].App, "unknown"), links[0].Reasons)
		default:
			message := fmt.Sprintf("select one heuristic %s support resource candidate: %s", requirement.Kind, strings.Join(linkedResourceCandidateLabels(links), ", "))
			plan.addGate(ReadinessNeedsDecision, SeverityWarn, "linked_resource.select_candidate", message, resourceRef, requirement.Evidence)
		}
	}
}

func addEnvironmentActions(plan *AppPlan) {
	if len(plan.Resources.EnvFiles) == 0 {
		return
	}

	items := []string{}
	for _, envFile := range plan.Resources.EnvFiles {
		if count := len(envFile.Keys); count > 0 {
			items = append(items, fmt.Sprintf("%s (%d vars)", envFile.Path, count))
		}
	}
	if len(items) == 0 {
		return
	}

	severity := SeverityInfo
	if plan.hasGate("env.values_redacted") {
		severity = SeverityWarn
	}
	plan.add(severity, "environment", fmt.Sprintf("review and fill exported env examples before deploy: %s", strings.Join(items, ", ")))
}

func addRouteActions(plan *AppPlan, target string) {
	if len(plan.Resources.Domains) == 0 {
		if plan.hasGate("routes.none") {
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
		severity := SeverityWarn
		message := fmt.Sprintf("needs %s data store preparation for service %s with %s", dataStoreLabel(store), planutil.Fallback(store.Service, "unknown"), planutil.Fallback(store.Strategy, "manual_review"))
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
			plan.add(SeverityWarn, "external-requirement", fmt.Sprintf("needs external %s requirement resolved%s", requirement.Kind, evidence))
			continue
		}

		links := linksByKind[requirement.Kind]
		switch {
		case len(links) == 0:
			plan.add(SeverityWarn, "linked-resource", fmt.Sprintf("needs support resource selection for external %s requirement%s", requirement.Kind, evidence))
		case len(links) == 1:
			severity := SeverityInfo
			if links[0].Confidence != "likely" {
				severity = SeverityWarn
			}
			plan.add(severity, "linked-resource", fmt.Sprintf("needs confirmation of %s support resource %s with %s confidence", requirement.Kind, planutil.Fallback(links[0].App, "unknown"), planutil.Fallback(links[0].Confidence, "unknown")))
		default:
			plan.add(SeverityWarn, "linked-resource", fmt.Sprintf("needs one %s support resource candidate selected: %s", requirement.Kind, strings.Join(linkedResourceCandidateLabels(links), ", ")))
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
			plan.add(SeverityWarn, "volume", fmt.Sprintf("review bind mount portability for %s", volumeResourceLabel(volume)))
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
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || !strings.HasSuffix(name, ".example") {
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
		labels = append(labels, supportResourceLabel(link.App, link.Confidence))
	}
	return planutil.UniqueStrings(labels)
}

func supportResourceLabel(app, confidence string) string {
	label := planutil.Fallback(app, "unknown")
	if confidence != "" {
		label += " (" + confidence + ")"
	}
	return label
}

func volumeResourceLabel(volume VolumeResource) string {
	return volumeTargetLabel(volume.Service, volume.Target)
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

func WorseStatus(a, b Status) Status {
	if a == StatusRed || b == StatusRed {
		return StatusRed
	}
	if a == StatusYellow || b == StatusYellow {
		return StatusYellow
	}
	return StatusGreen
}
