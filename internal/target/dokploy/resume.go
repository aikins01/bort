package dokploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
)

func (c *Client) loadPersistedTargetCompose(ctx context.Context, actx *applyContext, app preparer.AppPlan, identity TargetIdentity) error {
	compose, err := c.GetCompose(ctx, identity.ComposeID)
	if err != nil {
		return fmt.Errorf("load persisted target compose %s for app %s: %w", identity.ComposeID, app.Name, err)
	}
	if compose.ComposeID != identity.ComposeID {
		return fmt.Errorf("persisted target compose %s for app %s resolved to unexpected compose %s", identity.ComposeID, app.Name, compose.ComposeID)
	}
	if identity.EnvironmentID != "" && compose.EnvironmentID != "" && compose.EnvironmentID != identity.EnvironmentID {
		return fmt.Errorf("persisted target compose %s for app %s moved from environment %s to %s", identity.ComposeID, app.Name, identity.EnvironmentID, compose.EnvironmentID)
	}
	if identity.ComposeAppName != "" && compose.AppName != "" && compose.AppName != identity.ComposeAppName {
		return fmt.Errorf("persisted target compose %s for app %s changed Docker project identity from %s to %s", identity.ComposeID, app.Name, identity.ComposeAppName, compose.AppName)
	}
	composeAppName := strings.TrimSpace(compose.AppName)
	if composeAppName == "" {
		composeAppName = identity.ComposeAppName
	}
	if composeAppName == "" {
		return fmt.Errorf("persisted target compose %s for app %s has no Docker project identity", identity.ComposeID, app.Name)
	}
	entry := actx.entry(app.Name)
	entry.ProjectID = identity.ProjectID
	entry.EnvironmentID = identity.EnvironmentID
	entry.ComposeID = identity.ComposeID
	entry.ComposeAppName = composeAppName
	return nil
}

func (c *Client) hydratePersistedTargetIdentities(ctx context.Context, actx *applyContext) error {
	projects := map[string]*Project{}
	for _, app := range actx.plan.Prepare.Apps {
		identity := actx.plan.TargetIdentities[app.Name]
		if identity.ProjectID == "" {
			if identity.ComposeID != "" || identity.EnvironmentID != "" || identity.ComposeAppName != "" {
				return fmt.Errorf("persisted target identity for app %s has no project ID", app.Name)
			}
			continue
		}
		project := projects[identity.ProjectID]
		if project == nil {
			var err error
			project, err = c.verifyPersistedProjectEnvironment(ctx, app.Name, identity)
			if err != nil {
				return err
			}
			projects[identity.ProjectID] = project
		} else if err := verifyPersistedProjectEnvironment(app.Name, identity, project); err != nil {
			return err
		}
		if identity.ComposeID != "" {
			if app.TargetResources == nil || app.TargetResources.Dokploy == nil {
				return fmt.Errorf("target app %s has no reviewed Dokploy identity", app.Name)
			}
			if err := c.loadPersistedTargetCompose(ctx, actx, app, identity); err != nil {
				return err
			}
			continue
		}
		entry := actx.entry(app.Name)
		entry.ProjectID = identity.ProjectID
		if identity.EnvironmentID != "" {
			entry.EnvironmentID = identity.EnvironmentID
			continue
		}
		_, environmentName := dokployProjectSelection(actx.plan, app.Name, app.Name)
		environment := FindEnvironmentInProject(project, environmentName)
		if environment == nil {
			return fmt.Errorf("persisted target project %s for app %s has no environments", identity.ProjectID, app.Name)
		}
		entry.EnvironmentID = environment.EnvironmentID
	}
	return nil
}

func (c *Client) verifyPersistedProjectEnvironment(ctx context.Context, appName string, identity TargetIdentity) (*Project, error) {
	project, err := c.GetProject(ctx, identity.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load persisted target project %s for app %s: %w", identity.ProjectID, appName, err)
	}
	if err := verifyPersistedProjectEnvironment(appName, identity, project); err != nil {
		return nil, err
	}
	return project, nil
}

func verifyPersistedProjectEnvironment(appName string, identity TargetIdentity, project *Project) error {
	if project.ProjectID != identity.ProjectID {
		return fmt.Errorf("persisted target project %s for app %s resolved to unexpected project %s", identity.ProjectID, appName, project.ProjectID)
	}
	if identity.EnvironmentID == "" {
		return nil
	}
	for _, environment := range project.Environments {
		if environment.EnvironmentID == identity.EnvironmentID {
			return nil
		}
	}
	return fmt.Errorf("persisted target environment %s for app %s no longer belongs to project %s", identity.EnvironmentID, appName, identity.ProjectID)
}
