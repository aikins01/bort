package dokploy

import (
	"context"
	"testing"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func TestSourceQuiesceTargetsSkipsDataStoreServices(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db"}}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "db", Type: "volume", SourceContainerID: "db-id"},
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
	}
	ids := sourceQuiesceTargets(app)
	if len(ids) != 2 || ids[0] != "web-id" || ids[1] != "worker-id" {
		t.Fatalf("expected [web-id worker-id], got %v", ids)
	}
}

func TestSourceQuiesceTargetsIncludesStatelessWorkers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db"}}
	app.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "db", ContainerID: "db-id"},
		{ServiceName: "web", ContainerID: "web-id"},
		{ServiceName: "scheduler", ContainerID: "scheduler-id"},
	}
	// only "web" owns a volume, but "scheduler" must still be quiesced.
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
	}
	ids := sourceQuiesceTargets(app)
	if len(ids) != 2 || ids[0] != "web-id" || ids[1] != "scheduler-id" {
		t.Fatalf("expected [web-id scheduler-id], got %v", ids)
	}
}

func TestPlanFromArtifactsInjectsPauseSourceBeforeVolumeSync(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{
		Name: "api",
		Steps: []syncplan.Step{
			{ResourceType: "data_store", ResourceRef: "data-store:db"},
			{ResourceType: "volume", ResourceRef: "volume:web -> /data"},
		},
	}}}
	plan := PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty())
	kinds := []StepKind{}
	for _, step := range plan.Steps {
		switch step.Kind {
		case StepDumpDataStore, StepRestoreDataStore, StepPauseSource, StepSyncVolume:
			kinds = append(kinds, step.Kind)
		}
	}
	want := []StepKind{StepDumpDataStore, StepRestoreDataStore, StepPauseSource, StepSyncVolume}
	if len(kinds) != len(want) {
		t.Fatalf("expected %v, got %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d: expected %s, got %s (full=%v)", i, want[i], kinds[i], kinds)
		}
	}
}

func TestPlanFromArtifactsOmitsPauseSourceWhenNoVolumes(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{
		Name: "api",
		Steps: []syncplan.Step{
			{ResourceType: "data_store", ResourceRef: "data-store:db"},
		},
	}}}
	plan := PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty())
	for _, step := range plan.Steps {
		if step.Kind == StepPauseSource {
			t.Fatalf("did not expect StepPauseSource without volume sync, got plan=%v", plan.Steps)
		}
	}
}

// when the only volume sync targets a datastore-backing volume, raw copy
// is skipped and pause must not run; otherwise we'd needlessly stop the
// app's web/worker services for a no-op.
func TestPlanFromArtifactsOmitsPauseSourceForDataStoreBackingVolumes(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db", Kind: "postgres", Strategy: "migrate"}}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "db", Type: "volume", Target: "/var/lib/postgresql/data"},
	}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{
		Name: "api",
		Steps: []syncplan.Step{
			{ResourceType: "data_store", ResourceRef: "data-store:db"},
			{ResourceType: "volume", ResourceRef: "volume:db -> /var/lib/postgresql/data"},
		},
	}}}
	plan := PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty())
	for _, step := range plan.Steps {
		if step.Kind == StepPauseSource || step.Kind == StepSyncVolume {
			t.Fatalf("did not expect %s for datastore-backing volume, got plan=%v", step.Kind, plan.Steps)
		}
	}
}

func TestApplyPauseSourceStopsRunningContainers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container web-id":    []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
			"inspect --type container worker-id": []byte(`[{"Id":"worker-id","Name":"/worker","State":{"Running":true,"Status":"running"}}]`),
			"stop web-id":                        []byte("web-id\n"),
			"stop worker-id":                     []byte("worker-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	if err := client.applyPauseSource(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"}); err != nil {
		t.Fatalf("applyPauseSource: %v", err)
	}
}

func TestApplyPauseSourceSkipsAlreadyStopped(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
	}
	// stub omits "stop web-id" on purpose: applyPauseSource must skip
	// already-stopped containers and not invoke docker stop.
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container web-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	if err := client.applyPauseSource(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"}); err != nil {
		t.Fatalf("applyPauseSource: %v", err)
	}
}

func TestApplyResumeSourceStartsStoppedContainers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container web-id":    []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
			"inspect --type container worker-id": []byte(`[{"Id":"worker-id","Name":"/worker","State":{"Running":true,"Status":"running"}}]`),
			"start web-id":                       []byte("web-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	// stub omits "start worker-id" on purpose: the running worker must not
	// be restarted. if applyResumeSource calls it, fakeDockerRunner.Output
	// returns an error because the key is unstubbed.
	if err := client.applyResumeSource(context.Background(), actx, Step{Kind: StepResumeSource, App: "api"}); err != nil {
		t.Fatalf("applyResumeSource: %v", err)
	}
}

func gatewayResultEmpty() gateway.Result { return gateway.Result{} }
