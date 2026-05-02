package cli

import (
	"fmt"

	commitplan "github.com/aikins01/bort/internal/commit"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/planfile"
	"github.com/aikins01/bort/internal/preparer"
	rollbackplan "github.com/aikins01/bort/internal/rollback"
	syncplan "github.com/aikins01/bort/internal/sync"
)

type artifactExpectations struct {
	BundleDir string
	Target    string
	AppName   string
}

func readPrepareArtifact(path string, expect artifactExpectations) (preparer.Result, error) {
	var result preparer.Result
	if err := planfile.Read(path, &result); err != nil {
		return preparer.Result{}, err
	}
	if err := planfile.CheckAPIVersion(path, result.APIVersion, preparer.APIVersion); err != nil {
		return preparer.Result{}, err
	}
	if err := planfile.CheckBundle(path, result.BundleDir, expect.BundleDir); err != nil {
		return preparer.Result{}, err
	}
	if err := planfile.CheckTarget(path, result.Target, expect.Target); err != nil {
		return preparer.Result{}, err
	}
	for _, app := range result.Apps {
		if app.TargetResources != nil && !app.TargetResources.DryRun {
			return preparer.Result{}, fmt.Errorf("%s app %q target resources are not dry-run resources", path, app.Name)
		}
	}
	return filterPrepareResult(path, result, expect.AppName)
}

func readSyncArtifact(path string, expect artifactExpectations) (syncplan.Result, error) {
	var result syncplan.Result
	if err := planfile.Read(path, &result); err != nil {
		return syncplan.Result{}, err
	}
	if err := planfile.CheckAPIVersion(path, result.APIVersion, syncplan.APIVersion); err != nil {
		return syncplan.Result{}, err
	}
	if err := planfile.CheckDryRun(path, result.DryRun); err != nil {
		return syncplan.Result{}, err
	}
	if err := planfile.CheckBundle(path, result.BundleDir, expect.BundleDir); err != nil {
		return syncplan.Result{}, err
	}
	if err := planfile.CheckTarget(path, result.Target, expect.Target); err != nil {
		return syncplan.Result{}, err
	}
	return filterSyncResult(path, result, expect.AppName)
}

func readCutoverArtifact(path string, expect artifactExpectations) (gateway.Result, error) {
	var result gateway.Result
	if err := planfile.Read(path, &result); err != nil {
		return gateway.Result{}, err
	}
	if err := planfile.CheckAPIVersion(path, result.APIVersion, gateway.APIVersion); err != nil {
		return gateway.Result{}, err
	}
	if err := planfile.CheckDryRun(path, result.DryRun); err != nil {
		return gateway.Result{}, err
	}
	if err := planfile.CheckBundle(path, result.BundleDir, expect.BundleDir); err != nil {
		return gateway.Result{}, err
	}
	if err := planfile.CheckTarget(path, result.Target, expect.Target); err != nil {
		return gateway.Result{}, err
	}
	return filterCutoverResult(path, result, expect.AppName)
}

func readRollbackArtifact(path string, expect artifactExpectations) (rollbackplan.Result, error) {
	var result rollbackplan.Result
	if err := planfile.Read(path, &result); err != nil {
		return rollbackplan.Result{}, err
	}
	if err := planfile.CheckAPIVersion(path, result.APIVersion, rollbackplan.APIVersion); err != nil {
		return rollbackplan.Result{}, err
	}
	if err := planfile.CheckDryRun(path, result.DryRun); err != nil {
		return rollbackplan.Result{}, err
	}
	if err := planfile.CheckBundle(path, result.BundleDir, expect.BundleDir); err != nil {
		return rollbackplan.Result{}, err
	}
	if err := planfile.CheckTarget(path, result.Target, expect.Target); err != nil {
		return rollbackplan.Result{}, err
	}
	return filterRollbackResult(path, result, expect.AppName)
}

func readCommitArtifact(path string, expect artifactExpectations) (commitplan.Result, error) {
	var result commitplan.Result
	if err := planfile.Read(path, &result); err != nil {
		return commitplan.Result{}, err
	}
	if err := planfile.CheckAPIVersion(path, result.APIVersion, commitplan.APIVersion); err != nil {
		return commitplan.Result{}, err
	}
	if err := planfile.CheckDryRun(path, result.DryRun); err != nil {
		return commitplan.Result{}, err
	}
	if err := planfile.CheckBundle(path, result.BundleDir, expect.BundleDir); err != nil {
		return commitplan.Result{}, err
	}
	if err := planfile.CheckTarget(path, result.Target, expect.Target); err != nil {
		return commitplan.Result{}, err
	}
	return filterCommitResult(path, result, expect.AppName)
}

func filterPrepareResult(path string, result preparer.Result, appName string) (preparer.Result, error) {
	if appName == "" {
		if len(result.Apps) == 0 {
			return preparer.Result{}, fmt.Errorf("%s has no apps", path)
		}
		result.Status = prepareResultStatus(result.Apps)
		return result, nil
	}

	apps := []preparer.AppPlan{}
	for _, app := range result.Apps {
		if planfile.MatchApp(app.Name, app.Directory, appName) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return preparer.Result{}, fmt.Errorf("app %q not found in %s", appName, path)
	}
	result.Apps = apps
	result.Status = prepareResultStatus(apps)
	return result, nil
}

func filterSyncResult(path string, result syncplan.Result, appName string) (syncplan.Result, error) {
	if appName == "" {
		if len(result.Apps) == 0 {
			return syncplan.Result{}, fmt.Errorf("%s has no apps", path)
		}
		result.Status = syncResultStatus(result.Apps)
		return result, nil
	}

	apps := []syncplan.AppPlan{}
	for _, app := range result.Apps {
		if planfile.MatchApp(app.Name, app.Directory, appName) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return syncplan.Result{}, fmt.Errorf("app %q not found in %s", appName, path)
	}
	result.Apps = apps
	result.Status = syncResultStatus(apps)
	return result, nil
}

func filterCutoverResult(path string, result gateway.Result, appName string) (gateway.Result, error) {
	if appName == "" {
		if len(result.Apps) == 0 {
			return gateway.Result{}, fmt.Errorf("%s has no apps", path)
		}
		result.Status = cutoverResultStatus(result.Apps)
		return result, nil
	}

	apps := []gateway.AppPlan{}
	for _, app := range result.Apps {
		if planfile.MatchApp(app.Name, app.Directory, appName) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return gateway.Result{}, fmt.Errorf("app %q not found in %s", appName, path)
	}
	result.Apps = apps
	result.Status = cutoverResultStatus(apps)
	return result, nil
}

func filterRollbackResult(path string, result rollbackplan.Result, appName string) (rollbackplan.Result, error) {
	if appName == "" {
		if len(result.Apps) == 0 {
			return rollbackplan.Result{}, fmt.Errorf("%s has no apps", path)
		}
		result.Status = rollbackResultStatus(result.Apps)
		return result, nil
	}

	apps := []rollbackplan.AppPlan{}
	for _, app := range result.Apps {
		if planfile.MatchApp(app.Name, app.Directory, appName) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return rollbackplan.Result{}, fmt.Errorf("app %q not found in %s", appName, path)
	}
	result.Apps = apps
	result.Status = rollbackResultStatus(apps)
	return result, nil
}

func filterCommitResult(path string, result commitplan.Result, appName string) (commitplan.Result, error) {
	if appName == "" {
		if len(result.Apps) == 0 {
			return commitplan.Result{}, fmt.Errorf("%s has no apps", path)
		}
		result.Status = commitResultStatus(result.Apps)
		return result, nil
	}

	apps := []commitplan.AppPlan{}
	for _, app := range result.Apps {
		if planfile.MatchApp(app.Name, app.Directory, appName) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return commitplan.Result{}, fmt.Errorf("app %q not found in %s", appName, path)
	}
	result.Apps = apps
	result.Status = commitResultStatus(apps)
	return result, nil
}

func prepareResultStatus(apps []preparer.AppPlan) preparer.Status {
	status := preparer.StatusGreen
	for _, app := range apps {
		status = preparer.WorseStatus(status, app.Status)
	}
	return status
}

func syncResultStatus(apps []syncplan.AppPlan) preparer.Status {
	status := preparer.StatusGreen
	for _, app := range apps {
		status = preparer.WorseStatus(status, app.Status)
	}
	return status
}

func cutoverResultStatus(apps []gateway.AppPlan) preparer.Status {
	status := preparer.StatusGreen
	for _, app := range apps {
		status = preparer.WorseStatus(status, app.Status)
	}
	return status
}

func rollbackResultStatus(apps []rollbackplan.AppPlan) preparer.Status {
	status := preparer.StatusGreen
	for _, app := range apps {
		status = preparer.WorseStatus(status, app.Status)
	}
	return status
}

func commitResultStatus(apps []commitplan.AppPlan) preparer.Status {
	status := preparer.StatusGreen
	for _, app := range apps {
		status = preparer.WorseStatus(status, app.Status)
	}
	return status
}
