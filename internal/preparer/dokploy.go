package preparer

type TargetResources struct {
	Platform string            `json:"platform"`
	DryRun   bool              `json:"dryRun"`
	Dokploy  *DokployResources `json:"dokploy,omitempty"`
}

type DokployResources struct {
	ComposeApp           DokployComposeApp            `json:"composeApp"`
	Domains              []DokployDomain              `json:"domains,omitempty"`
	EnvFiles             []DokployEnvFile             `json:"envFiles,omitempty"`
	Volumes              []DokployVolume              `json:"volumes,omitempty"`
	DataStores           []DokployDataStore           `json:"dataStores,omitempty"`
	ExternalRequirements []DokployExternalRequirement `json:"externalRequirements,omitempty"`
	LinkedResources      []DokployLinkedResource      `json:"linkedResources,omitempty"`
}

type DokployComposeApp struct {
	Name            string    `json:"name"`
	DisplayName     string    `json:"displayName"`
	SourceDirectory string    `json:"sourceDirectory"`
	ComposePath     string    `json:"composePath"`
	Type            string    `json:"type"`
	Readiness       Readiness `json:"readiness"`
	ComposeMissing  bool      `json:"composeMissing,omitempty"`
	MissingInputs   []string  `json:"missingInputs,omitempty"`
}

type DokployDomain struct {
	Host        string    `json:"host"`
	ServiceName string    `json:"serviceName,omitempty"`
	Port        string    `json:"port,omitempty"`
	AttachTo    string    `json:"attachTo"`
	Readiness   Readiness `json:"readiness"`
}

type DokployEnvFile struct {
	Path          string   `json:"path"`
	Keys          []string `json:"keys,omitempty"`
	MissingValues []string `json:"missingValues,omitempty"`
	NeedsValues   bool     `json:"needsValues"`
}

type DokployVolume struct {
	Name        string    `json:"name,omitempty"`
	Service     string    `json:"service,omitempty"`
	Source      string    `json:"source,omitempty"`
	Target      string    `json:"target"`
	Type        string    `json:"type"`
	Action      string    `json:"action"`
	Portability string    `json:"portability,omitempty"`
	Readiness   Readiness `json:"readiness"`
}

type DokployDataStore struct {
	Kind        string    `json:"kind"`
	Engine      string    `json:"engine,omitempty"`
	Service     string    `json:"service"`
	Strategy    string    `json:"strategy"`
	Fallback    string    `json:"fallback,omitempty"`
	Criticality string    `json:"criticality"`
	Action      string    `json:"action"`
	Readiness   Readiness `json:"readiness"`
}

type DokployExternalRequirement struct {
	Kind      string    `json:"kind"`
	Evidence  []string  `json:"evidence,omitempty"`
	Linkable  bool      `json:"linkable"`
	Action    string    `json:"action"`
	Readiness Readiness `json:"readiness"`
}

type DokployLinkedResource struct {
	Kind                 string    `json:"kind"`
	CandidateApp         string    `json:"candidateApp"`
	CandidateAppID       string    `json:"candidateAppId,omitempty"`
	Confidence           string    `json:"confidence"`
	Source               string    `json:"source"`
	RequiresConfirmation bool      `json:"requiresConfirmation"`
	Action               string    `json:"action"`
	Readiness            Readiness `json:"readiness"`
}

func dokployResources(plan AppPlan) *DokployResources {
	name := targetSafeName(plan.Name, plan.Directory)
	resources := &DokployResources{
		ComposeApp: DokployComposeApp{
			Name:            name,
			DisplayName:     plan.Name,
			SourceDirectory: plan.Directory,
			ComposePath:     plan.Resources.App.ComposePath,
			Type:            plan.Resources.App.Type,
			Readiness:       plan.Resources.App.Readiness,
			ComposeMissing:  plan.Resources.App.ComposeMissing,
			MissingInputs:   plan.Resources.App.MissingInputs,
		},
	}

	for _, domain := range plan.Resources.Domains {
		resources.Domains = append(resources.Domains, DokployDomain{
			Host:        domain.Host,
			ServiceName: domain.ServiceName,
			Port:        domain.Port,
			AttachTo:    name,
			Readiness:   domain.Readiness,
		})
	}
	for _, envFile := range plan.Resources.EnvFiles {
		resources.EnvFiles = append(resources.EnvFiles, DokployEnvFile{
			Path:          envFile.Path,
			Keys:          envFile.Keys,
			MissingValues: envFile.MissingValues,
			NeedsValues:   len(envFile.MissingValues) > 0,
		})
	}
	for _, volume := range plan.Resources.Volumes {
		resources.Volumes = append(resources.Volumes, DokployVolume{
			Name:        volume.Name,
			Service:     volume.Service,
			Source:      volume.Source,
			Target:      volume.Target,
			Type:        volume.Type,
			Action:      dokployVolumeAction(volume),
			Portability: volume.Portability,
			Readiness:   volume.Readiness,
		})
	}
	for _, store := range plan.Resources.DataStores {
		resources.DataStores = append(resources.DataStores, DokployDataStore{
			Kind:        store.Kind,
			Engine:      store.Engine,
			Service:     store.Service,
			Strategy:    store.Strategy,
			Fallback:    store.Fallback,
			Criticality: store.Criticality,
			Action:      dokployDataStoreAction(store),
			Readiness:   store.Readiness,
		})
	}
	for _, requirement := range plan.Resources.ExternalRequirements {
		resources.ExternalRequirements = append(resources.ExternalRequirements, DokployExternalRequirement{
			Kind:      requirement.Kind,
			Evidence:  requirement.Evidence,
			Linkable:  requirement.Linkable,
			Action:    dokployExternalRequirementAction(requirement),
			Readiness: requirement.Readiness,
		})
	}
	for _, link := range plan.Resources.LinkedResources {
		resources.LinkedResources = append(resources.LinkedResources, DokployLinkedResource{
			Kind:                 link.Kind,
			CandidateApp:         link.App,
			CandidateAppID:       link.AppID,
			Confidence:           link.Confidence,
			Source:               link.Source,
			RequiresConfirmation: link.RequiresConfirmation,
			Action:               "confirm_support_resource_candidate",
			Readiness:            link.Readiness,
		})
	}

	return resources
}

func targetSafeName(name, fallbackName string) string {
	if value := slug(name); value != "" {
		return value
	}
	if value := slug(fallbackName); value != "" {
		return value
	}
	return "app"
}

func dokployVolumeAction(volume VolumeResource) string {
	switch volume.Type {
	case "volume":
		return "create_volume_and_sync_state"
	case "bind":
		return "review_bind_mount_portability"
	default:
		return "review_stateful_volume"
	}
}

func dokployDataStoreAction(store DataStoreResource) string {
	if store.Readiness == ReadinessBlocked {
		return "manual_data_store_review"
	}
	return "confirm_data_store_strategy"
}

func dokployExternalRequirementAction(requirement ExternalRequirementResource) string {
	if requirement.Linkable {
		return "select_or_confirm_support_resource"
	}
	return "resolve_external_requirement"
}
