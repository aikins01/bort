package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
	"github.com/aikins01/bort/internal/target/dokploy"
)

const cleanupAPIVersion = "bort.cleanup/v1alpha1"

var defaultStaleDokployPlatformProjects = []string{"coolify-proxy", "proxy", "source"}

type cleanupResult struct {
	APIVersion           string                         `json:"apiVersion"`
	RunName              string                         `json:"runName,omitempty"`
	RunDir               string                         `json:"runDir,omitempty"`
	Target               string                         `json:"target"`
	DryRun               bool                           `json:"dryRun"`
	Applied              bool                           `json:"applied,omitempty"`
	BackupPath           string                         `json:"backupPath,omitempty"`
	DeletedProjects      []dokploy.StalePlatformProject `json:"deletedProjects,omitempty"`
	StalePlatformRecords []cleanupStalePlatformRecord   `json:"stalePlatformRecords,omitempty"`
	SourceControls       []cleanupSourceControl         `json:"sourceControls,omitempty"`
	SourceContainers     []cleanupSourceContainer       `json:"sourceContainers,omitempty"`
	SourceVolumes        []cleanupSourceVolume          `json:"sourceVolumes,omitempty"`
	SourceNetworks       []cleanupSourceNetwork         `json:"sourceNetworks,omitempty"`
	TargetArtifacts      []cleanupTargetArtifact        `json:"targetArtifacts,omitempty"`
	Actions              []cleanupAction                `json:"actions"`
	Warnings             []string                       `json:"warnings,omitempty"`
}

type cleanupStalePlatformRecord struct {
	Name        string   `json:"name"`
	ProjectID   string   `json:"projectId,omitempty"`
	ComposeIDs  []string `json:"composeIds,omitempty"`
	DomainCount int      `json:"domainCount,omitempty"`
	Status      string   `json:"status"`
	Message     string   `json:"message"`
}

type cleanupSourceControl struct {
	App        string `json:"app"`
	Repository string `json:"repository,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Auth       string `json:"auth,omitempty"`
	Action     string `json:"action"`
}

type cleanupSourceContainer struct {
	App           string `json:"app"`
	Service       string `json:"service"`
	ContainerID   string `json:"containerId,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	Action        string `json:"action"`
}

type cleanupSourceVolume struct {
	App     string `json:"app"`
	Service string `json:"service,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
	Source  string `json:"source,omitempty"`
	Target  string `json:"target"`
	Action  string `json:"action"`
}

type cleanupSourceNetwork struct {
	App    string `json:"app"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

type cleanupTargetArtifact struct {
	App    string `json:"app"`
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Action string `json:"action"`
}

type cleanupAction struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Safety  string `json:"safety"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

func runCleanup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var runRef string
	var target string
	var format string
	var outputPath string
	var apply bool
	var backupDir string

	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.BoolVar(&apply, "apply", false, "remove only safe stale Dokploy metadata after taking a database backup")
	fs.StringVar(&backupDir, "backup-dir", filepath.Join(".bort", "backups"), "directory for Dokploy DB backups before metadata cleanup")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkOutputFormat("cleanup", format); err != nil {
		return err
	}
	if target != "dokploy" {
		return fmt.Errorf("cleanup currently supports target dokploy only, got %q", target)
	}

	run, err := loadCleanupRun(runRef)
	if err != nil {
		return err
	}
	result := planCleanup(ctx, run, target)
	result.DryRun = !apply
	if apply {
		client, err := resolveDokployClient(ctx, target, os.Stdin, stderr)
		if err != nil {
			return err
		}
		applied, err := client.CleanupStalePlatformProjects(ctx, dokploy.StalePlatformCleanupOptions{
			ProjectNames: defaultStaleDokployPlatformProjects,
			BackupDir:    backupDir,
			BackupPrefix: "dokploy-stale-platform-records",
		})
		if err != nil {
			return err
		}
		result.Applied = true
		result.BackupPath = applied.BackupPath
		result.DeletedProjects = applied.Deleted
		result.Actions = append(result.Actions, cleanupAction{
			Kind:    "dokploy_metadata",
			Ref:     strings.Join(defaultStaleDokployPlatformProjects, ","),
			Safety:  "metadata_only",
			Status:  "applied",
			Message: fmt.Sprintf("deleted %d stale zero-domain Dokploy platform project record(s); backup written before delete", len(applied.Deleted)),
		})
	}

	return writeFormattedOutput(stdout, outputPath, format, result, writeCleanupText)
}

func loadCleanupRun(runRef string) (loadedMigrationRun, error) {
	if strings.TrimSpace(runRef) != "" {
		return loadMigrationRun(runRef)
	}
	if latest, ok := latestRunRef(); ok {
		return loadMigrationRun(latest)
	}
	return loadedMigrationRun{}, fmt.Errorf("no migration run found; run `bort` or pass --run before cleanup")
}

func planCleanup(ctx context.Context, run loadedMigrationRun, target string) cleanupResult {
	result := cleanupResult{
		APIVersion: cleanupAPIVersion,
		RunName:    run.Run.Name,
		RunDir:     run.Run.RunDir,
		Target:     target,
		DryRun:     true,
	}
	result.StalePlatformRecords, result.Warnings = inspectStalePlatformRecords(ctx, target)
	for _, record := range result.StalePlatformRecords {
		status := "planned"
		message := "remove Dokploy metadata only if the project still exists and has zero domains"
		if record.Status == "absent" {
			status = "noop"
			message = "already absent"
		} else if record.Status == "blocked" {
			status = "blocked"
			message = record.Message
		}
		result.Actions = append(result.Actions, cleanupAction{Kind: "dokploy_metadata", Ref: record.Name, Safety: "metadata_only", Status: status, Message: message})
	}

	for _, app := range run.Prepare.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		if control := cleanupSourceControlForApp(app); control != nil {
			result.SourceControls = append(result.SourceControls, *control)
		}
		result.SourceContainers = append(result.SourceContainers, cleanupContainersForApp(app)...)
		result.SourceVolumes = append(result.SourceVolumes, cleanupVolumesForApp(app)...)
		result.SourceNetworks = append(result.SourceNetworks, cleanupNetworksForApp(run, app)...)
		result.TargetArtifacts = append(result.TargetArtifacts, cleanupTargetArtifactsForApp(app)...)
	}
	sortCleanupResult(&result)
	if len(result.SourceContainers) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "source_containers", Ref: "run", Safety: "destructive_explicit_only", Status: "inventory", Message: "source containers are listed for later purge; cleanup --apply does not remove them"})
	}
	if len(result.SourceControls) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "source_control", Ref: "run", Safety: "credentials_not_touched", Status: "inventory", Message: "Coolify and Dokploy source-control credentials are not copied, revoked, or removed by cleanup"})
	}
	if len(result.SourceVolumes) > 0 || len(result.SourceNetworks) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "source_state", Ref: "run", Safety: "manual_after_acceptance", Status: "inventory", Message: "volumes and networks are preserved by default and are not removed by cleanup --apply"})
	}
	if len(result.TargetArtifacts) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "target_artifacts", Ref: "run", Safety: "preserve_target", Status: "inventory", Message: "Bort-created target artifacts are listed for audit and kept by default"})
	}
	return result
}

func inspectStalePlatformRecords(ctx context.Context, target string) ([]cleanupStalePlatformRecord, []string) {
	records := make([]cleanupStalePlatformRecord, 0, len(defaultStaleDokployPlatformProjects))
	for _, name := range defaultStaleDokployPlatformProjects {
		records = append(records, cleanupStalePlatformRecord{Name: name, Status: "unknown", Message: "will verify zero domains against Dokploy DB before deleting"})
	}
	client, err := lookupDokployClient(target)
	if err != nil {
		return records, []string{err.Error()}
	}
	projects, err := client.ListProjects(ctx)
	if err != nil {
		return records, []string{fmt.Sprintf("inspect Dokploy projects: %v", err)}
	}
	byName := map[string]dokploy.Project{}
	for _, project := range projects {
		byName[project.Name] = project
	}
	warnings := []string{}
	for i := range records {
		project, ok := byName[records[i].Name]
		if !ok {
			records[i].Status = "absent"
			records[i].Message = "no Dokploy project with this stale platform name is visible"
			continue
		}
		inspectionIncomplete := false
		fresh, err := client.GetProject(ctx, project.ProjectID)
		if err == nil && fresh != nil {
			project = *fresh
		} else if err != nil {
			warnings = append(warnings, fmt.Sprintf("inspect Dokploy project %s: %v", project.Name, err))
			inspectionIncomplete = true
		}
		records[i].ProjectID = project.ProjectID
		for _, compose := range project.Compose {
			if strings.TrimSpace(compose.ComposeID) == "" {
				continue
			}
			records[i].ComposeIDs = append(records[i].ComposeIDs, compose.ComposeID)
			domains, err := client.ListDomainsByCompose(ctx, compose.ComposeID)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("inspect domains for %s/%s: %v", project.Name, compose.Name, err))
				inspectionIncomplete = true
				continue
			}
			records[i].DomainCount += len(domains)
		}
		if inspectionIncomplete {
			records[i].Status = "unknown"
			records[i].Message = "live domain inspection was incomplete; cleanup --apply will recheck zero domains in the Dokploy DB before deleting"
			continue
		}
		if records[i].DomainCount > 0 {
			records[i].Status = "blocked"
			records[i].Message = fmt.Sprintf("refusing automatic metadata cleanup because %d domain(s) are attached", records[i].DomainCount)
			continue
		}
		records[i].Status = "present"
		records[i].Message = "safe metadata cleanup candidate: zero domains visible"
	}
	return records, warnings
}

func cleanupSourceControlForApp(app preparer.AppPlan) *cleanupSourceControl {
	if app.Resources.SourceControl == nil {
		return nil
	}
	control := app.Resources.SourceControl
	return &cleanupSourceControl{
		App:        app.Name,
		Repository: control.Repository,
		Provider:   control.Provider,
		Auth:       control.Auth,
		Action:     "manual_connect_or_revoke_in_platform_ui_after_acceptance",
	}
}

func cleanupContainersForApp(app preparer.AppPlan) []cleanupSourceContainer {
	containers := []cleanupSourceContainer{}
	seen := map[string]struct{}{}
	for _, service := range app.Resources.SourceServices {
		ref := service.ContainerID + "/" + service.ContainerName
		if ref == "/" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		containers = append(containers, cleanupSourceContainer{
			App:           app.Name,
			Service:       service.ServiceName,
			ContainerID:   service.ContainerID,
			ContainerName: service.ContainerName,
			Action:        "inventory_only_cleanup_apply_does_not_remove",
		})
	}
	return containers
}

func cleanupVolumesForApp(app preparer.AppPlan) []cleanupSourceVolume {
	volumes := []cleanupSourceVolume{}
	for _, volume := range app.Resources.Volumes {
		volumes = append(volumes, cleanupSourceVolume{
			App:     app.Name,
			Service: volume.Service,
			Type:    volume.Type,
			Name:    volume.Name,
			Source:  volume.Source,
			Target:  volume.Target,
			Action:  "preserve_until_manual_purge_after_acceptance",
		})
	}
	return volumes
}

func cleanupNetworksForApp(run loadedMigrationRun, app preparer.AppPlan) []cleanupSourceNetwork {
	topology := cleanupReadTopology(run, app)
	if len(topology.Networks) == 0 {
		return nil
	}
	networks := make([]cleanupSourceNetwork, 0, len(topology.Networks))
	for _, network := range topology.Networks {
		if strings.TrimSpace(network) == "" {
			continue
		}
		networks = append(networks, cleanupSourceNetwork{App: app.Name, Name: network, Action: "preserve_until_manual_purge_after_acceptance"})
	}
	return networks
}

func cleanupReadTopology(run loadedMigrationRun, app preparer.AppPlan) analyzer.Topology {
	bundleDir := run.Prepare.BundleDir
	appDir := filepath.Join(bundleDir, filepath.FromSlash(app.Directory))
	if err := safepath.ContainedPath(bundleDir, appDir); err != nil {
		return analyzer.Topology{}
	}
	path := filepath.Join(appDir, "topology.json")
	if err := safepath.ContainedPath(appDir, path); err != nil {
		return analyzer.Topology{}
	}
	contents, err := safepath.ReadFileNoFollow(path)
	if err != nil {
		return analyzer.Topology{}
	}
	var topology analyzer.Topology
	if err := json.Unmarshal(contents, &topology); err != nil {
		return analyzer.Topology{}
	}
	return topology
}

func cleanupTargetArtifactsForApp(app preparer.AppPlan) []cleanupTargetArtifact {
	if app.TargetResources == nil || app.TargetResources.Dokploy == nil {
		return nil
	}
	dokployResources := app.TargetResources.Dokploy
	artifacts := []cleanupTargetArtifact{
		{App: app.Name, Kind: "project", Ref: dokployResources.Project.Name, Action: "keep_target"},
		{App: app.Name, Kind: "compose", Ref: dokployResources.ComposeApp.Name, Action: "keep_target"},
	}
	for _, domain := range dokployResources.Domains {
		artifacts = append(artifacts, cleanupTargetArtifact{App: app.Name, Kind: "domain", Ref: domain.Host, Action: "keep_target"})
	}
	for _, volume := range dokployResources.Volumes {
		ref := firstCleanupValue(volume.Name, volume.Source, volume.Target)
		artifacts = append(artifacts, cleanupTargetArtifact{App: app.Name, Kind: "volume", Ref: ref, Action: "keep_target"})
	}
	return artifacts
}

func sortCleanupResult(result *cleanupResult) {
	sort.Slice(result.SourceControls, func(i, j int) bool {
		return cleanupSortKey(result.SourceControls[i].App, result.SourceControls[i].Repository) < cleanupSortKey(result.SourceControls[j].App, result.SourceControls[j].Repository)
	})
	sort.Slice(result.SourceContainers, func(i, j int) bool {
		return cleanupSortKey(result.SourceContainers[i].App, result.SourceContainers[i].Service, result.SourceContainers[i].ContainerName) < cleanupSortKey(result.SourceContainers[j].App, result.SourceContainers[j].Service, result.SourceContainers[j].ContainerName)
	})
	sort.Slice(result.SourceVolumes, func(i, j int) bool {
		return cleanupSortKey(result.SourceVolumes[i].App, result.SourceVolumes[i].Service, result.SourceVolumes[i].Target) < cleanupSortKey(result.SourceVolumes[j].App, result.SourceVolumes[j].Service, result.SourceVolumes[j].Target)
	})
	sort.Slice(result.SourceNetworks, func(i, j int) bool {
		return cleanupSortKey(result.SourceNetworks[i].App, result.SourceNetworks[i].Name) < cleanupSortKey(result.SourceNetworks[j].App, result.SourceNetworks[j].Name)
	})
	sort.Slice(result.TargetArtifacts, func(i, j int) bool {
		return cleanupSortKey(result.TargetArtifacts[i].App, result.TargetArtifacts[i].Kind, result.TargetArtifacts[i].Ref) < cleanupSortKey(result.TargetArtifacts[j].App, result.TargetArtifacts[j].Kind, result.TargetArtifacts[j].Ref)
	})
}

func cleanupSortKey(parts ...string) string {
	return strings.Join(parts, "\x00")
}

func firstCleanupValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "unknown"
}

func writeCleanupText(w io.Writer, result cleanupResult) {
	mode := "dry run"
	if result.Applied {
		mode = "applied"
	}
	fmt.Fprintf(w, "Cleanup plan: %s -> %s\n", firstCleanupValue(result.RunDir, result.RunName, "run"), result.Target)
	fmt.Fprintf(w, "Mode: %s\n\n", mode)

	if result.Applied {
		fmt.Fprintf(w, "Backup: %s\n", result.BackupPath)
		if len(result.DeletedProjects) == 0 {
			fmt.Fprintln(w, "Deleted stale Dokploy metadata: none")
		} else {
			fmt.Fprintln(w, "Deleted stale Dokploy metadata:")
			for _, project := range result.DeletedProjects {
				fmt.Fprintf(w, "  - %s (%s)\n", project.Name, project.ProjectID)
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Safe Dokploy metadata cleanup:")
	for _, record := range result.StalePlatformRecords {
		ref := record.Name
		if record.ProjectID != "" {
			ref += " (" + record.ProjectID + ")"
		}
		fmt.Fprintf(w, "  [%s] %s: %s", record.Status, ref, record.Message)
		if record.DomainCount > 0 {
			fmt.Fprintf(w, " (%d domains)", record.DomainCount)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)

	if len(result.SourceContainers) > 0 {
		fmt.Fprintln(w, "Source containers inventoried for later purge:")
		for _, container := range result.SourceContainers {
			fmt.Fprintf(w, "  - %s/%s %s\n", container.App, container.Service, firstCleanupValue(container.ContainerName, container.ContainerID))
		}
		fmt.Fprintln(w)
	}
	if len(result.SourceControls) > 0 {
		fmt.Fprintln(w, "Source-control credentials left untouched:")
		for _, control := range result.SourceControls {
			fmt.Fprintf(w, "  - %s %s (%s/%s)\n", control.App, firstCleanupValue(control.Repository, "repository"), firstCleanupValue(control.Provider, "git"), firstCleanupValue(control.Auth, "unknown-auth"))
		}
		fmt.Fprintln(w)
	}
	if len(result.SourceVolumes) > 0 {
		fmt.Fprintln(w, "Source volumes and bind mounts preserved:")
		for _, volume := range result.SourceVolumes {
			fmt.Fprintf(w, "  - %s %s -> %s (%s)\n", volume.App, firstCleanupValue(volume.Name, volume.Source), volume.Target, volume.Type)
		}
		fmt.Fprintln(w)
	}
	if len(result.SourceNetworks) > 0 {
		fmt.Fprintln(w, "Source networks preserved:")
		for _, network := range result.SourceNetworks {
			fmt.Fprintf(w, "  - %s %s\n", network.App, network.Name)
		}
		fmt.Fprintln(w)
	}
	if len(result.TargetArtifacts) > 0 {
		fmt.Fprintln(w, "Target artifacts kept by default:")
		for _, artifact := range result.TargetArtifacts {
			fmt.Fprintf(w, "  - %s %s %s\n", artifact.App, artifact.Kind, artifact.Ref)
		}
		fmt.Fprintln(w)
	}

	if len(result.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range result.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}

	if result.Applied {
		fmt.Fprintln(w, "Applied only the safe Dokploy metadata cleanup. Source containers, volumes, networks, source-control credentials, and target apps were not removed.")
		return
	}
	fmt.Fprintln(w, "Dry run only: run `bort cleanup --apply` to remove only stale zero-domain Dokploy platform metadata after a DB backup.")
	fmt.Fprintln(w, "Source containers, volumes, networks, source-control credentials, and target apps are inventoried only and are preserved by this command.")
}
