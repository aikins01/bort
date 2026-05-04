package dokploy

import (
	"context"
	"testing"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func TestSourceQuiesceTargetsSkipsLogicalDumpStores(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db", Kind: "postgres", Strategy: "migrate"}}
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

func TestSourceQuiesceTargetsIncludesVolumeStrategyDataStores(t *testing.T) {
	// redis has no logical-dump path, so it migrates via stopped-volume
	// copy and must be paused along with the rest of the app.
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "redis", Kind: "redis", Strategy: "migrate"}}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "redis", Type: "volume", SourceContainerID: "redis-id"},
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
	}
	ids := sourceQuiesceTargets(app)
	if len(ids) != 2 || ids[0] != "redis-id" || ids[1] != "web-id" {
		t.Fatalf("expected [redis-id web-id], got %v", ids)
	}
}

func TestSourceQuiesceTargetsIncludesStatelessWorkers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db", Kind: "postgres", Strategy: "migrate"}}
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

func TestPlanFromArtifactsPausesBeforeDumpAndVolumeSync(t *testing.T) {
	// pause must run before dump/restore so app writers stop before
	// pg_dump captures its snapshot, and before volume copy so the
	// on-disk format is consistent.
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "db", Kind: "postgres", Strategy: "migrate"}}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
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
	want := []StepKind{StepPauseSource, StepDumpDataStore, StepRestoreDataStore, StepSyncVolume}
	if len(kinds) != len(want) {
		t.Fatalf("expected %v, got %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d: expected %s, got %s (full=%v)", i, want[i], kinds[i], kinds)
		}
	}
}

func TestPlanFromArtifactsOmitsPauseSourceWhenNoState(t *testing.T) {
	// no data stores and no volumes => nothing to pause for.
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api"}}}
	plan := PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty())
	for _, step := range plan.Steps {
		if step.Kind == StepPauseSource {
			t.Fatalf("did not expect StepPauseSource without state work, got plan=%v", plan.Steps)
		}
	}
}

// volume-strategy data stores migrate by raw volume copy, so the plan
// must keep their volume sync step and run pause beforehand. it also
// must skip the logical dump/restore steps because there is none.
func TestPlanFromArtifactsCopiesVolumeStrategyDataStoreVolumes(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Service: "redis", Kind: "redis", Strategy: "migrate"}}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "redis", Type: "volume", Target: "/data"},
	}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{
		Name: "api",
		Steps: []syncplan.Step{
			{ResourceType: "data_store", ResourceRef: "data-store:redis"},
			{ResourceType: "volume", ResourceRef: "volume:redis -> /data"},
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
	want := []StepKind{StepPauseSource, StepSyncVolume}
	if len(kinds) != len(want) {
		t.Fatalf("expected %v, got %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d: expected %s, got %s (full=%v)", i, want[i], kinds[i], kinds)
		}
	}
}

// for a logical-dump store backed by its own volume, the raw volume
// copy must be suppressed (logical owns migration), but pause must
// still run so app writers stop before pg_dump.
func TestPlanFromArtifactsSkipsRawCopyForLogicalStoreVolume(t *testing.T) {
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
		if step.Kind == StepSyncVolume {
			t.Fatalf("did not expect StepSyncVolume for logical-store volume, got plan=%v", plan.Steps)
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
