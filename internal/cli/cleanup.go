package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/analyzer"
	"github.com/aikins01/bort/internal/planutil"
	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
	"github.com/aikins01/bort/internal/target/dokploy"
)

const cleanupAPIVersion = "bort.cleanup/v1alpha1"
const cleanupPurgeAPIVersion = "bort.cleanup.purge/v1alpha1"

var cleanupPurgeLiveApplySkipKinds = map[dokploy.StepKind]struct{}{
	dokploy.StepResumeSource: {},
	dokploy.StepResumeTarget: {},
}

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
	App            string `json:"app"`
	Service        string `json:"service,omitempty"`
	Type           string `json:"type"`
	Name           string `json:"name,omitempty"`
	Source         string `json:"source,omitempty"`
	Target         string `json:"target"`
	ExpectedAbsent bool   `json:"expectedAbsent,omitempty"`
	Action         string `json:"action"`
}

type cleanupSourceNetwork struct {
	App              string `json:"app"`
	Name             string `json:"name"`
	ExpectedIdentity string `json:"expectedIdentity,omitempty"`
	ExpectedAbsent   bool   `json:"expectedAbsent,omitempty"`
	Action           string `json:"action"`
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

type cleanupPurgeResult struct {
	APIVersion         string                     `json:"apiVersion"`
	RunName            string                     `json:"runName,omitempty"`
	RunDir             string                     `json:"runDir,omitempty"`
	Target             string                     `json:"target"`
	DryRun             bool                       `json:"dryRun"`
	Applied            bool                       `json:"applied,omitempty"`
	CompletesLifecycle bool                       `json:"completesLifecycle,omitempty"`
	LifecycleCompleted bool                       `json:"lifecycleCompleted,omitempty"`
	ManualCompletion   bool                       `json:"manualCompletion,omitempty"`
	ExecutionStartedAt *time.Time                 `json:"executionStartedAt,omitempty"`
	BackupDir          string                     `json:"backupDir,omitempty"`
	BackupPath         string                     `json:"backupPath,omitempty"`
	Filters            cleanupPurgeFilters        `json:"filters"`
	SourceControls     []cleanupSourceControl     `json:"sourceControls,omitempty"`
	SourceContainers   []cleanupSourceContainer   `json:"sourceContainers,omitempty"`
	SourceVolumes      []cleanupSourceVolume      `json:"sourceVolumes,omitempty"`
	SourceNetworks     []cleanupSourceNetwork     `json:"sourceNetworks,omitempty"`
	SourcePaths        []cleanupSourcePath        `json:"sourcePaths,omitempty"`
	Actions            []cleanupAction            `json:"actions"`
	PurgeResult        *dokploy.SourcePurgeResult `json:"purgeResult,omitempty"`
	Warnings           []string                   `json:"warnings,omitempty"`
}

type cleanupPurgeFilters struct {
	Apps            []string `json:"apps,omitempty"`
	Projects        []string `json:"projects,omitempty"`
	AllApps         bool     `json:"allApps,omitempty"`
	IncludePlatform bool     `json:"includePlatform,omitempty"`
}

type cleanupSourcePath struct {
	App            string `json:"app,omitempty"`
	Path           string `json:"path"`
	Source         string `json:"source"`
	AllowPlatform  bool   `json:"allowPlatform,omitempty"`
	ExpectedAbsent bool   `json:"expectedAbsent,omitempty"`
	Action         string `json:"action"`
}

type stringListFlag []string

func runCleanup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runCleanupWithInput(ctx, args, nil, stdout, stderr)
}

func runCleanupWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "purge" {
		return runCleanupPurge(ctx, args[1:], stdin, stdout, stderr)
	}

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
	if fs.NArg() > 0 {
		if fs.Arg(0) == "purge" {
			return fmt.Errorf("cleanup purge must immediately follow cleanup; use `%s`", bortCommand("cleanup purge"))
		}
		return fmt.Errorf("cleanup does not accept positional argument %q", fs.Arg(0))
	}
	if err := checkOutputFormat("cleanup", format); err != nil {
		return err
	}
	if target != "dokploy" {
		return fmt.Errorf("cleanup currently supports target dokploy only, got %q", target)
	}

	resolvedRunRef, err := resolveRunRef(runRef, false)
	if err != nil {
		return err
	}
	var operationLock *applyLock
	if apply {
		operationLock, err = acquireRunOperationLock(resolvedRunRef)
		if err != nil {
			return fmt.Errorf("cleanup run %q: %w", resolvedRunRef, err)
		}
		defer operationLock.Release()
	}
	run, err := loadMigrationRun(resolvedRunRef)
	if err != nil {
		return err
	}
	result := planCleanup(ctx, run, target)
	result.DryRun = !apply
	if apply {
		if collisions := cleanupStaleProjectNameCollisions(run, defaultStaleDokployPlatformProjects); len(collisions) > 0 {
			return fmt.Errorf("refusing cleanup --apply because stale platform project name(s) match target app project(s): %s", strings.Join(collisions, ", "))
		}
		applyNames := cleanupStaleProjectNamesForApply(result.StalePlatformRecords)
		if len(applyNames) == 0 {
			result.Applied = true
			result.Actions = append(result.Actions, cleanupAction{Kind: "dokploy_metadata", Ref: strings.Join(defaultStaleDokployPlatformProjects, ","), Safety: "metadata_only", Status: "noop", Message: "no empty zero-domain stale Dokploy platform project records are ready to delete"})
			return writeFormattedOutput(stdout, outputPath, format, result, writeCleanupText)
		}
		client, err := lookupDokployClient(target)
		if err != nil {
			return err
		}
		applied, err := client.CleanupStalePlatformProjects(ctx, dokploy.StalePlatformCleanupOptions{
			ProjectNames: applyNames,
			ProjectIDs:   cleanupStaleProjectIDs(result.StalePlatformRecords),
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
			Message: fmt.Sprintf("deleted %d stale empty zero-domain Dokploy platform project record(s); backup written before delete", len(applied.Deleted)),
		})
	}

	return writeFormattedOutput(stdout, outputPath, format, result, writeCleanupText)
}

func runCleanupPurge(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cleanup purge", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var runRef string
	var target string
	var format string
	var outputPath string
	var apply bool
	var backupDir string
	var confirm string
	var allApps bool
	var includePlatform bool
	var apps stringListFlag
	var projects stringListFlag
	var sourceDirs stringListFlag

	fs.StringVar(&runRef, "run", "", "run name under .bort/runs, or a run directory path")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&format, "format", "text", "output format: text, json")
	fs.StringVar(&outputPath, "output", "-", "output path, or - for stdout")
	fs.BoolVar(&apply, "apply", false, "remove eligible source containers and networks, or verify all resources absent when manual completion is required")
	fs.StringVar(&backupDir, "backup-dir", filepath.Join(".bort", "backups"), "directory for purge plan backups before destructive cleanup")
	fs.StringVar(&confirm, "confirm", "", "required with --apply; must equal `purge <run-name>`")
	fs.BoolVar(&allApps, "all-apps", false, "purge all non-platform apps in the run")
	fs.BoolVar(&includePlatform, "include-platform", false, "allow purge planning for platform-role apps such as coolify-proxy/source")
	fs.Var(&apps, "app", "app name to purge (repeatable or comma-separated)")
	fs.Var(&projects, "project", "source or target project name to purge (repeatable or comma-separated)")
	fs.Var(&sourceDirs, "source-dir", "extra absolute host source directory that must be absent before apply (repeatable or comma-separated)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("cleanup purge does not accept positional argument %q", fs.Arg(0))
	}
	if err := checkOutputFormat("cleanup purge", format); err != nil {
		return err
	}
	if target != "dokploy" {
		return fmt.Errorf("cleanup purge currently supports target dokploy only, got %q", target)
	}
	if allApps && (len(cleanupStringListValues(apps)) > 0 || len(cleanupStringListValues(projects)) > 0) {
		return fmt.Errorf("cleanup purge accepts either --all-apps or explicit --app/--project filters, not both")
	}

	resolvedRunRef, err := resolveRunRef(runRef, false)
	if err != nil {
		return err
	}
	var operationLock *applyLock
	if apply {
		operationLock, err = acquireRunOperationLock(resolvedRunRef)
		if err != nil {
			return fmt.Errorf("cleanup purge run %q: %w", resolvedRunRef, err)
		}
		defer operationLock.Release()
	}
	run, err := loadMigrationRun(resolvedRunRef)
	if err != nil {
		return err
	}
	filters := cleanupPurgeFilters{
		Apps:            cleanupStringListValues(apps),
		Projects:        cleanupStringListValues(projects),
		AllApps:         allApps,
		IncludePlatform: includePlatform,
	}
	result, err := planCleanupPurge(run, target, filters, cleanupStringListValues(sourceDirs))
	if err != nil {
		return err
	}
	if flagSet(fs, "backup-dir") {
		result.BackupDir = backupDir
	}
	result.DryRun = !apply
	if !apply {
		return writeFormattedOutput(stdout, outputPath, format, result, writeCleanupPurgeText)
	}

	if err := validateCleanupPurgeApplyScope(filters); err != nil {
		return err
	}
	if len(result.SourceContainers) == 0 && len(cleanupPurgeVolumeChecks(result.SourceVolumes)) == 0 && len(result.SourceNetworks) == 0 && len(result.SourcePaths) == 0 {
		return fmt.Errorf("cleanup purge --apply found no selected source resources to remove")
	}
	selectedApps, err := cleanupPurgeSelectedApps(run, filters)
	if err != nil {
		return err
	}
	if err := requireLiveApplySucceededForAppsSkipping(run, cleanupSelectedAppNames(selectedApps), cleanupPurgeLiveApplySkipKinds); err != nil {
		return fmt.Errorf("refusing cleanup purge --apply before a successful live apply: %w", err)
	}
	if run.Run.CommittedAt == nil {
		return fmt.Errorf("refusing cleanup purge --apply before `%s`; the source is still the rollback path", runScopedCommand(run, "commit --apply"))
	}
	client := &dokploy.Client{}
	identified, err := client.IdentifySourcePurgeResources(ctx, cleanupPurgeOptions(result))
	if err != nil {
		return fmt.Errorf("identify selected source resources before confirmation: %w", err)
	}
	applyCleanupPurgeIdentities(&result, identified)
	if err := confirmCleanupPurgeApply(stdin, stderr, run.Run.Name, confirm); err != nil {
		return err
	}
	backupPath, err := writeCleanupPurgeBackup(backupDir, result)
	if err != nil {
		return err
	}
	result.BackupPath = backupPath
	recorder := cleanupPurgeBackupRecorder{path: backupPath, result: &result, persist: updateCleanupPurgeBackup}
	if err := recorder.Start(); err != nil {
		return fmt.Errorf("record source purge execution start in private backup %s: %w", backupPath, err)
	}
	options := cleanupPurgeOptions(result)
	options.OnProgress = recorder.Record
	purged, err := client.PurgeSourceResources(ctx, options)
	if err != nil {
		result.DryRun = false
		result.PurgeResult = &purged
		result.Warnings = append(result.Warnings, "source purge stopped before completion; earlier resources may already have been removed")
		result.Actions = append(result.Actions, cleanupAction{
			Kind:    "source_purge",
			Ref:     cleanupPurgeScopeLabel(filters),
			Safety:  "destructive_confirmed",
			Status:  "error",
			Message: "purge stopped after partial execution; inspect the per-resource results and private backup before retrying",
		})
		purgeErr := fmt.Errorf("source purge stopped after partial execution; inspect the reported results and backup %s before retrying: %w", backupPath, err)
		if backupErr := recorder.Persist(); backupErr != nil {
			result.Warnings = append(result.Warnings, "private purge backup could not be updated with the partial resource results")
			purgeErr = fmt.Errorf("%v; update private purge backup: %w", purgeErr, backupErr)
		}
		if outputErr := writeFormattedOutput(stdout, outputPath, format, result, writeCleanupPurgeText); outputErr != nil {
			return fmt.Errorf("%v; write partial purge results: %w", purgeErr, outputErr)
		}
		return purgeErr
	}
	result.Applied = true
	result.DryRun = false
	result.PurgeResult = &purged
	purgeSafety := "destructive_confirmed"
	if result.ManualCompletion {
		purgeSafety = "manual_absence_verified"
	}
	result.Actions = append(result.Actions, cleanupAction{
		Kind:    "source_purge",
		Ref:     cleanupPurgeScopeLabel(filters),
		Safety:  purgeSafety,
		Status:  "applied",
		Message: "removed eligible source containers and networks, or verified all selected resources absent when manual completion was required, after writing a private purge plan backup",
	})
	if err := recorder.Persist(); err != nil {
		result.Warnings = append(result.Warnings, "source purge completed, but the private purge backup could not be updated with the resource results")
		purgeErr := fmt.Errorf("source purge completed and initial backup is available at %s, but its resource results could not be persisted: %w", backupPath, err)
		if outputErr := writeFormattedOutput(stdout, outputPath, format, result, writeCleanupPurgeText); outputErr != nil {
			return fmt.Errorf("%v; write completed purge results: %w", purgeErr, outputErr)
		}
		return purgeErr
	}
	if result.CompletesLifecycle {
		if err := markRunPurgedLocked(run.Run); err != nil {
			result.Warnings = append(result.Warnings, "source purge completed, but migration lifecycle completion was not recorded")
			result.Actions = append(result.Actions, cleanupAction{Kind: "lifecycle", Ref: run.Run.Name, Safety: "metadata", Status: "error", Message: "source resources were purged, but PurgedAt could not be persisted"})
			purgeErr := fmt.Errorf("source purge completed and backup is available at %s, but migration lifecycle completion could not be recorded: %w", backupPath, err)
			if backupErr := recorder.Persist(); backupErr != nil {
				result.Warnings = append(result.Warnings, "private purge backup could not be updated with the completed resource results")
				purgeErr = fmt.Errorf("%v; update private purge backup: %w", purgeErr, backupErr)
			}
			if outputErr := writeFormattedOutput(stdout, outputPath, format, result, writeCleanupPurgeText); outputErr != nil {
				return fmt.Errorf("%v; write completed purge results: %w", purgeErr, outputErr)
			}
			return purgeErr
		}
		result.LifecycleCompleted = true
		if err := recorder.Persist(); err != nil {
			result.Warnings = append(result.Warnings, "migration lifecycle completion was recorded, but the private purge backup could not be updated with that outcome")
			purgeErr := fmt.Errorf("migration lifecycle completion was recorded, but backup %s could not be finalized: %w", backupPath, err)
			if outputErr := writeFormattedOutput(stdout, outputPath, format, result, writeCleanupPurgeText); outputErr != nil {
				return fmt.Errorf("%v; write completed purge results: %w", purgeErr, outputErr)
			}
			return purgeErr
		}
	}
	return writeFormattedOutput(stdout, outputPath, format, result, writeCleanupPurgeText)
}

func planCleanupPurge(run loadedMigrationRun, target string, filters cleanupPurgeFilters, extraSourceDirs []string) (cleanupPurgeResult, error) {
	selected, err := cleanupPurgeSelectedApps(run, filters)
	if err != nil {
		return cleanupPurgeResult{}, err
	}
	selectedNames := cleanupSelectedAppNames(selected)
	owners, err := cleanupPurgeResourceOwners(run)
	if err != nil {
		return cleanupPurgeResult{}, err
	}
	result := cleanupPurgeResult{
		APIVersion:         cleanupPurgeAPIVersion,
		RunName:            run.Run.Name,
		RunDir:             run.Run.RunDir,
		Target:             target,
		DryRun:             true,
		CompletesLifecycle: cleanupPurgeCoversAllRunApps(run, filters),
		Filters:            filters,
	}

	for _, app := range selected {
		if control := cleanupSourceControlForApp(app); control != nil {
			control.Action = "credentials_not_touched_by_source_purge"
			result.SourceControls = append(result.SourceControls, *control)
		}
		for _, unresolved := range cleanupUnresolvedContainersForApp(app) {
			result.CompletesLifecycle = false
			result.Warnings = append(result.Warnings, fmt.Sprintf("source container for %s/%s has no stable container ID and is not scheduled for purge", app.Name, unresolved))
		}
		containers, err := cleanupContainersForApp(app)
		if err != nil {
			return cleanupPurgeResult{}, err
		}
		for _, container := range containers {
			if strings.TrimSpace(container.ContainerID) == "" {
				continue
			}
			if sharedWith := cleanupContainerOwnersOutsideSelected(owners, container, selectedNames); len(sharedWith) > 0 {
				result.CompletesLifecycle = false
				result.Warnings = append(result.Warnings, fmt.Sprintf("source container %s for %s is also referenced by unselected app(s): %s", firstCleanupValue(container.ContainerName, container.ContainerID), app.Name, strings.Join(sharedWith, ", ")))
				continue
			}
			container.Action = "remove_on_purge_apply"
			result.SourceContainers = append(result.SourceContainers, container)
		}
		for _, volume := range cleanupVolumesForApp(app) {
			switch volume.Type {
			case "volume":
				if strings.TrimSpace(volume.Name) == "" {
					result.CompletesLifecycle = false
					volume.Action = "inspect_manually_before_purge"
					result.Warnings = append(result.Warnings, fmt.Sprintf("named volume for %s/%s has no recorded name and is not scheduled for purge", app.Name, volume.Service))
				} else if sharedWith := cleanupOwnersOutsideSelected(owners.NamedVolumes[volume.Name], selectedNames); len(sharedWith) > 0 {
					result.CompletesLifecycle = false
					volume.Action = "preserve_shared_with_unselected_app"
					result.Warnings = append(result.Warnings, fmt.Sprintf("named volume %s for %s is also referenced by unselected app(s): %s", volume.Name, app.Name, strings.Join(sharedWith, ", ")))
				} else {
					volume.Action = "require_absent_before_purge_apply"
					result.Warnings = append(result.Warnings, fmt.Sprintf("named volume %s for %s must be removed manually before cleanup purge --apply; Bort will verify it remains absent", volume.Name, app.Name))
				}
			case "bind":
				volume.Action = "require_absent_before_purge_apply"
				path, warning := cleanupSourcePathForBindMount(volume, filters.IncludePlatform)
				if warning != "" {
					result.CompletesLifecycle = false
					result.Warnings = append(result.Warnings, warning)
				} else if path == nil {
					result.CompletesLifecycle = false
					volume.Action = "inspect_manually_before_purge"
				} else if sharedWith := cleanupPathOwnersOutsideSelected(owners.BindPaths, path.Path, selectedNames); len(sharedWith) > 0 {
					result.CompletesLifecycle = false
					volume.Action = "preserve_shared_with_unselected_app"
					result.Warnings = append(result.Warnings, fmt.Sprintf("bind mount source %s for %s is also referenced by unselected app(s): %s", path.Path, app.Name, strings.Join(sharedWith, ", ")))
				} else {
					path.Action = "require_absent_before_purge_apply"
					result.SourcePaths = append(result.SourcePaths, *path)
					result.Warnings = append(result.Warnings, fmt.Sprintf("bind mount source %s for %s must be removed manually before cleanup purge --apply; Bort will verify it remains absent", path.Path, app.Name))
				}
			default:
				result.CompletesLifecycle = false
				volume.Action = "inspect_manually_before_purge"
			}
			result.SourceVolumes = append(result.SourceVolumes, volume)
		}
		networks, err := cleanupNetworksForApp(run, app)
		if err != nil {
			return cleanupPurgeResult{}, fmt.Errorf("inspect source networks for %s before purge: %w", app.Name, err)
		}
		for _, network := range networks {
			if dokploy.IsProtectedSourcePurgeNetwork(network.Name) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("source network %s for %s is a Docker built-in network and is not scheduled for purge", network.Name, app.Name))
				continue
			}
			if cleanupPlatformNetwork(network.Name) && !filters.IncludePlatform {
				result.CompletesLifecycle = false
				result.Warnings = append(result.Warnings, fmt.Sprintf("source network %s for %s is a platform network and requires --include-platform for purge", network.Name, app.Name))
				continue
			}
			if sharedWith := cleanupOwnersOutsideSelected(owners.Networks[network.Name], selectedNames); len(sharedWith) > 0 {
				result.CompletesLifecycle = false
				result.Warnings = append(result.Warnings, fmt.Sprintf("source network %s for %s is also referenced by unselected app(s): %s", network.Name, app.Name, strings.Join(sharedWith, ", ")))
				continue
			}
			network.Action = "remove_on_purge_apply"
			result.SourceNetworks = append(result.SourceNetworks, network)
		}
	}
	for _, path := range extraSourceDirs {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		cleaned := filepath.Clean(path)
		if err := dokploy.ValidateSourcePurgePath(cleaned, filters.IncludePlatform); err != nil {
			return cleanupPurgeResult{}, err
		}
		if sharedWith := cleanupPathOwnersOutsideSelected(owners.BindPaths, cleaned, selectedNames); len(sharedWith) > 0 {
			return cleanupPurgeResult{}, fmt.Errorf("refusing source-dir %s because it overlaps bind mount(s) for unselected app(s): %s", cleaned, strings.Join(sharedWith, ", "))
		}
		result.SourcePaths = append(result.SourcePaths, cleanupSourcePath{Path: cleaned, Source: "explicit", AllowPlatform: filters.IncludePlatform, Action: "require_absent_before_purge_apply"})
		result.Warnings = append(result.Warnings, fmt.Sprintf("source directory %s must be removed manually before cleanup purge --apply; Bort will verify it remains absent", cleaned))
	}
	if filters.AllApps && !cleanupPurgeCoversAllRunApps(run, filters) {
		result.Warnings = append(result.Warnings, "--all-apps excludes platform-role apps unless --include-platform is set; this purge will not complete the migration lifecycle")
	}
	if len(cleanupPurgeVolumeChecks(result.SourceVolumes)) > 0 || len(result.SourcePaths) > 0 {
		setCleanupPurgeManualCompletion(&result)
	}
	if filters.AllApps && !result.CompletesLifecycle {
		result.Warnings = append(result.Warnings, "the purge plan does not cover every removable source resource; successful apply will not complete the migration lifecycle")
	}
	result.Warnings = append(result.Warnings, "source-control credentials and target Dokploy artifacts are not removed by cleanup purge")
	result.Warnings = append(result.Warnings, "Docker images are not removed by cleanup purge; remove unused images separately after confirming they are not shared")
	sortCleanupPurgeResult(&result)
	addCleanupPurgeActions(&result)
	return result, nil
}

type cleanupPurgeOwners struct {
	ContainerIDs   map[string][]string
	ContainerNames map[string][]string
	NamedVolumes   map[string][]string
	BindPaths      map[string][]string
	Networks       map[string][]string
}

func cleanupSelectedAppNames(apps []preparer.AppPlan) map[string]struct{} {
	names := map[string]struct{}{}
	for _, app := range apps {
		if strings.TrimSpace(app.Name) != "" {
			names[app.Name] = struct{}{}
		}
	}
	return names
}

func cleanupPurgeResourceOwners(run loadedMigrationRun) (cleanupPurgeOwners, error) {
	owners := cleanupPurgeOwners{
		ContainerIDs:   map[string][]string{},
		ContainerNames: map[string][]string{},
		NamedVolumes:   map[string][]string{},
		BindPaths:      map[string][]string{},
		Networks:       map[string][]string{},
	}
	for _, app := range run.Prepare.Apps {
		appName := strings.TrimSpace(app.Name)
		if appName == "" {
			continue
		}
		containers, err := cleanupContainersForApp(app)
		if err != nil {
			return cleanupPurgeOwners{}, err
		}
		for _, container := range containers {
			if containerID := strings.TrimSpace(container.ContainerID); containerID != "" {
				owners.ContainerIDs[containerID] = append(owners.ContainerIDs[containerID], appName)
			}
			if containerName := cleanupContainerName(container.ContainerName); containerName != "" {
				owners.ContainerNames[containerName] = append(owners.ContainerNames[containerName], appName)
			}
		}
		for _, volume := range cleanupVolumesForApp(app) {
			switch volume.Type {
			case "volume":
				if strings.TrimSpace(volume.Name) != "" {
					owners.NamedVolumes[volume.Name] = append(owners.NamedVolumes[volume.Name], appName)
				}
			case "bind":
				if strings.TrimSpace(volume.Source) != "" {
					path := filepath.Clean(volume.Source)
					owners.BindPaths[path] = append(owners.BindPaths[path], appName)
				}
			}
		}
		networks, err := cleanupNetworksForApp(run, app)
		if err != nil {
			return cleanupPurgeOwners{}, fmt.Errorf("inspect source networks for %s before purge: %w", app.Name, err)
		}
		for _, network := range networks {
			if strings.TrimSpace(network.Name) != "" {
				owners.Networks[network.Name] = append(owners.Networks[network.Name], appName)
			}
		}
	}
	return owners, nil
}

func cleanupContainerOwnersOutsideSelected(owners cleanupPurgeOwners, container cleanupSourceContainer, selected map[string]struct{}) []string {
	containerOwners := []string{}
	if containerID := strings.TrimSpace(container.ContainerID); containerID != "" {
		for ownedID, ownedBy := range owners.ContainerIDs {
			if cleanupContainerIDsMatch(containerID, ownedID) {
				containerOwners = append(containerOwners, ownedBy...)
			}
		}
	}
	if containerName := cleanupContainerName(container.ContainerName); containerName != "" {
		containerOwners = append(containerOwners, owners.ContainerNames[containerName]...)
	}
	return cleanupOwnersOutsideSelected(containerOwners, selected)
}

func cleanupContainerIDsMatch(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return a != ""
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 12 && strings.HasPrefix(b, a)
}

func cleanupContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func cleanupOwnersOutsideSelected(owners []string, selected map[string]struct{}) []string {
	outside := []string{}
	seen := map[string]struct{}{}
	for _, owner := range owners {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		if _, ok := selected[owner]; ok {
			continue
		}
		if _, ok := seen[owner]; ok {
			continue
		}
		seen[owner] = struct{}{}
		outside = append(outside, owner)
	}
	sort.Strings(outside)
	return outside
}

func cleanupPathOwnersOutsideSelected(pathOwners map[string][]string, path string, selected map[string]struct{}) []string {
	owners := []string{}
	for ownedPath, pathOwners := range pathOwners {
		if cleanupPathsOverlap(ownedPath, path) {
			owners = append(owners, pathOwners...)
		}
	}
	return cleanupOwnersOutsideSelected(owners, selected)
}

func cleanupPathsOverlap(a, b string) bool {
	a = filepath.Clean(strings.TrimSpace(a))
	b = filepath.Clean(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return cleanupPathContains(a, b) || cleanupPathContains(b, a)
}

func cleanupPathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != "" && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func cleanupPurgeSelectedApps(run loadedMigrationRun, filters cleanupPurgeFilters) ([]preparer.AppPlan, error) {
	appFilters := cleanupStringListValues(filters.Apps)
	projectFilters := cleanupStringListValues(filters.Projects)
	filterActive := len(appFilters) > 0 || len(projectFilters) > 0
	matchedApps := map[string][]string{}
	matchedProjects := map[string]bool{}
	selected := []preparer.AppPlan{}
	for _, app := range run.Prepare.Apps {
		if isPlatformRunApp(app.Role) && !filters.IncludePlatform {
			continue
		}
		appMatch := false
		projectMatch := false
		for _, name := range appFilters {
			if cleanupPurgeAppMatches(app, name) {
				appMatch = true
				matchedApps[name] = append(matchedApps[name], app.Name)
			}
		}
		for _, name := range projectFilters {
			if cleanupPurgeProjectMatches(app, name) {
				projectMatch = true
				matchedProjects[name] = true
			}
		}
		if filters.AllApps || !filterActive || appMatch || projectMatch {
			selected = append(selected, app)
		}
	}
	missing := []string{}
	for _, name := range appFilters {
		if len(matchedApps[name]) == 0 {
			missing = append(missing, "app="+name)
		}
	}
	for _, name := range projectFilters {
		if !matchedProjects[name] {
			missing = append(missing, "project="+name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cleanup purge filter(s) matched no selected app: %s", strings.Join(missing, ", "))
	}
	for _, name := range appFilters {
		matches := matchedApps[name]
		if len(matches) > 1 {
			sort.Strings(matches)
			return nil, fmt.Errorf("cleanup purge --app %q is ambiguous; it matches: %s", name, strings.Join(matches, ", "))
		}
	}
	return selected, nil
}

func cleanupPurgeCoversAllRunApps(run loadedMigrationRun, filters cleanupPurgeFilters) bool {
	if !filters.AllApps {
		return false
	}
	if filters.IncludePlatform {
		return true
	}
	for _, app := range run.Prepare.Apps {
		if isPlatformRunApp(app.Role) {
			return false
		}
	}
	return true
}

func cleanupPlatformNetwork(name string) bool {
	switch strings.TrimSpace(name) {
	case "coolify", "coolify-proxy", "proxy":
		return true
	default:
		return false
	}
}

func cleanupPurgeAppMatches(app preparer.AppPlan, name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return false
	}
	for _, candidate := range []string{app.Name, app.Directory, planutil.Slug(app.Name)} {
		if strings.ToLower(strings.TrimSpace(candidate)) == want {
			return true
		}
	}
	return false
}

func cleanupPurgeProjectMatches(app preparer.AppPlan, name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return false
	}
	candidates := []string{}
	if app.ProjectGroup != nil {
		candidates = append(candidates, app.ProjectGroup.Name, app.ProjectGroup.Source)
	}
	if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
		candidates = append(candidates, app.TargetResources.Dokploy.Project.Name)
	}
	for _, candidate := range candidates {
		if strings.ToLower(strings.TrimSpace(candidate)) == want {
			return true
		}
	}
	return false
}

func cleanupSourcePathForBindMount(volume cleanupSourceVolume, allowPlatform bool) (*cleanupSourcePath, string) {
	path := strings.TrimSpace(volume.Source)
	if path == "" {
		return nil, fmt.Sprintf("bind mount for %s/%s -> %s has no host source path; not scheduled for purge", volume.App, volume.Service, volume.Target)
	}
	cleaned := filepath.Clean(path)
	if err := dokploy.ValidateSourcePurgePath(cleaned, allowPlatform); err != nil {
		return nil, fmt.Sprintf("bind mount source %s for %s/%s not scheduled for purge: %v", cleaned, volume.App, volume.Service, err)
	}
	return &cleanupSourcePath{App: volume.App, Path: cleaned, Source: "bind_mount", AllowPlatform: allowPlatform, Action: "require_absent_before_purge_apply"}, ""
}

func validateCleanupPurgeApplyScope(filters cleanupPurgeFilters) error {
	if filters.AllApps || len(filters.Apps) > 0 || len(filters.Projects) > 0 {
		return nil
	}
	return fmt.Errorf("cleanup purge --apply requires an explicit scope: pass --app, --project, or --all-apps")
}

func confirmCleanupPurgeApply(stdin io.Reader, stderr io.Writer, runName, provided string) error {
	expected := cleanupPurgeConfirmPhrase(runName)
	provided = strings.TrimSpace(provided)
	if provided == expected {
		return nil
	}
	if provided != "" {
		return fmt.Errorf("cleanup purge --apply confirmation mismatch: pass --confirm %q", expected)
	}
	if stdin != nil && stdinIsTerminal(stdin) {
		fmt.Fprintf(stderr, "cleanup purge is destructive. Type %q to continue: ", expected)
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return err
		}
		if strings.TrimSpace(line) == expected {
			return nil
		}
	}
	return fmt.Errorf("cleanup purge --apply requires --confirm %q", expected)
}

func cleanupPurgeConfirmPhrase(runName string) string {
	return "purge " + strings.TrimSpace(runName)
}

func writeCleanupPurgeBackup(backupDir string, result cleanupPurgeResult) (string, error) {
	backupDir = strings.TrimSpace(backupDir)
	if backupDir == "" {
		backupDir = filepath.Join(".bort", "backups")
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(backupDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(backupDir, fmt.Sprintf("source-purge-%s-%s.json", cleanupBackupFilePart(result.RunName), time.Now().UTC().Format("20060102-150405.000000000")))
	return path, updateCleanupPurgeBackup(path, result)
}

func updateCleanupPurgeBackup(path string, result cleanupPurgeResult) error {
	result.BackupPath = path
	return writeJSONArtifact(path, result)
}

type cleanupPurgeBackupRecorder struct {
	path    string
	result  *cleanupPurgeResult
	persist func(string, cleanupPurgeResult) error
	err     error
}

func (r *cleanupPurgeBackupRecorder) Start() error {
	now := time.Now().UTC()
	r.result.ExecutionStartedAt = &now
	return r.Persist()
}

func (r *cleanupPurgeBackupRecorder) Record(progress dokploy.SourcePurgeResult) error {
	r.result.PurgeResult = &progress
	return r.Persist()
}

func (r *cleanupPurgeBackupRecorder) Persist() error {
	if r.err != nil {
		return r.err
	}
	if err := r.persist(r.path, *r.result); err != nil {
		r.err = err
		return err
	}
	return nil
}

func cleanupBackupFilePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "run"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	part := strings.Trim(builder.String(), "-")
	if part == "" {
		return "run"
	}
	return part
}

func cleanupPurgeOptions(result cleanupPurgeResult) dokploy.SourcePurgeOptions {
	opts := dokploy.SourcePurgeOptions{}
	for _, container := range result.SourceContainers {
		opts.Containers = append(opts.Containers, dokploy.SourcePurgeContainer{
			App:           container.App,
			Service:       container.Service,
			ContainerID:   container.ContainerID,
			ContainerName: container.ContainerName,
		})
	}
	for _, volume := range cleanupPurgeVolumeChecks(result.SourceVolumes) {
		opts.Volumes = append(opts.Volumes, dokploy.SourcePurgeVolume{App: volume.App, Service: volume.Service, Name: volume.Name, ExpectedAbsent: volume.ExpectedAbsent})
	}
	for _, network := range result.SourceNetworks {
		opts.Networks = append(opts.Networks, dokploy.SourcePurgeNetwork{App: network.App, Name: network.Name, ExpectedIdentity: network.ExpectedIdentity, ExpectedAbsent: network.ExpectedAbsent})
	}
	for _, path := range result.SourcePaths {
		opts.Paths = append(opts.Paths, dokploy.SourcePurgePath{App: path.App, Source: path.Source, Path: path.Path, AllowPlatform: path.AllowPlatform, ExpectedAbsent: path.ExpectedAbsent})
	}
	return opts
}

func applyCleanupPurgeIdentities(result *cleanupPurgeResult, identified dokploy.SourcePurgeOptions) {
	result.SourceContainers = nil
	for _, container := range identified.Containers {
		result.SourceContainers = append(result.SourceContainers, cleanupSourceContainer{
			App:           container.App,
			Service:       container.Service,
			ContainerID:   container.ContainerID,
			ContainerName: container.ContainerName,
			Action:        "remove_on_purge_apply",
		})
	}
	volumes := map[string]dokploy.SourcePurgeVolume{}
	for _, volume := range identified.Volumes {
		volumes[volume.Name] = volume
	}
	for i := range result.SourceVolumes {
		if volume, ok := volumes[result.SourceVolumes[i].Name]; ok {
			result.SourceVolumes[i].ExpectedAbsent = volume.ExpectedAbsent
		}
	}
	networks := map[string]dokploy.SourcePurgeNetwork{}
	for _, network := range identified.Networks {
		networks[network.Name] = network
	}
	for i := range result.SourceNetworks {
		if network, ok := networks[result.SourceNetworks[i].Name]; ok {
			result.SourceNetworks[i].ExpectedIdentity = network.ExpectedIdentity
			result.SourceNetworks[i].ExpectedAbsent = network.ExpectedAbsent
		}
	}
	paths := map[string]dokploy.SourcePurgePath{}
	for _, path := range identified.Paths {
		paths[path.Path] = path
	}
	for i := range result.SourcePaths {
		if path, ok := paths[result.SourcePaths[i].Path]; ok {
			result.SourcePaths[i].ExpectedAbsent = path.ExpectedAbsent
		}
	}
	manualCompletion := len(identified.Volumes) > 0 || len(identified.Paths) > 0
	for _, network := range identified.Networks {
		manualCompletion = manualCompletion || network.ExpectedAbsent
	}
	if manualCompletion {
		setCleanupPurgeManualCompletion(result)
	}
}

func setCleanupPurgeManualCompletion(result *cleanupPurgeResult) {
	result.ManualCompletion = true
	result.CompletesLifecycle = false
	for i := range result.SourceContainers {
		result.SourceContainers[i].Action = "require_absent_before_purge_apply"
	}
	for i := range result.SourceNetworks {
		result.SourceNetworks[i].Action = "require_absent_before_purge_apply"
	}
	for i := range result.Actions {
		switch result.Actions[i].Kind {
		case "source_containers", "source_networks":
			result.Actions[i].Safety = "manual_removal_required"
			result.Actions[i].Status = "prerequisite"
			result.Actions[i].Message = "selected resources must be removed manually because this scope contains absence-only prerequisites"
		}
	}
	warning := "this purge has absence-only prerequisites, so Bort will not automatically delete any selected resource in this scope; remove the listed containers and networks manually too, then rerun apply to verify everything remains absent. Because external recreation cannot be locked out, verification-only purges do not record lifecycle completion"
	for _, existing := range result.Warnings {
		if existing == warning {
			return
		}
	}
	result.Warnings = append(result.Warnings, warning)
}

func cleanupPurgeVolumeChecks(volumes []cleanupSourceVolume) []cleanupSourceVolume {
	named := []cleanupSourceVolume{}
	for _, volume := range volumes {
		if volume.Type == "volume" && strings.TrimSpace(volume.Name) != "" && volume.Action == "require_absent_before_purge_apply" {
			named = append(named, volume)
		}
	}
	return named
}

func addCleanupPurgeActions(result *cleanupPurgeResult) {
	if len(result.SourceContainers) > 0 {
		action := cleanupAction{Kind: "source_containers", Ref: "run", Safety: "destructive_confirm_required", Status: "planned", Message: "selected source containers will be removed only by cleanup purge --apply"}
		if result.ManualCompletion {
			action.Safety = "manual_removal_required"
			action.Status = "prerequisite"
			action.Message = "selected source containers must be removed manually because this scope contains absence-only prerequisites"
		}
		result.Actions = append(result.Actions, action)
	}
	if len(cleanupPurgeVolumeChecks(result.SourceVolumes)) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "source_named_volumes", Ref: "run", Safety: "manual_removal_required", Status: "prerequisite", Message: "selected source named volumes must be absent before cleanup purge --apply and are rechecked after confirmation"})
	}
	if len(result.SourcePaths) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "source_paths", Ref: "run", Safety: "manual_removal_required", Status: "prerequisite", Message: "selected host source paths must be absent before cleanup purge --apply and are rechecked after confirmation"})
	}
	if len(result.SourceNetworks) > 0 {
		action := cleanupAction{Kind: "source_networks", Ref: "run", Safety: "destructive_confirm_required", Status: "planned", Message: "selected source networks will be removed only by cleanup purge --apply"}
		if result.ManualCompletion {
			action.Safety = "manual_removal_required"
			action.Status = "prerequisite"
			action.Message = "selected source networks must be removed manually because this scope contains absence-only prerequisites"
		}
		result.Actions = append(result.Actions, action)
	}
	if len(result.SourceControls) > 0 {
		result.Actions = append(result.Actions, cleanupAction{Kind: "source_control", Ref: "run", Safety: "credentials_not_touched", Status: "preserved", Message: "source-control credentials are not copied, revoked, or removed by cleanup purge"})
	}
	result.Actions = append(result.Actions, cleanupAction{Kind: "target_artifacts", Ref: "run", Safety: "preserve_target", Status: "preserved", Message: "Dokploy target projects, compose apps, domains, and volumes are not removed by cleanup purge"})
}

func sortCleanupPurgeResult(result *cleanupPurgeResult) {
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
	sort.Slice(result.SourcePaths, func(i, j int) bool {
		return cleanupSortKey(result.SourcePaths[i].App, result.SourcePaths[i].Path) < cleanupSortKey(result.SourcePaths[j].App, result.SourcePaths[j].Path)
	})
	sort.Strings(result.Warnings)
}

func cleanupPurgeScopeLabel(filters cleanupPurgeFilters) string {
	if filters.AllApps {
		return "all-apps"
	}
	parts := []string{}
	for _, app := range filters.Apps {
		parts = append(parts, "app="+app)
	}
	for _, project := range filters.Projects {
		parts = append(parts, "project="+project)
	}
	if len(parts) == 0 {
		return "run"
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (f *stringListFlag) Set(value string) error {
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			*f = append(*f, item)
		}
	}
	return nil
}

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func cleanupStringListValues(values []string) []string {
	seen := map[string]struct{}{}
	cleaned := []string{}
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key := strings.ToLower(item)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			cleaned = append(cleaned, item)
		}
	}
	sort.Strings(cleaned)
	return cleaned
}

func loadCleanupRun(runRef string, allowLatest bool) (loadedMigrationRun, error) {
	resolved, err := resolveRunRef(runRef, allowLatest)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	return loadMigrationRun(resolved)
}

func cleanupStaleProjectNameCollisions(run loadedMigrationRun, names []string) []string {
	staleNames := map[string]struct{}{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			staleNames[name] = struct{}{}
		}
	}
	if len(staleNames) == 0 {
		return nil
	}
	collisions := []string{}
	seen := map[string]struct{}{}
	for _, app := range run.Prepare.Apps {
		if app.TargetResources == nil || app.TargetResources.Dokploy == nil {
			continue
		}
		name := strings.TrimSpace(app.TargetResources.Dokploy.Project.Name)
		if _, ok := staleNames[name]; !ok {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		collisions = append(collisions, name)
	}
	sort.Strings(collisions)
	return collisions
}

func cleanupStaleProjectIDs(records []cleanupStalePlatformRecord) map[string]string {
	ids := map[string]string{}
	for _, record := range records {
		if record.Status != "present" || strings.TrimSpace(record.ProjectID) == "" {
			continue
		}
		ids[record.Name] = record.ProjectID
	}
	return ids
}

func cleanupStaleProjectNamesForApply(records []cleanupStalePlatformRecord) []string {
	names := []string{}
	for _, record := range records {
		if record.Status == "present" && strings.TrimSpace(record.ProjectID) != "" {
			names = append(names, record.Name)
		}
	}
	sort.Strings(names)
	return names
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
		containers, err := cleanupContainersForApp(app)
		if err != nil {
			result.Warnings = append(result.Warnings, err.Error())
		} else {
			result.SourceContainers = append(result.SourceContainers, containers...)
		}
		result.SourceVolumes = append(result.SourceVolumes, cleanupVolumesForApp(app)...)
		networks, err := cleanupNetworksForApp(run, app)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("inspect source networks for %s: %v", app.Name, err))
		} else {
			result.SourceNetworks = append(result.SourceNetworks, networks...)
		}
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
		records = append(records, cleanupStalePlatformRecord{Name: name, Status: "unknown", Message: "will verify the project is empty and has zero domains against Dokploy DB before deleting"})
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
			records[i].Message = "live inspection was incomplete; cleanup --apply will recheck empty project and zero-domain conditions in the Dokploy DB before deleting"
			continue
		}
		if records[i].DomainCount > 0 {
			records[i].Status = "blocked"
			records[i].Message = fmt.Sprintf("refusing automatic metadata cleanup because %d domain(s) are attached", records[i].DomainCount)
			continue
		}
		if len(records[i].ComposeIDs) > 0 {
			records[i].Status = "blocked"
			records[i].Message = fmt.Sprintf("refusing automatic metadata cleanup because %d compose app record(s) are still attached", len(records[i].ComposeIDs))
			continue
		}
		records[i].Status = "present"
		records[i].Message = "safe metadata cleanup candidate: empty project with zero domains visible"
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

func cleanupContainersForApp(app preparer.AppPlan) ([]cleanupSourceContainer, error) {
	containers := []cleanupSourceContainer{}
	seenIDs := map[string]int{}
	seenNames := map[string]int{}
	idsByName := map[string]string{}
	add := func(service, containerID, containerName string) error {
		service = strings.TrimSpace(service)
		containerID = strings.TrimSpace(containerID)
		containerName = cleanupContainerName(containerName)
		if containerID == "" && containerName == "" {
			return nil
		}
		if containerID != "" {
			if existingID, ok := idsByName[containerName]; containerName != "" && ok {
				if !cleanupContainerIDsMatch(existingID, containerID) {
					return fmt.Errorf("source container name %q for app %s refers to conflicting container IDs %q and %q", containerName, app.Name, existingID, containerID)
				}
				if len(existingID) > len(containerID) {
					containerID = existingID
				}
			}
			if containerName != "" {
				idsByName[containerName] = containerID
			}
			index, ok := seenIDs[containerID]
			matchedID := containerID
			if !ok {
				for seenID, seenIndex := range seenIDs {
					if cleanupContainerIDsMatch(seenID, containerID) {
						index, ok, matchedID = seenIndex, true, seenID
						break
					}
				}
			}
			if ok {
				existing := containers[index]
				if existing.ContainerName != "" && containerName != "" && existing.ContainerName != containerName {
					return fmt.Errorf("source container ID %q for app %s refers to conflicting names %q and %q", containerID, app.Name, existing.ContainerName, containerName)
				}
				if existing.Service != "" && service != "" && existing.Service != service {
					return fmt.Errorf("source container ID %q for app %s refers to conflicting services %q and %q", containerID, app.Name, existing.Service, service)
				}
				if existing.ContainerName == "" {
					existing.ContainerName = containerName
				}
				if existing.Service == "" {
					existing.Service = service
				}
				if len(containerID) > len(existing.ContainerID) {
					existing.ContainerID = containerID
					delete(seenIDs, matchedID)
					seenIDs[containerID] = index
				}
				if existing.ContainerName != "" {
					idsByName[existing.ContainerName] = existing.ContainerID
				}
				containers[index] = existing
				return nil
			}
		} else {
			if index, ok := seenNames[containerName]; ok {
				existing := containers[index]
				if existing.Service != "" && service != "" && existing.Service != service {
					return fmt.Errorf("source container name %q for app %s refers to conflicting services %q and %q", containerName, app.Name, existing.Service, service)
				}
				return nil
			}
		}
		index := len(containers)
		containers = append(containers, cleanupSourceContainer{
			App:           app.Name,
			Service:       service,
			ContainerID:   containerID,
			ContainerName: containerName,
			Action:        "inventory_only_cleanup_apply_does_not_remove",
		})
		if containerID != "" {
			seenIDs[containerID] = index
		} else {
			seenNames[containerName] = index
		}
		return nil
	}
	for _, service := range app.Resources.SourceServices {
		if err := add(service.ServiceName, service.ContainerID, service.ContainerName); err != nil {
			return nil, err
		}
	}
	for _, volume := range app.Resources.Volumes {
		if err := add(volume.Service, volume.SourceContainerID, volume.SourceContainerName); err != nil {
			return nil, err
		}
	}
	for _, store := range app.Resources.DataStores {
		if err := add(store.Service, store.SourceContainerID, store.SourceContainerName); err != nil {
			return nil, err
		}
	}
	return containers, nil
}

func cleanupUnresolvedContainersForApp(app preparer.AppPlan) []string {
	unresolved := []string{}
	add := func(service, containerID, containerName string) {
		if strings.TrimSpace(containerID) == "" {
			unresolved = append(unresolved, firstCleanupValue(service, "unknown-service"))
		}
	}
	for _, service := range app.Resources.SourceServices {
		add(service.ServiceName, service.ContainerID, service.ContainerName)
	}
	for _, volume := range app.Resources.Volumes {
		add(volume.Service, volume.SourceContainerID, volume.SourceContainerName)
	}
	for _, store := range app.Resources.DataStores {
		add(store.Service, store.SourceContainerID, store.SourceContainerName)
	}
	sort.Strings(unresolved)
	return unresolved
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

func cleanupNetworksForApp(run loadedMigrationRun, app preparer.AppPlan) ([]cleanupSourceNetwork, error) {
	topology, err := cleanupReadTopology(run, app)
	if err != nil {
		return nil, err
	}
	if len(topology.Networks) == 0 {
		return nil, nil
	}
	networks := make([]cleanupSourceNetwork, 0, len(topology.Networks))
	for _, network := range topology.Networks {
		if strings.TrimSpace(network) == "" {
			continue
		}
		networks = append(networks, cleanupSourceNetwork{App: app.Name, Name: network, Action: "preserve_until_manual_purge_after_acceptance"})
	}
	return networks, nil
}

func cleanupReadTopology(run loadedMigrationRun, app preparer.AppPlan) (analyzer.Topology, error) {
	bundleDir := run.Prepare.BundleDir
	appDir := filepath.Join(bundleDir, filepath.FromSlash(app.Directory))
	if err := safepath.ContainedPath(bundleDir, appDir); err != nil {
		return analyzer.Topology{}, err
	}
	path := filepath.Join(appDir, "topology.json")
	if err := safepath.ContainedPath(appDir, path); err != nil {
		return analyzer.Topology{}, err
	}
	contents, err := safepath.ReadFileNoFollow(path)
	if err != nil {
		return analyzer.Topology{}, err
	}
	var topology analyzer.Topology
	if err := json.Unmarshal(contents, &topology); err != nil {
		return analyzer.Topology{}, err
	}
	return topology, nil
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

func writeCleanupPurgeText(w io.Writer, result cleanupPurgeResult) {
	mode := "dry run"
	if !result.DryRun {
		mode = "incomplete"
	}
	if result.Applied {
		mode = "applied"
	}
	fmt.Fprintf(w, "Cleanup purge plan: %s -> %s\n", firstCleanupValue(result.RunDir, result.RunName, "run"), result.Target)
	fmt.Fprintf(w, "Mode: %s\n", mode)
	fmt.Fprintf(w, "Scope: %s\n\n", cleanupPurgeScopeLabel(result.Filters))

	if result.BackupPath != "" {
		fmt.Fprintf(w, "Purge plan backup: %s\n\n", result.BackupPath)
	}

	if len(result.SourceContainers) > 0 {
		heading := "Source containers scheduled for purge:"
		if result.ManualCompletion {
			heading = "Source containers that must be absent before apply:"
		}
		fmt.Fprintln(w, heading)
		for _, container := range result.SourceContainers {
			fmt.Fprintf(w, "  - %s/%s %s\n", container.App, container.Service, firstCleanupValue(container.ContainerName, container.ContainerID))
		}
		fmt.Fprintln(w)
	}
	if named := cleanupPurgeVolumeChecks(result.SourceVolumes); len(named) > 0 {
		fmt.Fprintln(w, "Named source volumes that must be absent before apply:")
		for _, volume := range named {
			fmt.Fprintf(w, "  - %s %s -> %s\n", volume.App, volume.Name, volume.Target)
		}
		fmt.Fprintln(w)
	}
	if len(result.SourcePaths) > 0 {
		fmt.Fprintln(w, "Host source paths that must be absent before apply:")
		for _, path := range result.SourcePaths {
			app := firstCleanupValue(path.App, "explicit")
			fmt.Fprintf(w, "  - %s %s (%s)\n", app, path.Path, path.Source)
		}
		fmt.Fprintln(w)
	}
	if len(result.SourceNetworks) > 0 {
		heading := "Source networks scheduled for purge:"
		if result.ManualCompletion {
			heading = "Source networks that must be absent before apply:"
		}
		fmt.Fprintln(w, heading)
		for _, network := range result.SourceNetworks {
			fmt.Fprintf(w, "  - %s %s\n", network.App, network.Name)
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

	if result.PurgeResult != nil {
		writeCleanupPurgeAppliedResults(w, *result.PurgeResult)
	}

	if len(result.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range result.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}

	if result.Applied {
		if result.ManualCompletion {
			if result.LifecycleCompleted {
				fmt.Fprintln(w, "Verified the manually completed source purge and completed the migration lifecycle. Target Dokploy artifacts and source-control credentials were not removed.")
			} else {
				fmt.Fprintln(w, "Verified the manually completed source purge for the selected scope. The migration lifecycle remains incomplete; target Dokploy artifacts and source-control credentials were not removed.")
			}
			return
		}
		if result.LifecycleCompleted {
			fmt.Fprintln(w, "Applied destructive source purge and completed the migration lifecycle. Target Dokploy artifacts and source-control credentials were not removed.")
		} else {
			fmt.Fprintln(w, "Applied destructive source purge for the selected scope. The migration lifecycle remains incomplete; target Dokploy artifacts and source-control credentials were not removed.")
		}
		return
	}
	if !result.DryRun {
		fmt.Fprintln(w, "Source purge stopped before completion. Earlier resources may already have been removed; inspect the results and backup before retrying.")
		return
	}
	command := cleanupPurgeApplyCommand(result)
	if command == "" {
		fmt.Fprintln(w, "Dry run only: rerun with an explicit --app, --project, or --all-apps scope before applying a purge.")
		return
	}
	action := "remove the reviewed source-resource scope"
	if result.ManualCompletion {
		action = "verify the manually removed source-resource scope remains absent"
	}
	fmt.Fprintf(w, "Dry run only: run `%s` to %s.\n", command, action)
	fmt.Fprintln(w, "Use --project <name> or --all-apps instead of --app when that is the intended explicit scope.")
}

func cleanupPurgeApplyCommand(result cleanupPurgeResult) string {
	args := []string{"cleanup", "purge", "--apply"}
	filters := result.Filters
	if !filters.AllApps && len(filters.Apps) == 0 && len(filters.Projects) == 0 {
		return ""
	}
	if filters.AllApps {
		args = append(args, "--all-apps")
	} else {
		for _, app := range cleanupStringListValues(filters.Apps) {
			args = append(args, "--app", shellQuote(app))
		}
		for _, project := range cleanupStringListValues(filters.Projects) {
			args = append(args, "--project", shellQuote(project))
		}
	}
	if filters.IncludePlatform {
		args = append(args, "--include-platform")
	}
	if strings.TrimSpace(result.BackupDir) != "" {
		args = append(args, "--backup-dir", shellQuote(result.BackupDir))
	}
	for _, path := range result.SourcePaths {
		if path.Source == "explicit" {
			args = append(args, "--source-dir", shellQuote(path.Path))
		}
	}
	args = append(args, "--confirm", shellQuote(cleanupPurgeConfirmPhrase(result.RunName)))
	run := loadedMigrationRun{Run: migrationRun{Name: result.RunName, RunDir: result.RunDir}}
	return runScopedCommand(run, strings.Join(args, " "))
}

func writeCleanupPurgeAppliedResults(w io.Writer, result dokploy.SourcePurgeResult) {
	fmt.Fprintln(w, "Purge results:")
	writeCleanupPurgeResourceResults(w, "containers", result.Containers)
	writeCleanupPurgeResourceResults(w, "volumes", result.Volumes)
	writeCleanupPurgeResourceResults(w, "networks", result.Networks)
	writeCleanupPurgeResourceResults(w, "paths", result.Paths)
	fmt.Fprintln(w)
}

func writeCleanupPurgeResourceResults(w io.Writer, label string, items []dokploy.SourcePurgeResourceResult) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s:\n", label)
	for _, item := range items {
		message := item.Message
		if message != "" {
			message = ": " + message
		}
		fmt.Fprintf(w, "    [%s] %s%s\n", item.Status, item.Ref, message)
	}
}

func writeCleanupText(w io.Writer, result cleanupResult) {
	mode := "dry run"
	if result.Applied {
		mode = "applied"
	}
	fmt.Fprintf(w, "Cleanup plan: %s -> %s\n", firstCleanupValue(result.RunDir, result.RunName, "run"), result.Target)
	fmt.Fprintf(w, "Mode: %s\n\n", mode)

	if result.Applied && result.BackupPath != "" {
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
	run := loadedMigrationRun{Run: migrationRun{Name: result.RunName, RunDir: result.RunDir}}
	fmt.Fprintf(w, "Dry run only: run `%s` to remove only stale zero-domain Dokploy platform metadata after a DB backup.\n", runScopedCommand(run, "cleanup --apply"))
	fmt.Fprintln(w, "Source containers, volumes, networks, source-control credentials, and target apps are inventoried only and are preserved by this command.")
}
