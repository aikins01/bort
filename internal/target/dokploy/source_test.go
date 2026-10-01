package dokploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func TestCoolifyDeploymentFenceRequiresStoppedNoRestartControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		policy  string
		ps      string
		want    string
		wantErr bool
	}{
		{name: "fenced", policy: "no", ps: "dokploy-traefik traefik:v3.6\napi-helper-cache coollabsio/coolify-helper-cache:1\n"},
		{name: "running", running: true, policy: "no", want: "docker update --restart=no coolify", wantErr: true},
		{name: "restart enabled", policy: "always", want: "docker update --restart=no coolify", wantErr: true},
		{name: "docker hub helper still deploying", policy: "no", ps: "x8k2 docker.io/coollabsio/coolify-helper:1.0.17\n", want: "helper container(s) x8k2", wantErr: true},
		{name: "registry helper pinned by digest", policy: "no", ps: "x9 registry.local:5000/coollabsio/coolify-helper@sha256:abc\n", want: "helper container(s) x9", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeDockerRunner{outputs: map[string][]byte{
				"inspect --type container coolify":             []byte(fmt.Sprintf(`[{"Id":"coolify-id","Name":"/coolify","State":{"Running":%t,"Status":"exited"},"HostConfig":{"RestartPolicy":{"Name":"%s"}}}]`, tc.running, tc.policy)),
				"ps --no-trunc --format {{.Names}} {{.Image}}": []byte(tc.ps),
			}}
			err := requireCoolifyDeploymentFence(context.Background(), runner)
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("expected actionable fence refusal, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid Coolify fence was refused: %v", err)
			}
		})
	}
}

func TestStatefulCoolifyPlanRequiresDeploymentFence(t *testing.T) {
	plan := Plan{
		Prepare: preparer.Result{Source: "coolify-local"},
		Steps:   []Step{{Kind: StepPauseSource, App: "api"}, {Kind: StepSyncVolume, App: "api"}, {Kind: StepPushImage, App: "api"}},
	}
	if !planRequiresCoolifyDeploymentFence(plan) {
		t.Fatal("stateful Coolify plan did not require a deployment fence")
	}
	plan.Prepare.Source = "docker"
	plan.Prepare.Apps = []preparer.AppPlan{{Name: "api", Platform: "docker"}, {Name: "worker", Platform: "coolify"}}
	if planRequiresCoolifyDeploymentFence(plan) {
		t.Fatal("plain Docker source unexpectedly required the Coolify control-plane fence")
	}
	plan.Steps = append(plan.Steps, Step{Kind: StepSyncVolume, App: "worker"})
	if !planRequiresCoolifyDeploymentFence(plan) {
		t.Fatal("Docker-scanned Coolify app did not require the deployment fence")
	}
}

func TestVerifySourceContainersBatchesReviewedIDs(t *testing.T) {
	webID := strings.Repeat("a", 64)
	workerID := strings.Repeat("b", 64)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container aaaaaaaaaaaa bbbbbbbbbbbb": []byte(`[{"Id":"` + webID + `","Name":"/web"},{"Id":"` + workerID + `","Name":"/worker"}]`),
	}}
	apps := []preparer.AppPlan{{
		Name: "api",
		Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
			{ContainerID: "aaaaaaaaaaaa", ContainerName: "web"},
			{ContainerID: "bbbbbbbbbbbb", ContainerName: "worker"},
		}},
	}}
	if err := verifySourceContainers(context.Background(), runner, apps); err != nil {
		t.Fatal(err)
	}
	if len(runner.outputArgs) != 1 || strings.Join(runner.outputArgs[0], " ") != "inspect --type container aaaaaaaaaaaa bbbbbbbbbbbb" {
		t.Fatalf("source attestation was not batched: %#v", runner.outputArgs)
	}
}

func TestVerifySourceContainersSkipsPlatformApps(t *testing.T) {
	webID := strings.Repeat("a", 64)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container aaaaaaaaaaaa": []byte(`[{"Id":"` + webID + `","Name":"/web"}]`),
	}}
	apps := []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
				{ContainerID: "aaaaaaaaaaaa", ContainerName: "web"},
			}},
		},
		{
			Name: "coolify-proxy",
			Role: "platform",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
				{ContainerID: "stale-platform-id", ContainerName: "coolify-proxy"},
			}},
		},
	}
	if err := verifySourceContainers(context.Background(), runner, apps); err != nil {
		t.Fatal(err)
	}
	if len(runner.outputArgs) != 1 || strings.Join(runner.outputArgs[0], " ") != "inspect --type container aaaaaaaaaaaa" {
		t.Fatalf("source attestation inspected an excluded platform app: %#v", runner.outputArgs)
	}
}

func TestValidateSourceContainerAttestationsRequiresStableSelectedIDs(t *testing.T) {
	apps := []preparer.AppPlan{{
		Name: "api",
		Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
			{ContainerName: "web"},
		}},
	}}
	if err := ValidateSourceContainerAttestations(apps); err == nil || !strings.Contains(err.Error(), "no stable container ID") {
		t.Fatalf("expected missing reviewed ID refusal, got %v", err)
	}
	apps[0].Resources.SourceServices[0].ContainerID = "aaaaaaaaaaaa"
	apps[0].Resources.SourceServices[0].ContainerName = ""
	if err := ValidateSourceContainerAttestations(apps); err == nil || !strings.Contains(err.Error(), "no reviewed container name") {
		t.Fatalf("expected missing reviewed name refusal, got %v", err)
	}
	apps[0].Resources.SourceServices[0].ContainerName = "web"
	if err := ValidateSourceContainerAttestations(apps); err != nil {
		t.Fatalf("expected stable reviewed identity to pass static validation, got %v", err)
	}
}

func TestValidateSourceContainerAttestationsRequiresEverySelectedApp(t *testing.T) {
	apps := []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
				{ContainerID: "aaaaaaaaaaaa", ContainerName: "web"},
			}},
		},
		{Name: "worker"},
	}
	if err := ValidateSourceContainerAttestations(apps); err == nil || !strings.Contains(err.Error(), "app worker has no reviewed source container identities") {
		t.Fatalf("expected unattested selected app refusal, got %v", err)
	}
}

func TestValidateSourceContainerAttestationsRejectsConflictingDuplicateID(t *testing.T) {
	apps := []preparer.AppPlan{
		{
			Name: "api",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
				{ContainerID: "aaaaaaaaaaaa", ContainerName: "web"},
			}},
		},
		{
			Name: "worker",
			Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
				{ContainerID: "aaaaaaaaaaaa", ContainerName: "worker"},
			}},
		},
	}
	if err := ValidateSourceContainerAttestations(apps); err == nil || !strings.Contains(err.Error(), "conflicting names") {
		t.Fatalf("expected conflicting duplicate source identity refusal, got %v", err)
	}
}

func TestValidateSourceContainerAttestationsRejectsSameAppConflictingDuplicateID(t *testing.T) {
	apps := []preparer.AppPlan{{
		Name: "api",
		Resources: preparer.ResourceSpecs{
			SourceServices: []preparer.SourceServiceRef{{ContainerID: "aaaaaaaaaaaa", ContainerName: "web"}},
			Volumes:        []preparer.VolumeResource{{SourceContainerID: "aaaaaaaaaaaa", SourceContainerName: "replacement"}},
		},
	}}
	if err := ValidateSourceContainerAttestations(apps); err == nil || !strings.Contains(err.Error(), "conflicting names") {
		t.Fatalf("expected same-app conflicting source identity refusal, got %v", err)
	}
}

func TestVerifySourceContainersNarrowsFailedBatchWithAttribution(t *testing.T) {
	webID := strings.Repeat("a", 64)
	replacementID := strings.Repeat("c", 64)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container aaaaaaaaaaaa": []byte(`[{"Id":"` + webID + `","Name":"/web"}]`),
			"inspect --type container bbbbbbbbbbbb": []byte(`[{"Id":"` + replacementID + `","Name":"/worker"}]`),
		},
		outputErrs: map[string]error{
			"inspect --type container aaaaaaaaaaaa bbbbbbbbbbbb": errors.New("batch unavailable"),
		},
	}
	apps := []preparer.AppPlan{{
		Name: "api",
		Resources: preparer.ResourceSpecs{SourceServices: []preparer.SourceServiceRef{
			{ContainerID: "aaaaaaaaaaaa", ContainerName: "web"},
			{ContainerID: "bbbbbbbbbbbb", ContainerName: "worker"},
		}},
	}}
	err := verifySourceContainers(context.Background(), runner, apps)
	if err == nil || !strings.Contains(err.Error(), "verify reviewed source container bbbbbbbbbbbb for app api") || !strings.Contains(err.Error(), "resolved to ID") {
		t.Fatalf("batch isolation did not preserve container attribution: %v", err)
	}
	if len(runner.outputArgs) != 3 {
		t.Fatalf("failed batch was not isolated by bisection: %#v", runner.outputArgs)
	}
}

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
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api"}}}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{
		Name: "api",
		Steps: []syncplan.Step{
			{ResourceType: "data_store", ResourceRef: "data-store:db"},
			{ResourceType: "volume", ResourceRef: "volume:web -> /data", Strategy: syncplan.StrategyDockerVolumeArchive},
		},
	}}}
	filter := func(plan Plan) []StepKind {
		kinds := []StepKind{}
		for _, step := range plan.Steps {
			switch step.Kind {
			case StepPushImage, StepDumpDataStore, StepRestoreDataStore, StepPauseSource, StepSyncVolume, StepResumeSource, StepResumeTarget:
				kinds = append(kinds, step.Kind)
			}
		}
		return kinds
	}
	staged := filter(PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty()))
	wantStaged := []StepKind{StepPauseSource, StepDumpDataStore, StepRestoreDataStore, StepSyncVolume, StepPushImage}
	if !slices.Equal(staged, wantStaged) {
		t.Fatalf("staged: expected %v, got %v", wantStaged, staged)
	}
	inPlace := filter(PlanFromArtifactsV1Alpha2(prepare, syncResult, gatewayResultEmpty()))
	wantInPlace := []StepKind{StepPushImage, StepPauseSource, StepDumpDataStore, StepRestoreDataStore, StepSyncVolume, StepResumeSource, StepResumeTarget}
	if !slices.Equal(inPlace, wantInPlace) {
		t.Fatalf("v1alpha2: expected %v, got %v", wantInPlace, inPlace)
	}
}

func TestPlanFromArtifactsDeploysStatelessAppsBeforeStateWork(t *testing.T) {
	stateful := preparer.AppPlan{Name: "api"}
	stateful.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Target: "/data"}}
	stateful.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api"}}}
	stateless := preparer.AppPlan{Name: "docs"}
	stateless.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "docs"}}}
	plan := PlanFromArtifacts(
		preparer.Result{Apps: []preparer.AppPlan{stateful, stateless}},
		syncplan.Result{Apps: []syncplan.AppPlan{
			{Name: "api", Steps: []syncplan.Step{{ResourceType: "volume", ResourceRef: "volume:web -> /data", Strategy: syncplan.StrategyDockerVolumeArchive}}},
			{Name: "docs"},
		}},
		gatewayResultEmpty(),
	)
	pushIndex := map[string]int{}
	syncIndex := -1
	for index, step := range plan.Steps {
		switch step.Kind {
		case StepPushImage:
			pushIndex[step.App] = index
		case StepSyncVolume:
			syncIndex = index
		}
	}
	docsPush, hasDocsPush := pushIndex["docs"]
	apiPush, hasAPIPush := pushIndex["api"]
	if !hasDocsPush || !hasAPIPush || syncIndex < 0 {
		t.Fatalf("expected push_image for both apps and a sync_volume step, steps=%v", stepKinds(plan.Steps))
	}
	if !(docsPush < syncIndex && syncIndex < apiPush) {
		t.Fatalf("expected stateless push before state work and stateful push after it, steps=%v", stepKinds(plan.Steps))
	}
	if !appStateIsStaged(plan, "api") || appStateIsStaged(plan, "docs") {
		t.Fatalf("expected only the stateful app to be staged, steps=%v", stepKinds(plan.Steps))
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

func TestPlanFromArtifactsDeploysRoutedStatefulTargetAfterStateTransfer(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Target: "/data"}}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{Name: "api"}}}
	plan := PlanFromArtifacts(
		preparer.Result{Apps: []preparer.AppPlan{app}},
		syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{ResourceType: "volume", ResourceRef: "volume:web -> /data", Strategy: syncplan.StrategyDockerVolumeArchive}}}}},
		gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	)
	syncIndex, pushIndex, gatewayIndex := -1, -1, -1
	for index, step := range plan.Steps {
		switch step.Kind {
		case StepSyncVolume:
			syncIndex = index
		case StepPushImage:
			pushIndex = index
		case StepInstallGateway:
			gatewayIndex = index
		case StepResumeTarget, StepResumeSource:
			t.Fatalf("routed staged app must not resume source or target writers, steps=%v", stepKinds(plan.Steps))
		}
	}
	if syncIndex < 0 || pushIndex <= syncIndex || gatewayIndex <= pushIndex {
		t.Fatalf("expected sync_volume < push_image < install_gateway, steps=%v", stepKinds(plan.Steps))
	}
}

func TestPlanFromArtifactsV1Alpha2StartsRoutedStatefulTargetDuringFinalProxyHandoff(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", Target: "/data"}}
	plan := PlanFromArtifactsV1Alpha2(
		preparer.Result{Apps: []preparer.AppPlan{app}},
		syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api", Steps: []syncplan.Step{{ResourceType: "volume", ResourceRef: "volume:web -> /data", Strategy: syncplan.StrategyDockerVolumeArchive}}}}},
		gateway.Result{Apps: []gateway.AppPlan{{Name: "api", Routes: []gateway.Route{{Host: "api.example.com"}}}}},
	)
	stopProxyIndex, targetIndex, activateRoutesIndex, startProxyIndex := -1, -1, -1, -1
	for index, step := range plan.Steps {
		if step.Kind == StepStopCoolifyProxy {
			stopProxyIndex = index
		}
		if step.Kind == StepStartDokployProxy {
			startProxyIndex = index
		}
		if step.Kind == StepResumeTarget && step.App == "api" {
			targetIndex = index
		}
		if step.Kind == StepActivateRoutes && step.App == "api" {
			activateRoutesIndex = index
		}
	}
	if stopProxyIndex < 0 || targetIndex <= stopProxyIndex || activateRoutesIndex <= targetIndex || startProxyIndex <= activateRoutesIndex {
		t.Fatalf("expected target writers to start after the source proxy stops and before route activation and target proxy start, steps=%v", plan.Steps)
	}
}

func TestPlanFromArtifactsPreservesBindMountsWithoutStateWork(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{Volumes: []preparer.DokployVolume{{Type: "bind", Source: "/srv/api/uploads", Target: "/uploads"}}}}
	prepare := preparer.Result{Apps: []preparer.AppPlan{app}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{
		Name: "api",
		Steps: []syncplan.Step{
			{ResourceType: "volume", ResourceRef: "volume:web -> /uploads", Strategy: syncplan.StrategyNone, TargetAction: "preserve_vps_file_mount"},
		},
	}}}
	plan := PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty())
	for _, step := range plan.Steps {
		switch step.Kind {
		case StepCreateVolume, StepPauseSource, StepSyncVolume:
			t.Fatalf("did not expect state work for preserved bind mount, got step=%#v plan=%v", step, plan.Steps)
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
			{ResourceType: "volume", ResourceRef: "volume:redis -> /data", Strategy: syncplan.StrategyDockerVolumeArchive},
		},
	}}}
	plan := PlanFromArtifacts(prepare, syncResult, gatewayResultEmpty())
	kinds := []StepKind{}
	for _, step := range plan.Steps {
		switch step.Kind {
		case StepDumpDataStore, StepRestoreDataStore, StepPauseSource, StepSyncVolume, StepResumeSource, StepResumeTarget:
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
			{ResourceType: "volume", ResourceRef: "volume:db -> /var/lib/postgresql/data", Strategy: syncplan.StrategyDockerVolumeArchive},
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
			"inspect --type container web-id worker-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}},{"Id":"worker-id","Name":"/worker","State":{"Running":true,"Status":"running"}}]`),
			"stop web-id":    []byte("web-id\n"),
			"stop worker-id": []byte("worker-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	if err := client.applyPauseSource(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"}); err != nil {
		t.Fatalf("applyPauseSource: %v", err)
	}
	entry := actx.entry("api")
	if !entry.SourcePauseRecorded || len(entry.SourcePausedContainers) != 2 || entry.SourcePausedContainers[0].ID != "web-id" || entry.SourcePausedContainers[1].ID != "worker-id" {
		t.Fatalf("unexpected source pause ownership: %#v", entry)
	}
}

func TestApplyPauseSourceRefusesStaleContainerID(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "stale-id", ContainerName: "web-current"}}
	runner := &fakeDockerRunner{
		outputErrs: map[string]error{"inspect --type container stale-id": errors.New("Error: No such object: stale-id")},
		outputs: map[string][]byte{
			"inspect --type container web-current": []byte(`[{"Id":"current-id","Name":"/web-current","State":{"Running":true,"Status":"running"}}]`),
			"stop current-id":                      []byte("current-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	err := client.applyPauseSource(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "stale-id") {
		t.Fatalf("expected stale source identity refusal, got %v", err)
	}
	if fakeOutputCalled(runner, "stop", "current-id") {
		t.Fatalf("stale identity stopped a replacement container, calls=%v", runner.outputArgs)
	}
}

func TestApplyPauseSourceRefusesRecreatedComposeContainer(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "stale-id", ContainerName: "web-proj-123"}}
	runner := &fakeDockerRunner{
		outputErrs: map[string]error{"inspect --type container stale-id": errors.New("Error: No such object: stale-id")},
		outputs: map[string][]byte{
			"ps -a --filter label=com.docker.compose.project=proj --filter label=com.docker.compose.service=web --format {{.ID}}": []byte("live-id\n"),
			"inspect --type container live-id": []byte(`[{"Id":"live-id","Name":"/web-proj-456","Config":{"Labels":{"com.docker.compose.service":"web"}},"State":{"Running":true,"Status":"running"}}]`),
			"stop live-id":                     []byte("live-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	err := client.applyPauseSource(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "stale-id") {
		t.Fatalf("expected recreated source identity refusal, got %v", err)
	}
	if fakeOutputCalled(runner, "stop", "live-id") {
		t.Fatalf("stale identity stopped a recreated Compose container, calls=%v", runner.outputArgs)
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
	entry := actx.entry("api")
	if !entry.SourcePauseRecorded || !slices.Equal(entry.SourcePausedContainers, []sourcePausedContainer{{ID: "web-id"}}) {
		t.Fatalf("already-stopped source was claimed by Bort: %#v", entry)
	}
}

func TestApplyResumeSourceStartsOnlyOwnedStoppedContainers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container web-id worker-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":false,"Status":"exited"}},{"Id":"worker-id","Name":"/worker","State":{"Running":false,"Status":"exited"}}]`),
			"start web-id": []byte("web-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	entry := actx.entry("api")
	entry.SourcePauseRecorded = true
	entry.SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true}, {ID: "worker-id"}}
	if err := client.applyResumeSource(context.Background(), actx, Step{Kind: StepResumeSource, App: "api"}); err != nil {
		t.Fatalf("applyResumeSource: %v", err)
	}
	if !fakeOutputCalled(runner, "start", "web-id") {
		t.Fatalf("resume did not start the container Bort stopped: %#v", runner.outputArgs)
	}
	if fakeOutputCalled(runner, "start", "worker-id") {
		t.Fatalf("resume started a container Bort did not stop: %#v", runner.outputArgs)
	}
	if !entry.SourcePauseRecorded || !slices.Equal(entry.SourcePausedContainers, []sourcePausedContainer{{ID: "web-id", Stopped: true}, {ID: "worker-id"}}) {
		t.Fatalf("resume cleared source ownership before terminal progress was durable: %#v", entry)
	}
}

type sourceOwnershipRunner struct {
	fakeDockerRunner
	running    map[string]bool
	startedAt  map[string]string
	finishedAt map[string]string
}

func (r *sourceOwnershipRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	r.outputArgs = append(r.outputArgs, append([]string{}, args...))
	if len(args) >= 4 && args[0] == "inspect" && args[1] == "--type" && args[2] == "container" {
		entries := make([]string, 0, len(args)-3)
		for _, id := range args[3:] {
			entries = append(entries, fmt.Sprintf(`{"Id":%q,"Name":%q,"State":{"Running":%t,"Status":"exited","StartedAt":%q,"FinishedAt":%q}}`, id, "/"+id, r.running[id], r.startedAt[id], r.finishedAt[id]))
		}
		return []byte("[" + strings.Join(entries, ",") + "]"), nil
	}
	if len(args) == 2 && args[0] == "stop" {
		r.running[args[1]] = false
		if r.finishedAt != nil {
			r.finishedAt[args[1]] = fmt.Sprintf("stopped-%d", len(r.outputArgs))
		}
		return []byte(args[1] + "\n"), nil
	}
	if len(args) == 2 && args[0] == "start" {
		r.running[args[1]] = true
		if r.startedAt != nil {
			r.startedAt[args[1]] = fmt.Sprintf("started-%d", len(r.outputArgs))
		}
		return []byte(args[1] + "\n"), nil
	}
	return nil, fmt.Errorf("docker output not stubbed: %s", strings.Join(args, " "))
}

func TestApplyFailureResumesOnlySourceContainersItStopped(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": true, "worker-id": false}}
	client := &Client{Docker: runner}
	beforeCalls := 0
	beforeStep := func(StepProgress) error {
		beforeCalls++
		if beforeCalls == 2 {
			return errors.New("injected post-pause failure")
		}
		return nil
	}
	plan := Plan{
		RunDir:     t.TempDir(),
		Steps:      []Step{{Kind: StepPauseSource, App: "api", Ref: "api"}, {Kind: StepCreateVolume, App: "api", Ref: "data"}},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
		BeforeStep: &beforeStep,
	}
	err := client.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "injected post-pause failure") {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if !runner.running["web-id"] {
		t.Fatal("Bort-owned source container was not resumed")
	}
	if runner.running["worker-id"] || fakeOutputCalled(&runner.fakeDockerRunner, "start", "worker-id") {
		t.Fatalf("pre-stopped source container was started: running=%t calls=%#v", runner.running["worker-id"], runner.outputArgs)
	}
	statePath, err := sourcePauseStatePath(plan.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	loaded := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := loaded.loadSourcePauseState(); err != nil {
		t.Fatalf("load final source pause state %s: %v", statePath, err)
	}
	want := []sourcePausedContainer{{ID: "web-id", Stopped: true, StartedAt: "", FinishedAt: runner.finishedAt["web-id"]}, {ID: "worker-id"}}
	if !loaded.entry("api").SourcePauseRecorded || !slices.Equal(loaded.entry("api").SourcePausedContainers, want) {
		t.Fatalf("source ownership was cleared before compensation progress became durable: %#v", loaded.entry("api"))
	}
}

func TestFinalizeSourceResumesClearsDurableOwnership(t *testing.T) {
	runDir := t.TempDir()
	plan := Plan{RunDir: runDir, Steps: []Step{{Kind: StepResumeSource, App: "api", Ref: "api"}}}
	paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id"}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	client := &Client{}
	if err := client.FinalizeSourceResumes(plan); err != nil {
		t.Fatal(err)
	}
	loaded := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := loaded.loadSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	if loaded.entry("api").SourcePauseRecorded {
		t.Fatalf("finalized source ownership remained durable: %#v", loaded.entry("api"))
	}
}

func TestApplyRetryRepausesOnlyDurablyOwnedSourceContainers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	runDir := t.TempDir()
	plan := Plan{
		RunDir:     runDir,
		ResumeFrom: 1,
		Steps:      []Step{{Kind: StepPauseSource, App: "api", Ref: "api"}, {Kind: StepCreateVolume, App: "api", Ref: "data"}},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true}, {ID: "worker-id"}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": true, "worker-id": true}, finishedAt: map[string]string{}}
	client := &Client{Docker: runner}
	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("retry paused source: %v", err)
	}
	if runner.running["web-id"] || !fakeOutputCalled(&runner.fakeDockerRunner, "stop", "web-id") {
		t.Fatalf("durably owned source was not re-paused: running=%t calls=%#v", runner.running["web-id"], runner.outputArgs)
	}
	if runner.running["worker-id"] || !fakeOutputCalled(&runner.fakeDockerRunner, "stop", "worker-id") {
		t.Fatalf("retry left a quiesce target that started since the pause running: running=%t calls=%#v", runner.running["worker-id"], runner.outputArgs)
	}
	loaded := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := loaded.loadSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	want := []sourcePausedContainer{
		{ID: "web-id", Stopped: true, FinishedAt: runner.finishedAt["web-id"]},
		{ID: "worker-id", Stopped: true, FinishedAt: runner.finishedAt["worker-id"]},
	}
	if got := loaded.entry("api").SourcePausedContainers; !slices.Equal(got, want) {
		t.Fatalf("retry did not adopt the container it stopped: got %#v, want %#v", got, want)
	}
}

func TestApplyRetryRefusesQuiesceTargetWithoutPauseEvidence(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	plan := Plan{
		RunDir:     t.TempDir(),
		ResumeFrom: 1,
		Steps:      []Step{{Kind: StepPauseSource, App: "api", Ref: "api"}, {Kind: StepCreateVolume, App: "api", Ref: "data"}},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": false, "worker-id": false}}
	client := &Client{Docker: runner}
	err := client.Apply(context.Background(), plan)
	if !isUnsafeSourceResumeError(err) || !strings.Contains(err.Error(), "worker-id") || !strings.Contains(err.Error(), "no durable pause evidence") {
		t.Fatalf("expected missing pause evidence refusal, got %v", err)
	}
	if runner.running["web-id"] || fakeOutputCalled(&runner.fakeDockerRunner, "start", "web-id") {
		t.Fatalf("source was restarted despite missing pause evidence: %#v", runner.outputArgs)
	}
}

func TestSourceRanSincePauseDetectsUnownedContainerThatRanAndStopped(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "worker", Type: "bind", SourceContainerID: "worker-id"},
	}
	actx := &applyContext{plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}, cache: map[string]*appCache{}}
	actx.entry("api").SourcePauseRecorded = true
	actx.entry("api").SourcePausedContainers = []sourcePausedContainer{
		{ID: "web-id", Stopped: true, StartedAt: "started-1", FinishedAt: "stopped-2"},
		{ID: "worker-id", StartedAt: "started-0", FinishedAt: "stopped-0"},
	}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"web-id": false, "worker-id": false},
		startedAt:  map[string]string{"web-id": "started-1", "worker-id": "started-5"},
		finishedAt: map[string]string{"web-id": "stopped-2", "worker-id": "stopped-6"},
	}
	client := &Client{Docker: runner}
	ran, err := client.sourceRanSincePause(context.Background(), actx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("a quiesce target Bort never stopped ran after the pause but was not detected")
	}
	runner.startedAt["worker-id"], runner.finishedAt["worker-id"] = "started-0", "stopped-0"
	ran, err = client.sourceRanSincePause(context.Background(), actx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("unchanged quiesce targets were reported as having run")
	}
}

func TestApplyStopSourceAppRefusesReviewedContainerDisappearance(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "reviewed-id", ContainerName: "api-web"}}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container reviewed-id": []byte(`[{"Id":"reviewed-id","Name":"/api-web","HostConfig":{"RestartPolicy":{"Name":"always"}},"State":{"Running":true,"Status":"running"}}]`),
			"update --restart=no reviewed-id":      []byte("reviewed-id\n"),
		},
		outputErrs: map[string]error{
			"stop reviewed-id": errors.New("Error response from daemon: No such container: reviewed-id"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}, cache: map[string]*appCache{}}
	err := client.applyStopSourceApp(context.Background(), actx, Step{Kind: StepStopSourceApp, App: "api", Ref: "api"})
	if err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("expected disappearing reviewed source to fail retirement, got %v", err)
	}
}

func gatewayResultEmpty() gateway.Result { return gateway.Result{} }

func TestPrimeResumeStateSkipsPauseReplayAfterCompletedResume(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	plan := Plan{
		RunDir: t.TempDir(),
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
			{Kind: StepResumeSource, App: "api", Ref: "api"},
			{Kind: StepPushImage, App: "api", Ref: "api"},
			{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer},
		},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	actx.entry("api").SourcePauseRecorded = true
	actx.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id"}}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": true}}
	client := &Client{Docker: runner}
	pausedApps := pausedSources{}
	coolifyProxyStopped := false
	if err := client.primeResumeState(context.Background(), actx, plan.Steps[:4], pausedApps, map[string]struct{}{}, &coolifyProxyStopped); err != nil {
		t.Fatalf("prime resume state: %v", err)
	}
	if !runner.running["web-id"] || fakeOutputCalled(&runner.fakeDockerRunner, "stop", "web-id") {
		t.Fatalf("a source resumed by a completed step was stopped again on retry: running=%t calls=%#v", runner.running["web-id"], runner.outputArgs)
	}
	if len(pausedApps) != 0 {
		t.Fatalf("resumed source still tracked as paused: %#v", pausedApps)
	}
}

func TestPrimeResumeStateKeepsSourceStoppedAfterStagedDeploy(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	plan := Plan{
		RunDir: t.TempDir(),
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
			{Kind: StepPushImage, App: "api", Ref: "api"},
			{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer},
		},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	actx.entry("api").SourcePauseRecorded = true
	actx.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true}}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": false}}
	client := &Client{Docker: runner}
	pausedApps := pausedSources{}
	coolifyProxyStopped := false
	if err := client.primeResumeState(context.Background(), actx, plan.Steps[:3], pausedApps, map[string]struct{}{}, &coolifyProxyStopped); err != nil {
		t.Fatalf("prime resume state: %v", err)
	}
	if len(pausedApps) != 0 {
		t.Fatalf("source handed to a staged deployment must not be restartable by cleanup: %#v", pausedApps)
	}
	if err := client.bestEffortResume(context.Background(), actx, plan, len(plan.Steps), pausedApps, false, true); err != nil {
		t.Fatal(err)
	}
	if runner.running["web-id"] {
		t.Fatal("cleanup restarted a source whose state was already handed to Dokploy")
	}
}

func TestApplyPauseSourceRecordsStoppedRunTimestamps(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	plan := Plan{RunDir: t.TempDir(), Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"web-id": true},
		startedAt:  map[string]string{"web-id": "started-0"},
		finishedAt: map[string]string{"web-id": "never"},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := client.applyPauseSource(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"}); err != nil {
		t.Fatalf("applyPauseSource: %v", err)
	}
	loaded := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := loaded.loadSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	want := []sourcePausedContainer{{ID: "web-id", Stopped: true, StartedAt: "started-0", FinishedAt: runner.finishedAt["web-id"]}}
	if got := loaded.entry("api").SourcePausedContainers; !slices.Equal(got, want) || want[0].FinishedAt == "never" {
		t.Fatalf("durable pause state = %#v, want post-stop snapshot %#v", got, want)
	}
}

func TestPrimeResumeStateInvalidatesTransferWhenSourceRanAfterPause(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	var recorded []StepProgress
	beforeStep := func(progress StepProgress) error {
		recorded = append(recorded, progress)
		return nil
	}
	plan := Plan{
		RunDir:     t.TempDir(),
		ResumeFrom: 2,
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
			{Kind: StepCreateVolume, App: "api", Ref: "data"},
		},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
		BeforeStep: &beforeStep,
	}
	paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true, StartedAt: "started-1", FinishedAt: "stopped-2"}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"web-id": false},
		startedAt:  map[string]string{"web-id": "started-9"},
		finishedAt: map[string]string{"web-id": "stopped-10"},
	}
	client := &Client{Docker: runner}
	err := client.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "invalidated the transfer") {
		t.Fatalf("expected invalidated transfer refusal, got %v", err)
	}
	if fakeOutputCalled(&runner.fakeDockerRunner, "stop", "web-id") {
		t.Fatalf("a source that ran after transfer was silently re-paused: %#v", runner.outputArgs)
	}
	if !runner.running["web-id"] {
		t.Fatal("source was not resumed after its transfer was invalidated")
	}
	if len(recorded) != 1 || recorded[0].Step.Kind != StepResumeSource || recorded[0].Index != 0 || recorded[0].Status != StepStatusStarted {
		t.Fatalf("invalidating resume was not recorded at the pause index before restart: %#v", recorded)
	}
}

func TestPrimeResumeStateRefusesStaleStagedDeployWhenSourceRan(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	plan := Plan{
		RunDir: t.TempDir(),
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
			{Kind: StepPushImage, App: "api", Ref: "api"},
			{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer},
		},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	actx.entry("api").SourcePauseRecorded = true
	actx.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", StartedAt: "started-1", FinishedAt: "stopped-2"}}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": true}, startedAt: map[string]string{"web-id": "started-7"}}
	client := &Client{Docker: runner}
	pausedApps := pausedSources{}
	coolifyProxyStopped := false
	err := client.primeResumeState(context.Background(), actx, plan.Steps[:3], pausedApps, map[string]struct{}{}, &coolifyProxyStopped)
	if !isUnsafeSourceResumeError(err) || !strings.Contains(err.Error(), "staged data is stale") {
		t.Fatalf("expected stale staged deployment refusal, got %v", err)
	}
	if fakeOutputCalled(&runner.fakeDockerRunner, "stop", "web-id") || !runner.running["web-id"] {
		t.Fatalf("source that ran past the staged handoff was touched: %#v", runner.outputArgs)
	}
	if len(pausedApps) != 0 {
		t.Fatalf("stale staged source must not be restartable by cleanup: %#v", pausedApps)
	}
}

func TestPausedSourcesObserveCompleted(t *testing.T) {
	paused := pausedSources{"api": false, "other": false, "untransferred": false}
	handedOff := map[string]struct{}{}
	paused.observeCompleted(Step{Kind: StepSyncVolume, App: "api"}, handedOff)
	paused.observeCompleted(Step{Kind: StepDumpDataStore, App: "stateless"}, handedOff)
	if !paused["api"] || paused["other"] {
		t.Fatalf("transfer must mark only the transferred app: %#v", paused)
	}
	if _, tracked := paused["stateless"]; tracked {
		t.Fatalf("transfer for an unpaused app must not track it: %#v", paused)
	}
	paused.observeCompleted(Step{Kind: StepPushImage, App: "api"}, handedOff)
	paused.observeCompleted(Step{Kind: StepPushImage, App: "untransferred"}, handedOff)
	paused.observeCompleted(Step{Kind: StepResumeSource, App: "other"}, handedOff)
	if len(paused) != 0 {
		t.Fatalf("deploy and resume must release their apps: %#v", paused)
	}
	if _, ok := handedOff["api"]; !ok || len(handedOff) != 1 {
		t.Fatalf("only a deploy over transferred state is a staged handoff: %#v", handedOff)
	}
}

func TestApplyRecordsCleanupResumeBeforeRestartingTransferredSource(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	app.Resources.DataStores = []preparer.DataStoreResource{{Kind: "redis", Service: "db", Strategy: "migrate"}}
	runner := &sourceOwnershipRunner{running: map[string]bool{"web-id": true}}
	client := &Client{Docker: runner}
	var before []StepProgress
	beforeStep := func(p StepProgress) error {
		before = append(before, p)
		if len(before) == 3 {
			return errors.New("injected failure after transfer")
		}
		if p.Step.Kind == StepResumeSource {
			if runner.running["web-id"] {
				t.Fatal("cleanup started the source before recording the restart")
			}
			return errors.New("ledger unavailable")
		}
		return nil
	}
	plan := Plan{
		RunDir: t.TempDir(),
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepDumpDataStore, App: "api", Ref: "data-store:db"},
			{Kind: StepCreateVolume, App: "api", Ref: "data"},
		},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
		BeforeStep: &beforeStep,
	}
	err := client.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "remains stopped") || !strings.Contains(err.Error(), "bort status") || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("expected refused restart when the cleanup cannot be recorded, got %v", err)
	}
	if runner.running["web-id"] {
		t.Fatal("source restarted although its completed transfer could not be invalidated in the ledger")
	}
	last := before[len(before)-1]
	if last.Step.Kind != StepResumeSource || last.Index != 0 || last.Status != StepStatusStarted {
		t.Fatalf("cleanup must be recorded at the pause index before restart, got %#v", last)
	}
}

func TestApplyFinalizationRetryRefusesStaleStagedDeployWhenSourceRan(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	plan := Plan{
		RunDir:     t.TempDir(),
		ResumeFrom: 3,
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
			{Kind: StepPushImage, App: "api", Ref: "api"},
		},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
	}
	paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true, StartedAt: "started-1", FinishedAt: "stopped-2"}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"web-id": false},
		startedAt:  map[string]string{"web-id": "started-9"},
		finishedAt: map[string]string{"web-id": "stopped-10"},
	}
	client := &Client{Docker: runner}
	err := client.Apply(context.Background(), plan)
	if !isUnsafeSourceResumeError(err) || !strings.Contains(err.Error(), "staged data is stale") {
		t.Fatalf("expected stale staged deployment refusal on finalization retry, got %v", err)
	}
	if fakeOutputCalled(&runner.fakeDockerRunner, "stop", "web-id") || fakeOutputCalled(&runner.fakeDockerRunner, "start", "web-id") {
		t.Fatalf("finalization retry touched the source: %#v", runner.outputArgs)
	}
}

func TestApplyRefusesStagedDeployWhenSourceRanAfterInProcessTransfer(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{Kind: "redis", Service: "cache", Strategy: "recreate"}}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", SourceContainerID: "web-id"},
		{Service: "cache", Type: "volume", Target: "/data", SourceContainerID: "cache-id"},
	}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"web-id": true},
		startedAt:  map[string]string{"web-id": "started-0"},
		finishedAt: map[string]string{"web-id": "never"},
	}
	client := &Client{Docker: runner}
	var recorded []StepProgress
	beforeStep := func(progress StepProgress) error {
		recorded = append(recorded, progress)
		if progress.Step.Kind == StepPushImage {
			runner.startedAt["web-id"], runner.finishedAt["web-id"] = "started-9", "stopped-10"
		}
		return nil
	}
	plan := Plan{
		RunDir: t.TempDir(),
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:cache -> /data"},
			{Kind: StepPushImage, App: "api", Ref: "api"},
		},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
		BeforeStep: &beforeStep,
	}
	err := client.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "invalidated the transfer") {
		t.Fatalf("expected invalidated transfer refusal before the staged deploy, got %v", err)
	}
	if !runner.running["web-id"] {
		t.Fatal("source was not resumed after its transfer was invalidated")
	}
	last := recorded[len(recorded)-1]
	if last.Step.Kind != StepResumeSource || last.Index != 0 || last.Status != StepStatusStarted {
		t.Fatalf("invalidating resume was not recorded at the pause index before restart: %#v", recorded)
	}
}

func TestApplyRefusesRouteActivationWhenSourceRanAfterStagedHandoff(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "web-id"}}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"web-id": false, coolifyProxyContainer: true},
		startedAt:  map[string]string{"web-id": "started-0"},
		finishedAt: map[string]string{"web-id": "stopped-1"},
	}
	client := &Client{Docker: runner}
	beforeStep := func(progress StepProgress) error {
		if progress.Step.Kind == StepActivateRoutes {
			runner.startedAt["web-id"], runner.finishedAt["web-id"] = "started-9", "stopped-10"
		}
		return nil
	}
	plan := Plan{
		ResumeFrom: 3,
		RunDir:     t.TempDir(),
		Steps: []Step{
			{Kind: StepPauseSource, App: "api", Ref: "api"},
			{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
			{Kind: StepPushImage, App: "api", Ref: "api"},
			{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer},
			{Kind: StepActivateRoutes, App: "api", Ref: "routes"},
		},
		Prepare:    preparer.Result{Apps: []preparer.AppPlan{app}},
		BeforeStep: &beforeStep,
	}
	paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
	paused.entry("api").SourcePauseRecorded = true
	paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true, StartedAt: "started-0", FinishedAt: "stopped-1"}}
	if err := paused.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	err := client.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "staged data is stale") {
		t.Fatalf("expected stale staged handoff refusal before route activation, got %v", err)
	}
	if runner.running["web-id"] || fakeOutputCalled(&runner.fakeDockerRunner, "start", "web-id") {
		t.Fatalf("source that ran past the staged handoff was restarted: %#v", runner.outputArgs)
	}
	if !runner.running[coolifyProxyContainer] {
		t.Fatal("coolify proxy was left stopped after refusing route activation")
	}
}

func TestRouteActivationGateRechecksEveryHandedOffAppPerStep(t *testing.T) {
	api := preparer.AppPlan{Name: "api"}
	api.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "volume", SourceContainerID: "api-id"}}
	worker := preparer.AppPlan{Name: "worker"}
	worker.Resources.Volumes = []preparer.VolumeResource{{Service: "jobs", Type: "volume", SourceContainerID: "worker-id"}}
	runner := &sourceOwnershipRunner{
		running:    map[string]bool{"api-id": false, "worker-id": false},
		startedAt:  map[string]string{"api-id": "started-0", "worker-id": "started-0"},
		finishedAt: map[string]string{"api-id": "stopped-1", "worker-id": "stopped-1"},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{api, worker}}}, cache: map[string]*appCache{}}
	for app, id := range map[string]string{"api": "api-id", "worker": "worker-id"} {
		actx.entry(app).SourcePauseRecorded = true
		actx.entry(app).SourcePausedContainers = []sourcePausedContainer{{ID: id, Stopped: true, StartedAt: "started-0", FinishedAt: "stopped-1"}}
	}
	handedOff := map[string]struct{}{"api": {}, "worker": {}}
	first := Step{Kind: StepActivateRoutes, App: "api", Ref: "routes"}
	if err := client.requireTransferredSourceStillPaused(context.Background(), actx, first, pausedSources{}, handedOff); err != nil {
		t.Fatalf("first activation with quiet sources: %v", err)
	}
	if len(runner.outputArgs) != 1 || strings.Join(runner.outputArgs[0], " ") != "inspect --type container api-id worker-id" {
		t.Fatalf("expected one batched inspect of every handed-off source, got %#v", runner.outputArgs)
	}
	runner.startedAt["worker-id"], runner.finishedAt["worker-id"] = "started-9", "stopped-10"
	second := Step{Kind: StepActivateRoutes, App: "api", Ref: "routes"}
	err := client.requireTransferredSourceStillPaused(context.Background(), actx, second, pausedSources{}, handedOff)
	if err == nil || !strings.Contains(err.Error(), "source app worker ran after its state was handed") || !isUnsafeSourceResumeError(err) {
		t.Fatalf("a handed-off source that ran must block every later activation, not only its own, got %v", err)
	}
}
