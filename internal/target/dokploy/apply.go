package dokploy

import (
	"context"
	"fmt"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

type StepKind string

const (
	StepCreateProject    StepKind = "create_project"
	StepCreateService    StepKind = "create_service"
	StepPushImage        StepKind = "push_image"
	StepUploadEnv        StepKind = "upload_env"
	StepCreateVolume     StepKind = "create_volume"
	StepSyncVolume       StepKind = "sync_volume"
	StepDumpDataStore    StepKind = "dump_data_store"
	StepRestoreDataStore StepKind = "restore_data_store"
	StepInstallGateway   StepKind = "install_gateway"
)

type Step struct {
	Kind StepKind
	App  string
	Ref  string
}

type Plan struct {
	Steps []Step
}

func PlanFromArtifacts(prepare preparer.Result, sync syncplan.Result, cutover gateway.Result) Plan {
	plan := Plan{}
	for _, app := range prepare.Apps {
		plan.Steps = append(plan.Steps, Step{Kind: StepCreateProject, App: app.Name, Ref: app.Name})
		if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
			composeRef := app.TargetResources.Dokploy.ComposeApp.Name
			plan.Steps = append(plan.Steps, Step{Kind: StepCreateService, App: app.Name, Ref: composeRef})
			plan.Steps = append(plan.Steps, Step{Kind: StepPushImage, App: app.Name, Ref: composeRef})
			for _, envFile := range app.TargetResources.Dokploy.EnvFiles {
				plan.Steps = append(plan.Steps, Step{Kind: StepUploadEnv, App: app.Name, Ref: envFile.Path})
			}
			for _, volume := range app.TargetResources.Dokploy.Volumes {
				ref := volume.Name
				if ref == "" {
					ref = volume.Target
				}
				plan.Steps = append(plan.Steps, Step{Kind: StepCreateVolume, App: app.Name, Ref: ref})
			}
		}
	}
	for _, app := range sync.Apps {
		for _, step := range app.Steps {
			switch step.ResourceType {
			case "volume":
				plan.Steps = append(plan.Steps, Step{Kind: StepSyncVolume, App: app.Name, Ref: step.ResourceRef})
			case "data_store":
				plan.Steps = append(plan.Steps, Step{Kind: StepDumpDataStore, App: app.Name, Ref: step.ResourceRef})
				plan.Steps = append(plan.Steps, Step{Kind: StepRestoreDataStore, App: app.Name, Ref: step.ResourceRef})
			}
		}
	}
	for _, app := range cutover.Apps {
		for _, route := range app.Routes {
			plan.Steps = append(plan.Steps, Step{Kind: StepInstallGateway, App: app.Name, Ref: route.Host})
		}
	}
	return plan
}

func (c *Client) Apply(ctx context.Context, plan Plan) error {
	for _, step := range plan.Steps {
		if err := c.applyStep(ctx, step); err != nil {
			return fmt.Errorf("dokploy step %s for %s (%s): %w", step.Kind, step.App, step.Ref, err)
		}
	}
	return nil
}

func (c *Client) applyStep(_ context.Context, step Step) error {
	switch step.Kind {
	case StepCreateProject, StepCreateService, StepPushImage, StepUploadEnv, StepCreateVolume, StepSyncVolume, StepDumpDataStore, StepRestoreDataStore, StepInstallGateway:
		return ErrNotImplemented
	default:
		return fmt.Errorf("unknown step kind %q", step.Kind)
	}
}
